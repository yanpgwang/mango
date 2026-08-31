package sandbox

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	// Provider names identify persisted sandbox bindings. Local remains for
	// legacy test fixtures but is not registered by the Mango binary.
	LocalProviderName  = "local"
	DockerProviderName = "docker"
)

// ProviderFactory constructs one provider client from worker-local
// configuration. Factories are invoked only for the selected provider, so an
// optional adapter never requires credentials or dependencies merely because
// it is registered.
type ProviderFactory func() (Provider, error)

// ProviderCapabilities are admission-safe facts about an adapter. They can be
// inspected without constructing the provider or loading its credentials.
type ProviderCapabilities struct {
	PackageSetup    bool
	LimitedNetwork  bool
	FileResources   bool
	SessionOutputs  bool
	SkillBundles    bool
	MemoryStores    bool
	GitRepositories bool
}

// ProviderRegistration is one deployment-selectable sandbox adapter.
type ProviderRegistration struct {
	Name         string
	Factory      ProviderFactory
	Capabilities ProviderCapabilities
}

// ProviderRegistry resolves a deployment-level provider name to its adapter.
// It is deliberately internal to worker composition: provider mechanics do not
// belong in the Mango Environment or Session wire models.
//
// A registry is immutable after construction and safe for concurrent reads.
type ProviderRegistry struct {
	factories    map[string]ProviderFactory
	capabilities map[string]ProviderCapabilities
	names        []string
}

// NewProviderRegistry validates and freezes the available provider set without
// invoking any factory.
func NewProviderRegistry(registrations ...ProviderRegistration) (*ProviderRegistry, error) {
	if len(registrations) == 0 {
		return nil, errors.New("sandbox: provider registry requires at least one registration")
	}
	registry := &ProviderRegistry{
		factories:    make(map[string]ProviderFactory, len(registrations)),
		capabilities: make(map[string]ProviderCapabilities, len(registrations)),
		names:        make([]string, 0, len(registrations)),
	}
	for _, registration := range registrations {
		if err := validateProviderName(registration.Name); err != nil {
			return nil, err
		}
		if registration.Factory == nil {
			return nil, fmt.Errorf(
				"sandbox: provider %q has no factory",
				registration.Name,
			)
		}
		if _, exists := registry.factories[registration.Name]; exists {
			return nil, fmt.Errorf(
				"sandbox: provider %q is registered more than once",
				registration.Name,
			)
		}
		registry.factories[registration.Name] = registration.Factory
		registry.capabilities[registration.Name] = registration.Capabilities
		registry.names = append(registry.names, registration.Name)
	}
	sort.Strings(registry.names)
	return registry, nil
}

// Capabilities returns admission-safe adapter capabilities without invoking
// the provider factory.
func (r *ProviderRegistry) Capabilities(name string) (ProviderCapabilities, error) {
	if r == nil {
		return ProviderCapabilities{}, errors.New("sandbox: provider registry is required")
	}
	capabilities, ok := r.capabilities[name]
	if !ok {
		return ProviderCapabilities{}, fmt.Errorf(
			"sandbox: unsupported provider %q (available: %s)",
			name,
			strings.Join(r.names, ", "),
		)
	}
	return capabilities, nil
}

