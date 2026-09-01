// Package sandbox provides isolated execution for agent tools.
//
// OpenSandbox is the sole control plane. There is no host-process or direct
// Docker executor. Each OpenSandbox runtime profile's documented trust boundary
// still applies; local container execution is not a hostile multi-tenant guarantee.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

// ErrNotFound means a durable provider reference no longer resolves to a
// sandbox. Callers must not silently provision an empty replacement: doing so
// would present lost session workspace state as a successful resume.
var ErrNotFound = errors.New("sandbox: durable reference not found")

// PermanentError marks invalid ownership or configuration that retrying on the
// same worker cannot repair. Turn Activities convert it into an honest public
// terminal result; standalone cleanup Activities use a non-retryable failure
// so operators can correct configuration before starting a fresh cleanup run.
type PermanentError struct {
	err error
}

func (e *PermanentError) Error() string { return e.err.Error() }
func (e *PermanentError) Unwrap() error { return e.err }

// Permanent marks an adapter error as unsafe to retry on the same worker.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	var target *PermanentError
	if errors.As(err, &target) {
		return err
	}
	return &PermanentError{err: err}
}

func IsPermanent(err error) bool {
	var target *PermanentError
	return errors.As(err, &target)
}

// Spec describes the sandbox to provision.
type Spec struct {
	// Timeout bounds each Exec call. Zero means no per-command timeout.
	Timeout time.Duration

	// Resource settings are interpreted by the selected provider.
	Image     string // container image ref; empty uses the provider default
	Memory    string // e.g. "512m"; empty uses the provider/daemon default
	CPUs      string // e.g. "1.0"; empty uses the default
	Network   string // "none" (default), "limited", or "bridge"
	PidsLimit int    // max processes; 0 uses the default
	// NetworkAllowedHosts is the final allowlist for a limited network. Setup
	// NetworkAllowedHosts may temporarily add package registries while package
	// setup runs; the final policy is restored before binding publication.
	NetworkAllowedHosts      []string
	SetupNetworkAllowedHosts []string
	// Packages are installed once while provisioning, before the durable
	// sandbox binding becomes visible to tool execution.
	Packages PackageSet
	// MemoryStores are immutable Session attachment descriptors. Providers with
	// Memory Store capability expose one durable mount for each descriptor.
	MemoryStores []MemoryStoreMount
}

func validateSandboxNetworkSpec(spec Spec) error {
	switch spec.Network {
	case "", "none", "bridge", "limited":
	default:
		return fmt.Errorf("sandbox: unsupported network mode %q", spec.Network)
	}
	if spec.Network != "limited" &&
		(len(spec.NetworkAllowedHosts) > 0 || len(spec.SetupNetworkAllowedHosts) > 0) {
		return errors.New("sandbox: network allowlists require limited networking")
	}
	return nil
}

// PackageSet is the normalized Mango Environment package plan.
type PackageSet struct {
	Apt   []string
	Cargo []string
	Gem   []string
	Go    []string
	NPM   []string
	Pip   []string
}

func (p PackageSet) Empty() bool {
	return len(p.Apt) == 0 && len(p.Cargo) == 0 && len(p.Gem) == 0 &&
		len(p.Go) == 0 && len(p.NPM) == 0 && len(p.Pip) == 0
}

// Command is a single process invocation within a sandbox.
type Command struct {
	Path  string
	Args  []string
	Stdin []byte
}

// Result is the outcome of an Exec call.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	// TimedOut is true when the command was killed because the timeout or
	// context deadline elapsed.
	TimedOut bool
}

// Ref is the durable, provider-owned identity of one sandbox. It is safe to
// persist in the control-plane database: credentials and connection details
// remain in worker configuration, never in the reference.
type Ref struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

func (r Ref) validate() error {
	if r.Provider == "" {
		return errors.New("sandbox: reference provider is required")
	}
	if r.ID == "" {
		return errors.New("sandbox: reference id is required")
	}
	return nil
}

