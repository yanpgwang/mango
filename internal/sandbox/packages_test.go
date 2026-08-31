package sandbox

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestPackageSetupCommandsUseArgumentVectors(t *testing.T) {
	commands := packageSetupCommands(PackageSet{
		Apt:   []string{"git", "libpq-dev"},
		Cargo: []string{"ripgrep@14.1.1"},
		Gem:   []string{"rake:13.2.1"},
		Go:    []string{"golang.org/x/tools/gopls@v0.20.0"},
		NPM:   []string{"typescript@5.9.2"},
		Pip:   []string{"httpx==0.28.1", "; touch /tmp/not-a-command"},
	})
	want := []packageSetupCommand{
		{manager: "apt", command: Command{Path: "apt-get", Args: []string{"update"}}},
		{manager: "apt", command: Command{Path: "apt-get", Args: []string{
			"install", "-y", "--no-install-recommends", "git", "libpq-dev",
		}}},
		{manager: "cargo", command: Command{Path: "cargo", Args: []string{"install", "ripgrep@14.1.1"}}},
		{manager: "gem", command: Command{Path: "gem", Args: []string{"install", "rake:13.2.1"}}},
		{manager: "go", command: Command{Path: "go", Args: []string{"install", "golang.org/x/tools/gopls@v0.20.0"}}},
		{manager: "npm", command: Command{Path: "npm", Args: []string{"install", "--global", "typescript@5.9.2"}}},
		{manager: "pip", command: Command{Path: "python3", Args: []string{
			"-m", "pip", "install", "httpx==0.28.1", "; touch /tmp/not-a-command",
		}}},
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("setup commands = %#v, want %#v", commands, want)
	}
}

type packageSetupSandbox struct {
	mu              sync.Mutex
	commands        []Command
	results         []*Result
	errors          []error
	networkPolicies [][]string
	operations      []string
}

func (s *packageSetupSandbox) Exec(_ context.Context, command Command) (*Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, command)
	s.operations = append(s.operations, "exec:"+command.Path)
	index := len(s.commands) - 1
	if index < len(s.errors) && s.errors[index] != nil {
		return nil, s.errors[index]
	}
	if index < len(s.results) {
		return s.results[index], nil
	}
	return &Result{}, nil
}

func (*packageSetupSandbox) ReadFile(context.Context, string) ([]byte, error) {
	return nil, errors.New("not implemented")
}
func (*packageSetupSandbox) WriteFile(context.Context, string, []byte) error {
	return errors.New("not implemented")
}
func (*packageSetupSandbox) Root() string                  { return "/workspace" }
func (*packageSetupSandbox) Destroy(context.Context) error { return nil }

func (s *packageSetupSandbox) ApplyLimitedNetwork(
	_ context.Context,
	allowedHosts []string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cloned := append([]string(nil), allowedHosts...)
	s.networkPolicies = append(s.networkPolicies, cloned)
	s.operations = append(s.operations, "network:"+strings.Join(cloned, ","))
	return nil
}

func (s *packageSetupSandbox) commandCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.commands)
}

func (s *packageSetupSandbox) networkState() ([][]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	policies := make([][]string, len(s.networkPolicies))
	for index, policy := range s.networkPolicies {
		policies[index] = append([]string(nil), policy...)
	}
	return policies, append([]string(nil), s.operations...)
}

type packageSetupProvider struct {
	box      *packageSetupSandbox
	creates  int
	attaches int
}

func (*packageSetupProvider) Name() string { return "package-test" }

func (*packageSetupProvider) SupportsPackageSetup() bool { return true }

func (*packageSetupProvider) SupportsLimitedNetwork() bool { return true }

func (p *packageSetupProvider) Create(
	context.Context,
	string,
	Spec,
) (Ref, Sandbox, error) {
	p.creates++
	return Ref{Provider: p.Name(), ID: "box_1"}, p.box, nil
}

func (p *packageSetupProvider) Attach(
	context.Context,
	string,
	Ref,
	Spec,
) (Sandbox, error) {
	p.attaches++
	return p.box, nil
}

