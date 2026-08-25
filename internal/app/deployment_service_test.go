package app

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

func TestDeploymentServicePinsAgentAndCreatesDeploymentSession(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	repositoryMount := "/workspace/audit"
	repo := newMemoryDeploymentRepository()
	sessions := &deploymentSessionCreatorFake{}
	service := newTestDeploymentService(t, repo, sessions, now)

	item, err := service.Create(context.Background(), DeploymentCreateInput{
		AgentID: "agent_test", EnvironmentID: "env_test", Name: "Hourly audit",
		Budget: &domain.SessionBudget{MaxListCostCents: 500},
		Resources: []domain.DeploymentResource{{
			Type:                    domain.SessionResourceTypeGitRepository,
			RepositoryURL:           "https://github.com/acme/widgets.git",
			RepositoryCheckoutType:  domain.GitRepositoryCheckoutBranch,
			RepositoryCheckoutValue: "main", MountPath: &repositoryMount,
		}},
		InitialEvents: []domain.EventDraft{{
			Type: domain.EvUserMessage,
			Payload: map[string]any{"content": []any{map[string]any{
				"type": "text", "text": "Audit the repository",
			}}},
		}},
		Schedule: &domain.DeploymentSchedule{
			Expression: "0 * * * *", Timezone: "America/Los_Angeles",
		},
	})
	if err != nil {
		t.Fatalf("create Deployment: %v", err)
	}
	if item.AgentVersion != 2 {
		t.Fatalf("pinned Agent version = %d, want 2", item.AgentVersion)
	}
	if item.Budget == nil || item.Budget.MaxListCostCents != 500 {
		t.Fatalf("Deployment budget = %+v", item.Budget)
	}
	if len(item.Schedule.UpcomingRunsAt) != 5 ||
		!item.Schedule.UpcomingRunsAt[0].After(now) {
		t.Fatalf("upcoming runs = %v", item.Schedule.UpcomingRunsAt)
	}
	updatedName := "Hourly audit v2"
	firstUpdate, err := service.Update(context.Background(), item.ID, domain.DeploymentPatch{
		Name: &updatedName,
	})
	if err != nil {
		t.Fatalf("first Deployment update: %v", err)
	}
	updatedDescription := "second update in the same clock tick"
	secondUpdate, err := service.Update(context.Background(), item.ID, domain.DeploymentPatch{
		Description: &updatedDescription,
	})
	if err != nil {
		t.Fatalf("second Deployment update: %v", err)
	}
	if !secondUpdate.UpdatedAt.After(firstUpdate.UpdatedAt) {
		t.Fatalf("updated_at did not advance: first=%v second=%v", firstUpdate.UpdatedAt, secondUpdate.UpdatedAt)
	}
	item = secondUpdate
	if _, err := service.RunScheduled(context.Background(), item.ID, item.Schedule.UpcomingRunsAt[0]); err != nil {
		t.Fatalf("successful scheduled run: %v", err)
	}
	if stored, err := service.Get(context.Background(), item.ID); err != nil ||
		stored.Status != domain.DeploymentStatusActive || stored.PausedReason != nil {
		t.Fatalf("Deployment after successful scheduled run = %+v, %v", stored, err)
	}

	run, err := service.Run(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("manual run: %v", err)
	}
	if run.SessionID == nil || *run.SessionID != "sesn_deployment" {
		t.Fatalf("run session = %+v", run.SessionID)
	}
	if sessions.last.DeploymentID == nil || *sessions.last.DeploymentID != item.ID ||
		sessions.last.DeploymentRun == nil || sessions.last.AgentVersion == nil ||
		*sessions.last.AgentVersion != 2 || sessions.last.Budget == nil ||
		sessions.last.Budget.MaxListCostCents != 500 {
		t.Fatalf("Session create input = %+v", sessions.last)
	}
	if len(sessions.last.RepositoryResources) != 1 ||
		sessions.last.RepositoryResources[0].URL != "https://github.com/acme/widgets.git" ||
		sessions.last.RepositoryResources[0].Checkout == nil ||
		sessions.last.RepositoryResources[0].Checkout.Type != domain.GitRepositoryCheckoutBranch ||
		sessions.last.RepositoryResources[0].Checkout.Value != "main" ||
		sessions.last.RepositoryResources[0].MountPath == nil ||
		*sessions.last.RepositoryResources[0].MountPath != repositoryMount {
		t.Fatalf("Session repository resources = %+v", sessions.last.RepositoryResources)
	}
}

