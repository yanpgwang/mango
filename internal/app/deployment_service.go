package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/robfig/cron"
	"github.com/yanpgwang/mango/internal/domain"
)

const (
	DefaultDeploymentListLimit    = 20
	MaxDeploymentListLimit        = 100
	DefaultDeploymentRunListLimit = 20
	MaxDeploymentRunListLimit     = 1000
	MaxDeploymentVaults           = 50
	MaxDeploymentInitialEvents    = 50
	ScheduleClaimLease            = 2 * time.Minute
)

type DeploymentRepository interface {
	Create(context.Context, domain.Deployment) (domain.Deployment, error)
	Get(context.Context, string) (domain.Deployment, error)
	Update(
		context.Context,
		string,
		func(domain.Deployment) (domain.Deployment, bool, error),
	) (domain.Deployment, error)
	List(context.Context, DeploymentListQuery) (DeploymentListPage, error)
	CreateRun(context.Context, domain.DeploymentRun) (domain.DeploymentRun, error)
	GetRun(context.Context, string) (domain.DeploymentRun, error)
	GetScheduledRun(context.Context, string, time.Time) (domain.DeploymentRun, error)
	ListRuns(context.Context, DeploymentRunListQuery) (DeploymentRunListPage, error)
	ClaimDue(context.Context, time.Time, time.Time, string, int) ([]DeploymentScheduleClaim, error)
	RenewScheduleClaim(context.Context, string, time.Time, string, time.Time) error
	CompleteSchedule(
		context.Context,
		string,
		time.Time,
		time.Time,
		[]time.Time,
	) error
}

type DeploymentSessionCreator interface {
	Create(context.Context, CreateSessionInput) (domain.Session, error)
}

type DeploymentFileReader interface {
	Get(context.Context, string) (domain.File, error)
}

type DeploymentMemoryReader interface {
	GetStore(context.Context, string) (domain.MemoryStore, error)
}

type DeploymentVaultReader interface {
	GetVault(context.Context, string) (domain.Vault, error)
}

type DeploymentServiceConfig struct {
	Repository   DeploymentRepository
	Agents       AgentRepository
	Environments EnvironmentRepository
	Sessions     DeploymentSessionCreator
	Files        DeploymentFileReader
	Memory       DeploymentMemoryReader
	// CloudMemoryStores gates only resources executed by the transitional
	// server-managed cloud sandbox. Self-hosted Sessions use the Memory API.
	CloudMemoryStores bool
	Vaults            DeploymentVaultReader
	IDGenerator       domain.IDGenerator
	Clock             domain.Clock
}

type DeploymentService struct {
	repo              DeploymentRepository
	agents            AgentRepository
	environments      EnvironmentRepository
	sessions          DeploymentSessionCreator
	files             DeploymentFileReader
	memory            DeploymentMemoryReader
	cloudMemoryStores bool
	vaults            DeploymentVaultReader
	ids               domain.IDGenerator
	clock             domain.Clock
}

func NewDeploymentService(config DeploymentServiceConfig) *DeploymentService {
	return &DeploymentService{
		repo: config.Repository, agents: config.Agents,
		environments: config.Environments, sessions: config.Sessions,
		files: config.Files, memory: config.Memory,
		cloudMemoryStores: config.CloudMemoryStores, vaults: config.Vaults,
		ids: config.IDGenerator, clock: config.Clock,
	}
}

type DeploymentCreateInput struct {
	AgentID       string
	AgentVersion  *int
	EnvironmentID string
	Name          string
	Description   string
	InitialEvents []domain.EventDraft
	Resources     []domain.DeploymentResource
	VaultIDs      []string
	Budget        *domain.SessionBudget
	Metadata      map[string]string
	Schedule      *domain.DeploymentSchedule
}

type DeploymentListBoundary struct {
	CreatedAt time.Time
	ID        string
}

type DeploymentListQuery struct {
	AgentID         string
	CreatedAtGte    *time.Time
	CreatedAtLte    *time.Time
	IncludeArchived bool
	Status          string
	Boundary        *DeploymentListBoundary
	Limit           int
}

type DeploymentListPage struct {
	Deployments []domain.Deployment
	HasMore     bool
}

type DeploymentRunListBoundary struct {
	CreatedAt time.Time
	ID        string
}