func TestSessionManagerPublishesBindingOnlyAfterPackageSetup(t *testing.T) {
	ctx := context.Background()
	box := &packageSetupSandbox{results: []*Result{
		{ExitCode: 1, Stderr: []byte("registry unavailable")},
		{ExitCode: 0},
	}}
	provider := &packageSetupProvider{box: box}
	bindings := newMemoryBindingStore()
	manager := NewSessionManager(provider, bindings)
	spec := Spec{Packages: PackageSet{Pip: []string{"httpx==0.28.1"}}}

	if _, err := manager.Acquire(ctx, "sess_packages", spec); err == nil {
		t.Fatal("package setup failure was accepted")
	}
	if _, found, err := bindings.GetSandboxBinding(ctx, "sess_packages"); err != nil || found {
		t.Fatalf("binding after failed setup: found=%v err=%v", found, err)
	}
	if _, found, err := bindings.GetSandboxProvisioningIntent(ctx, "sess_packages"); err != nil || !found {
		t.Fatalf("intent after failed setup: found=%v err=%v", found, err)
	}

	if _, err := manager.Acquire(ctx, "sess_packages", spec); err != nil {
		t.Fatalf("retry package setup: %v", err)
	}
	if _, found, err := bindings.GetSandboxBinding(ctx, "sess_packages"); err != nil || !found {
		t.Fatalf("binding after successful setup: found=%v err=%v", found, err)
	}
	if got := box.commandCount(); got != 2 {
		t.Fatalf("setup command count = %d, want 2", got)
	}

	restarted := NewSessionManager(provider, bindings)
	if _, err := restarted.Acquire(ctx, "sess_packages", spec); err != nil {
		t.Fatalf("attach after restart: %v", err)
	}
	if got := box.commandCount(); got != 2 {
		t.Fatalf("setup reran after binding: command count = %d", got)
	}
	if provider.attaches != 1 {
		t.Fatalf("attach count = %d, want 1", provider.attaches)
	}
}

func TestSessionManagerRejectsUndeclaredPackageSetupBeforeProvisioning(t *testing.T) {
	bindings := newMemoryBindingStore()
	manager := NewSessionManager(newStateProvider(), bindings)
	_, err := manager.Acquire(context.Background(), "sess_unsupported_packages", Spec{
		Packages: PackageSet{Pip: []string{"httpx==0.28.1"}},
	})
	if err == nil || !IsPermanent(err) {
		t.Fatalf("unsupported package setup error = %v, want permanent rejection", err)
	}
	if _, found, loadErr := bindings.GetSandboxProvisioningIntent(
		context.Background(), "sess_unsupported_packages",
	); loadErr != nil || found {
		t.Fatalf("unsupported package setup created intent: found=%v err=%v", found, loadErr)
	}
}

func TestSessionManagerRejectsBindingWithoutPackageSetupEvidence(t *testing.T) {
	ctx := context.Background()
	box := &packageSetupSandbox{}
	provider := &packageSetupProvider{box: box}
	bindings := newMemoryBindingStore()
	_, err := bindings.PutSandboxBinding(ctx, Binding{
		SessionID: "sess_legacy_binding",
		Ref:       Ref{Provider: provider.Name(), ID: "box_legacy"},
		SpecHash:  specHash(Spec{}),
	})
	if err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	manager := NewSessionManager(provider, bindings)
	_, err = manager.Acquire(ctx, "sess_legacy_binding", Spec{
		Packages: PackageSet{Pip: []string{"httpx==0.28.1"}},
	})
	if err == nil || !IsPermanent(err) {
		t.Fatalf("legacy binding error = %v, want permanent rejection", err)
	}
	if provider.attaches != 0 || box.commandCount() != 0 {
		t.Fatalf("legacy binding was attached or initialized: attaches=%d commands=%d",
			provider.attaches, box.commandCount())
	}
}

