package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

type mutationAwareAgentRepository struct {
	*memoryAgentRepository
	inMutation bool
}

func (r *mutationAwareAgentRepository) UpdateVersion(
	ctx context.Context,
	id string,
	update func(domain.Agent) (domain.Agent, bool, error),
) (domain.Agent, error) {
	return r.memoryAgentRepository.UpdateVersion(ctx, id, func(agent domain.Agent) (domain.Agent, bool, error) {
		r.inMutation = true
		defer func() { r.inMutation = false }()
		return update(agent)
	})
}

type mutationAwareSkillResolver struct {
	repo               *mutationAwareAgentRepository
	resolvedInMutation bool
}

func (r *mutationAwareSkillResolver) ResolveSkillReferences(
	_ context.Context,
	references []domain.SkillReference,
) ([]domain.SkillReference, error) {
	if r.repo.inMutation {
		r.resolvedInMutation = true
	}
	resolved := make([]domain.SkillReference, len(references))
	for index, reference := range references {
		reference.Version = "100"
		resolved[index] = reference
	}
	return resolved, nil
}

type mutableSkillResolver struct {
	latest map[string]string
}

func (r *mutableSkillResolver) ResolveSkillReferences(
	_ context.Context,
	references []domain.SkillReference,
) ([]domain.SkillReference, error) {
	resolved := make([]domain.SkillReference, len(references))
	for index, reference := range references {
		version := reference.Version
		if version == "" || version == "latest" {
			version = r.latest[reference.SkillID]
		}
		if version == "" {
			return nil, domain.Validation("custom Skill Version not found")
		}
		resolved[index] = domain.SkillReference{
			Type: "custom", SkillID: reference.SkillID, Version: version,
		}
	}
	return resolved, nil
}

func TestAgentService_ResolvesAndPinsSkillVersions(t *testing.T) {
	ctx := context.Background()
	resolver := &mutableSkillResolver{latest: map[string]string{"skill_reports": "100"}}
	service := NewAgentService(
		newMemoryAgentRepository(),
		domain.NewSeqIDGen(),
		domain.FixedClock{T: time.Unix(1, 0).UTC()},
		resolver,
	)
	agent, err := service.Create(ctx, domain.Agent{
		Name: "reports", Model: domain.Model{ID: "model"},
		Tools: []any{map[string]any{"type": domain.BuiltinToolsetType}},
		Skills: []domain.SkillReference{{
			Type: "custom", SkillID: "skill_reports", Version: "latest",
		}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := agent.Skills[0].Version; got != "100" {
		t.Fatalf("created Skill Version = %q, want 100", got)
	}

	resolver.latest["skill_reports"] = "200"
	name := "renamed"
	updated, err := service.Update(ctx, agent.ID, domain.AgentPatch{Name: &name})
	if err != nil {
		t.Fatalf("unrelated Update: %v", err)
	}
	if updated.Version != 2 || updated.Skills[0].Version != "100" {
		t.Fatalf("unrelated Update changed pin: %+v", updated)
	}

	replacement := []domain.SkillReference{{
		Type: "custom", SkillID: "skill_reports", Version: "latest",
	}}
	updated, err = service.Update(ctx, agent.ID, domain.AgentPatch{Skills: &replacement})
	if err != nil {
		t.Fatalf("Skill Update: %v", err)
	}
	if updated.Version != 3 || updated.Skills[0].Version != "200" {
		t.Fatalf("Skill Update = %+v, want Agent v3 pinned to 200", updated)
	}
	noOp, err := service.Update(ctx, agent.ID, domain.AgentPatch{Skills: &replacement})
	if err != nil || noOp.Version != 3 {
		t.Fatalf("same resolved Skill update = v%d, %v; want no-op v3", noOp.Version, err)
	}
}

func TestAgentService_ResolvesSkillsBeforeRepositoryMutation(t *testing.T) {
	repo := &mutationAwareAgentRepository{memoryAgentRepository: newMemoryAgentRepository()}
	resolver := &mutationAwareSkillResolver{repo: repo}
	service := NewAgentService(
		repo,
		domain.NewSeqIDGen(),
		domain.FixedClock{T: time.Unix(1, 0).UTC()},
		resolver,
	)
	agent, err := service.Create(context.Background(), domain.Agent{
		Name: "reports", Model: domain.Model{ID: "model"},
		Tools: []any{map[string]any{"type": domain.BuiltinToolsetType}},
	})
	if err != nil {
		t.Fatal(err)
	}
	replacement := []domain.SkillReference{{
		Type: "custom", SkillID: "skill_reports", Version: "latest",
	}}
	if _, err := service.Update(context.Background(), agent.ID, domain.AgentPatch{
		Skills: &replacement,
	}); err != nil {
		t.Fatal(err)
	}
	if resolver.resolvedInMutation {
		t.Fatal("Skill resolver ran while the Agent repository mutation was holding its serialization boundary")
	}
}

func TestResolveAgentSkillReferences_ValidationAndUnsupportedProviders(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		references []domain.SkillReference
		resolver   SkillReferenceResolver
		kind       domain.ErrKind
	}{
		{
			name: "unknown type", references: []domain.SkillReference{{Type: "third_party", SkillID: "x"}},
			resolver: &mutableSkillResolver{}, kind: domain.KindValidation,
		},
		{
			name: "missing id", references: []domain.SkillReference{{Type: "custom"}},
			resolver: &mutableSkillResolver{}, kind: domain.KindValidation,
		},
		{
			name: "anthropic", references: []domain.SkillReference{{Type: "anthropic", SkillID: "xlsx"}},
			resolver: &mutableSkillResolver{}, kind: domain.KindUnsupported,
		},
		{
			name: "custom resolver disabled", references: []domain.SkillReference{{Type: "custom", SkillID: "skill_x"}},
			kind: domain.KindUnsupported,
		},
	}
	tooMany := make([]domain.SkillReference, MaxSessionSkills+1)
	for index := range tooMany {
		tooMany[index] = domain.SkillReference{Type: "custom", SkillID: "skill_x"}
	}
	cases = append(cases, struct {
		name       string
		references []domain.SkillReference
		resolver   SkillReferenceResolver
		kind       domain.ErrKind
	}{name: "too many", references: tooMany, resolver: &mutableSkillResolver{}, kind: domain.KindValidation})

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveAgentSkillReferences(ctx, test.resolver, test.references)
			var domainErr *domain.DomainError
			if !errors.As(err, &domainErr) || domainErr.Kind != test.kind {
				t.Fatalf("error = %T %v, want DomainError kind %v", err, err, test.kind)
			}
		})
	}
}

