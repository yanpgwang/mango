package pg

import (
	"context"
	"errors"
	"fmt"
)

// Readiness checks connectivity and transaction mode through the API's pool.
// It does not write data or promise that a subsequent application write succeeds.
func (s *Store) Readiness(ctx context.Context) error {
	var readOnly bool
	if err := s.pool.QueryRow(ctx, "SELECT current_setting('transaction_read_only')::boolean").Scan(&readOnly); err != nil {
		return fmt.Errorf("pg: readiness: %w", err)
	}
	if readOnly {
		return errors.New("pg: database is read-only")
	}
	return nil
}
