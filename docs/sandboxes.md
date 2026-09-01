---
title: Sandbox runtime
slug: /sandboxes
---

# Sandbox runtime

Mango uses [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) as its only
sandbox control plane. Docker and Kubernetes are OpenSandbox runtime choices;
Mango does not operate either one directly and does not keep a direct-Docker
fallback.

```text
Mango Activity
  -> SessionManager
  -> Mango sandbox lifecycle interface
  -> OpenSandbox Go SDK
  -> OpenSandbox server
       -> Docker                 local development and CI
       -> Kubernetes + Kata      production-candidate profile
```

This split keeps product semantics in Mango and infrastructure mechanics in
OpenSandbox:

- Mango owns Session identity, durable bindings, retries, resource
  reconciliation, Memory writeback, and deletion fencing.
- OpenSandbox owns sandbox creation, attachment, command/file transport,
  volumes, network policy, and its Docker or Kubernetes workload lifecycle.
- The Mango public API never exposes OpenSandbox, Docker, Kubernetes, or Kata
  resource models.

Mango uses the official OpenSandbox Go SDK. Its internal `Provider` and
`Sandbox` interfaces remain as trust and lifecycle boundaries for Mango code;
they are not a deployment-selectable plugin system.

## Current support

| Profile | Purpose | Status | Isolation statement |
| --- | --- | --- | --- |
| OpenSandbox on Docker | Local development and public CI | Supported for development | Container isolation on the developer/CI Docker host; not a hostile multi-tenant boundary |
| OpenSandbox on Kubernetes with Kata | Production-candidate qualification | Preview | Must pass the committed RuntimeClass, workload, egress, restart, failure, and upgrade gates before a production claim |

The repository pins the local OpenSandbox server and helper images by version
and multi-architecture digest. An adapter that can run `echo` is not thereby
production-ready: operational recovery and isolation evidence are separate
requirements.

## Lifecycle contract

Each Session has one logical sandbox. PostgreSQL persists the stable provider
name `opensandbox`, the opaque OpenSandbox ID, and a hash of the applied spec.
The raw Session key is hashed before it becomes OpenSandbox metadata.

The lifecycle requires Mango to:

1. idempotently find or create one OpenSandbox resource for a Session key;
2. persist the opaque reference only after package and resource setup succeeds;
3. attach to that reference after a worker restart rather than create an empty
   replacement;
4. preserve workspace and resource state across attachment;
5. reject a resource owned by a different Session;
6. propagate command cancellation, timeout, exit status, and exact stdout and
   stderr bytes;
7. destroy the resource idempotently after Session deletion is durably fenced.

A missing persisted resource fails explicitly. Mango never replaces lost state
with a fresh sandbox silently.

OpenSandbox's command event stream is line-oriented, so the adapter captures
stdout and stderr into unpredictable sandbox-local files and downloads the
exact bytes after the command. Capture files live outside the user workspace
and are removed after each command.

## Agent and maintenance identities

Agent commands run with numeric UID/GID `1000:1000`. Trusted package and
resource maintenance commands use the sandbox's root identity. The local
OpenSandbox Docker profile also enables `no_new_privileges`; the Kubernetes
candidate requires cluster admission to make `allowPrivilegeEscalation: false`
explicit on every generated regular container, and the live gate verifies the
resulting Pod rather than trusting the template or RuntimeClass name alone.

This is an internal trust split, not an end-user identity system. The
OpenSandbox server is trusted infrastructure. In the local profile only that
service receives the Docker socket; neither the Mango API nor worker mounts it,
and both application processes retain their non-root image user.

## Runtime image contract

The configured image must provide:

- a POSIX `/bin/sh`;
- `tar`, `find`, `chmod`, `chown`, `mv`, and `rm`;
- any package manager selected by the Environment (`apt`, `cargo`, `gem`,
  `go`, `npm`, or `pip`);
- commands required by the Agent's tools.

Local development defaults to `python:3.12-slim`. Production should use a
reviewed, immutable image digest rather than a mutable tag.

## Resource capabilities

The sole backend supports package setup, File Resources, Git repositories,
Session Outputs, Skills, Memory Stores, and limited egress. Each capability
still has its own lifecycle and tests; core command execution alone does not
establish resource correctness.

### File Resources

Mango validates size and SHA-256, uploads the canonical object through the
OpenSandbox file API, records an identity marker, and publishes it beneath
`/mnt/session/uploads`. A new resource identity replaces the sandbox copy
idempotently. The source object in Mango's S3-compatible store is never
mutated.

The current OpenSandbox File Resource is an independent, writable
sandbox-local copy. Agent shell commands and Mango file tools may edit it;
those edits never mutate the immutable source object in Mango's S3-compatible
store and are discarded with the Session sandbox unless copied to Session
Outputs. This intentionally differs from CMA's read-only presentation and is a
documented limitation of the current copy-based materializer.

