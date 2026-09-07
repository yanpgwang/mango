package app

import (
	"context"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

func newEnvService(t *testing.T) *EnvironmentService {
	t.Helper()
	return NewEnvironmentService(newMemoryEnvironmentRepository(),
		domain.NewSeqIDGen(), domain.FixedClock{T: time.Unix(1, 0).UTC()},
		EnvironmentCapabilities{PackageSetup: true, LimitedNetwork: true})
}

func TestEnvironmentService_RejectsLimitedNetworkingWithoutRuntimeCapability(t *testing.T) {
	svc := NewEnvironmentService(
		newMemoryEnvironmentRepository(),
		domain.NewSeqIDGen(),
		domain.FixedClock{T: time.Unix(1, 0).UTC()},
		EnvironmentCapabilities{PackageSetup: true},
	)
	_, err := svc.Create(context.Background(), domain.Environment{
		Name: "limited",
		Config: map[string]any{
			"type": "cloud", "networking": map[string]any{"type": "limited"},
		},
	})
	if err == nil {
		t.Fatal("limited networking was accepted without runtime capability")
	}
	domainError, ok := err.(*domain.DomainError)
	if !ok || domainError.Kind != domain.KindUnsupported {
		t.Fatalf("limited networking error = %v, want unsupported", err)
	}
}

func TestEnvironmentService_RejectsPackagesWithoutRuntimeCapability(t *testing.T) {
	svc := NewEnvironmentService(
		newMemoryEnvironmentRepository(),
		domain.NewSeqIDGen(),
		domain.FixedClock{T: time.Unix(1, 0).UTC()},
	)
	_, err := svc.Create(context.Background(), domain.Environment{
		Name: "packages",
		Config: map[string]any{
			"type": "cloud",
			"packages": map[string]any{
				"pip": []any{"httpx==0.28.1"},
			},
		},
	})
	if err == nil {
		t.Fatal("package config was accepted without runtime capability")
	}
	domainError, ok := err.(*domain.DomainError)
	if !ok || domainError.Kind != domain.KindUnsupported {
		t.Fatalf("package config error = %v, want unsupported", err)
	}
	if _, err := svc.Create(context.Background(), domain.Environment{
		Name: "empty packages",
		Config: map[string]any{
			"type":     "cloud",
			"packages": map[string]any{"pip": []any{}},
		},
	}); err != nil {
		t.Fatalf("empty package config requires no runtime capability: %v", err)
	}
}

func TestEnvironmentService_DeleteReferenced(t *testing.T) {
	repository := newMemoryEnvironmentRepository()
	envSvc := NewEnvironmentService(repository,
		domain.NewSeqIDGen(), domain.FixedClock{T: time.Unix(1, 0).UTC()})
	ctx := context.Background()
	env, _ := envSvc.Create(ctx, domain.Environment{Name: "e", ConfigType: "cloud"})
	repository.markReferenced(env.ID)
	if err := envSvc.Delete(ctx, env.ID); err == nil {
		t.Fatal("expected conflict deleting referenced environment")
	}
}

func TestEnvironmentService_DeleteUnreferencedSucceeds(t *testing.T) {
	svc := newEnvService(t)
	ctx := context.Background()

	env, err := svc.Create(ctx, domain.Environment{Name: "unreferenced", ConfigType: "cloud"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := svc.Delete(ctx, env.ID); err != nil {
		t.Fatalf("delete unreferenced env: %v", err)
	}

	_, err = svc.Get(ctx, env.ID)
	if err == nil {
		t.Fatal("expected NotFound after delete, got nil error")
	}
	de, ok := err.(*domain.DomainError)
	if !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("expected DomainError KindNotFound, got %v", err)
	}
}

func TestEnvironmentService_DeleteMissingReturnsNotFound(t *testing.T) {
	svc := newEnvService(t)
	ctx := context.Background()

	err := svc.Delete(ctx, "env_bogus_id")
	if err == nil {
		t.Fatal("expected NotFound error for missing env ID")
	}
	de, ok := err.(*domain.DomainError)
	if !ok || de.Kind != domain.KindNotFound {
		t.Fatalf("expected DomainError KindNotFound, got %v", err)
	}
}

func TestEnvironmentService_RejectsUnenforcedConfiguration(t *testing.T) {
	svc := newEnvService(t)
	ctx := context.Background()
	cases := []map[string]any{
		{"type": "cloud", "future_policy": true},
		{"type": 42},
	}
	for _, config := range cases {
		_, err := svc.Create(ctx, domain.Environment{
			Name: "unsupported", ConfigType: "cloud", Config: config,
		})
		if err == nil {
			t.Fatalf("unsupported config was accepted: %#v", config)
		}
	}
}

func TestEnvironmentService_NormalizesAndPatchesLimitedNetworking(t *testing.T) {
	svc := newEnvService(t)
	created, err := svc.Create(context.Background(), domain.Environment{
		Name: "limited",
		Config: map[string]any{
			"type": "cloud",
			"networking": map[string]any{
				"type": "limited", "allow_mcp_servers": true,
				"allowed_hosts": []any{"api.example.com", "*.assets.example.com"},
			},
			"packages": map[string]any{"pip": []any{"httpx==0.28.1"}},
		},
	})
	if err != nil {
		t.Fatalf("create limited environment: %v", err)
	}
	networking := created.Config["networking"].(map[string]any)
	if networking["allow_mcp_servers"] != true ||
		networking["allow_package_managers"] != false {
		t.Fatalf("normalized limited networking = %#v", networking)
	}

	patch := map[string]any{
		"type": "cloud",
		"networking": map[string]any{
			"type": "limited", "allowed_hosts": []any{"next.example.com"},
		},
	}
	updated, err := svc.Update(context.Background(), created.ID, domain.EnvironmentPatch{Config: &patch})
	if err != nil {
		t.Fatalf("patch limited networking: %v", err)
	}
	networking = updated.Config["networking"].(map[string]any)
	if networking["allow_mcp_servers"] != true ||
		networking["allow_package_managers"] != false ||
		networking["allowed_hosts"].([]any)[0] != "next.example.com" {
		t.Fatalf("patched limited networking = %#v", networking)
	}
	packages := updated.Config["packages"].(map[string]any)
	if packages["pip"].([]any)[0] != "httpx==0.28.1" {
		t.Fatalf("network patch lost packages: %#v", packages)
	}
}

func TestEnvironmentService_NormalizesSupportedConfiguration(t *testing.T) {
	svc := newEnvService(t)
	created, err := svc.Create(context.Background(), domain.Environment{
		Name: "cloud", Description: "analysis", Metadata: map[string]any{"team": "data"}, ConfigType: "cloud",
		Config: map[string]any{
			"networking": map[string]any{"type": "unrestricted"},
			"packages": map[string]any{
				"type": "packages", "apt": []any{}, "pip": []string{},
			},
		},
	})
	if err != nil {
		t.Fatalf("create default cloud environment: %v", err)
	}
	if created.ConfigType != "cloud" || created.Config["type"] != "cloud" {
		t.Fatalf("normalized config = %#v", created.Config)
	}
	packages := created.Config["packages"].(map[string]any)
	if values, ok := packages["pip"].([]string); !ok || len(values) != 0 {
		t.Fatalf("normalized packages = %#v", packages)
	}
	if created.Description != "analysis" || created.Metadata["team"] != "data" {
		t.Fatalf("resource fields = %#v", created)
	}

	defaults, err := svc.Create(context.Background(), domain.Environment{Name: "defaults"})
	if err != nil {
		t.Fatalf("create default fields: %v", err)
	}
	if defaults.Description != "" || defaults.Scope != "" || defaults.Metadata == nil || len(defaults.Metadata) != 0 {
		t.Fatalf("default resource fields = %#v", defaults)
	}
	if defaults.ConfigType != "self_hosted" || defaults.Config["type"] != "self_hosted" {
		t.Fatalf("default config = %#v", defaults.Config)
	}
}

func TestEnvironmentService_PreservesConfiguredPackages(t *testing.T) {
	svc := newEnvService(t)
	created, err := svc.Create(context.Background(), domain.Environment{
		Name: "packages",
		Config: map[string]any{
			"type": "cloud",
			"packages": map[string]any{
				"apt": []any{"git"},
				"pip": []any{"httpx==0.28.1"},
			},
		},
	})
	if err != nil {
		t.Fatalf("create package environment: %v", err)
	}
	packages := created.Config["packages"].(map[string]any)
	if packages["apt"].([]any)[0] != "git" || packages["pip"].([]any)[0] != "httpx==0.28.1" {
		t.Fatalf("stored packages = %#v", packages)
	}

	config := map[string]any{
		"type":       "cloud",
		"networking": map[string]any{"type": "unrestricted"},
	}
	updated, err := svc.Update(context.Background(), created.ID, domain.EnvironmentPatch{Config: &config})
	if err != nil {
		t.Fatalf("update networking only: %v", err)
	}
	packages = updated.Config["packages"].(map[string]any)
	if packages["apt"].([]any)[0] != "git" || packages["pip"].([]any)[0] != "httpx==0.28.1" {
		t.Fatalf("omitted packages were not preserved: %#v", packages)
	}
}

func TestEnvironmentService_RejectsMalformedConfiguration(t *testing.T) {
	svc := newEnvService(t)
	cases := []map[string]any{
		{"type": "cloud", "networking": nil},
		{"type": "cloud", "networking": "unrestricted"},
		{"type": "cloud", "networking": map[string]any{}},
		{"type": "cloud", "networking": map[string]any{"type": "future"}},
		{"type": "cloud", "networking": map[string]any{"type": "unrestricted", "future": true}},
		{"type": "cloud", "networking": map[string]any{"type": "limited", "allowed_hosts": []any{1}}},
		{"type": "cloud", "networking": map[string]any{"type": "limited", "allow_mcp_servers": "yes"}},
		{"type": "cloud", "networking": map[string]any{"type": "limited", "allowed_hosts": []any{"https://example.com"}}},
		{"type": "cloud", "networking": map[string]any{"type": "limited", "allowed_hosts": []any{"example.com:443"}}},
		{"type": "cloud", "networking": map[string]any{"type": "limited", "allowed_hosts": []any{"foo.*.example.com"}}},
		{"type": "cloud", "packages": nil},
		{"type": "cloud", "packages": []any{}},
		{"type": "cloud", "packages": map[string]any{"type": "future"}},
		{"type": "cloud", "packages": map[string]any{"apt": nil}},
		{"type": "cloud", "packages": map[string]any{"apt": []any{1}}},
		{"type": "cloud", "packages": map[string]any{"pip": []any{"--target=/tmp"}}},
		{"type": "cloud", "packages": map[string]any{"pip": []any{" "}}},
		{"type": "cloud", "packages": map[string]any{"apt": []any{"curl"}, "pip": []any{1}}},
		{"type": "cloud", "packages": map[string]any{"future": []any{}}},
		{"type": "self_hosted", "networking": map[string]any{"type": "unrestricted"}},
	}
	for _, config := range cases {
		if _, err := svc.Create(context.Background(), domain.Environment{
			Name: "invalid", Config: config,
		}); err == nil {
			t.Fatalf("malformed config was accepted: %#v", config)
		}
	}
}

func TestEnvironmentService_ValidatesMetadataAndScope(t *testing.T) {
	svc := newEnvService(t)
	cases := []domain.Environment{
		{Name: "bad metadata", Metadata: map[string]any{"bad": 1}},
		{Name: "bad scope", ConfigType: "self_hosted", Scope: "workspace"},
		{Name: "cloud scope", ConfigType: "cloud", Scope: "account"},
	}
	for _, environment := range cases {
		if _, err := svc.Create(context.Background(), environment); err == nil {
			t.Fatalf("invalid environment was accepted: %#v", environment)
		}
	}

	created, err := svc.Create(context.Background(), domain.Environment{
		Name: "self-hosted", ConfigType: "self_hosted", Scope: "account",
	})
	if err != nil {
		t.Fatalf("create scoped self-hosted environment: %v", err)
	}
	if created.Scope != "account" {
		t.Fatalf("scope = %q", created.Scope)
	}
}

func TestEnvironmentService_UpdateResourceFieldsAndConfigType(t *testing.T) {
	svc := newEnvService(t)
	created, err := svc.Create(context.Background(), domain.Environment{
		Name: "local", Description: "old", Metadata: map[string]any{
			"keep": "old", "drop": "value",
		}, Scope: "account", ConfigType: "self_hosted",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	name, description := "renamed", "new"
	cloud := map[string]any{"type": "cloud"}
	updated, err := svc.Update(context.Background(), created.ID, domain.EnvironmentPatch{
		Name: &name, Description: &description,
		Metadata: map[string]any{"keep": "updated", "drop": "", "add": "value"},
		Config:   &cloud,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "renamed" || updated.Description != "new" ||
		updated.ConfigType != "cloud" || updated.Scope != "" {
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

func TestEnvironmentService_UpdateRejectsInvalidOrArchivedChanges(t *testing.T) {
	svc := newEnvService(t)
	created, err := svc.Create(context.Background(), domain.Environment{Name: "cloud"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	malformed := map[string]any{
		"type": "cloud", "networking": map[string]any{
			"type": "limited", "allowed_hosts": []any{"https://example.com"},
		},
	}
	if _, err := svc.Update(context.Background(), created.ID, domain.EnvironmentPatch{
		Config: &malformed,
	}); err == nil {
		t.Fatal("malformed limited networking update was accepted")
	}
	badMetadata := map[string]any{"bad": 1}
	if _, err := svc.Update(context.Background(), created.ID, domain.EnvironmentPatch{
		Metadata: badMetadata,
	}); err == nil {
		t.Fatal("non-string metadata update was accepted")
	}
	if _, err := svc.Archive(context.Background(), created.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	name := "after archive"
	if _, err := svc.Update(context.Background(), created.ID, domain.EnvironmentPatch{Name: &name}); err == nil {
		t.Fatal("archived environment update was accepted")
	}
}