// Sandbox is a provisioned execution environment. Relative file paths passed
// to ReadFile/WriteFile resolve beneath Root. Providers may additionally expose
// explicit absolute resource mounts, but must keep every other path confined
// and enforce each mount's read-only/read-write boundary.
type Sandbox interface {
	Exec(ctx context.Context, cmd Command) (*Result, error)
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte) error
	Root() string
	// Destroy is idempotent. Repeating it after the resource is already gone
	// must succeed so deletion workflows can safely retry a lost acknowledgement.
	Destroy(ctx context.Context) error
}

// BoundedFileReader is the file-tool data plane for reads that must not scale
// worker memory with an untrusted sandbox file. All Mango providers implement
// it by validating the same path authority as ReadFile, then returning at most
// maxBytes and reporting whether more bytes exist.
type BoundedFileReader interface {
	ReadFileBounded(
		ctx context.Context,
		path string,
		maxBytes int64,
	) (data []byte, truncated bool, err error)
}

func readFileBoundedByCommand(
	ctx context.Context,
	executor interface {
		Exec(context.Context, Command) (*Result, error)
	},
	resolvedPath string,
	maxBytes int64,
) ([]byte, bool, error) {
	if maxBytes <= 0 || maxBytes >= maxOutput {
		return nil, false, fmt.Errorf(
			"sandbox: bounded read limit must be between 1 and %d bytes",
			maxOutput-1,
		)
	}
	result, err := executor.Exec(ctx, Command{
		Path: "head",
		Args: []string{"-c", fmt.Sprintf("%d", maxBytes+1), "--", resolvedPath},
	})
	if err != nil {
		return nil, false, err
	}
	if result.TimedOut {
		return nil, false, fmt.Errorf("sandbox: bounded read timed out")
	}
	if result.ExitCode != 0 {
		message := strings.TrimSpace(string(result.Stderr))
		if message == "" {
			message = fmt.Sprintf("head exited with code %d", result.ExitCode)
		}
		return nil, false, fmt.Errorf("sandbox: bounded read: %s", message)
	}
	if int64(len(result.Stdout)) > maxBytes {
		return append([]byte(nil), result.Stdout[:maxBytes]...), true, nil
	}
	return append([]byte(nil), result.Stdout...), false, nil
}

// Provider owns sandbox resources outside the agent loop. Create must expose a
// stable provider-side lookup key so a retry after a lost response resolves the
// same logical resource. When a provider cannot atomically create-if-absent,
// SessionManager's durable binding election destroys the losing resource from
// concurrent successful creates. Attach reconstructs a client from a persisted
// Ref after a worker restart and must verify that the provider resource still
// belongs to the given sessionKey. Destroy remains on Sandbox so execution and
// teardown use the same authenticated provider client.
type Provider interface {
	Name() string
	Create(ctx context.Context, sessionKey string, spec Spec) (Ref, Sandbox, error)
	Attach(ctx context.Context, sessionKey string, ref Ref, spec Spec) (Sandbox, error)
}

// BoundSessionDestroyer removes every provider resource for a Session while
// preserving the durable binding as the authority for ownership validation and
// deletion order. Sandbox.Destroy must remain resource-scoped because it is
// also used to discard a losing resource after a concurrent binding election.
type BoundSessionDestroyer interface {
	DestroyBoundSession(context.Context, string, Ref) error
}

// PackageSetupProvider declares that package-manager commands execute inside
// the provider's isolation boundary rather than on the worker host. Providers
// must opt in explicitly; an undeclared capability is denied.
type PackageSetupProvider interface {
	SupportsPackageSetup() bool
}

// LimitedNetworkProvider declares support for enforcing per-sandbox host
// allowlists. Providers must opt in explicitly; a bridge/none toggle alone is
// not sufficient.
type LimitedNetworkProvider interface {
	SupportsLimitedNetwork() bool
}

