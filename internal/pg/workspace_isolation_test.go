package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

func TestWorkspaceIsolationAcrossTopLevelResources(t *testing.T) {
	system := testStore(t)
	ctx := context.Background()
	workspaceA, err := system.CreateWorkspace(ctx, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	workspaceB, err := system.CreateWorkspace(ctx, "team-b")
	if err != nil {
		t.Fatal(err)
	}
	ctxA := workspace.WithScope(ctx, workspaceA.ID)
	ctxB := workspace.WithScope(ctx, workspaceB.ID)
	store := NewStore(system.pool, &seqIDGen{}, fixedClock{})
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	agent := domain.Agent{
		ID: "agent_team_a", Version: 1, Name: "Agent A",
		Model: domain.Model{ID: "claude-test"}, CreatedAt: now, UpdatedAt: now,
	}
	agents := NewAgentRepository(store)
	if err := agents.PutVersion(ctxA, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := agents.Latest(ctxB, agent.ID); !isNotFound(err) {
		t.Fatalf("cross-workspace Agent get = %v, want not found", err)
	}

	environment := domain.Environment{
		ID: "env_team_a", Name: "Environment A", ConfigType: "self_hosted",
		Config: map[string]any{"type": "self_hosted"}, CreatedAt: now, UpdatedAt: now,
	}
	environments := NewEnvironmentRepository(store)
	if err := environments.Put(ctxA, environment); err != nil {
		t.Fatal(err)
	}
	if _, err := environments.Get(ctxB, environment.ID); !isNotFound(err) {
		t.Fatalf("cross-workspace Environment get = %v, want not found", err)
	}

	session := domain.Session{
		ID: "sesn_team_a", AgentID: agent.ID, AgentVersion: agent.Version,
		EnvironmentID: environment.ID, AgentSnapshot: agent,
		Status: domain.StatusIdle, Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := store.CreateAPISession(ctxA, session, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSessionForWorkspace(ctxB, session.ID); !isNotFound(err) {
		t.Fatalf("cross-workspace Session get = %v, want not found", err)
	}
	page, err := store.ListSessions(ctxB, app.ListPage{Limit: 10})
	if err != nil || len(page.Sessions) != 0 {
		t.Fatalf("workspace B Sessions = %+v, %v; want empty", page, err)
	}
	crossSession := session
	crossSession.ID = "sesn_team_b_cross_reference"
	if _, err := store.CreateAPISession(ctxB, crossSession, nil); err == nil {
		t.Fatal("workspace B created a Session using workspace A dependencies")
	}
	files := NewFileRepository(store)
	file := domain.File{
		ID: "file_team_a", Filename: "a.txt", MimeType: "text/plain",
		BlobKey: workspace.BlobKey(ctxA, "files/file_team_a"),
		State:   domain.FileStateUploading, CreatedAt: now, UpdatedAt: now,
	}
	if err := files.BeginUpload(ctxA, file); err != nil {
		t.Fatal(err)
	}
	if _, err := files.CompleteUpload(ctxA, file.ID, app.BlobInfo{}); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Get(ctxB, file.ID); !isNotFound(err) {
		t.Fatalf("cross-workspace File get = %v, want not found", err)
	}
	fileService := app.NewFileService(files, nil, &seqIDGen{}, fixedClock{})
	if _, err := fileService.Download(ctxB, file.ID); !isNotFound(err) {
		t.Fatalf("cross-workspace File download = %v, want not found before opening blob storage", err)
	}
	if _, err := fileService.ReadOutcomeRubric(ctxB, file.ID); !isNotFound(err) {
		t.Fatalf("cross-workspace outcome rubric read = %v, want not found", err)
	}
	if _, err := fileService.ReadMessageFile(ctxB, file.ID); !isNotFound(err) {
		t.Fatalf("cross-workspace message File read = %v, want not found", err)
	}

	skills := NewSkillRepository(store)
	skill := domain.Skill{
		ID: "skill_team_a", DisplayTitle: "Skill A", Source: "custom",
		CreatedAt: now, UpdatedAt: now,
	}
	version := domain.SkillVersion{
		ID: "1", SkillID: skill.ID, Version: "1", Name: "skill-a",
		BlobKey: workspace.BlobKey(ctxA, "skills/skill_team_a/1.zip"),
		State:   domain.SkillVersionUploading, Initial: true, CreatedAt: now,
	}
	if err := skills.BeginSkill(ctxA, skill, version); err != nil {
		t.Fatal(err)
	}
	if _, _, err := skills.CompleteVersion(ctxA, skill.ID, version.Version, app.BlobInfo{}); err != nil {
		t.Fatal(err)
	}
	if _, err := skills.GetSkill(ctxB, skill.ID); !isNotFound(err) {
		t.Fatalf("cross-workspace Skill get = %v, want not found", err)
	}

	memory := NewMemoryRepository(store)
	memoryStore := domain.MemoryStore{
		ID: "memstore_team_a", Name: "Memory A", Metadata: map[string]string{},
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := memory.CreateStore(ctxA, memoryStore); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.GetStore(ctxB, memoryStore.ID); !isNotFound(err) {
		t.Fatalf("cross-workspace Memory Store get = %v, want not found", err)
	}

	vaults := NewVaultRepository(store)
	vault := domain.Vault{
		ID: "vlt_team_a", DisplayName: "Vault A", Metadata: map[string]string{},
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := vaults.CreateVault(ctxA, vault); err != nil {
		t.Fatal(err)
	}
	if _, err := vaults.GetVault(ctxB, vault.ID); !isNotFound(err) {
		t.Fatalf("cross-workspace Vault get = %v, want not found", err)
	}

	deployments := NewDeploymentRepository(store)
	deployment := domain.Deployment{
		ID: "depl_team_a", AgentID: agent.ID, AgentVersion: agent.Version,
		EnvironmentID: environment.ID, Name: "Deployment A",
		Status: domain.DeploymentStatusActive, Metadata: map[string]string{},
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := deployments.Create(ctxA, deployment); err != nil {
		t.Fatal(err)
	}
	if _, err := system.pool.Exec(ctx, `
INSERT INTO deployments (
    id, workspace_id, agent_id, agent_version, environment_id,
    status, body, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, 'active', '{}', $6, $6)`,
		"depl_team_b_raw_cross_reference", workspaceB.ID, agent.ID,
		agent.Version, environment.ID, now,
	); err == nil {
		t.Fatal("database accepted a cross-workspace Deployment dependency")
	}
	if _, err := deployments.Get(ctxB, deployment.ID); !isNotFound(err) {
		t.Fatalf("cross-workspace Deployment get = %v, want not found", err)
	}
	deploymentPage, err := deployments.List(ctxB, app.DeploymentListQuery{Limit: 10})
	if err != nil || len(deploymentPage.Deployments) != 0 {
		t.Fatalf("workspace B Deployments = %+v, %v; want empty", deploymentPage, err)
	}
}

func TestWorkspaceAPIKeysShareOnlyTheirWorkspace(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	item, err := store.CreateWorkspace(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	first, firstSecret, err := store.CreateAPIKey(ctx, item.ID, "first")
	if err != nil {
		t.Fatal(err)
	}
	_, secondSecret, err := store.CreateAPIKey(ctx, item.ID, "second")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{firstSecret, secondSecret} {
		got, err := store.AuthenticateAPIKey(ctx, secret)
		if err != nil || got != item.ID {
			t.Fatalf("AuthenticateAPIKey = %q, %v; want %q", got, err, item.ID)
		}
	}
	if err := store.RevokeAPIKey(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateAPIKey(ctx, firstSecret); !errors.Is(err, workspace.ErrInvalidAPIKey) {
		t.Fatalf("revoked key error = %v, want ErrInvalidAPIKey", err)
	}
	if got, err := store.AuthenticateAPIKey(ctx, secondSecret); err != nil || got != item.ID {
		t.Fatalf("second key after revoke = %q, %v", got, err)
	}
}

func TestSystemStoreTenantAccessFailsClosedWithoutWorkspace(t *testing.T) {
	defaultStore := testStore(t)
	system := NewSystemStore(defaultStore.pool, &seqIDGen{}, fixedClock{})
	agents := NewAgentRepository(system)
	ctx := context.Background()

	if _, err := agents.Latest(ctx, "agent_missing_scope"); !errors.Is(err, workspace.ErrMissingScope) {
		t.Fatalf("unscoped system Agent read error = %v, want ErrMissingScope", err)
	}
	if err := agents.PutVersion(ctx, domain.Agent{
		ID: "agent_missing_scope", Version: 1, Name: "unscoped",
		Model:     domain.Model{ID: "claude-test"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); !errors.Is(err, workspace.ErrMissingScope) {
		t.Fatalf("unscoped system Agent write error = %v, want ErrMissingScope", err)
	}

	scoped := workspace.WithScope(ctx, workspace.DefaultID)
	if err := agents.PutVersion(scoped, domain.Agent{
		ID: "agent_explicit_scope", Version: 1, Name: "scoped",
		Model:     domain.Model{ID: "claude-test"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("explicitly scoped system Agent write: %v", err)
	}
	if _, err := agents.Latest(scoped, "agent_explicit_scope"); err != nil {
		t.Fatalf("explicitly scoped system Agent read: %v", err)
	}
}

func TestWorkspaceCompositeConstraintsAreValidated(t *testing.T) {
	store := testStore(t)
	rows, err := store.pool.Query(context.Background(), `
SELECT conname, convalidated
FROM pg_constraint
WHERE conname = ANY($1)
  AND connamespace = current_schema()::regnamespace
ORDER BY conname`, []string{
		"deployments_agent_workspace_fk",
		"deployments_environment_workspace_fk",
		"sessions_deployment_workspace_fk",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var name string
		var validated bool
		if err := rows.Scan(&name, &validated); err != nil {
			t.Fatal(err)
		}
		if !validated {
			t.Fatalf("constraint %s is not validated", name)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("validated composite constraint count = %d, want 3", count)
	}
}

func isNotFound(err error) bool {
	var domainErr *domain.DomainError
	return errors.As(err, &domainErr) && domainErr.Kind == domain.KindNotFound
}