func TestDeploymentServiceNormalizesAndValidatesGitRepositoryTemplates(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	service := newTestDeploymentService(
		t, newMemoryDeploymentRepository(), &deploymentSessionCreatorFake{}, now,
	)
	uppercaseCommit := "0123456789ABCDEF0123456789ABCDEF01234567"
	item, err := service.Create(context.Background(), DeploymentCreateInput{
		AgentID: "agent_test", EnvironmentID: "env_test", Name: "Pinned repository",
		InitialEvents: []domain.EventDraft{{Type: domain.EvUserMessage}},
		Resources: []domain.DeploymentResource{{
			Type:                    domain.SessionResourceTypeGitRepository,
			RepositoryURL:           "https://github.com/acme/widgets.git",
			RepositoryCheckoutType:  domain.GitRepositoryCheckoutCommit,
			RepositoryCheckoutValue: uppercaseCommit,
		}},
	})
	if err != nil {
		t.Fatalf("create Deployment: %v", err)
	}
	if got := item.Resources[0].RepositoryCheckoutValue; got != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("normalized commit = %q", got)
	}

	left, right := "/workspace/project", "/workspace/project/generated"
	_, err = service.Create(context.Background(), DeploymentCreateInput{
		AgentID: "agent_test", EnvironmentID: "env_test", Name: "Overlapping repositories",
		InitialEvents: []domain.EventDraft{{Type: domain.EvUserMessage}},
		Resources: []domain.DeploymentResource{
			{Type: domain.SessionResourceTypeGitRepository, RepositoryURL: "https://github.com/acme/one.git", MountPath: &left},
			{Type: domain.SessionResourceTypeGitRepository, RepositoryURL: "https://github.com/acme/two.git", MountPath: &right},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "must not overlap") {
		t.Fatalf("overlapping repository mounts = %v", err)
	}
}

func TestClassifyDeploymentRunGitRepositoryFailure(t *testing.T) {
	t.Parallel()
	errorType, _ := classifyDeploymentRunError(
		domain.SessionResourceNotFound("public Git repository could not be cloned or read"),
	)
	if errorType != "session_resource_not_found_error" {
		t.Fatalf("Git repository error type = %q", errorType)
	}
}

func TestDeploymentServiceRecordsAndPausesOnScheduledCreationFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	repo := newMemoryDeploymentRepository()
	sessions := &deploymentSessionCreatorFake{
		err: domain.Validation("environment is archived"),
	}
	service := newTestDeploymentService(t, repo, sessions, now)
	item, err := service.Create(context.Background(), DeploymentCreateInput{
		AgentID: "agent_test", EnvironmentID: "env_test", Name: "Hourly audit",
		InitialEvents: []domain.EventDraft{{
			Type: domain.EvUserMessage,
			Payload: map[string]any{"content": []any{map[string]any{
				"type": "text", "text": "Audit the repository",
			}}},
		}},
		Schedule: &domain.DeploymentSchedule{Expression: "0 * * * *", Timezone: "UTC"},
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduledAt := item.Schedule.UpcomingRunsAt[0]
	run, err := service.RunScheduled(context.Background(), item.ID, scheduledAt)
	if err != nil {
		t.Fatalf("scheduled run: %v", err)
	}
	if run.ErrorType != "environment_archived_error" || run.SessionID != nil {
		t.Fatalf("failed run = %+v", run)
	}
	stored, err := service.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.DeploymentStatusPaused || stored.PausedReason == nil ||
		stored.PausedReason.ErrorType != run.ErrorType {
		t.Fatalf("paused Deployment = %+v", stored)
	}
	if got, err := repo.GetScheduledRun(context.Background(), item.ID, scheduledAt); err != nil ||
		got.ID != run.ID {
		t.Fatalf("stored scheduled run = %+v err=%v", got, err)
	}
}

func TestDeploymentServiceRecoversPauseAfterFailedRunWasAlreadyCommitted(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	repo := newMemoryDeploymentRepository()
	service := newTestDeploymentService(t, repo, &deploymentSessionCreatorFake{}, now)
	item, err := service.Create(context.Background(), DeploymentCreateInput{
		AgentID: "agent_test", EnvironmentID: "env_test", Name: "Hourly audit",
		InitialEvents: []domain.EventDraft{{Type: domain.EvUserMessage}},
		Schedule:      &domain.DeploymentSchedule{Expression: "0 * * * *", Timezone: "UTC"},
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduledAt := item.Schedule.UpcomingRunsAt[0]
	committed, err := repo.CreateRun(context.Background(), domain.DeploymentRun{
		ID: "drun_committed", DeploymentID: item.ID,
		AgentID: item.AgentID, AgentVersion: item.AgentVersion,
		ErrorType: "environment_archived_error", ErrorMessage: "environment is archived",
		TriggerType: domain.DeploymentTriggerSchedule, ScheduledAt: &scheduledAt, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	recovered, err := service.RunScheduled(context.Background(), item.ID, scheduledAt)
	if err != nil || recovered.ID != committed.ID {
		t.Fatalf("recovered run = %+v, %v", recovered, err)
	}
	stored, err := service.Get(context.Background(), item.ID)
	if err != nil || stored.Status != domain.DeploymentStatusPaused || stored.PausedReason == nil ||
		stored.PausedReason.ErrorType != committed.ErrorType {
		t.Fatalf("recovered Deployment pause = %+v, %v", stored, err)
	}
}

func newTestDeploymentService(
	t *testing.T,
	repo *memoryDeploymentRepository,
	sessions DeploymentSessionCreator,
	now time.Time,
) *DeploymentService {
	t.Helper()
	agents := newMemoryAgentRepository()
	for version := 1; version <= 2; version++ {
		if err := agents.PutVersion(context.Background(), domain.Agent{
			ID: "agent_test", Version: version, Name: "Agent",
			Model:     domain.Model{ID: "claude-sonnet-4-5"},
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	environments := newMemoryEnvironmentRepository()
	if err := environments.Put(context.Background(), domain.Environment{
		ID: "env_test", Name: "Environment", ConfigType: "cloud",
		Config: map[string]any{"type": "cloud"}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return NewDeploymentService(DeploymentServiceConfig{
		Repository: repo, Agents: agents, Environments: environments,
		Sessions: sessions, IDGenerator: domain.NewSeqIDGen(), Clock: domain.FixedClock{T: now},
	})
}

type deploymentSessionCreatorFake struct {
	last CreateSessionInput
	err  error
}

func (f *deploymentSessionCreatorFake) Create(
	_ context.Context,
	input CreateSessionInput,
) (domain.Session, error) {
	f.last = input
	if f.err != nil {
		return domain.Session{}, f.err
	}
	return domain.Session{ID: "sesn_deployment"}, nil
}

type memoryDeploymentRepository struct {
	items map[string]domain.Deployment
	runs  map[string]domain.DeploymentRun
}

func newMemoryDeploymentRepository() *memoryDeploymentRepository {
	return &memoryDeploymentRepository{
		items: map[string]domain.Deployment{}, runs: map[string]domain.DeploymentRun{},
	}
}

func (r *memoryDeploymentRepository) Create(
	_ context.Context,
	item domain.Deployment,
) (domain.Deployment, error) {
	r.items[item.ID] = cloneDeployment(item)
	return item, nil
}

func (r *memoryDeploymentRepository) Get(
	_ context.Context,
	id string,
) (domain.Deployment, error) {
	item, ok := r.items[id]
	if !ok {
		return domain.Deployment{}, domain.NotFound("deployment not found")
	}
	return cloneDeployment(item), nil
}

func (r *memoryDeploymentRepository) Update(
	_ context.Context,
	id string,
	mutate func(domain.Deployment) (domain.Deployment, bool, error),
) (domain.Deployment, error) {
	item, ok := r.items[id]
	if !ok {
		return domain.Deployment{}, domain.NotFound("deployment not found")
	}
	next, changed, err := mutate(cloneDeployment(item))
	if err != nil {
		return domain.Deployment{}, err
	}
	if changed {
		r.items[id] = cloneDeployment(next)
		return next, nil
	}
	return item, nil
}

func (r *memoryDeploymentRepository) List(
	context.Context,
	DeploymentListQuery,
) (DeploymentListPage, error) {
	items := make([]domain.Deployment, 0, len(r.items))
	for _, item := range r.items {
		items = append(items, cloneDeployment(item))
	}
	return DeploymentListPage{Deployments: items}, nil
}

func (r *memoryDeploymentRepository) CreateRun(
	_ context.Context,
	run domain.DeploymentRun,
) (domain.DeploymentRun, error) {
	for _, existing := range r.runs {
		if run.ScheduledAt != nil && existing.ScheduledAt != nil &&
			existing.DeploymentID == run.DeploymentID && existing.ScheduledAt.Equal(*run.ScheduledAt) {
			return domain.DeploymentRun{}, domain.Conflict("duplicate scheduled run")
		}
	}
	r.runs[run.ID] = run
	return run, nil
}

func (r *memoryDeploymentRepository) GetRun(
	_ context.Context,
	id string,
) (domain.DeploymentRun, error) {
	run, ok := r.runs[id]
	if !ok {
		return domain.DeploymentRun{}, domain.NotFound("deployment run not found")
	}
	return run, nil
}

func (r *memoryDeploymentRepository) GetScheduledRun(
	_ context.Context,
	id string,
	scheduledAt time.Time,
) (domain.DeploymentRun, error) {
	for _, run := range r.runs {
		if run.DeploymentID == id && run.ScheduledAt != nil && run.ScheduledAt.Equal(scheduledAt) {
			return run, nil
		}
	}
	return domain.DeploymentRun{}, domain.NotFound("deployment run not found")
}

func (r *memoryDeploymentRepository) ListRuns(
	context.Context,
	DeploymentRunListQuery,
) (DeploymentRunListPage, error) {
	runs := make([]domain.DeploymentRun, 0, len(r.runs))
	for _, run := range r.runs {
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
	return DeploymentRunListPage{Runs: runs}, nil
}

func (r *memoryDeploymentRepository) ClaimDue(
	context.Context,
	time.Time,
	time.Time,
	int,
) ([]DeploymentScheduleClaim, error) {
	return nil, nil
}

func (r *memoryDeploymentRepository) CompleteSchedule(
	_ context.Context,
	id string,
	_ time.Time,
	lastRunAt time.Time,
	upcoming []time.Time,
) error {
	item := r.items[id]
	if item.Schedule != nil {
		item.Schedule.LastRunAt = &lastRunAt
		item.Schedule.UpcomingRunsAt = upcoming
	}
	r.items[id] = item
	return nil
}