type DeploymentRunListQuery struct {
	CreatedAtGt  *time.Time
	CreatedAtGte *time.Time
	CreatedAtLt  *time.Time
	CreatedAtLte *time.Time
	DeploymentID *string
	HasError     *bool
	TriggerType  string
	Boundary     *DeploymentRunListBoundary
	Limit        int
}

type DeploymentRunListPage struct {
	Runs    []domain.DeploymentRun
	HasMore bool
}

type DeploymentScheduleClaim struct {
	WorkspaceID  string
	DeploymentID string
	ScheduledAt  time.Time
	Token        string
}

func (s *DeploymentService) Create(
	ctx context.Context,
	input DeploymentCreateInput,
) (domain.Deployment, error) {
	agent, err := s.resolveAgent(ctx, input.AgentID, input.AgentVersion)
	if err != nil {
		return domain.Deployment{}, err
	}
	resources, err := normalizeDeploymentResources(input.Resources)
	if err != nil {
		return domain.Deployment{}, err
	}
	now := s.clock.Now().UTC()
	item := domain.Deployment{
		ID: s.ids.NewID(domain.PrefixDeployment), AgentID: agent.ID,
		AgentVersion: agent.Version, EnvironmentID: input.EnvironmentID,
		Name: input.Name, Description: input.Description,
		InitialEvents: cloneEventDrafts(input.InitialEvents),
		Resources:     resources,
		VaultIDs:      append([]string(nil), input.VaultIDs...),
		Budget:        cloneSessionBudget(input.Budget),
		Metadata:      cloneDeploymentStringMap(input.Metadata), Schedule: cloneDeploymentSchedule(input.Schedule),
		Status: domain.DeploymentStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.validate(ctx, item); err != nil {
		return domain.Deployment{}, err
	}
	if item.Schedule != nil {
		item.Schedule.UpcomingRunsAt, err = deploymentOccurrences(item.Schedule, now, 5)
		if err != nil {
			return domain.Deployment{}, err
		}
	}
	return s.repo.Create(ctx, item)
}

func (s *DeploymentService) Get(ctx context.Context, id string) (domain.Deployment, error) {
	return s.repo.Get(ctx, id)
}

func (s *DeploymentService) List(
	ctx context.Context,
	query DeploymentListQuery,
) (DeploymentListPage, error) {
	if query.Limit <= 0 {
		query.Limit = DefaultDeploymentListLimit
	}
	if query.Limit > MaxDeploymentListLimit {
		return DeploymentListPage{}, domain.Validation("limit must not exceed 100")
	}
	return s.repo.List(ctx, query)
}

func (s *DeploymentService) Update(
	ctx context.Context,
	id string,
	patch domain.DeploymentPatch,
) (domain.Deployment, error) {
	if patch.AgentID != nil {
		agent, err := s.resolveAgent(ctx, *patch.AgentID, patch.AgentVersion)
		if err != nil {
			return domain.Deployment{}, err
		}
		patch.AgentID = &agent.ID
		patch.AgentVersion = &agent.Version
	}
	current, err := s.repo.Get(ctx, id)
	if err != nil {
		return domain.Deployment{}, err
	}
	if current.ArchivedAt != nil {
		return domain.Deployment{}, domain.Validation("archived deployment is read-only")
	}
	next := cloneDeployment(current)
	if patch.AgentID != nil {
		next.AgentID, next.AgentVersion = *patch.AgentID, *patch.AgentVersion
	}
	if patch.EnvironmentID != nil {
		next.EnvironmentID = *patch.EnvironmentID
	}
	if patch.Name != nil {
		next.Name = *patch.Name
	}
	if patch.Description != nil {
		next.Description = *patch.Description
	}
	if patch.InitialEvents != nil {
		next.InitialEvents = cloneEventDrafts(*patch.InitialEvents)
	}
	if patch.Resources != nil {
		next.Resources, err = normalizeDeploymentResources(*patch.Resources)
		if err != nil {
			return domain.Deployment{}, err
		}
	}
	if patch.VaultIDs != nil {
		next.VaultIDs = append([]string(nil), (*patch.VaultIDs)...)
	}
	for key, value := range patch.Metadata {
		if value == nil {
			delete(next.Metadata, key)
			continue
		}
		if next.Metadata == nil {
			next.Metadata = map[string]string{}
		}
		next.Metadata[key] = *value
	}
	if patch.ScheduleSet {
		next.Schedule = cloneDeploymentSchedule(patch.Schedule)
	}
	if patch.BudgetSet {
		next.Budget = cloneSessionBudget(patch.Budget)
	}
	if err := s.validate(ctx, next); err != nil {
		return domain.Deployment{}, err
	}
	if patch.ScheduleSet && next.Schedule != nil {
		next.Schedule.UpcomingRunsAt, err = deploymentOccurrences(
			next.Schedule, s.clock.Now().UTC(), 5,
		)
		if err != nil {
			return domain.Deployment{}, err
		}
	}
	if deploymentConfigurationEqual(current, next) {
		return current, nil
	}
	next.UpdatedAt = s.changedAt(current.UpdatedAt)
	return s.repo.Update(ctx, id, func(locked domain.Deployment) (domain.Deployment, bool, error) {
		if !locked.UpdatedAt.Equal(current.UpdatedAt) {
			return domain.Deployment{}, false, domain.Conflict("deployment changed concurrently")
		}
		return next, true, nil
	})
}

func (s *DeploymentService) Pause(ctx context.Context, id string) (domain.Deployment, error) {
	return s.repo.Update(ctx, id, func(item domain.Deployment) (domain.Deployment, bool, error) {
		if item.ArchivedAt != nil {
			return domain.Deployment{}, false, domain.Validation("archived deployment is read-only")
		}
		if item.Status == domain.DeploymentStatusPaused && item.PausedReason != nil &&
			item.PausedReason.Type == "manual" {
			return item, false, nil
		}
		item.Status = domain.DeploymentStatusPaused
		item.PausedReason = &domain.DeploymentPausedReason{Type: "manual"}
		item.UpdatedAt = s.changedAt(item.UpdatedAt)
		return item, true, nil
	})
}

func (s *DeploymentService) Unpause(ctx context.Context, id string) (domain.Deployment, error) {
	return s.repo.Update(ctx, id, func(item domain.Deployment) (domain.Deployment, bool, error) {
		if item.ArchivedAt != nil {
			return domain.Deployment{}, false, domain.Validation("archived deployment is read-only")
		}
		if item.Status == domain.DeploymentStatusActive {
			return item, false, nil
		}
		item.Status = domain.DeploymentStatusActive
		item.PausedReason = nil
		item.UpdatedAt = s.changedAt(item.UpdatedAt)
		if item.Schedule != nil {
			var err error
			item.Schedule.UpcomingRunsAt, err = deploymentOccurrences(
				item.Schedule, item.UpdatedAt, 5,
			)
			if err != nil {
				return domain.Deployment{}, false, err
			}
		}
		return item, true, nil
	})
}

func (s *DeploymentService) Archive(ctx context.Context, id string) (domain.Deployment, error) {
	return s.repo.Update(ctx, id, func(item domain.Deployment) (domain.Deployment, bool, error) {
		if item.ArchivedAt != nil {
			return item, false, nil
		}
		now := s.changedAt(item.UpdatedAt)
		item.ArchivedAt, item.UpdatedAt = &now, now
		if item.Schedule != nil {
			item.Schedule.UpcomingRunsAt = []time.Time{}
		}
		return item, true, nil
	})
}

func (s *DeploymentService) Run(
	ctx context.Context,
	id string,
) (domain.DeploymentRun, error) {
	return s.run(ctx, id, domain.DeploymentTriggerManual, nil)
}

func (s *DeploymentService) RunScheduled(
	ctx context.Context,
	id string,
	scheduledAt time.Time,
) (domain.DeploymentRun, error) {
	scheduledAt = scheduledAt.UTC().Truncate(time.Microsecond)
	if existing, err := s.repo.GetScheduledRun(ctx, id, scheduledAt); err == nil {
		if err := s.recoverScheduledFailure(ctx, id, existing); err != nil {
			return domain.DeploymentRun{}, err
		}
		if err := s.completeSchedule(ctx, id, scheduledAt, existing.CreatedAt); err != nil {
			return domain.DeploymentRun{}, err
		}
		return existing, nil
	} else if !isNotFound(err) {
		return domain.DeploymentRun{}, err
	}
	run, err := s.run(ctx, id, domain.DeploymentTriggerSchedule, &scheduledAt)
	if err != nil {
		return domain.DeploymentRun{}, err
	}
	if err := s.recoverScheduledFailure(ctx, id, run); err != nil {
		return domain.DeploymentRun{}, err
	}
	if err := s.completeSchedule(ctx, id, scheduledAt, run.CreatedAt); err != nil {
		return domain.DeploymentRun{}, err
	}
	return run, nil
}

func (s *DeploymentService) run(
	ctx context.Context,
	id string,
	trigger string,
	scheduledAt *time.Time,
) (domain.DeploymentRun, error) {
	item, err := s.repo.Get(ctx, id)
	if err != nil {
		return domain.DeploymentRun{}, err
	}
	if item.ArchivedAt != nil {
		return domain.DeploymentRun{}, domain.Validation("archived deployment cannot run")
	}
	if trigger == domain.DeploymentTriggerSchedule && item.Status != domain.DeploymentStatusActive {
		return domain.DeploymentRun{}, domain.Conflict("deployment schedule is not active")
	}
	now := s.clock.Now().UTC()
	run := domain.DeploymentRun{
		ID: s.ids.NewID(domain.PrefixDeploymentRun), DeploymentID: item.ID,
		AgentID: item.AgentID, AgentVersion: item.AgentVersion,
		TriggerType: trigger, ScheduledAt: scheduledAt, CreatedAt: now,
	}
	files, memories, repositories := deploymentSessionResources(item.Resources)
	session, createErr := s.sessions.Create(ctx, CreateSessionInput{
		AgentID: item.AgentID, AgentVersion: &item.AgentVersion,
		EnvironmentID: item.EnvironmentID, Title: item.Name,
		Metadata:      deploymentSessionMetadata(item.Metadata),
		InitialEvents: cloneEventDrafts(item.InitialEvents),
		Resources:     files, MemoryResources: memories,
		RepositoryResources: repositories,
		VaultIDs:            append([]string(nil), item.VaultIDs...),
		Budget:              cloneSessionBudget(item.Budget),
		DeploymentID:        &item.ID, DeploymentRun: &run,
	})
	if createErr == nil {
		run.SessionID = &session.ID
		return run, nil
	}
	if trigger == domain.DeploymentTriggerSchedule {
		if existing, getErr := s.repo.GetScheduledRun(ctx, id, *scheduledAt); getErr == nil {
			return existing, nil
		}
	}
	run.ErrorType, run.ErrorMessage = classifyDeploymentRunError(createErr)
	created, err := s.repo.CreateRun(ctx, run)
	if err != nil {
		if trigger == domain.DeploymentTriggerSchedule {
			if existing, getErr := s.repo.GetScheduledRun(ctx, id, *scheduledAt); getErr == nil {
				return existing, nil
			}
		}
		return domain.DeploymentRun{}, err
	}
	if trigger == domain.DeploymentTriggerSchedule && shouldPauseDeployment(run.ErrorType) {
		_, _ = s.pauseForError(ctx, item.ID, run.ErrorType)
	}
	return created, nil
}

func (s *DeploymentService) GetRun(ctx context.Context, id string) (domain.DeploymentRun, error) {
	return s.repo.GetRun(ctx, id)
}

func (s *DeploymentService) ListRuns(
	ctx context.Context,
	query DeploymentRunListQuery,
) (DeploymentRunListPage, error) {
	if query.Limit <= 0 {
		query.Limit = DefaultDeploymentRunListLimit
	}
	if query.Limit > MaxDeploymentRunListLimit {
		return DeploymentRunListPage{}, domain.Validation("limit must not exceed 1000")
	}
	return s.repo.ListRuns(ctx, query)
}

func (s *DeploymentService) ClaimDue(
	ctx context.Context,
	limit int,
) ([]DeploymentScheduleClaim, error) {
	now := s.clock.Now().UTC()
	token := s.ids.NewID(domain.PrefixDeploymentClaim)
	return s.repo.ClaimDue(ctx, now, now.Add(-ScheduleClaimLease), token, limit)
}

func (s *DeploymentService) RenewClaim(
	ctx context.Context,
	claim DeploymentScheduleClaim,
) error {
	err := s.repo.RenewScheduleClaim(
		ctx, claim.DeploymentID, claim.ScheduledAt, claim.Token, s.clock.Now().UTC(),
	)
	if err == nil {
		return nil
	}
	// A successful scheduled Run fences all duplicate admission through its
	// unique occurrence row. Treat a heartbeat racing schedule completion as
	// success rather than canceling work that already committed.
	if _, getErr := s.repo.GetScheduledRun(
		ctx, claim.DeploymentID, claim.ScheduledAt,
	); getErr == nil {
		return nil
	}
	return err
}

func (s *DeploymentService) completeSchedule(
	ctx context.Context,
	id string,
	scheduledAt time.Time,
	lastRunAt time.Time,
) error {
	item, err := s.repo.Get(ctx, id)
	if err != nil {
		return err
	}
	var upcoming []time.Time
	if item.Schedule != nil && item.ArchivedAt == nil {
		upcoming, err = deploymentOccurrences(item.Schedule, s.clock.Now().UTC(), 5)
		if err != nil {
			return err
		}
	}
	return s.repo.CompleteSchedule(ctx, id, scheduledAt, lastRunAt, upcoming)
}

func (s *DeploymentService) pauseForError(
	ctx context.Context,
	id string,
	errorType string,
) (domain.Deployment, error) {
	return s.repo.Update(ctx, id, func(item domain.Deployment) (domain.Deployment, bool, error) {
		if item.ArchivedAt != nil {
			return item, false, nil
		}
		// A user's explicit pause wins over delayed recovery of an earlier
		// failure record.
		if item.Status == domain.DeploymentStatusPaused && item.PausedReason != nil &&
			item.PausedReason.Type == "manual" {
			return item, false, nil
		}
		if item.Status == domain.DeploymentStatusPaused && item.PausedReason != nil &&
			item.PausedReason.Type == "error" && item.PausedReason.ErrorType == errorType {
			return item, false, nil
		}
		item.Status = domain.DeploymentStatusPaused
		item.PausedReason = &domain.DeploymentPausedReason{Type: "error", ErrorType: errorType}
		item.UpdatedAt = s.changedAt(item.UpdatedAt)
		return item, true, nil
	})
}

func (s *DeploymentService) recoverScheduledFailure(
	ctx context.Context,
	id string,
	run domain.DeploymentRun,
) error {
	if !shouldPauseDeployment(run.ErrorType) {
		return nil
	}
	_, err := s.pauseForError(ctx, id, run.ErrorType)
	return err
}

func (s *DeploymentService) changedAt(previous time.Time) time.Time {
	previous = previous.UTC().Truncate(time.Microsecond)
	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	if !now.After(previous) {
		return previous.Add(time.Microsecond)
	}
	return now
}

func (s *DeploymentService) resolveAgent(
	ctx context.Context,
	id string,
	version *int,
) (domain.Agent, error) {
	var (
		agent domain.Agent
		err   error
	)
	if id == "" {
		return domain.Agent{}, domain.Validation("agent is required")
	}
	if version == nil {
		agent, err = s.agents.Latest(ctx, id)
	} else {
		agent, err = s.agents.GetVersion(ctx, id, *version)
	}
	if err != nil {
		return domain.Agent{}, domain.Validation("agent not found")
	}
	if agent.ArchivedAt != nil {
		return domain.Agent{}, domain.Validation("agent is archived")
	}
	return agent, nil
}

func (s *DeploymentService) validate(ctx context.Context, item domain.Deployment) error {
	if item.Name == "" {
		return domain.Validation("name is required")
	}
	if len(item.InitialEvents) == 0 || len(item.InitialEvents) > MaxDeploymentInitialEvents {
		return domain.Validation("initial_events must contain between 1 and 50 events")
	}
	for _, event := range item.InitialEvents {
		if !domain.IsInitialEventType(event.Type) && event.Type != domain.EvSystemMessage {
			return domain.Validation("deployment initial_events contains an unsupported event type")
		}
	}
	if len(item.Resources) > MaxSessionResources {
		return domain.Validation("resources must contain at most 500 entries")
	}
	if len(item.VaultIDs) > MaxDeploymentVaults {
		return domain.Validation("vault_ids must contain at most 50 entries")
	}
	if err := ValidateMetadata(stringMetadataToAny(item.Metadata)); err != nil {
		return err
	}
	if item.Status != domain.DeploymentStatusActive && item.Status != domain.DeploymentStatusPaused {
		return domain.Validation("deployment status is invalid")
	}
	agent, err := s.resolveAgent(ctx, item.AgentID, &item.AgentVersion)
	if err != nil {
		return err
	}
	if item.Budget != nil {
		priceable, err := s.deploymentModelsPriceable(ctx, agent)
		if err != nil {
			return err
		}
		if !priceable {
			return domain.Validation(
				"budgeted deployments require every agent model to have a known Anthropic public list price",
			)
		}
	}
	environment, err := s.environments.Get(ctx, item.EnvironmentID)
	if err != nil {
		return domain.Validation("environment not found")
	}
	if environment.ArchivedAt != nil {
		return domain.Validation("environment is archived")
	}
	if item.Schedule != nil {
		if _, err := deploymentOccurrences(item.Schedule, s.clock.Now().UTC(), 1); err != nil {
			return err
		}
	}
	seenVaults := map[string]struct{}{}
	for _, id := range item.VaultIDs {
		if id == "" {
			return domain.Validation("vault_ids must not contain empty IDs")
		}
		if _, duplicate := seenVaults[id]; duplicate {
			return domain.Validation("vault_ids must not contain duplicates")
		}
		seenVaults[id] = struct{}{}
		if s.vaults == nil {
			return domain.Unsupported("Vaults are unavailable for the configured deployment")
		}
		vault, err := s.vaults.GetVault(ctx, id)
		if err != nil || vault.ArchivedAt != nil {
			return domain.Validation("vault_ids references a missing or archived Vault")
		}
	}
	seenFiles, seenMemories := map[string]struct{}{}, map[string]struct{}{}
	var repositoryMountPaths []string
	for _, resource := range item.Resources {
		switch resource.Type {
		case domain.SessionResourceTypeFile:
			if resource.FileID == "" {
				return domain.Validation("file resource requires file_id")
			}
			if _, duplicate := seenFiles[resource.FileID]; duplicate {
				return domain.Validation("a File may be attached only once")
			}
			seenFiles[resource.FileID] = struct{}{}
			if s.files == nil {
				return domain.Unsupported("File resources are unavailable for the configured deployment")
			}
			file, err := s.files.Get(ctx, resource.FileID)
			if err != nil || file.Internal {
				return domain.Validation("file resource not found")
			}
		case domain.SessionResourceTypeMemoryStore:
			if environment.ConfigType == "cloud" && !s.cloudMemoryStores {
				return domain.Unsupported(
					"Memory Store resources are unavailable for the configured cloud sandbox provider",
				)
			}
			if resource.MemoryStoreID == "" {
				return domain.Validation("memory_store resource requires memory_store_id")
			}
			if _, duplicate := seenMemories[resource.MemoryStoreID]; duplicate {
				return domain.Validation("a Memory Store may be attached only once")
			}
			seenMemories[resource.MemoryStoreID] = struct{}{}
			if s.memory == nil {
				return domain.Unsupported("Memory Store resources are unavailable for the configured deployment")
			}
			store, err := s.memory.GetStore(ctx, resource.MemoryStoreID)
			if err != nil || store.ArchivedAt != nil {
				return domain.Validation("memory store is missing or archived")
			}
		case domain.SessionResourceTypeGitRepository:
			if err := domain.ValidateGitRepositoryURL(resource.RepositoryURL); err != nil {
				return err
			}
			if _, _, err := domain.NormalizeGitRepositoryCheckout(
				resource.RepositoryCheckoutType, resource.RepositoryCheckoutValue,
			); err != nil {
				return err
			}
			mountPath, err := domain.NormalizeGitRepositoryMountPath(
				resource.RepositoryURL, resource.MountPath,
			)
			if err != nil {
				return err
			}
			for _, existing := range repositoryMountPaths {
				if domain.SessionFileMountPathsConflict(existing, mountPath) {
					return domain.Validation("Git repository mount paths must not overlap")
				}
			}
			repositoryMountPaths = append(repositoryMountPaths, mountPath)
		default:
			return domain.Unsupported("unsupported Deployment Resource type")
		}
	}
	return nil
}

func deploymentOccurrences(
	schedule *domain.DeploymentSchedule,
	after time.Time,
	count int,
) ([]time.Time, error) {
	if schedule == nil {
		return nil, nil
	}
	if schedule.Expression == "" {
		return nil, domain.Validation("schedule expression is required")
	}
	location, err := time.LoadLocation(schedule.Timezone)
	if err != nil || schedule.Timezone == "" {
		return nil, domain.Validation("schedule timezone must be a valid IANA timezone")
	}
	parsed, err := cron.ParseStandard(schedule.Expression)
	if err != nil || strings.HasPrefix(strings.TrimSpace(schedule.Expression), "@") {
		return nil, domain.Validation("schedule expression must be a 5-field POSIX cron expression")
	}
	next := after.In(location)
	out := make([]time.Time, 0, count)
	for range count {
		next = parsed.Next(next)
		if next.IsZero() {
			return nil, domain.Validation("schedule expression has no future occurrence")
		}
		out = append(out, next.UTC())
	}
	return out, nil
}

func deploymentSessionResources(
	resources []domain.DeploymentResource,
) ([]FileSessionResourceInput, []MemorySessionResourceInput, []GitRepositorySessionResourceInput) {
	var files []FileSessionResourceInput
	var memories []MemorySessionResourceInput
	var repositories []GitRepositorySessionResourceInput
	for _, resource := range resources {
		switch resource.Type {
		case domain.SessionResourceTypeFile:
			files = append(files, FileSessionResourceInput{
				FileID: resource.FileID, MountPath: resource.MountPath,
			})
		case domain.SessionResourceTypeMemoryStore:
			memories = append(memories, MemorySessionResourceInput{
				MemoryStoreID: resource.MemoryStoreID, Access: resource.Access,
				Instructions: resource.Instructions,
			})
		case domain.SessionResourceTypeGitRepository:
			var checkout *GitRepositoryCheckoutInput
			if resource.RepositoryCheckoutType != "" {
				checkout = &GitRepositoryCheckoutInput{
					Type: resource.RepositoryCheckoutType, Value: resource.RepositoryCheckoutValue,
				}
			}
			repositories = append(repositories, GitRepositorySessionResourceInput{
				URL: resource.RepositoryURL, Checkout: checkout, MountPath: resource.MountPath,
			})
		}
	}
	return files, memories, repositories
}

func classifyDeploymentRunError(err error) (string, string) {
	message := err.Error()
	lower := strings.ToLower(message)
	var classified interface{ DeploymentRunErrorType() string }
	if errors.As(err, &classified) {
		return classified.DeploymentRunErrorType(), message
	}
	var domainErr *domain.DomainError
	if errors.As(err, &domainErr) && domainErr.Code != "" {
		return domainErr.Code, message
	}
	switch {
	case strings.Contains(lower, "agent") && strings.Contains(lower, "archiv"):
		return "agent_archived_error", message
	case strings.Contains(lower, "environment") && strings.Contains(lower, "archiv"):
		return "environment_archived_error", message
	case strings.Contains(lower, "environment") && strings.Contains(lower, "not found"):
		return "environment_not_found_error", message
	case strings.Contains(lower, "vault") && strings.Contains(lower, "archiv"):
		return "vault_archived_error", message
	case strings.Contains(lower, "vault"):
		return "vault_not_found_error", message
	case strings.Contains(lower, "memory store") && strings.Contains(lower, "archiv"):
		return "memory_store_archived_error", message
	case strings.Contains(lower, "file") && strings.Contains(lower, "not found"):
		return "file_not_found_error", message
	case strings.Contains(lower, "self-hosted") && strings.Contains(lower, "resource"):
		return "self_hosted_resources_unsupported_error", message
	case strings.Contains(lower, "mcp") && strings.Contains(lower, "egress"):
		return "mcp_egress_blocked_error", message
	}
	if errors.As(err, &domainErr) &&
		(domainErr.Kind == domain.KindValidation || domainErr.Kind == domain.KindUnsupported) {
		return "session_creation_rejected_error", message
	}
	return "unknown_error", message
}

func shouldPauseDeployment(errorType string) bool {
	return errorType != "" && errorType != "session_rate_limited_error" &&
		errorType != "unknown_error"
}

func isNotFound(err error) bool {
	var domainErr *domain.DomainError
	return errors.As(err, &domainErr) && domainErr.Kind == domain.KindNotFound
}

func deploymentSessionMetadata(metadata map[string]string) map[string]any {
	out := make(map[string]any, len(metadata))
	for key, value := range metadata {
		out[key] = value
	}
	return out
}

func stringMetadataToAny(metadata map[string]string) map[string]any {
	return deploymentSessionMetadata(metadata)
}

func cloneDeployment(item domain.Deployment) domain.Deployment {
	item.InitialEvents = cloneEventDrafts(item.InitialEvents)
	item.Resources = cloneDeploymentResources(item.Resources)
	item.VaultIDs = append([]string(nil), item.VaultIDs...)
	item.Metadata = cloneDeploymentStringMap(item.Metadata)
	item.Schedule = cloneDeploymentSchedule(item.Schedule)
	item.Budget = cloneSessionBudget(item.Budget)
	if item.PausedReason != nil {
		reason := *item.PausedReason
		item.PausedReason = &reason
	}
	return item
}

func cloneSessionBudget(input *domain.SessionBudget) *domain.SessionBudget {
	if input == nil {
		return nil
	}
	out := *input
	return &out
}

func (s *DeploymentService) deploymentModelsPriceable(
	ctx context.Context,
	agent domain.Agent,
) (bool, error) {
	if !domain.HasAnthropicPublicListPrice(agent.Model) {
		return false, nil
	}
	if agent.Multiagent == nil {
		return true, nil
	}
	if !agent.Multiagent.IsResolved() {
		return false, nil
	}
	for _, reference := range agent.Multiagent.Agents {
		if reference.Type == "self" || reference.ID == agent.ID {
			continue
		}
		member, err := s.agents.GetVersion(ctx, reference.ID, reference.Version)
		if err != nil {
			return false, domain.Validation("multiagent roster member not found")
		}
		if !domain.HasAnthropicPublicListPrice(member.Model) {
			return false, nil
		}
	}
	return true, nil
}

func cloneEventDrafts(events []domain.EventDraft) []domain.EventDraft {
	out := make([]domain.EventDraft, len(events))
	for index, event := range events {
		out[index] = domain.EventDraft{ID: event.ID, Type: event.Type}
		if event.Payload != nil {
			out[index].Payload = make(map[string]any, len(event.Payload))
			for key, value := range event.Payload {
				out[index].Payload[key] = value
			}
		}
	}
	return out
}

func cloneDeploymentResources(resources []domain.DeploymentResource) []domain.DeploymentResource {
	out := append([]domain.DeploymentResource(nil), resources...)
	for index := range out {
		if out[index].MountPath != nil {
			value := *out[index].MountPath
			out[index].MountPath = &value
		}
	}
	return out
}

func normalizeDeploymentResources(
	resources []domain.DeploymentResource,
) ([]domain.DeploymentResource, error) {
	out := cloneDeploymentResources(resources)
	for index := range out {
		resource := &out[index]
		if resource.Type != domain.SessionResourceTypeGitRepository {
			continue
		}
		if err := domain.ValidateGitRepositoryURL(resource.RepositoryURL); err != nil {
			return nil, err
		}
		checkoutType, checkoutValue, err := domain.NormalizeGitRepositoryCheckout(
			resource.RepositoryCheckoutType, resource.RepositoryCheckoutValue,
		)
		if err != nil {
			return nil, err
		}
		if _, err := domain.NormalizeGitRepositoryMountPath(
			resource.RepositoryURL, resource.MountPath,
		); err != nil {
			return nil, err
		}
		resource.RepositoryCheckoutType = checkoutType
		resource.RepositoryCheckoutValue = checkoutValue
	}
	return out, nil
}

func cloneDeploymentStringMap(input map[string]string) map[string]string {
	if input == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func cloneDeploymentSchedule(input *domain.DeploymentSchedule) *domain.DeploymentSchedule {
	if input == nil {
		return nil
	}
	out := *input
	out.UpcomingRunsAt = append([]time.Time(nil), input.UpcomingRunsAt...)
	if input.LastRunAt != nil {
		value := *input.LastRunAt
		out.LastRunAt = &value
	}
	return &out
}

func deploymentConfigurationEqual(left, right domain.Deployment) bool {
	left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	left.ScheduleClaimedAt, right.ScheduleClaimedAt = nil, nil
	return reflect.DeepEqual(left, right)
}
