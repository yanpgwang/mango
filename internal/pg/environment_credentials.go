package pg

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

// CreateEnvironmentKey uses the operator credential lifecycle while restricting
// the issued key to a self-hosted Environment in the selected Workspace.
func (s *Store) CreateEnvironmentKey(ctx context.Context, workspaceID, environmentID, label string) (APIKey, string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return APIKey{}, "", fmt.Errorf("API key label is required")
	}
	secret, err := randomAPIKey()
	if err != nil {
		return APIKey{}, "", err
	}
	digest := sha256.Sum256([]byte(secret))
	item := APIKey{ID: s.ids.NewID(PrefixAPIKey), WorkspaceID: workspaceID,
		EnvironmentID: environmentID, Label: label, CreatedAt: s.clock.Now().UTC()}
	result, err := s.pool.Exec(ctx, `
INSERT INTO api_keys (id, workspace_id, environment_id, secret_hash, label, created_at)
SELECT $1, workspace_id, id, $4, $5, $6 FROM environments
WHERE id=$3 AND workspace_id=$2 AND config_type='self_hosted'`,
		item.ID, workspaceID, environmentID, digest[:], label, item.CreatedAt)
	if err != nil {
		return APIKey{}, "", err
	}
	if result.RowsAffected() == 0 {
		return APIKey{}, "", domain.NotFound("self-hosted Environment not found in Workspace")
	}
	return item, secret, nil
}

func (s *Store) AuthenticateEnvironmentKey(ctx context.Context, secret string) (string, workspace.EnvironmentScope, error) {
	if secret == "" {
		return "", workspace.EnvironmentScope{}, workspace.ErrInvalidEnvironmentKey
	}
	digest := sha256.Sum256([]byte(secret))
	var workspaceID string
	scope := workspace.EnvironmentScope{CredentialDigest: digest[:]}
	err := s.pool.QueryRow(ctx, `
SELECT workspace_id, environment_id, id FROM api_keys
WHERE secret_hash=$1 AND revoked_at IS NULL AND environment_id IS NOT NULL`, digest[:]).Scan(&workspaceID, &scope.EnvironmentID, &scope.KeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", workspace.EnvironmentScope{}, workspace.ErrInvalidEnvironmentKey
	}
	if err != nil {
		return "", workspace.EnvironmentScope{}, fmt.Errorf("pg: authenticate Environment key: %w", err)
	}
	return workspaceID, scope, nil
}

// Lock the Environment before its key, then Work/poller rows. Environment
// deletion cascades to keys and pollers, so reversing that order would deadlock
// a new poller FK check against deletion. Revoke conflicts with the key share
// lock, ordering claim/Ack commits against revocation. Long polls hold no locks
// between attempts.
func (s *Store) authorizeEnvironmentKeyTx(ctx context.Context, tx pgx.Tx, environmentID string) error {
	scope, _ := workspace.FromContext(ctx)
	if scope.Environment == nil {
		return nil
	}
	key := scope.Environment
	if key.EnvironmentID != environmentID || len(key.CredentialDigest) == 0 {
		return workspace.ErrInvalidEnvironmentKey
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM environments
WHERE id=$1 AND workspace_id=$2 FOR KEY SHARE`, environmentID, scope.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspace.ErrInvalidEnvironmentKey
	}
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `
SELECT id FROM api_keys
WHERE id=$1 AND workspace_id=$2 AND environment_id=$3 AND secret_hash=$4 AND revoked_at IS NULL
FOR SHARE`, key.KeyID, scope.ID, environmentID, key.CredentialDigest).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspace.ErrInvalidEnvironmentKey
	}
	return err
}
