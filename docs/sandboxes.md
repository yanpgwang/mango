---
title: Sandbox backends
slug: /sandboxes
---

# Sandbox backends

Sandbox support is intentionally incremental. A backend is not presented as
production-ready merely because it can execute a command: its isolation model,
lifecycle guarantees, operational dependencies, and known limits must also be
clear.

Docker execution does not add another HTTP service. Remote adapters
call an independently deployed sandbox service through the same in-process Go
boundary:

```text
Temporal Activity -> SessionManager -> sandbox.Provider -> sandbox.Sandbox
```

`SessionManager` gives each session one logical sandbox. PostgreSQL persists the
provider name and opaque external ID; a restarted worker calls `Attach` instead
of creating an empty replacement. `Sandbox` exposes command execution, confined
file access, a workspace root, and teardown. The execution worker selects one
compiled adapter through an internal registry. `MANGO_SANDBOX` accepts
`docker` (the default), `e2b`, `cube`, `opensandbox`, or `daytona`; `local` and
unknown names fail startup instead of falling back to host execution. Provider
selection does not add fields to the public Environment or Session resources.

The `serve` and `orchestrate` processes for one deployment must use the same
`MANGO_SANDBOX` value. API admission reads that provider's declared
capabilities without loading worker credentials; the worker verifies the same
capability again before provisioning.

## Support levels

- **Available**: implemented, documented, and exercised by repository tests.
- **Preview**: implemented with offline and opt-in live conformance, but still
  awaiting promotion based on repeatable service-level validation.
- **Planned**: selected for a dedicated adapter, but not implemented.
- **Evaluating**: useful integration shape, without a committed adapter.

These labels describe project support, not a security certification.

## Backend matrix

