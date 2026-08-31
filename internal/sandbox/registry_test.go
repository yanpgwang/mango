package sandbox

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type registryTestProvider struct {
	name           string
	packageSetup   bool
	limitedNetwork bool
	fileResources  bool
	sessionOutputs bool
	skillBundles   bool
}

func (p *registryTestProvider) Name() string { return p.name }
func (p *registryTestProvider) SupportsPackageSetup() bool {
	return p.packageSetup
}
func (p *registryTestProvider) SupportsLimitedNetwork() bool {
	return p.limitedNetwork
}
func (p *registryTestProvider) SupportsFileResources() bool  { return p.fileResources }
func (p *registryTestProvider) SupportsSessionOutputs() bool { return p.sessionOutputs }
func (p *registryTestProvider) SupportsSkillBundles() bool   { return p.skillBundles }

func (*registryTestProvider) Create(
	context.Context,
	string,
	Spec,
) (Ref, Sandbox, error) {
	panic("not used")
}

func (*registryTestProvider) Attach(
	context.Context,
	string,
	Ref,
	Spec,
) (Sandbox, error) {
	panic("not used")
}

func registryFactory(name string) ProviderFactory {
	return func() (Provider, error) {
		return &registryTestProvider{name: name}, nil
	}
}

func TestProviderRegistry_OpensSelectedFactoryLazily(t *testing.T) {
	var testCalls, dockerCalls int
	registry, err := NewProviderRegistry(
		ProviderRegistration{
			Name:         "test-provider",
			Capabilities: ProviderCapabilities{PackageSetup: false},
			Factory: func() (Provider, error) {
				testCalls++
				return &registryTestProvider{name: "test-provider"}, nil
			},
		},
		ProviderRegistration{
			Name:         DockerProviderName,
			Capabilities: ProviderCapabilities{PackageSetup: true},
			Factory: func() (Provider, error) {
				dockerCalls++
				return &registryTestProvider{name: DockerProviderName, packageSetup: true}, nil
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if testCalls != 0 || dockerCalls != 0 {
		t.Fatal("registry construction invoked an optional provider factory")
	}
	capabilities, err := registry.Capabilities(DockerProviderName)
	if err != nil || !capabilities.PackageSetup {
		t.Fatalf("docker capabilities = %+v, err=%v", capabilities, err)
	}
	if testCalls != 0 || dockerCalls != 0 {
		t.Fatal("capability lookup invoked a provider factory")
	}

	provider, err := registry.Open("test-provider")
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != "test-provider" {
		t.Fatalf("provider name = %q, want %q", provider.Name(), "test-provider")
	}
	if testCalls != 1 || dockerCalls != 0 {
		t.Fatalf("factory calls test=%d docker=%d, want 1/0", testCalls, dockerCalls)
	}
	if got := registry.Names(); !reflect.DeepEqual(
		got,
		[]string{DockerProviderName, "test-provider"},
	) {
		t.Fatalf("Names() = %v", got)
	}
}

func TestProviderRegistry_RejectsInvalidRegistrations(t *testing.T) {
	cases := []struct {
		name          string
		registrations []ProviderRegistration
		want          string
	}{
		{name: "empty", want: "at least one"},
		{
			name: "missing name",
			registrations: []ProviderRegistration{{
				Factory: registryFactory("local"),
			}},
			want: "name is required",
		},
		{
			name: "non-canonical name",
			registrations: []ProviderRegistration{{
				Name: "Open_Sandbox", Factory: registryFactory("Open_Sandbox"),
			}},
			want: "invalid provider name",
		},
		{
			name: "missing factory",
			registrations: []ProviderRegistration{{
				Name: "local",
			}},
			want: "has no factory",
		},
		{
			name: "duplicate",
			registrations: []ProviderRegistration{
				{Name: "local", Factory: registryFactory("local")},
				{Name: "local", Factory: registryFactory("local")},
			},
			want: "more than once",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewProviderRegistry(tc.registrations...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewProviderRegistry() error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestProviderRegistry_RejectsUnknownAndBrokenFactories(t *testing.T) {
	sentinel := errors.New("bad credentials")
	registry, err := NewProviderRegistry(
		ProviderRegistration{Name: "local", Factory: registryFactory("local")},
		ProviderRegistration{Name: "broken", Factory: func() (Provider, error) {
			return nil, sentinel
		}},
		ProviderRegistration{Name: "nil", Factory: func() (Provider, error) {
			return nil, nil
		}},
		ProviderRegistration{Name: "alias", Factory: registryFactory("different")},
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := registry.Open("missing"); err == nil ||
		!strings.Contains(err.Error(), "available: alias, broken, local, nil") {
		t.Fatalf("unknown provider error = %v", err)
	}
	if _, err := registry.Open("broken"); !errors.Is(err, sentinel) {
		t.Fatalf("factory error = %v, want wrapped sentinel", err)
	}
	if _, err := registry.Open("nil"); err == nil ||
		!strings.Contains(err.Error(), "returned nil") {
		t.Fatalf("nil factory error = %v", err)
	}
	if _, err := registry.Open("alias"); err == nil ||
		!strings.Contains(err.Error(), `registered as "alias" reports name "different"`) {
		t.Fatalf("name mismatch error = %v", err)
	}
}

func TestProviderRegistryRejectsCapabilityDrift(t *testing.T) {
	registry, err := NewProviderRegistry(ProviderRegistration{
		Name:         "isolated",
		Capabilities: ProviderCapabilities{PackageSetup: true},
		Factory:      registryFactory("isolated"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Open("isolated"); err == nil ||
		!strings.Contains(err.Error(), "registered as true but reports false") {
		t.Fatalf("capability drift error = %v", err)
	}
}

func TestProviderRegistryRejectsLimitedNetworkCapabilityDrift(t *testing.T) {
	registry, err := NewProviderRegistry(ProviderRegistration{
		Name:         "isolated",
		Capabilities: ProviderCapabilities{LimitedNetwork: true},
		Factory: func() (Provider, error) {
			return &registryTestProvider{name: "isolated"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Open("isolated"); err == nil ||
		!strings.Contains(err.Error(), "limited network capability is registered as true but reports false") {
		t.Fatalf("limited network capability drift error = %v", err)
	}
}

func TestProviderRegistryRejectsFileResourceCapabilityDrift(t *testing.T) {
	registry, err := NewProviderRegistry(ProviderRegistration{
		Name:         "isolated",
		Capabilities: ProviderCapabilities{FileResources: true},
		Factory: func() (Provider, error) {
			return &registryTestProvider{name: "isolated"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Open("isolated"); err == nil ||
		!strings.Contains(err.Error(), "file resource capability is registered as true but reports false") {
		t.Fatalf("file resource capability drift error = %v", err)
	}
}

func TestProviderRegistryRejectsSessionOutputCapabilityDrift(t *testing.T) {
	registry, err := NewProviderRegistry(ProviderRegistration{
		Name:         "isolated",
		Capabilities: ProviderCapabilities{SessionOutputs: true},
		Factory: func() (Provider, error) {
			return &registryTestProvider{name: "isolated"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Open("isolated"); err == nil ||
		!strings.Contains(err.Error(), "Session output capability is registered as true but reports false") {
		t.Fatalf("Session output capability drift error = %v", err)
	}
}

func TestProviderRegistryRejectsSkillBundleCapabilityDrift(t *testing.T) {
	registry, err := NewProviderRegistry(ProviderRegistration{
		Name:         "isolated",
		Capabilities: ProviderCapabilities{SkillBundles: true},
		Factory: func() (Provider, error) {
			return &registryTestProvider{name: "isolated"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Open("isolated"); err == nil ||
		!strings.Contains(err.Error(), "Skill bundle capability is registered as true but reports false") {
		t.Fatalf("Skill bundle capability drift error = %v", err)
	}
}
