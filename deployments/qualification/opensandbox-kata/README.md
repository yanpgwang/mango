# OpenSandbox on Kubernetes/Kata qualification profile

Status: **Qualification only**. This directory is not a supported production
bundle and does not install OpenSandbox, Kubernetes, Kata Containers, or Mango.
It records the exact deployment contract Mango intends to qualify before
publishing production manifests.

The selected execution path is:

```text
Mango/Temporal worker
  -> internal sandbox.Provider boundary
  -> OpenSandbox server and Go SDK
  -> Kubernetes BatchSandbox
  -> operator-selected Kata RuntimeClass
```

Docker remains the deterministic local-development and CI backend. An
OpenSandbox server backed by ordinary Docker can exercise its API, but it does
not satisfy this profile's hostile-workload isolation goal.

## Trust and lifecycle contract

- Mango remains authoritative for Sessions, events, durable workflow state,
  provider bindings, Files, and teardown intent. OpenSandbox is an execution
  data plane, not a second agent control plane.
- The OpenSandbox API must be private to trusted Mango workers, authenticated
  with a non-demo API key, and reached with
  `OPEN_SANDBOX_USE_SERVER_PROXY=true`. Do not expose the server or sandbox
  gateway directly to end users.
- PostgreSQL persists only OpenSandbox's opaque sandbox ID and provider name.
  Credentials remain in worker configuration. A worker restart attaches to the
  same ID; a missing sandbox is an explicit failure, never an empty replacement.
- Every sandbox workload must use one reviewed Kata `RuntimeClass`. The
  OpenSandbox server fails startup when the configured RuntimeClass is absent;
  Mango's live qualification independently checks the RuntimeClass, the created
  `BatchSandbox` resource, and its live Pod.
- `network = "none"` and `network = "limited"` map to an OpenSandbox
  deny-by-default policy. The profile uses the `dns+nft` egress implementation
  and disables IPv6 because the current policy path does not provide equivalent
  IPv6 enforcement.
- The audit sandbox requests one CPU and `512Mi` of memory and requires those
  exact Kubernetes requests and limits on the live sandbox container. Mango
  Environments do not yet expose resource sizing, so operator-selectable
  per-Environment limits remain a promotion blocker. Cluster quotas,
  pod-security admission, node placement, image policy, log retention, and
  capacity isolation remain operator responsibilities.
- Session deletion is the ordinary cleanup path. OpenSandbox resources use
  manual cleanup so their lifetime is not silently shortened behind Mango's
  durable deletion workflow.

## Render the OpenSandbox configuration

Start from [`server.toml.example`](server.toml.example) and
[`batchsandbox-template.yaml.example`](batchsandbox-template.yaml.example).
Replace every `REPLACE_WITH_...` value before deployment. Render the API key
through the cluster's secret-management path and mount the rendered server
configuration from a Kubernetes Secret rather than a ConfigMap; never commit
the rendered file.

OpenSandbox server `v0.2.2` synthesizes its main and egress containers after
loading the BatchSandbox template; the template can set Pod-level fields but
cannot enforce every generated container security field. The target namespace
therefore needs a reviewed admission policy that makes
`allowPrivilegeEscalation: false` explicit on every regular container. The live
audit reads the resulting Pod and rejects missing/true privilege escalation,
privileged regular containers, unsafe host namespaces or seccomp, unexpected
added capabilities, unpinned helper images, and missing CPU/memory bounds. The
trusted `execd-installer` init container may be privileged inside the Kata VM
when OpenSandbox disables IPv6 for egress enforcement; no other privileged init
container is accepted.

Pin one tested bill of materials. Floating tags such as `latest` and a checkout
of OpenSandbox `main` are not qualification evidence. Record at least:

- Kubernetes distribution and version;
- Kata Containers version, hypervisor, and RuntimeClass name;
- OpenSandbox server, BatchSandbox controller/CRD, Go SDK, `execd`, and egress
  image versions or digests;
- sandbox base-image digest;
- upgrade source, upgrade target, and rollback result.

The example deliberately uses placeholders because the upstream server,
controller, helper images, and chart do not currently share a Mango-owned
release lifecycle. A future supported bundle must pin and test the complete
matrix instead of inheriting whichever versions happen to be current.

## Configure the Mango worker

The worker needs a private OpenSandbox endpoint and the matching API key:

```sh
OPEN_SANDBOX_DOMAIN=http://opensandbox-server.opensandbox-system.svc.cluster.local
OPEN_SANDBOX_API_KEY=REPLACE_WITH_RANDOM_API_KEY
OPEN_SANDBOX_IMAGE=registry.example.test/agent-python@sha256:REPLACE_WITH_64_HEX_DIGEST
OPEN_SANDBOX_USE_SERVER_PROXY=true
```

OpenSandbox is the only Mango sandbox backend, so there is no provider selector.
Only the worker needs OpenSandbox credentials. Do not place model,
object-store, Vault, or Mango API credentials inside a sandbox image or pod.

## Run qualification

Provide two small HTTP endpoints on different hostnames. Both must normally be
reachable from a sandbox, must not redirect to another hostname, and must be
safe to call repeatedly. The test first proves both are reachable without a
policy, then allows only the first hostname and requires the second to fail.

```sh
scripts/with-dev-env env \
  OPEN_SANDBOX_IMAGE=registry.example.test/agent-python@sha256:REPLACE_WITH_64_HEX_DIGEST \
  OPEN_SANDBOX_USE_SERVER_PROXY=true \
  OPEN_SANDBOX_KATA_NAMESPACE=opensandbox \
  OPEN_SANDBOX_KATA_RUNTIME_CLASS=kata-qemu-runtime-rs \
  OPEN_SANDBOX_KATA_KUBECTL_CONTEXT=my-cluster \
  OPEN_SANDBOX_KATA_ALLOWED_URL=https://allowed.example.test/health \
  OPEN_SANDBOX_KATA_BLOCKED_URL=https://blocked.example.test/health \
  make test-sandbox-opensandbox-kata-live
```

The gate runs the shared lifecycle, File Resource, Session Output, and Skill
contracts, then verifies:

1. both endpoints are reachable from an unrestricted sandbox;
2. the configured Kubernetes RuntimeClass exists;
3. the created BatchSandbox and its live Pod name that RuntimeClass;
4. the actual Pod satisfies the container, namespace, seccomp, image-pinning,
   service-account-token, and fixed audit resource constraints above;
5. the allowed endpoint remains reachable under a deny-by-default policy;
6. the otherwise-reachable blocked endpoint is denied.

`kubectl` must be installed and authorized to read RuntimeClasses,
BatchSandboxes, and Pods. The target namespace must use the BatchSandbox
workload provider. Ordinary tests and public CI never run this credentialed
gate.

## Promotion blockers

Passing the gate is necessary but does not make the backend production-ready.
Promotion still requires repeated clean-cluster runs, worker and OpenSandbox
restart tests, sandbox-node loss and network-partition tests, capacity and
quota tests, metrics/alerts, image and dependency vulnerability policy,
documented upgrades and rollback, and an isolation review. Mango's managed
volume mapping for Memory Stores is covered on the Docker runtime, but it must
also pass the Kubernetes/Kata qualification gate before this profile can claim
that capability in production. Per-Environment CPU and memory configuration is
also not yet part of Mango's public Environment contract.
