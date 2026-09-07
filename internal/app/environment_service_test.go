package app

import (
	"context"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

func newEnvService(t *testing.T) *EnvironmentService {
	t.Helper()
	return NewEnvironmentService(
		newMemoryEnvironmentRepository(),
		domain.NewSeqIDGen(),
		domain.FixedClock{T: time.Unix(1, 0).UTC()},
	)
}

func TestEnvironmentService_DefaultsToSelfHosted(t *testing.T) {
	created, err := newEnvService(t).Create(context.Background(), domain.Environment{
		Name: "worker", Description: "operator runtime", Metadata: map[string]any{"team": "data"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ConfigType != "self_hosted" || created.Config["type"] != "self_hosted" {
		t.Fatalf("config = %#v", created.Config)
	}
	if created.Description != "operator runtime" || created.Metadata["team"] != "data" {
		t.Fatalf("resource fields = %#v", created)
	}
}

func TestEnvironmentService_RejectsManagedCloudConfiguration(t *testing.T) {
	svc := newEnvService(t)
	for _, environment := range []domain.Environment{
		{Name: "cloud", ConfigType: "cloud"},
		{Name: "cloud", Config: map[string]any{"type": "cloud"}},
		{Name: "packages", Config: map[string]any{
			"type": "self_hosted", "packages": map[string]any{"pip": []any{"httpx"}},
		}},
	} {
		if _, err := svc.Create(context.Background(), environment); err == nil {
			t.Fatalf("managed runtime config was accepted: %#v", environment)
		}
	}
}

func TestEnvironmentService_ValidatesMetadataAndScope(t *testing.T) {
	svc := newEnvService(t)
	for _, environment := range []domain.Environment{
		{Name: "bad metadata", Metadata: map[string]any{"bad": 1}},
		{Name: "bad scope", Scope: "workspace"},
	} {
		if _, err := svc.Create(context.Background(), environment); err == nil {
			t.Fatalf("invalid environment was accepted: %#v", environment)
		}
	}

	created, err := svc.Create(context.Background(), domain.Environment{
		Name: "self-hosted", Scope: "account", Config: map[string]any{"type": "self_hosted"},
	})
	if err != nil {
		t.Fatalf("create scoped environment: %v", err)
	}
	if created.Scope != "account" {
		t.Fatalf("scope = %q", created.Scope)
	}
}

func TestEnvironmentService_UpdateResourceFields(t *testing.T) {
	svc := newEnvService(t)
	created, err := svc.Create(context.Background(), domain.Environment{
		Name: "local", Description: "old", Metadata: map[string]any{
			"keep": "old", "drop": "value",
		}, Scope: "account",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	name, description, scope := "renamed", "new", "organization"
	config := map[string]any{"type": "self_hosted"}
	updated, err := svc.Update(context.Background(), created.ID, domain.EnvironmentPatch{
		Name: &name, Description: &description, Scope: &scope,
		Metadata: map[string]any{"keep": "updated", "drop": "", "add": "value"},
		Config:   &config,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "renamed" || updated.Description != "new" ||
		updated.ConfigType != "self_hosted" || updated.Scope != "organization" {
		t.Fatalf("updated environment = %#v", updated)
	}
	if len(updated.Metadata) != 2 || updated.Metadata["keep"] != "updated" ||
		updated.Metadata["add"] != "value" {
		t.Fatalf("updated metadata = %#v", updated.Metadata)
	}

	noOp, err := svc.Update(context.Background(), created.ID, domain.EnvironmentPatch{})
	if err != nil || noOp.UpdatedAt != updated.UpdatedAt {
		t.Fatalf("no-op update = %#v, err=%v", noOp, err)
	}
}

func TestEnvironmentService_UpdateRejectsCloudAndArchivedChanges(t *testing.T) {
	svc := newEnvService(t)
	created, err := svc.Create(context.Background(), domain.Environment{Name: "worker"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cloud := map[string]any{"type": "cloud"}
	if _, err := svc.Update(context.Background(), created.ID, domain.EnvironmentPatch{Config: &cloud}); err == nil {
		t.Fatal("cloud update was accepted")
	}
	if _, err := svc.Archive(context.Background(), created.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	name := "after archive"
	if _, err := svc.Update(context.Background(), created.ID, domain.EnvironmentPatch{Name: &name}); err == nil {
		t.Fatal("archived environment update was accepted")
	}
}

func TestEnvironmentService_DeleteReferenced(t *testing.T) {
	repository := newMemoryEnvironmentRepository()
	svc := NewEnvironmentService(repository, domain.NewSeqIDGen(), domain.FixedClock{T: time.Unix(1, 0).UTC()})
	created, err := svc.Create(context.Background(), domain.Environment{Name: "worker"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	repository.markReferenced(created.ID)
	if err := svc.Delete(context.Background(), created.ID); err == nil {
		t.Fatal("expected conflict deleting referenced environment")
	}
}

func TestEnvironmentService_DeleteUnreferenced(t *testing.T) {
	svc := newEnvService(t)
	created, err := svc.Create(context.Background(), domain.Environment{Name: "worker"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(context.Background(), created.ID); err == nil {
		t.Fatal("deleted environment remained readable")
	}
}

func TestEnvironmentService_DeleteMissingReturnsNotFound(t *testing.T) {
	err := newEnvService(t).Delete(context.Background(), "env_bogus_id")
	if domainError, ok := err.(*domain.DomainError); !ok || domainError.Kind != domain.KindNotFound {
		t.Fatalf("error = %v, want not found", err)
	}
}
