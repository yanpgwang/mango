package pg

import (
	"context"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestDeploymentSessionAndRunCommitAtomically(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	seedDeploymentDependencies(t, store, now)
	repo := NewDeploymentRepository(store)
	deployment, err := repo.Create(ctx, domain.Deployment{
		ID: "depl_atomic", AgentID: "agent_deployment", AgentVersion: 1,
		EnvironmentID: "env_deployment", Name: "Atomic", Status: domain.DeploymentStatusActive,
		InitialEvents: []domain.EventDraft{{
			Type: domain.EvUserMessage, Payload: map[string]any{"content": "run"},
		}},
		Resources: []domain.DeploymentResource{{
			Type:                    domain.SessionResourceTypeGitRepository,
			RepositoryURL:           "https://github.com/acme/widgets.git",
			RepositoryCheckoutType:  domain.GitRepositoryCheckoutBranch,
			RepositoryCheckoutValue: "main",
		}},
		Metadata: map[string]string{}, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create Deployment: %v", err)
	}
	storedDeployment, err := repo.Get(ctx, deployment.ID)
	if err != nil || len(storedDeployment.Resources) != 1 ||
		storedDeployment.Resources[0].RepositoryURL != "https://github.com/acme/widgets.git" ||
		storedDeployment.Resources[0].RepositoryCheckoutType != domain.GitRepositoryCheckoutBranch ||
		storedDeployment.Resources[0].RepositoryCheckoutValue != "main" {
		t.Fatalf("persisted Deployment repository template = %+v, %v", storedDeployment.Resources, err)
	}
	if err := NewEnvironmentRepository(store).DeleteIfUnreferenced(ctx, deployment.EnvironmentID); err == nil {
		t.Fatal("delete Deployment Environment succeeded; want conflict")
	} else if de, ok := err.(*domain.DomainError); !ok || de.Kind != domain.KindConflict {
		t.Fatalf("delete Deployment Environment = %v, want conflict", err)
	}
	sessionID := "sesn_deployment_atomic"
	session := domain.Session{
		ID: sessionID, AgentID: deployment.AgentID, AgentVersion: deployment.AgentVersion,
		EnvironmentID: deployment.EnvironmentID, DeploymentID: &deployment.ID,
		Status: domain.StatusIdle, Metadata: map[string]any{},
		AgentSnapshot: domain.Agent{
			ID: deployment.AgentID, Version: deployment.AgentVersion,
			Name: "Agent", Model: domain.Model{ID: "claude-sonnet-4-5"},
		},
		CreatedAt: now, UpdatedAt: now,
	}
	run := domain.DeploymentRun{
		ID: "drun_atomic", DeploymentID: deployment.ID,
		AgentID: deployment.AgentID, AgentVersion: deployment.AgentVersion,
		SessionID: &sessionID, TriggerType: domain.DeploymentTriggerManual,
		CreatedAt: now,
	}
	if _, err := store.CreateDeploymentSession(ctx, session, nil, run); err != nil {
		t.Fatalf("create Deployment Session: %v", err)
	}
	storedRun, err := repo.GetRun(ctx, run.ID)
	if err != nil || storedRun.SessionID == nil || *storedRun.SessionID != sessionID {
		t.Fatalf("stored Run = %+v, %v", storedRun, err)
	}
	filtered, err := store.ListSessions(ctx, app.ListPage{
		DeploymentID: &deployment.ID, Limit: 10,
	})
	if err != nil || len(filtered.Sessions) != 1 ||
		filtered.Sessions[0].DeploymentID == nil ||
		*filtered.Sessions[0].DeploymentID != deployment.ID {
		t.Fatalf("filtered Sessions = %+v, %v", filtered, err)
	}

	duplicate := session
	duplicate.ID = "sesn_duplicate"
	duplicate.CreatedAt, duplicate.UpdatedAt = now.Add(time.Second), now.Add(time.Second)
	duplicateRun := run
	duplicateRun.SessionID = &duplicate.ID
	if _, err := store.CreateDeploymentSession(
		ctx, duplicate, deployment.InitialEvents, duplicateRun,
	); err == nil {
		t.Fatal("duplicate run ID unexpectedly committed")
	}
	if _, err := store.GetSession(ctx, duplicate.ID); err == nil {
		t.Fatal("Session remained after Deployment Run insert rolled back")
	}
	if err := store.DeleteSession(ctx, sessionID); err != nil {
		t.Fatalf("delete successful Deployment Session: %v", err)
	}
	retainedRun, err := repo.GetRun(ctx, run.ID)
	if err != nil || retainedRun.SessionID == nil || *retainedRun.SessionID != sessionID {
		t.Fatalf("Run after Session deletion = %+v, %v", retainedRun, err)
	}
}

func TestDeploymentScheduleClaimsAreLeasedAndAdvanced(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	seedDeploymentDependencies(t, store, now)
	repo := NewDeploymentRepository(store)
	dueAt := now.Add(-time.Minute)
	item, err := repo.Create(ctx, domain.Deployment{
		ID: "depl_due", AgentID: "agent_deployment", AgentVersion: 1,
		EnvironmentID: "env_deployment", Name: "Due", Status: domain.DeploymentStatusActive,
		InitialEvents: []domain.EventDraft{{Type: domain.EvUserMessage}},
		Schedule: &domain.DeploymentSchedule{
			Expression: "* * * * *", Timezone: "UTC", UpcomingRunsAt: []time.Time{dueAt},
		},
		Metadata: map[string]string{}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := repo.ClaimDue(ctx, now, now.Add(-2*time.Minute), "dclaim_first", 10)
	if err != nil || len(claims) != 1 || claims[0].DeploymentID != item.ID ||
		!claims[0].ScheduledAt.Equal(dueAt) || claims[0].Token != "dclaim_first" {
		t.Fatalf("claims = %+v, %v", claims, err)
	}
	claimedAgain, err := repo.ClaimDue(
		ctx, now.Add(time.Second), now.Add(-2*time.Minute), "dclaim_second", 10,
	)
	if err != nil || len(claimedAgain) != 0 {
		t.Fatalf("claim before lease expiry = %+v, %v", claimedAgain, err)
	}
	laterUpdate := now.Add(30 * time.Second)
	if _, err := repo.Update(ctx, item.ID, func(current domain.Deployment) (domain.Deployment, bool, error) {
		current.Description = "updated while the scheduled run was in flight"
		current.UpdatedAt = laterUpdate
		return current, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.RenewScheduleClaim(
		ctx, item.ID, dueAt, "dclaim_first", now.Add(45*time.Second),
	); err != nil {
		t.Fatalf("ordinary update released the in-flight schedule claim: %v", err)
	}
	next := now.Add(time.Minute)
	if err := repo.CompleteSchedule(ctx, item.ID, dueAt, now, []time.Time{next}); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Get(ctx, item.ID)
	if err != nil || stored.Schedule.LastRunAt == nil || !stored.Schedule.LastRunAt.Equal(now) ||
		len(stored.Schedule.UpcomingRunsAt) != 1 || !stored.Schedule.UpcomingRunsAt[0].Equal(next) ||
		!stored.UpdatedAt.Equal(laterUpdate) {
		t.Fatalf("advanced Deployment = %+v, %v", stored, err)
	}
}

func TestDeploymentScheduleClaimRenewalFencesStaleOwner(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	seedDeploymentDependencies(t, store, now)
	repo := NewDeploymentRepository(store)
	dueAt := now.Add(-time.Minute)
	item, err := repo.Create(ctx, domain.Deployment{
		ID: "depl_renew", AgentID: "agent_deployment", AgentVersion: 1,
		EnvironmentID: "env_deployment", Name: "Renew", Status: domain.DeploymentStatusActive,
		InitialEvents: []domain.EventDraft{{Type: domain.EvUserMessage}},
		Schedule: &domain.DeploymentSchedule{
			Expression: "* * * * *", Timezone: "UTC", UpcomingRunsAt: []time.Time{dueAt},
		},
		Metadata: map[string]string{}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := repo.ClaimDue(ctx, now, now.Add(-2*time.Minute), "dclaim_old", 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("initial claim = %+v, %v", claims, err)
	}
	renewedAt := now.Add(90 * time.Second)
	if err := repo.RenewScheduleClaim(
		ctx, item.ID, dueAt, "dclaim_old", renewedAt,
	); err != nil {
		t.Fatalf("renew claim: %v", err)
	}
	claims, err = repo.ClaimDue(
		ctx, now.Add(3*time.Minute), now.Add(time.Minute), "dclaim_early", 1,
	)
	if err != nil || len(claims) != 0 {
		t.Fatalf("renewed claim was reclaimed early = %+v, %v", claims, err)
	}
	claims, err = repo.ClaimDue(
		ctx, now.Add(5*time.Minute), now.Add(3*time.Minute), "dclaim_new", 1,
	)
	if err != nil || len(claims) != 1 || claims[0].Token != "dclaim_new" {
		t.Fatalf("stale claim replacement = %+v, %v", claims, err)
	}
	if err := repo.RenewScheduleClaim(
		ctx, item.ID, dueAt, "dclaim_old", now.Add(6*time.Minute),
	); err == nil {
		t.Fatal("stale claim owner renewed after replacement")
	}
}

func seedDeploymentDependencies(t *testing.T, store *Store, now time.Time) {
	t.Helper()
	if err := NewAgentRepository(store).PutVersion(context.Background(), domain.Agent{
		ID: "agent_deployment", Version: 1, Name: "Agent",
		Model: domain.Model{ID: "claude-sonnet-4-5"}, Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := NewEnvironmentRepository(store).Put(context.Background(), domain.Environment{
		ID: "env_deployment", Name: "Environment", ConfigType: "cloud",
		Config: map[string]any{"type": "cloud"}, Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}
