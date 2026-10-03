package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestUploadLease_PostgresChecksExpiryAfterRowLock(t *testing.T) {
	for _, kind := range []string{"File", "Skill"} {
		for _, operation := range []string{"Renew", "Complete"} {
			t.Run(kind+"/"+operation, func(t *testing.T) {
				first := testStore(t)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				name := domain.NewRandomIDGen().NewID("lease_lock_")
				cfg := first.pool.Config().Copy()
				cfg.ConnConfig.RuntimeParams["application_name"] = name
				pool, err := pgxpool.NewWithConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer pool.Close()
				second := NewDefaultWorkspaceStore(pool, domain.NewRandomIDGen(), fixedClock{})
				now := time.Now().UTC()
				var expires time.Time
				var lockSQL string
				var execute func() error
				if kind == "File" {
					if err := NewFileRepository(first).BeginUpload(ctx, domain.File{ID: name, Filename: "pending.txt", MimeType: "text/plain", CreatedAt: now, UpdatedAt: now, BlobKey: name, State: domain.FileStateUploading}); err != nil {
						t.Fatal(err)
					}
					if err := first.pool.QueryRow(ctx, "UPDATE files SET upload_expires_at=clock_timestamp()+interval '500 milliseconds' WHERE id=$1 RETURNING upload_expires_at", name).Scan(&expires); err != nil {
						t.Fatal(err)
					}
					lockSQL = "SELECT 1 FROM files WHERE id=$1 FOR UPDATE"
					repo := NewFileRepository(second)
					execute = func() error {
						if operation == "Renew" {
							return repo.RenewUpload(ctx, name)
						}
						_, err := repo.CompleteUpload(ctx, name, app.BlobInfo{})
						return err
					}
				} else {
					skill := domain.Skill{ID: name, CreatedAt: now, UpdatedAt: now, DisplayTitle: name, Source: "custom"}
					version := domain.SkillVersion{SkillID: name, Version: "1", CreatedAt: now, Name: "pending", Directory: "pending", BlobKey: name, State: domain.SkillVersionUploading, Initial: true}
					if err := NewSkillRepository(first).BeginSkill(ctx, skill, version); err != nil {
						t.Fatal(err)
					}
					if err := first.pool.QueryRow(ctx, "UPDATE skill_versions SET upload_expires_at=clock_timestamp()+interval '500 milliseconds' WHERE skill_id=$1 RETURNING upload_expires_at", name).Scan(&expires); err != nil {
						t.Fatal(err)
					}
					lockSQL = "SELECT 1 FROM skill_versions WHERE skill_id=$1 FOR UPDATE"
					repo := NewSkillRepository(second)
					execute = func() error {
						if operation == "Renew" {
							return repo.RenewUpload(ctx, name, "1", name)
						}
						_, _, err := repo.CompleteVersion(ctx, name, "1", name, app.BlobInfo{})
						return err
					}
				}
				tx, err := first.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(context.Background()) }()
				if _, err := tx.Exec(ctx, lockSQL, name); err != nil {
					t.Fatal(err)
				}
				result := make(chan error, 1)
				go func() { result <- execute() }()
				for {
					var waiting bool
					if err := first.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock')", name).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting {
						break
					}
					select {
					case err := <-result:
						t.Fatalf("operation did not wait for lock: %v", err)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-time.After(5 * time.Millisecond):
					}
				}
				for {
					var expired bool
					if err := first.pool.QueryRow(ctx, "SELECT clock_timestamp()>$1::timestamptz", expires).Scan(&expired); err != nil {
						t.Fatal(err)
					}
					if expired {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-time.After(5 * time.Millisecond):
					}
				}
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				if err := <-result; !errors.Is(err, app.ErrUploadLeaseLost) {
					t.Fatalf("expired %s succeeded after row lock wait: %v", operation, err)
				}
			})
		}
	}
}
