package pg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

// insertPreparedSessionResources persists the only control-plane-managed
// resource available to self-hosted Sessions: Memory Store bindings. Files and
// repositories belong to the operator-owned worker workspace boundary.
func insertPreparedSessionResources(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	sessionID string,
	prepared []app.PreparedSessionResource,
) error {
	for _, item := range prepared {
		resource := item.Resource
		if resource.Type() != domain.SessionResourceTypeMemoryStore ||
			resource.SessionID != sessionID || resource.MemoryStoreID == "" ||
			resource.State != domain.SessionResourceActive ||
			(resource.MemoryAccess != domain.MemoryAccessReadWrite &&
				resource.MemoryAccess != domain.MemoryAccessReadOnly) {
			return errors.New("pg: invalid prepared Memory Store Resource ownership")
		}
		var active int
		if err := tx.QueryRow(ctx, `
SELECT 1 FROM memory_stores
WHERE id = $1 AND workspace_id = $2 AND archived_at IS NULL
FOR SHARE`, resource.MemoryStoreID, workspaceID).Scan(&active); errors.Is(err, pgx.ErrNoRows) {
			return domain.Validation("memory store is missing or archived")
		} else if err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
INSERT INTO session_resources (
    id, session_id, resource_type, memory_store_id, memory_access,
    memory_instructions, memory_store_name, memory_store_description,
    mount_path, state, created_at, updated_at
) VALUES ($1, $2, 'memory_store', $3, $4, $5, $6, $7, $8, 'active', $9, $10)`,
			resource.ID,
			resource.SessionID,
			resource.MemoryStoreID,
			resource.MemoryAccess,
			resource.MemoryInstructions,
			resource.MemoryStoreName,
			resource.MemoryStoreDescription,
			resource.MountPath,
			resource.CreatedAt.UTC(),
			resource.UpdatedAt.UTC(),
		)
		if isUniqueViolation(err) {
			return domain.Conflict("a Session Resource already uses this Memory Store or mount_path")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) FinalizeSessionMemoryResources(
	ctx context.Context,
	sessionID string,
) error {
	_, err := s.pool.Exec(ctx, `
DELETE FROM session_resources
WHERE session_id = $1 AND resource_type = 'memory_store' AND state = 'deleting'`,
		sessionID,
	)
	return err
}