### Git repositories

The control plane resolves a public HTTPS repository to an exact commit and
persists a bounded tar snapshot. The worker validates the archive and restores
it without sandbox network access or a Git executable. Publication uses a
sibling staging directory and an identity marker, so retries adopt an already
published tree and preserve Agent edits. The final worktree is writable by UID
1000.

### Session Outputs

`/mnt/session/outputs` is writable by the Agent. Before a primary Session
becomes idle, Mango attaches to the existing sandbox, takes the resource lock,
creates a bounded tar snapshot, validates all paths and entry types, and
publishes regular files to the configured Files store. It never provisions a
sandbox solely to inspect outputs.

### Skills

Custom Skill archives are validated in a bounded worker temporary directory,
uploaded to a sibling staging tree, made non-writable, and published below
`/workspace/skills`. The canonical S3 archive is immutable. Because this is a
permission-hardened copy rather than a native read-only mount, reconciliation
checks the identity and main `SKILL.md` content before relevant tools and
repairs detectable changes.

### Memory Stores

Each attached Memory Store becomes one OpenSandbox-managed PVC or Docker named
volume with two mounts in the same sandbox:

```text
one managed volume
  -> /mnt/memory/<name>                         Agent-visible, RO or RW
  -> /var/lib/mango/memory-control/<identity>   Mango maintenance, always RW
```

The public mount uses the Session Resource's `read_only` or `read_write`
access. The private parent is root-owned mode `0700`, so UID 1000 cannot
traverse it. Mango maintenance uses the private mount to refresh a read-only
Store without weakening its public mount. OpenSandbox deletes the managed
volume with the sandbox.

PostgreSQL remains the canonical Memory history. Mango records a per-sandbox
baseline, writes surviving local changes back before refresh, merges current
heads, and commits changed/created/deleted files under the resource lock.
OpenSandbox supplies storage and mount isolation; it does not replace Mango's
version, precondition, or writeback logic.

## Network policy

`bridge` gives the sandbox ordinary OpenSandbox runtime networking. `none`
disables it. `limited` is admitted only because OpenSandbox can reconcile an
exact host allowlist through its egress component.

For limited networking, Mango starts deny-by-default, temporarily expands the
allowlist for configured package setup, restores the final Environment policy
before publishing the sandbox binding, and reconciles later MCP-derived host
changes. The Kubernetes/Kata candidate uses OpenSandbox `dns+nft` egress with
IPv6 disabled.

## Configuration

The worker requires the OpenSandbox endpoint. Credentials stay in worker
configuration and are never written to PostgreSQL or returned by the Mango API.

| Variable | Required | Purpose |
| --- | ---: | --- |
| `OPEN_SANDBOX_DOMAIN` | Yes | OpenSandbox server base URL |
| `OPEN_SANDBOX_API_KEY` | Deployment-dependent | Server authentication |
| `OPEN_SANDBOX_IMAGE` | No | Sandbox image; defaults to `python:3.12-slim` |
| `OPEN_SANDBOX_USE_SERVER_PROXY` | No | Route execd/file traffic through the server; required by the committed profiles |

There is no `MANGO_SANDBOX` selector. Setting up Docker or Kubernetes happens
in OpenSandbox configuration, not in Mango.

## Local development and CI

The local Compose bundle starts a pinned OpenSandbox server on
`127.0.0.1:8090`. Only that service mounts the host Docker socket. The Mango
worker reaches `http://opensandbox:8090` over the Compose network and uses the
server proxy.

Run the real backend contract with:

```sh
make test-sandbox-opensandbox
```

The target starts OpenSandbox and exercises agent built-ins, lifecycle,
reattachment, exact command output, Files, Git, Outputs, Skills, Memory access,
and cleanup. Packages are serialized against one local OpenSandbox instance to
avoid its Docker host-port allocator racing concurrent sandbox creation.
Offline `make test` does not require Docker or an OpenSandbox server.

## Kubernetes/Kata qualification

The production-candidate topology is:

```text
Mango worker -> private authenticated OpenSandbox server
             -> Kubernetes BatchSandbox
             -> reviewed Kata RuntimeClass
```

The non-installing assets in
`deployments/qualification/opensandbox-kata` pin the expected server/helper
versions and define an opt-in live gate:

```sh
make test-sandbox-opensandbox-kata-live
```

The gate checks the RuntimeClass, BatchSandbox and live Pod, explicit audit CPU
and memory bounds, Pod/container security fields, digest-pinned images,
ordinary lifecycle/resources, and allowlisted versus blocked egress. Mango
Environments do not yet expose resource sizing. Promotion still requires
repeatable clean-cluster, control-plane restart, node loss, network partition,
capacity, upgrade, rollback, volume cleanup, and isolation evidence. The
committed qualification profile is not an installer or a security
certification.
