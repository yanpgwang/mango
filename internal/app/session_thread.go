package app

import (
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

const DefaultSessionThreadListLimit = 1000

type SessionThreadPageBoundary struct {
	CreatedAt time.Time
	ID        string
}

type SessionThreadListQuery struct {
	Limit    int
	Boundary *SessionThreadPageBoundary
	Statuses []domain.Status
}