| Backend | Status | Isolation model | Limited egress | Session state | Intended use |
|---|---|---|---|---|---|
| Docker | Available; default | Container filesystem, namespaces/cgroups, configurable limits; provider calls default to no network while cloud Environments request bridge networking | No; rejected | Reattaches by container ID on the same Docker daemon | Controlled single-host self-hosting |
| [E2B](https://github.com/e2b-dev/E2B) | Preview | Managed microVM service | No; rejected | E2B ID plus auto-pause filesystem persistence | Managed production |
| [Tencent CubeSandbox](https://github.com/TencentCloud/CubeSandbox) | Preview | E2B-compatible microVM service | No; rejected | Provider-owned durable sandbox ID | Self-hosted production on Linux/KVM |
| [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) | Preview; Docker runtime manually live-verified | Docker or Kubernetes-backed sandbox service | Yes; host allowlist | Provider-owned durable sandbox ID | Self-hosted production |
| [Daytona](https://www.daytona.io/docs/en/sandboxes/) | Preview | Managed or self-hosted sandbox service | No; rejected | Deterministic name, durable ID, and auto-pause | Managed production |
| [Modal](https://modal.com/docs/guide/sandboxes) | Planned | Managed sandbox service | Planned | Provider-owned durable sandbox ID | Managed production |
| [Runloop](https://docs.runloop.ai/docs/devboxes/overview) | Planned | Managed devbox service | Planned | Suspend, resume, and snapshot lifecycle | Managed production |
| [Kubernetes SIG Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) | Planned | Kubernetes CRD, controller, and routing layer | Planned | Stateful sandbox resource | Kubernetes deployments |

The Docker provider uses the Docker Engine API through the supported Moby Go
client with API-version negotiation; it has no runtime dependency on the
`docker` CLI. It has not been audited for hostile multi-tenant workloads.
No backend currently carries a production security claim. The old local provider
remains only for legacy repository test fixtures; the binary does not register
it and no unsafe-local setting re-enables it.

### Capability matrix

Capabilities are admitted independently. A backend that passes the core
lifecycle contract does not automatically claim Files, outputs, Skills,
Memory, or network enforcement.

| Backend | Packages | File Resources | Git repositories | Session Outputs | Skills | Memory | Limited egress |
|---|---:|---:|---:|---:|---:|---:|---:|
| Docker | Yes | Yes, read-only | Yes, writable | Yes | Yes, read-only | Yes | No |
| E2B | Yes | Yes, buffered writable-copy limitation | Yes, buffered writable | Yes, buffered | Yes, buffered hardened-copy limitation | No | No |
| CubeSandbox | Yes | Yes, buffered writable-copy limitation | Yes, buffered writable | Yes, buffered | Yes, buffered hardened-copy limitation | No | No |
| OpenSandbox | Yes | Yes, writable-copy limitation | Yes, writable | Yes | Yes, hardened-copy limitation | No | Yes |
| Daytona | Yes | Yes, writable-copy limitation | Yes, writable | Yes | Yes, hardened-copy limitation | No | No |

`No` means the adapter rejects admission for that capability; it does not mean
the external provider could never implement it. Preview backends retain their
Preview status even where an individual capability is implemented.

## File Resource mounts

File-backed Session Resources require more than ordinary workspace writes: the
provider must stream an independently stored object to its documented absolute
path and remove it idempotently after a crash. Provider capability admission is
explicit. Mango treats the mounted copy as read-only; providers that do not yet
enforce that property are recorded as limited rather than being presented as
complete implementations.

Docker stages validated bytes in a provider-owned host directory and
bind-mounts that directory read-only at `/mnt/session/uploads`. Every remote
adapter uses its pinned official Go SDK filesystem client to create the uploads
tree, validate File bytes, record the applied resource identity, and delete the
copy. They do not call a provider CLI or an in-sandbox shell command for
materialization. OpenSandbox and Daytona stream the transfer; the current
E2B/Cube-compatible Go client accepts only `[]byte`, so those adapters buffer
each complete File in worker memory and retain provider-default file modes
because that client does not expose per-operation mode options. Remote copies
are writable by the Agent. Sandbox edits remain local and do not mutate the
S3-backed source or the downloadable Session File; a later resource with a new
identity replaces the path, and an interrupted import is retried before the
next tool.

E2B and Cube retain the public File limits and checksum validation, but their
current buffering means the worker must have enough memory for the largest
accepted individual resource.

## Git repository mounts

Git repositories use one provider-neutral lifecycle. The control plane resolves
the remote and persists an exact, bounded tar snapshot before Session admission.
The worker validates snapshot size, SHA-256 digest, entry count, path
containment, entry types, `.git/HEAD`, and symlink containment before sending
anything to a sandbox. It then uploads one archive, extracts to a sibling
staging directory, and atomically publishes the writable worktree at the
documented `/workspace` path.

A provider-owned marker records resource identity, resolved commit, checksum,
and pending/ready state. A lost acknowledgement after publication is adopted
only when the pending marker matches; a path owned by another resource is never
overwritten. Retrying materialization preserves Agent edits instead of
re-extracting the canonical snapshot. Removal checks identity before deleting a
path, so an old tombstone cannot remove a replacement.

The sandbox does not need outbound network access or a Git executable for
materialization. It does need a POSIX shell and `tar`, the same image contract
already required for remote Session output export. Docker streams the archive
through the Engine API. OpenSandbox and Daytona stream through their official
filesystem clients; the E2B/Cube-compatible data plane currently buffers the
uploaded archive as an explicit Preview limitation.

## Session output mounts

Docker, E2B, CubeSandbox, OpenSandbox, and Daytona expose the writable
`/mnt/session/outputs` boundary. Before a primary Session becomes idle, the
worker attaches to the existing durable sandbox, takes the provider resource
lock, streams the directory as an archive, validates every path and entry type,
and publishes regular files to Mango's S3-compatible Files store. The worker
never creates a sandbox solely for output discovery.

Docker exports its provider-owned bind mount through the Engine archive API.
Remote adapters create a uniquely named tar snapshot in an adapter-owned
control directory, read it through their official SDK file clients, and remove
it when the returned reader closes. OpenSandbox and Daytona stream the archive;
E2B and Cube buffer the complete archive in worker memory. Mango's file tools
authorize the output root but reject the control directory. Remote images
selected for these adapters must provide a POSIX `tar` executable; a missing or
failed archiver is reported explicitly instead of treating the output tree as
empty.

The mount and publication capabilities are separate from File Resource input
mounts. Every provider must pass the shared output conformance suite: built-in
file tools and shell commands must see the same durable root, export must be
repeatable under the resource lock, and an adapter without that proof remains
fail-closed. E2B and Cube meet that lifecycle contract with a documented
buffering limitation rather than the streaming implementation used by the
other providers. Docker sandboxes created before the output mount existed must
be recreated rather than silently producing an empty export.

## Custom Skill mounts

Custom Skill execution uses a separate provider capability because a bundle is
an immutable directory tree, not a File Resource. Every capable adapter
verifies the compressed size and SHA-256, revalidates archive paths and entry
types, and publishes each pinned tree beneath `/workspace/skills/<name>/` or an
Agent-scoped directory below `/workspace/skills/.agents/`.

Docker stages canonical archives beneath the provider-owned per-Session host
root and exposes the complete `skills` directory through an unconditional
read-only bind mount. Attach after worker restart recovers the same host root.
E2B, CubeSandbox, OpenSandbox, and Daytona instead use one shared remote
materializer over their official SDK file clients. It validates and extracts
the archive in a bounded worker temporary directory, uploads a sibling staging
tree, makes that tree non-writable, publishes it inside the sandbox filesystem,
and records an adapter-owned identity plus `SKILL.md` checksum. OpenSandbox and
Daytona stream files; the current E2B/Cube-compatible SDK buffers each file.

Remote providers do not expose a native read-only bind mount through these
adapters. Mango's `write` and `edit` boundary rejects the Skill root and the
tree receives non-writable modes, but `bash` running with sufficient sandbox
privileges can still change its content. Before every tool, reconciliation
verifies the immutable identity and main instruction bytes and repairs
detectable changes.

The canonical S3 archive is never modified. This is a Preview limitation, not
equivalence to Docker's filesystem-enforced read-only mount; supporting-file
changes that preserve the main instruction entrypoint may remain local until a
new identity or other detectable damage causes rematerialization.

Remote images must provide a POSIX `/bin/sh` plus `chmod`, `mv`, and `rm` for
permission restoration, same-filesystem publication, rollback, and cleanup.

The worker checks provider markers before every relevant tool step, repairs
detectable damage, and removes abandoned extraction directories. Sandbox
destruction removes provider-owned Skill state with the sandbox. Docker
containers created before its mount existed fail closed for pinned Skills and
must be recreated; Docker cannot add a bind mount to a live container. Local
execution and Environment Worker execution do not advertise the capability.

## Memory Store mounts

Memory Stores use a distinct provider capability because they are durable,
cross-Session, writable resources rather than immutable attachments. Docker is
currently the only adapter that advertises it. Each attached Store is exposed
at `/mnt/memory/<store-slug>` as ordinary UTF-8 files. A `read_only` attachment
is a read-only bind mount even to container root; a `read_write` attachment is
writable during the tool step.

Before the first tool in a concurrent wave runs, the worker writes any surviving
local changes from an earlier interrupted Activity, merges the current
PostgreSQL heads, and records their IDs and SHA-256 values in provider-owned
state outside the mount. Concurrent Threads then share that filesystem wave;
the mount is not refreshed underneath an active tool. After every active tool
in the wave has released its shared resource lock, changed, created, and deleted
files are committed in one PostgreSQL transaction as immutable `session_actor`
Versions and the baseline is refreshed under an exclusive provider lock. A
concurrent external change to the same baseline returns a precondition error
instead of silently overwriting it. Session deletion performs a final
writeback before destroying the sandbox so a crash between tool execution and
ordinary writeback does not discard Memory changes.

## Backend contract

A backend implements the core lifecycle contract when it can:

1. expose a stable provider name;
2. idempotently create one resource for a session key;
3. attach to a persisted opaque reference after restart;
4. execute a command with cancellation and bounded output;
5. read and write paths relative to the workspace;
6. destroy the resource idempotently.

These requirements are executable in `internal/sandbox/sandboxtest`. Docker
and every remote provider's opt-in live test run the same suite,
including cross-client Create/Attach, workspace preservation, ownership
rejection, cancellation, and post-delete missing-reference behavior. Offline
adapter tests cover the same contract without credentials. Provider-specific
tests cover protocol translation, isolation, and resource controls separately.

The built-in toolset currently assumes a POSIX-like environment with
`/bin/sh`, `find`, and `grep`. A backend that does not provide those commands is
not compatible with all executing built-ins yet.

## Lifecycle today

- The first tool-using run idempotently creates the provider resource and
  persists `{provider, external_id, spec_hash}` in PostgreSQL. Before calling
  the provider it writes a non-secret provisioning intent, installs the
  Session's snapshotted Environment packages, and publishes the binding only
  after every package-manager command succeeds. A worker reconciler recovers
  and completes any resource left by a crash between those commits.
- Package configuration supports `apt`, `cargo`, `gem`, `go`, `npm`, and `pip`.
  Commands use argument vectors rather than shell interpolation. The selected
  image must contain each requested manager, and package validation remains the
  caller's responsibility. An install failure leaves the provisioning intent
  for retry and does not expose the sandbox to tool execution.
- Limited networking is admitted only when the selected provider declares and
  implements exact host-level egress reconciliation. OpenSandbox creates a
  deny-by-default policy, temporarily expands it for configured package setup,
  restores the final allowlist before binding, and reconciles MCP-derived
  changes on later turns and worker attach. Other implemented backends reject
  the policy at API admission.
- Remote services receive a fixed-length hash of the session key as their
  ownership label; credentials and raw session identifiers are not persisted in
  the provider reference.
- Later turns reuse the cached client; a restarted worker attaches through the
  persisted reference.
- Different sessions never share a logical sandbox.
- Becoming idle retains it.
- Deleting the session fences admission, stops its Session Workflow, durably
  retries provider teardown on the worker, removes the binding, and only then
  deletes the session row.
- A worker that discovers an interrupted deletion restarts or joins its
  deterministic cleanup Workflow and finalizes the fenced PostgreSQL row. An
  unbound provisioning intent is recovered and destroyed before finalization.
- A persisted reference that no longer exists fails explicitly; Mango does not
  silently replace lost workspace state with an empty sandbox.
- A deployment must keep a worker for every provider name still referenced by
  a binding or provisioning intent. Changing the configured provider does not
  migrate existing resources; remove their sessions or restore the old provider
  before retiring that adapter.

## Required production lifecycle

Remote adapters must use the same lifecycle tests. Production deployments still
need provider health reporting and provider-aware task routing when
heterogeneous workers share a control plane. Pause, snapshot, fork, quotas, and
eviction remain optional capabilities rather than requirements of the core
interface.

## Remote provider configuration

Credentials stay in worker configuration. They are never written to PostgreSQL
or returned by the Mango API.

| Provider | Required | Common optional values |
|---|---|---|
| `e2b` | `E2B_API_KEY` | `E2B_API_URL`, `E2B_TEMPLATE_ID`, `E2B_DOMAIN`, `E2B_IDLE_TIMEOUT` |
| `cube` | `CUBE_API_URL`, `CUBE_TEMPLATE_ID` | `CUBE_API_KEY`, `CUBE_SANDBOX_DOMAIN`, `CUBE_PROXY_*`, `CUBE_IDLE_TIMEOUT` |
| `opensandbox` | `OPEN_SANDBOX_DOMAIN` | `OPEN_SANDBOX_API_KEY`, `OPEN_SANDBOX_IMAGE`, `OPEN_SANDBOX_USE_SERVER_PROXY` |
| `daytona` | `DAYTONA_API_KEY` | `DAYTONA_API_URL`, `DAYTONA_TARGET`, `DAYTONA_SNAPSHOT`, `DAYTONA_IMAGE`, `DAYTONA_AUTO_PAUSE_MINUTES` |

For local development, `make dev-env-init` creates
`~/.config/mango/dev.env` from `config/dev.env.example` with mode `0600`.
`scripts/with-dev-env <command>` loads it explicitly and works across
worktrees.

Live conformance is opt-in. Each provider gate exercises lifecycle, File
Resources, Session Outputs, and custom Skill materialization:

```bash
scripts/with-dev-env env MANGO_LIVE_E2B=1 \
  go test ./internal/sandbox -run '^TestE2BLiveConformance$' -count=1

scripts/with-dev-env env MANGO_LIVE_OPENSANDBOX=1 \
  go test ./internal/sandbox -run '^TestOpenSandboxLiveConformance$' -count=1
```

The equivalent gates are `MANGO_LIVE_CUBE` and `MANGO_LIVE_DAYTONA`.
Ordinary tests never contact a service or create billable resources.

Claude's public environment guides historically informed the
Session/Environment distinction:
[cloud environment setup](https://platform.claude.com/docs/en/managed-agents/environments)
and
[self-hosted sandbox](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes).
These references do not define Mango's backend roadmap or imply hosted
isolation equivalence.

## Adding a backend

A backend contribution should:

- keep its external dependency optional and fail fast when explicitly selected
  but unavailable;
- register a lazy factory under a stable lowercase provider name and pass the
  shared `sandboxtest` lifecycle suite;
- preserve the session-scoped ownership contract;
- document its trust boundary, network defaults, resource controls, host
  requirements, and unsupported lifecycle features;
- keep default tests offline and make daemon/network-dependent tests opt-in;
- avoid changing the AgentRuntime or public HTTP API for provider-specific
  mechanics;
- avoid a production or multi-tenant safety claim without evidence and an
  explicit security review.

Before a substantial backend integration, record the intended use case and
lifecycle implications in the pull request, a design document, or an Issue so
they can be reviewed independently of the adapter code.
