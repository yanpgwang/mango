package app

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yanpgwang/mango/internal/domain"
)

const (
	DefaultEnvironmentWorkListLimit = 100
	MaxEnvironmentWorkListLimit     = 1000
	// MaxEnvironmentWorkTTLSeconds bounds the time a lost worker can retain a
	// Session execution lease. Healthy workers extend the lease continuously,
	// so longer individual TTLs only increase the stale-owner window.
	MaxEnvironmentWorkTTLSeconds = int64(300)
	defaultWorkReclaimAge        = 5 * time.Second
)

type EnvironmentWorkListQuery struct {
	After *ResourcePageBoundary
	Limit int
}

type EnvironmentWorkListPage struct {
	Work    []domain.EnvironmentWork
	HasNext bool
}

type EnvironmentWorkPollInput struct {
	WorkerID   string
	ReclaimAge time.Duration
}

type EnvironmentWorkRepository interface {
	GetWork(context.Context, string, string) (domain.EnvironmentWork, error)
	UpdateWorkMetadata(context.Context, string, string, map[string]*string) (domain.EnvironmentWork, error)
	ListWork(context.Context, string, EnvironmentWorkListQuery) (EnvironmentWorkListPage, error)
	PollWork(context.Context, string, EnvironmentWorkPollInput) (*domain.EnvironmentWork, error)
	AckWork(context.Context, string, string) (domain.EnvironmentWork, error)
	HeartbeatWork(context.Context, string, string, *string, *int64) (domain.EnvironmentWorkHeartbeat, error)
	FailWork(context.Context, string, string, string) error
	StopWork(context.Context, string, string, bool) error
	WorkStats(context.Context, string) (domain.EnvironmentWorkQueueStats, error)
}

// EnvironmentWorkService exposes Mango's self-hosted Environment worker
// protocol over the same durable Session ledger used by the managed runtime.
type EnvironmentWorkService struct {
	repository   EnvironmentWorkRepository
	environments EnvironmentRepository
}

func NewEnvironmentWorkService(
	repository EnvironmentWorkRepository,
	environments EnvironmentRepository,
) *EnvironmentWorkService {
	return &EnvironmentWorkService{repository: repository, environments: environments}
}

func (s *EnvironmentWorkService) validateEnvironment(ctx context.Context, id string) error {
	environment, err := s.environments.Get(ctx, id)
	if err != nil {
		return domain.NotFound("environment not found")
	}
	if environment.ConfigType != "self_hosted" {
		return domain.Validation("Environment Work is available only for self-hosted environments")
	}
	return nil
}

func (s *EnvironmentWorkService) Get(
	ctx context.Context,
	environmentID, workID string,
) (domain.EnvironmentWork, error) {
	if err := s.validateEnvironment(ctx, environmentID); err != nil {
		return domain.EnvironmentWork{}, err
	}
	return s.repository.GetWork(ctx, environmentID, workID)
}

func (s *EnvironmentWorkService) Update(
	ctx context.Context,
	environmentID, workID string,
	metadata map[string]*string,
) (domain.EnvironmentWork, error) {
	if err := s.validateEnvironment(ctx, environmentID); err != nil {
		return domain.EnvironmentWork{}, err
	}
	return s.repository.UpdateWorkMetadata(ctx, environmentID, workID, metadata)
}

func (s *EnvironmentWorkService) List(
	ctx context.Context,
	environmentID string,
	query EnvironmentWorkListQuery,
) (EnvironmentWorkListPage, error) {
	if err := s.validateEnvironment(ctx, environmentID); err != nil {
		return EnvironmentWorkListPage{}, err
	}
	if query.Limit <= 0 {
		query.Limit = DefaultEnvironmentWorkListLimit
	}
	if query.Limit > MaxEnvironmentWorkListLimit {
		return EnvironmentWorkListPage{}, domain.Validation("limit must not exceed 1000")
	}
	return s.repository.ListWork(ctx, environmentID, query)
}

func (s *EnvironmentWorkService) Ack(
	ctx context.Context,
	environmentID, workID string,
) (domain.EnvironmentWork, error) {
	if err := s.validateEnvironment(ctx, environmentID); err != nil {
		return domain.EnvironmentWork{}, err
	}
	return s.repository.AckWork(ctx, environmentID, workID)
}

func (s *EnvironmentWorkService) Heartbeat(
	ctx context.Context,
	environmentID, workID string,
	expected *string,
	desiredTTL *int64,
) (domain.EnvironmentWorkHeartbeat, error) {
	if err := s.validateEnvironment(ctx, environmentID); err != nil {
		return domain.EnvironmentWorkHeartbeat{}, err
	}
	if desiredTTL != nil && (*desiredTTL <= 0 || *desiredTTL > MaxEnvironmentWorkTTLSeconds) {
		return domain.EnvironmentWorkHeartbeat{}, domain.Validation(
			"desired_ttl_seconds must be an integer from 1 through 300",
		)
	}
	return s.repository.HeartbeatWork(ctx, environmentID, workID, expected, desiredTTL)
}

func (s *EnvironmentWorkService) Poll(
	ctx context.Context,
	environmentID, workerID string,
	block time.Duration,
	reclaimAge *time.Duration,
) (*domain.EnvironmentWork, error) {
	if err := s.validateEnvironment(ctx, environmentID); err != nil {
		return nil, err
	}
	if block < 0 || block > 999*time.Millisecond {
		return nil, domain.Validation("block_ms must be an integer from 1 through 999")
	}
	age := defaultWorkReclaimAge
	if reclaimAge != nil {
		if *reclaimAge < 0 {
			return nil, domain.Validation("reclaim_older_than_ms must be a non-negative integer")
		}
		age = *reclaimAge
	}
	deadline := time.Now().Add(block)
	for {
		work, err := s.repository.PollWork(ctx, environmentID, EnvironmentWorkPollInput{
			WorkerID: workerID, ReclaimAge: age,
		})
		if err != nil || work != nil || block == 0 {
			return work, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil
		}
		wait := min(25*time.Millisecond, remaining)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *EnvironmentWorkService) Stop(
	ctx context.Context,
	environmentID, workID string,
	force bool,
) error {
	if err := s.validateEnvironment(ctx, environmentID); err != nil {
		return err
	}
	return s.repository.StopWork(ctx, environmentID, workID, force)
}

func (s *EnvironmentWorkService) Fail(
	ctx context.Context,
	environmentID, workID, message string,
) error {
	if err := s.validateEnvironment(ctx, environmentID); err != nil {
		return err
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return domain.Validation("message is required")
	}
	if !utf8.ValidString(message) || utf8.RuneCountInString(message) > 1024 {
		return domain.Validation("message must contain at most 1024 valid UTF-8 characters")
	}
	return s.repository.FailWork(ctx, environmentID, workID, message)
}

func (s *EnvironmentWorkService) Stats(
	ctx context.Context,
	environmentID string,
) (domain.EnvironmentWorkQueueStats, error) {
	if err := s.validateEnvironment(ctx, environmentID); err != nil {
		return domain.EnvironmentWorkQueueStats{}, err
	}
	return s.repository.WorkStats(ctx, environmentID)
}
