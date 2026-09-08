package app

import (
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

// CreateSessionInput is the storage-independent command accepted by the
// control-plane session service.
type CreateSessionInput struct {
	AgentID         string
	AgentVersion    *int
	Overrides       *domain.AgentOverrides
	EnvironmentID   string
	Title           string
	Metadata        map[string]any
	InitialEvents   []domain.EventDraft
	MemoryResources []MemorySessionResourceInput
	VaultIDs        []string
	Budget          *domain.SessionBudget
	DeploymentID    *string
	DeploymentRun   *domain.DeploymentRun
}

type MemorySessionResourceInput struct {
	MemoryStoreID string
	Access        string
	Instructions  string
}

type PreparedSessionResource struct {
	Resource domain.SessionResource
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
