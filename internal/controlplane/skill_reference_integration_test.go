package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
)

func TestPostgresAgentAndSessionSkillVersionResolution(t *testing.T) {
	fixture := newPostgresFixture(t)
	ctx := context.Background()
	skillRepo := pg.NewSkillRepository(fixture.store)
	base := time.Date(2026, 8, 4, 15, 0, 0, 0, time.UTC)
	skill := domain.Skill{
		ID: "skill_controlplane_pin", CreatedAt: base, UpdatedAt: base,
		DisplayTitle: "Control Plane Pin", Source: "custom", TitleExplicit: true,
	}
	first := controlplaneSkillVersion(skill.ID, "100", base, true)
	if err := skillRepo.BeginSkill(ctx, skill, first); err != nil {
		t.Fatal(err)
	}
	if _, _, err := skillRepo.CompleteVersion(ctx, skill.ID, first.Version, app.BlobInfo{
		SizeBytes: 10, ChecksumSHA256: "first",
	}); err != nil {
		t.Fatal(err)
	}
	skillService := app.NewSkillService(skillRepo, nil, fixture.ids, fixture.clock)
	agents := app.NewAgentService(fixture.agentRepo, fixture.ids, fixture.clock, skillService)
	agent, err := agents.Create(ctx, domain.Agent{
		Name: "skill-agent", Model: domain.Model{ID: "claude-test"},
		Tools: []any{map[string]any{"type": domain.BuiltinToolsetType}},
		Skills: []domain.SkillReference{{
			Type: "custom", SkillID: skill.ID, Version: "latest",
		}},
	})
	if err != nil {
		t.Fatalf("create Agent: %v", err)
	}
	if agent.Skills[0].Version != first.Version {
		t.Fatalf("Agent pin = %+v", agent.Skills)
	}

	second := controlplaneSkillVersion(skill.ID, "200", base.Add(time.Second), false)
	if err := skillRepo.BeginVersion(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, _, err := skillRepo.CompleteVersion(ctx, skill.ID, second.Version, app.BlobInfo{
		SizeBytes: 20, ChecksumSHA256: "second",
	}); err != nil {
		t.Fatal(err)
	}
	environments := app.NewEnvironmentService(
		fixture.environmentRepo,
		fixture.ids,
		fixture.clock,
		app.EnvironmentCapabilities{PackageSetup: true, LimitedNetwork: true},
	)
	environment, err := environments.Create(ctx, domain.Environment{
		Name: "cloud", ConfigType: "cloud", Config: map[string]any{"type": "cloud"},
	})
	if err != nil {
		t.Fatalf("create Environment: %v", err)
	}
	sessions := NewSessionService(
		fixture.store,
		fixture.agentRepo,
		fixture.environmentRepo,
		temporalpkg.NewOrchestrator(fixture.store, nil),
		fixture.ids,
		fixture.clock,
		skillService,
	)
	inherited, err := sessions.Create(ctx, app.CreateSessionInput{
		AgentID: agent.ID, EnvironmentID: environment.ID,
	})
	if err != nil {
		t.Fatalf("create inherited Session: %v", err)
	}
	if inherited.AgentSnapshot.Skills[0].Version != first.Version {
		t.Fatalf("inherited Session pin = %+v", inherited.AgentSnapshot.Skills)
	}
	overrideSkills := []domain.SkillReference{{
		Type: "custom", SkillID: skill.ID, Version: "latest",
	}}
	overridden, err := sessions.Create(ctx, app.CreateSessionInput{
		AgentID: agent.ID, EnvironmentID: environment.ID,
		Overrides: &domain.AgentOverrides{Skills: &overrideSkills},
	})
	if err != nil {
		t.Fatalf("create overridden Session: %v", err)
	}
	if overridden.AgentSnapshot.Skills[0].Version != second.Version {
		t.Fatalf("overridden Session pin = %+v", overridden.AgentSnapshot.Skills)
	}

	// Cloud bundle materialization is adapter-gated. External worker Skill
	// activation is not implemented and must fail before Session admission.
	sessions.ConfigureCloudSkillBundles(false)
	if _, err := sessions.Create(ctx, app.CreateSessionInput{
		AgentID: agent.ID, EnvironmentID: environment.ID,
	}); err == nil {
		t.Fatal("cloud Session accepted Skills without adapter capability")
	}
	selfHosted, err := environments.Create(ctx, domain.Environment{
		Name: "self hosted", ConfigType: "self_hosted",
		Config: map[string]any{"type": "self_hosted"}, Scope: "account",
	})
	if err != nil {
		t.Fatalf("create self-hosted Environment: %v", err)
	}
	_, err = sessions.Create(ctx, app.CreateSessionInput{
		AgentID: agent.ID, EnvironmentID: selfHosted.ID,
		InitialEvents: []domain.EventDraft{{
			Type: domain.EvUserMessage, Payload: map[string]any{"content": "use the Skill"},
		}},
	})
	var capabilityErr *domain.DomainError
	if !errors.As(err, &capabilityErr) || capabilityErr.Kind != domain.KindUnsupported {
		t.Fatalf("self-hosted Session with Skill must fail admission: %v", err)
	}
	work, err := pg.NewEnvironmentWorkRepository(fixture.store).ListWork(
		ctx, selfHosted.ID, app.EnvironmentWorkListQuery{Limit: 10},
	)
	if err != nil || len(work.Work) != 0 {
		t.Fatalf("self-hosted Skill Work = %+v, err=%v", work, err)
	}
	for _, version := range []string{first.Version, second.Version} {
		if _, err := skillRepo.BeginDeleteVersion(ctx, skill.ID, version); err == nil {
			t.Fatalf("deleted Version %s while a Session pins it", version)
		}
	}
}

func controlplaneSkillVersion(
	skillID string,
	version string,
	createdAt time.Time,
	initial bool,
) domain.SkillVersion {
	return domain.SkillVersion{
		ID: version, SkillID: skillID, Version: version, CreatedAt: createdAt,
		Description: "Resolves and pins a custom Skill Version.",
		Directory:   "controlplane-pin", Name: "controlplane-pin",
		BlobKey: "skills/" + skillID + "/" + version + ".zip",
		State:   domain.SkillVersionUploading, Initial: initial,
	}
}
