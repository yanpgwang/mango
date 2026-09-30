package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

func TestSkillRepository_ImmutableLifecyclePagingAndDeleteGuard(t *testing.T) {
	store := testStore(t)
	repo := NewSkillRepository(store)
	ctx := context.Background()
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	skill := domain.Skill{
		ID: "skill_repo", CreatedAt: base, UpdatedAt: base,
		DisplayTitle: "Repository Skill", Source: "custom", TitleExplicit: true,
	}
	first := repositorySkillVersion(skill.ID, "100", base, true)
	if err := repo.BeginSkill(ctx, skill, first); err != nil {
		t.Fatalf("BeginSkill: %v", err)
	}
	if _, err := repo.GetSkill(ctx, skill.ID); err == nil {
		t.Fatal("creating Skill became visible before its archive committed")
	}
	readySkill, readyFirst, err := repo.CompleteVersion(ctx, skill.ID, first.Version, app.BlobInfo{
		SizeBytes: 10, ChecksumSHA256: "first",
	})
	if err != nil {
		t.Fatalf("CompleteVersion: %v", err)
	}
	if readySkill.LatestVersion != first.Version || readyFirst.State != domain.SkillVersionReady {
		t.Fatalf("completed = %+v, %+v", readySkill, readyFirst)
	}

	duplicate := skill
	duplicate.ID = "skill_duplicate"
	duplicate.CreatedAt = base.Add(time.Second)
	duplicate.UpdatedAt = duplicate.CreatedAt
	duplicateVersion := repositorySkillVersion(duplicate.ID, "150", duplicate.CreatedAt, true)
	if err := repo.BeginSkill(ctx, duplicate, duplicateVersion); err == nil {
		t.Fatal("duplicate explicit display_title was accepted")
	}

	second := repositorySkillVersion(skill.ID, "200", base.Add(2*time.Second), false)
	third := repositorySkillVersion(skill.ID, "300", base.Add(3*time.Second), false)
	for _, item := range []domain.SkillVersion{second, third} {
		if err := repo.BeginVersion(ctx, item); err != nil {
			t.Fatalf("BeginVersion %s: %v", item.Version, err)
		}
	}
	// Complete the newer Version first and the older Version last. Completion
	// order must not let an older upload steal latest_version.
	for _, item := range []domain.SkillVersion{third, second} {
		if _, _, err := repo.CompleteVersion(ctx, skill.ID, item.Version, app.BlobInfo{
			SizeBytes: 10, ChecksumSHA256: item.Version,
		}); err != nil {
			t.Fatalf("CompleteVersion %s: %v", item.Version, err)
		}
	}
	latest, err := repo.GetSkill(ctx, skill.ID)
	if err != nil || latest.LatestVersion != third.Version {
		t.Fatalf("latest after reverse completion = %+v, %v", latest, err)
	}
	page, err := repo.ListVersions(ctx, skill.ID, app.SkillVersionListQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertSkillVersions(t, page.Versions, "300", "200")
	if !page.HasNext {
		t.Fatal("first Version page missing next page")
	}
	last := page.Versions[len(page.Versions)-1]
	next, err := repo.ListVersions(ctx, skill.ID, app.SkillVersionListQuery{
		Limit: 2, After: &app.ResourcePageBoundary{CreatedAt: last.CreatedAt, ID: last.Version},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSkillVersions(t, next.Versions, "100")

	if _, err := repo.DeleteSkill(ctx, skill.ID); err == nil {
		t.Fatal("Skill with Versions was deleted")
	} else {
		var domainErr *domain.DomainError
		if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindValidation {
			t.Fatalf("delete guard error = %T %v", err, err)
		}
	}

	for _, version := range []string{"300", "200", "100"} {
		deleting, err := repo.BeginDeleteVersion(ctx, skill.ID, version)
		if err != nil || deleting.State != domain.SkillVersionDeleting {
			t.Fatalf("BeginDeleteVersion %s = %+v, %v", version, deleting, err)
		}
		if _, err := repo.GetVersion(ctx, skill.ID, version); err == nil {
			t.Fatalf("deleting Version %s remains visible", version)
		}
		if err := repo.RemoveIncompleteVersion(ctx, skill.ID, version); err != nil {
			t.Fatal(err)
		}
	}
	empty, err := repo.GetSkill(ctx, skill.ID)
	if err != nil || empty.LatestVersion != "" {
		t.Fatalf("empty Skill = %+v, %v", empty, err)
	}
	if _, err := repo.DeleteSkill(ctx, skill.ID); err != nil {
		t.Fatalf("DeleteSkill: %v", err)
	}
}

func TestAgentSkillPinsUseOneConnectionAndGuardDeletion(t *testing.T) {
	store := testStoreWithMaxConns(t, 1)
	skillRepo := NewSkillRepository(store)
	agentRepo := NewAgentRepository(store)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	base := time.Date(2026, 8, 4, 16, 0, 0, 0, time.UTC)
	skill := domain.Skill{
		ID: "skill_agent_pin", CreatedAt: base, UpdatedAt: base,
		DisplayTitle: "Agent Pin", Source: "custom", TitleExplicit: true,
	}
	first := repositorySkillVersion(skill.ID, "600", base, true)
	if err := skillRepo.BeginSkill(ctx, skill, first); err != nil {
		t.Fatal(err)
	}
	if _, _, err := skillRepo.CompleteVersion(ctx, skill.ID, first.Version, app.BlobInfo{
		SizeBytes: 10, ChecksumSHA256: "first",
	}); err != nil {
		t.Fatal(err)
	}
	skillService := app.NewSkillService(skillRepo, nil, &seqIDGen{}, fixedClock{})
	agents := app.NewAgentService(agentRepo, &seqIDGen{}, fixedClock{}, skillService)
	agent, err := agents.Create(ctx, domain.Agent{
		Name: "skill-agent", Model: domain.Model{ID: "claude-test"},
		Tools: []any{map[string]any{"type": domain.BuiltinToolsetType}},
		Skills: []domain.SkillReference{{
			Type: "custom", SkillID: skill.ID, Version: "latest",
		}},
	})
	if err != nil {
		t.Fatalf("create Agent with one pool connection: %v", err)
	}
	if _, err := skillRepo.BeginDeleteVersion(ctx, skill.ID, first.Version); err == nil {
		t.Fatal("deleted Skill Version pinned by an Agent")
	}

	second := repositorySkillVersion(skill.ID, "700", base.Add(time.Second), false)
	if err := skillRepo.BeginVersion(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, _, err := skillRepo.CompleteVersion(ctx, skill.ID, second.Version, app.BlobInfo{
		SizeBytes: 10, ChecksumSHA256: "second",
	}); err != nil {
		t.Fatal(err)
	}
	replacement := []domain.SkillReference{{
		Type: "custom", SkillID: skill.ID, Version: "latest",
	}}
	agent, err = agents.Update(ctx, agent.ID, domain.AgentPatch{Skills: &replacement})
	if err != nil {
		t.Fatalf("update Agent with one pool connection: %v", err)
	}
	if agent.Skills[0].Version != second.Version {
		t.Fatalf("updated Agent pin = %+v", agent.Skills)
	}
	if _, err := skillRepo.BeginDeleteVersion(ctx, skill.ID, first.Version); err != nil {
		t.Fatalf("delete released old Agent pin: %v", err)
	}
	if err := skillRepo.RemoveIncompleteVersion(ctx, skill.ID, first.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := agents.Archive(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := skillRepo.BeginDeleteVersion(ctx, skill.ID, second.Version); err != nil {
		t.Fatalf("delete Version after Agent archive: %v", err)
	}
}

func TestAgentSkillPinAndVersionDeletionLinearize(t *testing.T) {
	store := testStore(t)
	skillRepo := NewSkillRepository(store)
	agentRepo := NewAgentRepository(store)
	ctx := context.Background()
	base := time.Date(2026, 8, 4, 17, 0, 0, 0, time.UTC)
	skill := domain.Skill{
		ID: "skill_agent_pin_race", CreatedAt: base, UpdatedAt: base,
		DisplayTitle: "Agent Pin Race", Source: "custom", TitleExplicit: true,
	}
	version := repositorySkillVersion(skill.ID, "800", base, true)
	if err := skillRepo.BeginSkill(ctx, skill, version); err != nil {
		t.Fatal(err)
	}
	if _, _, err := skillRepo.CompleteVersion(ctx, skill.ID, version.Version, app.BlobInfo{
		SizeBytes: 10, ChecksumSHA256: "race",
	}); err != nil {
		t.Fatal(err)
	}
	agent := domain.Agent{
		ID: "agent_skill_pin_race", Version: 1, Name: "race",
		Model: domain.Model{ID: "claude-test"},
		Skills: []domain.SkillReference{{
			Type: "custom", SkillID: skill.ID, Version: version.Version,
		}},
		CreatedAt: base, UpdatedAt: base,
	}

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	var agentErr, deleteErr error
	go func() {
		defer wait.Done()
		<-start
		agentErr = agentRepo.PutVersion(ctx, agent)
	}()
	go func() {
		defer wait.Done()
		<-start
		_, deleteErr = skillRepo.BeginDeleteVersion(ctx, skill.ID, version.Version)
	}()
	close(start)
	wait.Wait()
	if (agentErr == nil) == (deleteErr == nil) {
		t.Fatalf("Agent/Delete race = agent:%v delete:%v; exactly one must commit", agentErr, deleteErr)
	}
	if agentErr == nil {
		if _, err := agentRepo.Archive(ctx, agent.ID, base.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := skillRepo.BeginDeleteVersion(ctx, skill.ID, version.Version); err != nil {
			t.Fatal(err)
		}
	}
	if err := skillRepo.RemoveIncompleteVersion(ctx, skill.ID, version.Version); err != nil {
		t.Fatal(err)
	}
}

func TestAgentArchiveSerializesWithVersionAndPinCreation(t *testing.T) {
	store := testStore(t)
	skillRepo := NewSkillRepository(store)
	agentRepo := NewAgentRepository(store)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	base := time.Date(2026, 8, 4, 17, 30, 0, 0, time.UTC)
	skill := domain.Skill{
		ID: "skill_archive_race", CreatedAt: base, UpdatedAt: base,
		DisplayTitle: "Archive Race", Source: "custom", TitleExplicit: true,
	}
	version := repositorySkillVersion(skill.ID, "850", base, true)
	if err := skillRepo.BeginSkill(ctx, skill, version); err != nil {
		t.Fatal(err)
	}
	if _, _, err := skillRepo.CompleteVersion(ctx, skill.ID, version.Version, app.BlobInfo{
		SizeBytes: 10, ChecksumSHA256: "archive-race",
	}); err != nil {
		t.Fatal(err)
	}
	agent := domain.Agent{
		ID: "agent_archive_race", Version: 1, Name: "before",
		Model: domain.Model{ID: "claude-test"},
		Skills: []domain.SkillReference{{
			Type: "custom", SkillID: skill.ID, Version: version.Version,
		}},
		CreatedAt: base, UpdatedAt: base,
	}
	if err := agentRepo.PutVersion(ctx, agent); err != nil {
		t.Fatal(err)
	}

	updateLocked := make(chan struct{})
	releaseUpdate := make(chan struct{})
	updateDone := make(chan error, 1)
	go func() {
		_, err := agentRepo.UpdateVersion(ctx, agent.ID, func(current domain.Agent) (domain.Agent, bool, error) {
			close(updateLocked)
			<-releaseUpdate
			current.Name = "after"
			current.UpdatedAt = base.Add(time.Second)
			return current, true, nil
		})
		updateDone <- err
	}()
	<-updateLocked

	archiveDone := make(chan error, 1)
	go func() {
		_, err := agentRepo.Archive(ctx, agent.ID, base.Add(2*time.Second))
		archiveDone <- err
	}()
	if err := waitForBlockedAgentStatement(ctx, store); err != nil {
		close(releaseUpdate)
		t.Fatal(err)
	}
	close(releaseUpdate)
	if err := <-updateDone; err != nil {
		t.Fatalf("concurrent Agent update: %v", err)
	}
	if err := <-archiveDone; err != nil {
		t.Fatalf("concurrent Agent archive: %v", err)
	}
	latest, err := agentRepo.Latest(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != 2 || latest.ArchivedAt == nil {
		t.Fatalf("latest Agent after Update/Archive race = %+v, want archived v2", latest)
	}
	var pins int
	if err := store.pool.QueryRow(ctx, `
SELECT count(*) FROM agent_skill_versions WHERE agent_id = $1`, agent.ID).Scan(&pins); err != nil {
		t.Fatal(err)
	}
	if pins != 0 {
		t.Fatalf("archived Agent retained %d Skill pins", pins)
	}
	if _, err := skillRepo.BeginDeleteVersion(ctx, skill.ID, version.Version); err != nil {
		t.Fatalf("delete Version after serialized archive: %v", err)
	}
}

func waitForBlockedAgentStatement(ctx context.Context, store *Store) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := store.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM pg_stat_activity
    WHERE application_name = current_setting('application_name')
      AND pid <> pg_backend_pid()
      AND wait_event_type = 'Lock'
      AND query ILIKE '%agents%'
)`).Scan(&waiting)
		if err != nil {
			return err
		}
		if waiting {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for blocked Agent statement: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestLegacyAgentSkillsSurviveReadAndUnrelatedUpdate(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 4, 19, 0, 0, 0, time.UTC)
	body := map[string]any{
		"ID": "agent_legacy_skills", "Version": 1, "Name": "legacy",
		"Model": map[string]any{"ID": "claude-test"},
		"Skills": []any{
			"former-provider-value",
			map[string]any{
				"type": "custom", "skill_id": "skill_old", "version": "1",
				"extension": true,
			},
		},
		"CreatedAt": base, "UpdatedAt": base,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `
	INSERT INTO agents (id, version, name, body, created_at, updated_at, workspace_id)
	VALUES ($1, 1, $2, $3, $4, $4, $5)`,
		"agent_legacy_skills", "legacy", encoded, base, workspace.DefaultID,
	); err != nil {
		t.Fatal(err)
	}
	repo := NewAgentRepository(store)
	agents := app.NewAgentService(repo, &seqIDGen{}, fixedClock{})
	persisted, err := agents.Get(ctx, "agent_legacy_skills")
	if err != nil || len(persisted.Skills) != 2 ||
		!persisted.Skills[0].IsLegacy() || !persisted.Skills[1].IsLegacy() {
		t.Fatalf("read legacy Agent Skills = %+v, %v", persisted.Skills, err)
	}
	name := "legacy-renamed"
	updated, err := agents.Update(ctx, persisted.ID, domain.AgentPatch{Name: &name})
	if err != nil {
		t.Fatalf("unrelated legacy Agent update: %v", err)
	}
	if updated.Version != 2 || len(updated.Skills) != 2 {
		t.Fatalf("updated legacy Agent = %+v", updated)
	}
	marshaled, err := json.Marshal(updated.Skills)
	if err != nil {
		t.Fatal(err)
	}
	var values []any
	if err := json.Unmarshal(marshaled, &values); err != nil {
		t.Fatal(err)
	}
	object, ok := values[1].(map[string]any)
	if !ok || object["extension"] != true {
		t.Fatalf("legacy Skill fields were lost: %s", marshaled)
	}
}

func TestLegacySessionSkillsRemainReadableAndDeletable(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sesn_legacy_skills")
	if err := json.Unmarshal(
		[]byte(`["former-provider-value"]`),
		&session.AgentSnapshot.Skills,
	); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `
	INSERT INTO sessions (id, status, body, created_at, updated_at, workspace_id)
	VALUES ($1, $2, $3, $4, $5, $6)`,
		session.ID, session.Status, body, session.CreatedAt, session.UpdatedAt,
		workspace.DefaultID,
	); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.GetSession(ctx, session.ID)
	if err != nil || len(persisted.AgentSnapshot.Skills) != 1 ||
		!persisted.AgentSnapshot.Skills[0].IsLegacy() {
		t.Fatalf("read legacy Session Skills = %+v, %v", persisted.AgentSnapshot.Skills, err)
	}
	if err := store.PrepareSessionDeletion(ctx, session.ID); err != nil {
		t.Fatalf("prepare legacy Session deletion: %v", err)
	}
	if err := store.FinalizeSessionDeletion(ctx, session.ID); err != nil {
		t.Fatalf("finalize legacy Session deletion: %v", err)
	}
}

func repositorySkillVersion(skillID, version string, createdAt time.Time, initial bool) domain.SkillVersion {
	return domain.SkillVersion{
		ID: version, SkillID: skillID, Version: version, CreatedAt: createdAt,
		Description: "Performs repository tests when requested.",
		Directory:   "repository-skill", Name: "repository-skill",
		BlobKey:               "skills/" + skillID + "/" + version + ".zip",
		UncompressedSizeBytes: 1024,
		State:                 domain.SkillVersionUploading,
		Initial:               initial,
	}
}

func assertSkillVersions(t *testing.T, items []domain.SkillVersion, want ...string) {
	t.Helper()
	if len(items) != len(want) {
		t.Fatalf("Version count = %d, want %d: %+v", len(items), len(want), items)
	}
	for index, version := range want {
		if items[index].Version != version {
			t.Fatalf("Version[%d] = %s, want %s", index, items[index].Version, version)
		}
	}
}

func TestSessionSkillPinsGuardVersionDeletionAndRollbackMissingReferences(t *testing.T) {
	store := testStore(t)
	repo := NewSkillRepository(store)
	ctx := context.Background()
	base := time.Date(2026, 8, 4, 13, 0, 0, 0, time.UTC)
	skill := domain.Skill{
		ID: "skill_session_pin", CreatedAt: base, UpdatedAt: base,
		DisplayTitle: "Session Pin", Source: "custom", TitleExplicit: true,
	}
	version := repositorySkillVersion(skill.ID, "400", base, true)
	if err := repo.BeginSkill(ctx, skill, version); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.CompleteVersion(ctx, skill.ID, version.Version, app.BlobInfo{
		SizeBytes: 10, ChecksumSHA256: "pin",
	}); err != nil {
		t.Fatal(err)
	}

	session := newSession("sesn_skill_pin")
	session.AgentSnapshot.Skills = []domain.SkillReference{{
		Type: "custom", SkillID: skill.ID, Version: version.Version,
	}}
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("CreateSession with Skill pin: %v", err)
	}
	persisted, err := store.GetSession(ctx, session.ID)
	if err != nil || len(persisted.AgentSnapshot.Skills) != 1 ||
		persisted.AgentSnapshot.Skills[0].Version != version.Version {
		t.Fatalf("persisted Session Skill pin = %+v, %v", persisted.AgentSnapshot.Skills, err)
	}
	runtimeSkills, err := store.SessionSkillsForRuntime(ctx, session.ID)
	if err != nil || len(runtimeSkills) != 1 ||
		runtimeSkills[0].SkillID != skill.ID ||
		runtimeSkills[0].Version != version.Version ||
		runtimeSkills[0].UncompressedSizeBytes != version.UncompressedSizeBytes {
		t.Fatalf("Session runtime Skills = %+v, err=%v", runtimeSkills, err)
	}
	if _, err := repo.BeginDeleteVersion(ctx, skill.ID, version.Version); err == nil {
		t.Fatal("deleted Skill Version referenced by Session")
	} else {
		var domainErr *domain.DomainError
		if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindValidation {
			t.Fatalf("delete referenced Version = %T %v", err, err)
		}
	}
	if err := store.PrepareSessionDeletion(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeSessionDeletion(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.BeginDeleteVersion(ctx, skill.ID, version.Version); err != nil {
		t.Fatalf("delete Version after Session release: %v", err)
	}
	if err := repo.RemoveIncompleteVersion(ctx, skill.ID, version.Version); err != nil {
		t.Fatal(err)
	}

	missing := newSession("sesn_missing_skill_pin")
	missing.AgentSnapshot.Skills = []domain.SkillReference{{
		Type: "custom", SkillID: skill.ID, Version: "missing",
	}}
	if _, err := store.CreateSession(ctx, missing, nil); err == nil {
		t.Fatal("Session with missing Skill Version was created")
	}
	if _, err := store.GetSession(ctx, missing.ID); err == nil {
		t.Fatal("failed Skill pin left a partial Session")
	}
}

func TestSessionSkillPinsCoverResolvedRosterExecutionScopes(t *testing.T) {
	store := testStore(t)
	repo := NewSkillRepository(store)
	ctx := context.Background()
	base := time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)
	skill := domain.Skill{
		ID: "skill_roster_scope", CreatedAt: base, UpdatedAt: base,
		DisplayTitle: "Roster Scope", Source: "custom", TitleExplicit: true,
	}
	primaryVersion := repositorySkillVersion(skill.ID, "600", base, true)
	if err := repo.BeginSkill(ctx, skill, primaryVersion); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.CompleteVersion(ctx, skill.ID, primaryVersion.Version, app.BlobInfo{
		SizeBytes: 10, ChecksumSHA256: "primary",
	}); err != nil {
		t.Fatal(err)
	}
	childVersion := repositorySkillVersion(
		skill.ID, "700", base.Add(time.Second), false,
	)
	if err := repo.BeginVersion(ctx, childVersion); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.CompleteVersion(ctx, skill.ID, childVersion.Version, app.BlobInfo{
		SizeBytes: 11, ChecksumSHA256: "child",
	}); err != nil {
		t.Fatal(err)
	}

	session := newSession("sesn_roster_skill_scopes")
	session.AgentSnapshot = domain.Agent{
		ID: session.AgentID, Version: session.AgentVersion, Name: "coordinator",
		Model: domain.NormalizeModel(domain.Model{ID: "claude-test"}),
		Skills: []domain.SkillReference{{
			Type: "custom", SkillID: skill.ID, Version: primaryVersion.Version,
		}},
		Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{
			Type: "agent", ID: "agent_skill_peer", Version: 3,
		}}},
	}
	session.MultiagentRoster = []domain.Agent{{
		ID: "agent_skill_peer", Version: 3, Name: "reviewer",
		Model: domain.NormalizeModel(domain.Model{ID: "claude-test"}),
		Skills: []domain.SkillReference{{
			Type: "custom", SkillID: skill.ID, Version: childVersion.Version,
		}},
	}}
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatal(err)
	}
	var pinCount int
	if err := store.pool.QueryRow(ctx, `
SELECT count(*) FROM session_skill_versions WHERE session_id = $1`,
		session.ID).Scan(&pinCount); err != nil {
		t.Fatal(err)
	}
	if pinCount != 2 {
		t.Fatalf("Session execution-scope pins = %d, want 2", pinCount)
	}
	primaryID, err := store.q.GetPrimarySessionThreadID(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	primaryRuntime, err := store.SessionThreadSkillRuntime(
		ctx, session.ID, primaryID,
	)
	if err != nil || primaryRuntime.Root != domain.SessionSkillsRoot ||
		len(primaryRuntime.Versions) != 1 ||
		primaryRuntime.Versions[0].Version != primaryVersion.Version {
		t.Fatalf("primary Skill runtime = %+v, err=%v", primaryRuntime, err)
	}
	child, _, err := store.CreateChildSessionThread(
		ctx, session.ID, primaryID, "reviewer",
	)
	if err != nil {
		t.Fatal(err)
	}
	childRuntime, err := store.SessionThreadSkillRuntime(
		ctx, session.ID, child.ID,
	)
	if err != nil || childRuntime.Root == domain.SessionSkillsRoot ||
		!strings.HasPrefix(childRuntime.Root, domain.SessionSkillsRoot+"/.agents/") ||
		len(childRuntime.Versions) != 1 ||
		childRuntime.Versions[0].Version != childVersion.Version {
		t.Fatalf("child Skill runtime = %+v, err=%v", childRuntime, err)
	}
	if _, err := repo.BeginDeleteVersion(ctx, skill.ID, childVersion.Version); err == nil {
		t.Fatal("roster Skill Version was not retained by the Session")
	}
}

func TestSessionSkillPinAndVersionDeletionLinearize(t *testing.T) {
	store := testStore(t)
	repo := NewSkillRepository(store)
	ctx := context.Background()
	base := time.Date(2026, 8, 4, 14, 0, 0, 0, time.UTC)
	skill := domain.Skill{
		ID: "skill_pin_race", CreatedAt: base, UpdatedAt: base,
		DisplayTitle: "Pin Race", Source: "custom", TitleExplicit: true,
	}
	version := repositorySkillVersion(skill.ID, "500", base, true)
	if err := repo.BeginSkill(ctx, skill, version); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.CompleteVersion(ctx, skill.ID, version.Version, app.BlobInfo{
		SizeBytes: 10, ChecksumSHA256: "race",
	}); err != nil {
		t.Fatal(err)
	}
	session := newSession("sesn_skill_pin_race")
	session.AgentSnapshot.Skills = []domain.SkillReference{{
		Type: "custom", SkillID: skill.ID, Version: version.Version,
	}}

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	var sessionErr, deleteErr error
	go func() {
		defer wait.Done()
		<-start
		_, sessionErr = store.CreateSession(ctx, session, nil)
	}()
	go func() {
		defer wait.Done()
		<-start
		_, deleteErr = repo.BeginDeleteVersion(ctx, skill.ID, version.Version)
	}()
	close(start)
	wait.Wait()
	if (sessionErr == nil) == (deleteErr == nil) {
		t.Fatalf("Session/Delete race = session:%v delete:%v; exactly one must commit", sessionErr, deleteErr)
	}
	if sessionErr == nil {
		if err := store.PrepareSessionDeletion(ctx, session.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.FinalizeSessionDeletion(ctx, session.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.BeginDeleteVersion(ctx, skill.ID, version.Version); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.RemoveIncompleteVersion(ctx, skill.ID, version.Version); err != nil {
		t.Fatal(err)
	}
}