// LimitedNetworkSandbox reconciles the exact runtime allowlist for a live
// sandbox. It is optional because most providers cannot enforce FQDN policy.
type LimitedNetworkSandbox interface {
	ApplyLimitedNetwork(ctx context.Context, allowedHosts []string) error
}

// SessionUploadsRoot is the absolute runtime directory Mango exposes for
// File-backed Session Resources.
const SessionUploadsRoot = domain.SessionUploadsRoot

// SessionOutputsRoot is the writable runtime directory whose regular files
// become downloadable Session-scoped Files at an idle boundary.
const SessionOutputsRoot = domain.SessionOutputsRoot

// SessionSkillsRoot is the provider-independent runtime directory containing
// immutable custom Skill trees. Callers must not derive this path from the
// provider's workspace root.
const SessionSkillsRoot = domain.SessionSkillsRoot

const SessionRepositoryRoot = domain.SessionRepositoryRoot

// SessionOutputProvider declares support for the Mango deliverable
// directory. Providers opt in because exporting an arbitrary workspace is not
// equivalent to confining and streaming /mnt/session/outputs.
type SessionOutputProvider interface {
	SupportsSessionOutputs() bool
}

// SessionOutputSandbox streams a tar archive containing the current children
// of SessionOutputsRoot. An absent or empty directory returns an empty archive.
// OpenSessionOutputs must be repeatable while the caller holds the Sandbox's
// ResourceSynchronizationSandbox lock. Consumers must still validate every
// archive entry before publishing it.
type SessionOutputSandbox interface {
	OpenSessionOutputs(context.Context) (io.ReadCloser, error)
}

// FileResourceMount describes one File copy expected inside a sandbox.
// RuntimePath must be a child of SessionUploadsRoot.
type FileResourceMount struct {
	// Identity distinguishes successive resources that reuse one runtime path.
	// It must remain stable across worker restarts.
	Identity       string
	RuntimePath    string
	SizeBytes      int64
	ChecksumSHA256 string
}

// FileResourceProvider declares support for materializing Session File
// resources. Providers must opt in explicitly because ordinary workspace
// writes do not expose the documented absolute uploads path.
type FileResourceProvider interface {
	SupportsFileResources() bool
}

// FileResourceSandbox reconciles File-backed Session Resources for a live
// sandbox. ImportFileResource must consume and validate the source bytes;
// provider-specific buffering limitations must be documented. Removal must be
// idempotent and must not remove a newer identity that reused the same runtime
// path. Individual providers may enforce a stronger read-only presentation.
type FileResourceSandbox interface {
	HasFileResource(context.Context, FileResourceMount) (bool, error)
	ImportFileResource(context.Context, FileResourceMount, io.Reader) error
	RemoveFileResource(context.Context, string, string) error
}

// GitRepositoryMount describes one immutable control-plane snapshot restored
// as a writable Git worktree. RuntimePath is a child of /workspace; the
// snapshot includes .git metadata at ResolvedCommit.
type GitRepositoryMount struct {
	Identity       string
	RuntimePath    string
	ResolvedCommit string
	SizeBytes      int64
	ChecksumSHA256 string
}

// GitRepositoryProvider declares support for restoring Mango-owned Git
// snapshots without requiring network access or Git in the sandbox image.
type GitRepositoryProvider interface {
	SupportsGitRepositories() bool
}

type GitRepositorySandbox interface {
	HasGitRepository(context.Context, GitRepositoryMount) (bool, error)
	ImportGitRepository(context.Context, GitRepositoryMount, io.Reader) error
	RemoveGitRepository(context.Context, string, string) error
}

// MemoryStoreProvider declares support for durable writable Memory Store
// mounts beneath /mnt/memory. Adapters opt in because an ordinary workspace
// directory does not provide cross-Session persistence or read-only access.
type MemoryStoreProvider interface {
	SupportsMemoryStores() bool
}