func TestSkillService_ResolvesLatestAndExplicitVersions(t *testing.T) {
	repo := newMemorySkillRepository()
	repo.skills["skill_a"] = domain.Skill{
		ID: "skill_a", Source: "custom", Ready: true, LatestVersion: "200",
	}
	repo.versions["skill_a"] = map[string]domain.SkillVersion{
		"100": {
			ID: "100", SkillID: "skill_a", Version: "100", Name: "skill-a-v1",
			Description: "first version", UncompressedSizeBytes: 1,
			State: domain.SkillVersionReady,
		},
		"200": {
			ID: "200", SkillID: "skill_a", Version: "200", Name: "skill-a-v2",
			Description: "second version", UncompressedSizeBytes: 1,
			State: domain.SkillVersionReady,
		},
	}
	service := NewSkillService(repo, nil, domain.NewSeqIDGen(), domain.FixedClock{})
	resolved, err := service.ResolveSkillReferences(context.Background(), []domain.SkillReference{
		{Type: "custom", SkillID: "skill_a"},
		{Type: "custom", SkillID: "skill_a", Version: "100"},
	})
	if err != nil {
		t.Fatalf("ResolveSkillReferences: %v", err)
	}
	if resolved[0].Version != "200" || resolved[1].Version != "100" {
		t.Fatalf("resolved = %+v", resolved)
	}
	if _, err := service.ResolveSkillReferences(context.Background(), []domain.SkillReference{{
		Type: "custom", SkillID: "skill_a", Version: "missing",
	}}); err == nil {
		t.Fatal("missing explicit Version was accepted")
	}
}

func TestSkillServiceRejectsNegativeExpandedSize(t *testing.T) {
	repo := newMemorySkillRepository()
	repo.skills["skill_size"] = domain.Skill{
		ID: "skill_size", Source: "custom", Ready: true, LatestVersion: "100",
	}
	repo.versions["skill_size"] = map[string]domain.SkillVersion{
		"100": {
			ID: "100", SkillID: "skill_size", Version: "100", Name: "skill-size",
			UncompressedSizeBytes: -1, State: domain.SkillVersionReady,
		},
	}
	service := NewSkillService(repo, nil, domain.NewSeqIDGen(), domain.FixedClock{})
	_, err := service.ResolveSkillReferences(context.Background(), []domain.SkillReference{{
		Type: "custom", SkillID: "skill_size", Version: "100",
	}})
	var domainErr *domain.DomainError
	if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindTooLarge {
		t.Fatalf("negative expanded size error = %v, want size rejection", err)
	}
}

func TestSkillService_RejectsRuntimeNameAndAggregateLimitConflicts(t *testing.T) {
	repo := newMemorySkillRepository()
	for _, id := range []string{"skill_a", "skill_b"} {
		repo.skills[id] = domain.Skill{
			ID: id, Source: "custom", Ready: true, LatestVersion: "100",
		}
		repo.versions[id] = map[string]domain.SkillVersion{
			"100": {
				ID: "100", SkillID: id, Version: "100", Name: "shared-name",
				Description: "shared", UncompressedSizeBytes: 1,
				State: domain.SkillVersionReady,
			},
		}
	}
	service := NewSkillService(repo, nil, domain.NewSeqIDGen(), domain.FixedClock{})
	_, err := service.ResolveSkillReferences(context.Background(), []domain.SkillReference{
		{Type: "custom", SkillID: "skill_a"},
		{Type: "custom", SkillID: "skill_b"},
	})
	var domainErr *domain.DomainError
	if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindValidation {
		t.Fatalf("runtime name conflict = %T %v", err, err)
	}

	repo.versions["skill_b"]["100"] = domain.SkillVersion{
		ID: "100", SkillID: "skill_b", Version: "100", Name: "large-skill",
		Description: "large", UncompressedSizeBytes: MaxSessionSkillBytes,
		State: domain.SkillVersionReady,
	}
	_, err = service.ResolveSkillReferences(context.Background(), []domain.SkillReference{
		{Type: "custom", SkillID: "skill_a"},
		{Type: "custom", SkillID: "skill_b"},
	})
	if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindTooLarge {
		t.Fatalf("expanded size limit = %T %v", err, err)
	}

}