// Open constructs the selected provider and verifies that the adapter reports
// the same stable name under which it was registered.
func (r *ProviderRegistry) Open(name string) (Provider, error) {
	if r == nil {
		return nil, errors.New("sandbox: provider registry is required")
	}
	factory, ok := r.factories[name]
	if !ok {
		return nil, fmt.Errorf(
			"sandbox: unsupported provider %q (available: %s)",
			name,
			strings.Join(r.names, ", "),
		)
	}
	provider, err := factory()
	if err != nil {
		return nil, fmt.Errorf("sandbox: initialize provider %q: %w", name, err)
	}
	if provider == nil {
		return nil, fmt.Errorf("sandbox: provider %q factory returned nil", name)
	}
	if provider.Name() != name {
		return nil, fmt.Errorf(
			"sandbox: provider registered as %q reports name %q",
			name,
			provider.Name(),
		)
	}
	declaredPackages := r.capabilities[name].PackageSetup
	packageCapability, implementsPackageCapability := provider.(PackageSetupProvider)
	actualPackages := implementsPackageCapability && packageCapability.SupportsPackageSetup()
	if declaredPackages != actualPackages {
		return nil, fmt.Errorf(
			"sandbox: provider %q package setup capability is registered as %t but reports %t",
			name,
			declaredPackages,
			actualPackages,
		)
	}
	declaredNetwork := r.capabilities[name].LimitedNetwork
	networkCapability, implementsNetworkCapability := provider.(LimitedNetworkProvider)
	actualNetwork := implementsNetworkCapability && networkCapability.SupportsLimitedNetwork()
	if declaredNetwork != actualNetwork {
		return nil, fmt.Errorf(
			"sandbox: provider %q limited network capability is registered as %t but reports %t",
			name,
			declaredNetwork,
			actualNetwork,
		)
	}
	declaredFiles := r.capabilities[name].FileResources
	fileCapability, implementsFileCapability := provider.(FileResourceProvider)
	actualFiles := implementsFileCapability && fileCapability.SupportsFileResources()
	if declaredFiles != actualFiles {
		return nil, fmt.Errorf(
			"sandbox: provider %q file resource capability is registered as %t but reports %t",
			name,
			declaredFiles,
			actualFiles,
		)
	}
	declaredOutputs := r.capabilities[name].SessionOutputs
	outputCapability, implementsOutputCapability := provider.(SessionOutputProvider)
	actualOutputs := implementsOutputCapability && outputCapability.SupportsSessionOutputs()
	if declaredOutputs != actualOutputs {
		return nil, fmt.Errorf(
			"sandbox: provider %q Session output capability is registered as %t but reports %t",
			name,
			declaredOutputs,
			actualOutputs,
		)
	}
	declaredSkills := r.capabilities[name].SkillBundles
	skillCapability, implementsSkillCapability := provider.(SkillBundleProvider)
	actualSkills := implementsSkillCapability && skillCapability.SupportsSkillBundles()
	if declaredSkills != actualSkills {
		return nil, fmt.Errorf(
			"sandbox: provider %q Skill bundle capability is registered as %t but reports %t",
			name,
			declaredSkills,
			actualSkills,
		)
	}
	declaredMemory := r.capabilities[name].MemoryStores
	memoryCapability, implementsMemoryCapability := provider.(MemoryStoreProvider)
	actualMemory := implementsMemoryCapability && memoryCapability.SupportsMemoryStores()
	if declaredMemory != actualMemory {
		return nil, fmt.Errorf(
			"sandbox: provider %q Memory Store capability is registered as %t but reports %t",
			name,
			declaredMemory,
			actualMemory,
		)
	}
	declaredRepositories := r.capabilities[name].GitRepositories
	repositoryCapability, implementsRepositoryCapability := provider.(GitRepositoryProvider)
	actualRepositories := implementsRepositoryCapability && repositoryCapability.SupportsGitRepositories()
	if declaredRepositories != actualRepositories {
		return nil, fmt.Errorf(
			"sandbox: provider %q Git repository capability is registered as %t but reports %t",
			name,
			declaredRepositories,
			actualRepositories,
		)
	}
	return provider, nil
}

// Names returns a sorted copy of the registered provider names.
func (r *ProviderRegistry) Names() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.names...)
}

func validateProviderName(name string) error {
	if name == "" {
		return errors.New("sandbox: provider name is required")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') ||
			(i > 0 && c >= '0' && c <= '9') ||
			(i > 0 && c == '-') {
			continue
		}
		return fmt.Errorf(
			"sandbox: invalid provider name %q (use a lowercase ASCII identifier)",
			name,
		)
	}
	return nil
}
