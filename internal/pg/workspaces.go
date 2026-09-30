package pg

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

const (
	PrefixAPIKey   = "key_"
	bootstrapKeyID = "key_bootstrap"
)

type Workspace struct {
	ID        string
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type APIKey struct {
	ID            string
	WorkspaceID   string
	EnvironmentID string
	Label         string
	CreatedAt     time.Time
	RevokedAt     *time.Time
	LastUsedAt    *time.Time
}

// AuthenticateAPIKey resolves only Workspace-wide credentials. Revoked keys
// deliberately look identical to unknown keys.
func (s *Store) AuthenticateAPIKey(ctx context.Context, secret string) (string, error) {
	if secret == "" {
		return "", workspace.ErrInvalidAPIKey
	}
	digest := sha256.Sum256([]byte(secret))
	var workspaceID string
	err := s.pool.QueryRow(ctx, `
SELECT workspace_id
FROM api_keys
WHERE secret_hash = $1 AND revoked_at IS NULL AND environment_id IS NULL`, digest[:]).Scan(&workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", workspace.ErrInvalidAPIKey
	}
	if err != nil {
		return "", fmt.Errorf("pg: authenticate API key: %w", err)
	}
	return workspaceID, nil
}

// BootstrapAPIKey binds an operator-supplied development secret to the fixed
// default Workspace. Re-running startup rotates that one bootstrap credential.
func (s *Store) BootstrapAPIKey(ctx context.Context, secret string) error {
	if strings.TrimSpace(secret) == "" {
		return fmt.Errorf("bootstrap API key must not be empty")
	}
	digest := sha256.Sum256([]byte(secret))
	_, err := s.pool.Exec(ctx, `
INSERT INTO api_keys (
    id, workspace_id, secret_hash, label, created_at
) VALUES ($1, $2, $3, 'bootstrap', $4)
ON CONFLICT (id) DO UPDATE
SET secret_hash = EXCLUDED.secret_hash,
    revoked_at = NULL`, bootstrapKeyID, workspace.DefaultID, digest[:], s.clock.Now().UTC())
	if err != nil {
		return fmt.Errorf("pg: bootstrap API key: %w", err)
	}
	return nil
}

func (s *Store) CountActiveAPIKeys(ctx context.Context) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM api_keys WHERE revoked_at IS NULL AND environment_id IS NULL`).Scan(&count)
	return count, err
}

func (s *Store) CreateWorkspace(ctx context.Context, name string) (Workspace, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Workspace{}, fmt.Errorf("workspace name is required")
	}
	now := s.clock.Now().UTC()
	item := Workspace{
		ID: s.ids.NewID(workspace.Prefix), Name: name, CreatedAt: now, UpdatedAt: now,
	}
	_, err := s.pool.Exec(ctx, `
INSERT INTO workspaces (id, name, created_at, updated_at)
VALUES ($1, $2, $3, $4)`, item.ID, item.Name, item.CreatedAt, item.UpdatedAt)
	return item, err
}

func (s *Store) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, name, created_at, updated_at
FROM workspaces
ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Workspace
	for rows.Next() {
		var item Workspace
		if err := rows.Scan(&item.ID, &item.Name, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) CreateAPIKey(
	ctx context.Context,
	workspaceID string,
	label string,
) (APIKey, string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return APIKey{}, "", fmt.Errorf("API key label is required")
	}
	secret, err := randomAPIKey()
	if err != nil {
		return APIKey{}, "", err
	}
	digest := sha256.Sum256([]byte(secret))
	item := APIKey{
		ID: s.ids.NewID(PrefixAPIKey), WorkspaceID: workspaceID,
		Label: label, CreatedAt: s.clock.Now().UTC(),
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO api_keys (id, workspace_id, secret_hash, label, created_at)
VALUES ($1, $2, $3, $4, $5)`,
		item.ID, item.WorkspaceID, digest[:], item.Label, item.CreatedAt)
	if err != nil {
		return APIKey{}, "", err
	}
	return item, secret, nil
}

func (s *Store) ListAPIKeys(ctx context.Context, workspaceID string) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, workspace_id, COALESCE(environment_id, ''), label, created_at, revoked_at, last_used_at
FROM api_keys
WHERE workspace_id = $1
ORDER BY created_at, id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []APIKey
	for rows.Next() {
		var item APIKey
		if err := rows.Scan(
			&item.ID, &item.WorkspaceID, &item.EnvironmentID, &item.Label, &item.CreatedAt,
			&item.RevokedAt, &item.LastUsedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) RevokeAPIKey(ctx context.Context, id string) error {
	result, err := s.pool.Exec(ctx, `
UPDATE api_keys
SET revoked_at = COALESCE(revoked_at, $2)
WHERE id = $1`, id, s.clock.Now().UTC())
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return domain.NotFound("API key not found")
	}
	return nil
}

func randomAPIKey() (string, error) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate API key: %w", err)
	}
	return workspace.KeyPrefix + hex.EncodeToString(value[:]), nil
}
