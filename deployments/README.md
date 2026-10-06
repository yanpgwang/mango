# Deployment assets

Deployment assets are organized by purpose. Support status is documented here;
it does not determine directory names.

| Directory | Support level | Purpose |
| --- | --- | --- |
| [`local`](local/) | Development | Source-built API and orchestrator with PostgreSQL, Temporal, NATS and SeaweedFS for local development and service tests |
| [`workers/docker`](workers/docker/) | Preview | Docker sandbox item image and instructions for the operator-run Environment Work supervisor |
| [`../charts/mango`](../charts/mango/) | Candidate | Kubernetes control plane with operator-provided state and Secrets; installation, restart and same-release quiesced restore tested, publication pending |
| [`../scripts/kubernetes`](../scripts/kubernetes/) | Test fixture | Isolated kind lifecycle and artifact acceptance tests; not a user installation bundle |

## Choose an installation

For development, use `local` to build the current checkout and run its control
plane and state services. To execute shell/file tools, separately start an
Environment worker using `workers/docker`. The Compose service named `worker`
runs `mango orchestrate`; it is not the sandbox worker.

For Kubernetes, use `charts/mango` and the
[Kubernetes guide](../docs/guides/kubernetes.md). The chart runs only the API,
orchestrator and one-shot migration. State services are managed separately by
the operator; they may run in the same cluster. The current Docker sandbox
supervisor runs separately on an operator-owned Docker host and connects to the
Mango API.

A single-host installation bundle consuming versioned release images is
planned. The development Compose configuration contains development keys and
builds source; it is not that release installation bundle. Add another bundle
only when its installation, state retention, restart and recovery lifecycle is
tested and documented.

## Component boundaries

The release topology uses one immutable Mango image with separate process
roles. The API (`mango serve`) and orchestrator (`mango orchestrate`) are separate
processes; `mango migrate` is the explicit one-shot schema role. Both long-lived
processes check the migration ledger without applying migrations. The
operator-owned sandbox supervisor (`mango-worker docker`) launches item
containers (`mango-worker run`) and owns their workspaces and Docker lifecycle.
Neither control-plane process needs a Docker socket.

The chart and runtime artifacts are built from the same recorded source and
release version. Installation manifests reference operator-provided PostgreSQL,
Temporal, NATS and object storage rather than managing those services implicitly.
Development defaults and test fixtures do not establish production or HA
guarantees. Their support boundaries are described in the
[deployment model](../docs/deployment.md).

Use the repository-level `Makefile` for stable commands:

```sh
make image
make image-smoke
make local-config
make local-up
make local-health
make local-down
```

See [Deployment model](../docs/deployment.md) for the current guarantees and
the promotion criteria for Docker and Kubernetes assets.