// MemoryStoreMount is the provider-independent mount contract captured in a
// Session. Identity is the Session Resource ID, not the mutable Store name.
type MemoryStoreMount struct {
	Identity    string
	StoreID     string
	RuntimePath string
	Access      string
}

// MemoryStoreFile is one canonical Memory head supplied by the control plane.
// Path is absolute within the Store (for example /preferences/editor.md), not
// the full sandbox mount path.
type MemoryStoreFile struct {
	MemoryID      string
	Path          string
	Content       []byte
	ContentSHA256 string
}

type MemoryStoreContent struct {
	Path    string
	Content []byte
}

// MemoryStoreSnapshot pairs the last control-plane baseline with the files an
// agent currently left in the mount. Initialized=false means no baseline has
// ever been published into this sandbox generation.
type MemoryStoreSnapshot struct {
	Initialized bool
	Baseline    []MemoryStoreFile
	Current     []MemoryStoreContent
}

// MemoryStoreSandbox lets the app layer converge PostgreSQL with provider-owned
// mounts without depending on Docker paths or remote-sandbox APIs.
type MemoryStoreSandbox interface {
	ReadMemoryStore(context.Context, MemoryStoreMount) (MemoryStoreSnapshot, error)
	ReplaceMemoryStore(context.Context, MemoryStoreMount, []MemoryStoreFile) error
}

// ResourceSynchronizationSandbox coordinates Session-shared tool execution
// with destructive resource refreshes. Tool operations take a shared lock so
// independent Threads may still run in parallel. A complete Memory snapshot,
// control-plane sync, and filesystem refresh take the exclusive lock.
//
// TryLockResourceSync is used before a tool: if another tool is already active,
// that tool joins the current resource wave instead of serializing behind a
// refresh. LockResourceSync is used after tools and during release, where the
// caller must wait for every active operation before publishing a new baseline.
type ResourceSynchronizationSandbox interface {
	LockResourceOperation(context.Context) (func(), error)
	TryLockResourceSync(context.Context) (context.Context, func(), bool, error)
	LockResourceSync(context.Context) (context.Context, func(), error)
}

// ReadOnlySkillMount describes one immutable canonical Skill archive expected
// beneath /workspace/skills. RuntimePath is the absolute Agent-scope directory;
// Name is its safe final component. ArchiveRoot is the upload's validated
// top-level directory and is stripped during extraction.
type ReadOnlySkillMount struct {
	Identity              string
	Name                  string
	RuntimePath           string
	ArchiveRoot           string
	SizeBytes             int64
	UncompressedSizeBytes int64
	ChecksumSHA256        string
}

// SkillBundleProvider declares support for isolated custom Skill trees whose
// canonical source is immutable. Providers with a native read-only mount may
// enforce immutability at the filesystem boundary; other providers must
// permission-harden and reconcile their sandbox-local copy.
type SkillBundleProvider interface {
	SupportsSkillBundles() bool
}

// SkillBundleSandbox reconciles immutable canonical Skill archives for a live
// sandbox. ImportReadOnlySkill must validate and publish the complete tree as
// one bundle; partial extraction must never become visible at the runtime path.
type SkillBundleSandbox interface {
	HasReadOnlySkill(context.Context, ReadOnlySkillMount) (bool, error)
	ImportReadOnlySkill(context.Context, ReadOnlySkillMount, io.Reader) error
}

func validateSandbox(provider Provider, ref Ref, box Sandbox) error {
	if provider == nil {
		return errors.New("sandbox: provider is required")
	}
	if box == nil {
		return errors.New("sandbox: provider returned a nil sandbox")
	}
	if err := ref.validate(); err != nil {
		return Permanent(err)
	}
	if ref.Provider != provider.Name() {
		return Permanent(fmt.Errorf(
			"sandbox: provider %q returned reference for %q",
			provider.Name(),
			ref.Provider,
		))
	}
	return nil
}
