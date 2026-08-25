package app

import (
	"context"
	"io"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

const (
	MaxSessionResources     = 500
	MaxSessionResourceBytes = MaxFileBytes
)

// CreateSessionInput is the storage-independent command accepted by the
// control-plane session service.
type CreateSessionInput struct {
	AgentID             string
	AgentVersion        *int
	Overrides           *domain.AgentOverrides
	EnvironmentID       string
	Title               string
	Metadata            map[string]any
	InitialEvents       []domain.EventDraft
	Resources           []FileSessionResourceInput
	MemoryResources     []MemorySessionResourceInput
	RepositoryResources []GitRepositorySessionResourceInput
	VaultIDs            []string
	Budget              *domain.SessionBudget
	DeploymentID        *string
	DeploymentRun       *domain.DeploymentRun
}

type FileSessionResourceInput struct {
	FileID    string
	MountPath *string
}

type MemorySessionResourceInput struct {
	MemoryStoreID string
	Access        string
	Instructions  string
}

type GitRepositoryCheckoutInput struct {
	Type  string
	Value string
}

type GitRepositorySessionResourceInput struct {
	URL       string
	Checkout  *GitRepositoryCheckoutInput
	MountPath *string
}

type GitRepositorySnapshotRequest struct {
	URL           string
	CheckoutType  string
	CheckoutValue string
}

type GitRepositorySnapshot struct {
	ResolvedCommit string
	Archive        io.ReadCloser
}

type deploymentRunClassifiedError struct {
	errorType string
	cause     error
}

func (e *deploymentRunClassifiedError) Error() string { return e.cause.Error() }
func (e *deploymentRunClassifiedError) Unwrap() error { return e.cause }
func (e *deploymentRunClassifiedError) DeploymentRunErrorType() string {
	return e.errorType
}

// SessionResourceNotFoundError preserves the ordinary Session HTTP validation
// envelope while carrying the narrower Deployment Run classification through
// the shared admission path.
func SessionResourceNotFoundError(message string) error {
	return &deploymentRunClassifiedError{
		errorType: "session_resource_not_found_error",
		cause:     domain.Validation(message),
	}
}

// GitRepositorySnapshotter resolves one public remote to an exact commit and
// returns a bounded tar snapshot containing both the worktree and .git data.
type GitRepositorySnapshotter interface {
	OpenSnapshot(context.Context, GitRepositorySnapshotRequest) (GitRepositorySnapshot, error)
}

type PreparedSessionResource struct {
	Resource domain.SessionResource
	File     domain.File
	Blob     BlobInfo
}

type SessionResourcePageBoundary struct {
	CreatedAt time.Time
	ID        string
}

type SessionResourceListQuery struct {
	Limit    int
	Boundary *SessionResourcePageBoundary
}

type SessionResourceListPage struct {
	Resources []domain.SessionResource
	HasMore   bool
}

// ListPage describes a keyset-paginated session query.
type ListPage struct {
	AgentID         string
	AgentVersion    *int
	CreatedAtGt     *time.Time
	CreatedAtGte    *time.Time
	CreatedAtLt     *time.Time
	CreatedAtLte    *time.Time
	IncludeArchived bool
	Statuses        []domain.Status
	DeploymentID    *string
	MemoryStoreID   *string
	Boundary        *SessionPageBoundary
	Limit           int
	Desc            bool
}

type SessionPageBoundary struct {
	CreatedAt time.Time
	ID        string
	Backward  bool
}

type SessionListPage struct {
	Sessions []domain.Session
	HasPrev  bool
	HasNext  bool
}
