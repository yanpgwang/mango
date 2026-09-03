// Package workspace defines Mango's only tenant boundary.
//
// A Workspace is resolved from an authenticated API key at the HTTP edge and
// carried in context. It is deliberately not part of CMA request or response
// bodies: the upstream contract scopes resources through credentials too.
package workspace

import (
	"context"
	"errors"
	"strings"
)

const (
	// DefaultID owns rows created before Workspace tenancy was introduced and
	// is used by the local bootstrap path.
	DefaultID = "wrkspc_default"
	Prefix    = "wrkspc_"
	KeyPrefix = "sk-mango-"
)

var (
	ErrMissingScope        = errors.New("workspace scope is required")
	ErrInvalidAPIKey       = errors.New("invalid API key")
	ErrInvalidSessionToken = errors.New("invalid session token")
)

type Scope struct {
	ID string
	// Session is non-nil only for a per-Work Session credential. API keys
	// retain Workspace-wide authority and leave it nil.
	Session *SessionScope
}

// SessionScope is the least-privilege identity issued with one claimed Work
// item. HTTP authorization further limits it to that Work's lease operations,
// Session execution APIs, and immutable inputs attached to the Session.
type SessionScope struct {
	EnvironmentID string
	WorkID        string
	SessionID     string
	// CredentialDigest is the server-side SHA-256 identity of the bearer.
	// Repositories use it to fence mutations in the same transaction as the
	// protected write, closing the middleware-to-database reclaim race.
	CredentialDigest []byte
	Skills           map[SkillVersion]struct{}
	Files            map[string]struct{}
}

type SkillVersion struct {
	ID      string
	Version string
}

type contextKey struct{}

func WithScope(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, Scope{ID: id})
}

func WithSessionScope(ctx context.Context, id string, session SessionScope) context.Context {
	return context.WithValue(ctx, contextKey{}, Scope{ID: id, Session: &session})
}

func FromContext(ctx context.Context) (Scope, bool) {
	scope, ok := ctx.Value(contextKey{}).(Scope)
	return scope, ok && scope.ID != ""
}

func Require(ctx context.Context) (Scope, error) {
	scope, ok := FromContext(ctx)
	if !ok {
		return Scope{}, ErrMissingScope
	}
	return scope, nil
}

// BlobKey places object-store data beneath the authenticated Workspace. The
// suffix is an internal stable path such as files/file_... or skills/skill_....
func BlobKey(ctx context.Context, suffix string) string {
	workspaceID := DefaultID
	if scope, ok := FromContext(ctx); ok {
		workspaceID = scope.ID
	}
	return workspaceID + "/" + strings.TrimPrefix(suffix, "/")
}