func TestSessionManagerRejectsCachedSandboxWithoutPackageSetupEvidence(t *testing.T) {
	ctx := context.Background()
	box := &packageSetupSandbox{}
	provider := &packageSetupProvider{box: box}
	manager := NewSessionManager(provider, newMemoryBindingStore())
	if _, err := manager.Acquire(ctx, "sess_cached_binding", Spec{}); err != nil {
		t.Fatalf("seed cached sandbox: %v", err)
	}
	_, err := manager.Acquire(ctx, "sess_cached_binding", Spec{
		Packages: PackageSet{Pip: []string{"httpx==0.28.1"}},
	})
	if err == nil || !IsPermanent(err) {
		t.Fatalf("cached binding error = %v, want permanent rejection", err)
	}
	if box.commandCount() != 0 {
		t.Fatalf("cached sandbox ran package setup without matching binding evidence")
	}
}

func TestSessionManagerEnforcesLimitedNetworkAcrossProvisioningAndAttach(t *testing.T) {
	ctx := context.Background()
	box := &packageSetupSandbox{}
	provider := &packageSetupProvider{box: box}
	bindings := newMemoryBindingStore()
	manager := NewSessionManager(provider, bindings)
	spec := Spec{
		Network:                  "limited",
		NetworkAllowedHosts:      []string{"api.example.com"},
		SetupNetworkAllowedHosts: []string{"api.example.com", "pypi.org"},
		Packages:                 PackageSet{Pip: []string{"httpx==0.28.1"}},
	}

	if _, err := manager.Acquire(ctx, "sess_limited", spec); err != nil {
		t.Fatalf("provision limited sandbox: %v", err)
	}
	policies, operations := box.networkState()
	wantOperations := []string{
		"network:api.example.com,pypi.org",
		"exec:python3",
		"network:api.example.com",
	}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Fatalf("provisioning operations = %v, want %v", operations, wantOperations)
	}
	if len(policies) != 2 {
		t.Fatalf("provisioning policy count = %d, want 2", len(policies))
	}

	if _, err := manager.Acquire(ctx, "sess_limited", spec); err != nil {
		t.Fatalf("reuse limited sandbox: %v", err)
	}
	policies, _ = box.networkState()
	if len(policies) != 2 || box.commandCount() != 1 {
		t.Fatalf("cached acquire repeated work: policies=%d commands=%d", len(policies), box.commandCount())
	}

	updated := spec
	updated.NetworkAllowedHosts = []string{"mcp.example.com"}
	updated.SetupNetworkAllowedHosts = []string{"mcp.example.com", "pypi.org"}
	if _, err := manager.Acquire(ctx, "sess_limited", updated); err != nil {
		t.Fatalf("reconcile updated limited policy: %v", err)
	}
	policies, _ = box.networkState()
	if len(policies) != 3 || !reflect.DeepEqual(policies[2], []string{"mcp.example.com"}) {
		t.Fatalf("updated policies = %v", policies)
	}
	if box.commandCount() != 1 {
		t.Fatalf("network update repeated package setup: commands=%d", box.commandCount())
	}

	restarted := NewSessionManager(provider, bindings)
	if _, err := restarted.Acquire(ctx, "sess_limited", updated); err != nil {
		t.Fatalf("attach limited sandbox after restart: %v", err)
	}
	policies, _ = box.networkState()
	if provider.attaches != 1 || len(policies) != 4 ||
		!reflect.DeepEqual(policies[3], []string{"mcp.example.com"}) {
		t.Fatalf("restart reconciliation: attaches=%d policies=%v", provider.attaches, policies)
	}
}

func TestSessionManagerRejectsUnsupportedLimitedNetworkBeforeProvisioning(t *testing.T) {
	bindings := newMemoryBindingStore()
	manager := NewSessionManager(newStateProvider(), bindings)
	_, err := manager.Acquire(context.Background(), "sess_unsupported_limited", Spec{
		Network: "limited", NetworkAllowedHosts: []string{"example.com"},
	})
	if err == nil || !IsPermanent(err) {
		t.Fatalf("unsupported limited network error = %v, want permanent rejection", err)
	}
	if _, found, loadErr := bindings.GetSandboxProvisioningIntent(
		context.Background(), "sess_unsupported_limited",
	); loadErr != nil || found {
		t.Fatalf("limited network created intent: found=%v err=%v", found, loadErr)
	}
}
