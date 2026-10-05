---
title: Kubernetes control plane
description: Configure the Helm candidate with your state services and connect your own sandbox worker.
---

# Kubernetes control plane

The Helm chart is a **candidate**. It has offline configuration and packaging
tests; isolated cluster, restart/restore acceptance and public artifact
publication are still pending. Use an inspected candidate image from your own
registry for now. The default `ghcr.io/yanpgwang/mango:0.1.0-alpha.2` image and
chart are not yet published. See [release candidates](release-candidates.md).

The chart runs `mango serve` and `mango orchestrate` as separate Deployments.
Your sandbox worker runs on your infrastructure and connects through
[Environment Work](../api/environment-work.md). The chart does not provision
sandboxes or state services.

## Prepare state and credentials

Use Helm 4.3.0 for the candidate checks. Before installation, provide:

- A fresh PostgreSQL database/schema, with application credentials and optional
  separate schema-owner credentials. The first alpha baseline is
  `20261005000001`; previous development version `1` is rejected, without reset.
- A reachable Temporal frontend and a namespace registered for this logical
  Mango installation. Independent Mango installations must use distinct
  Temporal namespaces; their workers use the same task queue names.
- A reachable NATS Core server, and an existing S3-compatible bucket with
  credentials for Mango object operations. Provision these separately.
- A configured Messages endpoint, model ID and credential. Model credentials
  are mounted only into orchestration. See [model configuration](model-configuration.md).
- An API bootstrap credential. Your application uses it to set up its Workspace.
- Optionally, the original [Vault keyring](../api/vaults.md#secret-boundary) for
  encrypted Vault credentials and Webhooks. Preserve every key needed by data.

The candidate expects dependencies reachable on the operator's trusted network.
Use TLS endpoints where the runtime supports them and system-trusted CAs;
Temporal client TLS/custom CA mounts are not provided by this chart. API Ingress
and TLS termination are operator-managed. This is not a production/HA guarantee.

Create Secrets in the chart's namespace before installing. Prepare scalar
credentials as UTF-8 files **without a trailing newline**, outside Git, and keep
the keyring as its original JSON file. These commands select files without
putting credential values in Helm values:

```sh
kubectl create namespace mango
kubectl -n mango create secret generic mango-database \
  --from-file=database-url=/secure/mango/database-url
kubectl -n mango create secret generic mango-api-auth \
  --from-file=api-key=/secure/mango/api-key
kubectl -n mango create secret generic mango-nats \
  --from-file=nats-url=/secure/mango/nats-url
kubectl -n mango create secret generic mango-s3 \
  --from-file=access-key=/secure/mango/s3-access-key \
  --from-file=secret-key=/secure/mango/s3-secret-key
kubectl -n mango create secret generic mango-model \
  --from-file=api-key=/secure/mango/model-api-key
# Optional: enables Vaults/Webhooks when selected below.
kubectl -n mango create secret generic mango-keyring \
  --from-file=keyring.json=/secure/mango/keyring.json
```

Do not use the local Compose development keys. Secret management and grants are
operator responsibilities; the chart never generates credentials. A distinct
migration database role must be able to initialize the schema and grant the
runtime role access to application tables/sequences and the migration ledger.

## Configure and install

Save nonsecret configuration in `/secure/mango/values.yaml`. Replace the image
with your existing inspected candidate; use a digest when available:

```yaml
image:
  repository: registry.example.com/team/mango
  tag: 0.1.0-alpha.2
  # digest: sha256:<the actual multi-platform image digest>
database:
  existingSecret: {name: mango-database, key: database-url}
auth:
  existingSecret: {name: mango-api-auth, key: api-key}
temporal:
  address: temporal.operator.svc:7233
  namespace: mango
nats:
  existingSecret: {name: mango-nats, key: nats-url}
files:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: mango-files
  pathStyle: true
  existingSecret: {name: mango-s3, accessKey: access-key, secretKey: secret-key}
model:
  baseURL: https://your-messages-endpoint.example.com
  id: your-model-id
  auth: bearer
  existingSecret: {name: mango-model, key: api-key}
vault:
  enabled: true # omit this section if no keyring was provided
  existingSecret: {name: mango-keyring, key: keyring.json}
```

For a separate migration credential, add
`database.migrationSecret: {name: mango-schema-owner, key: database-url}`.
For a private image registry, select existing `image.pullSecrets`, each as
`{name: your-pull-secret}`. [Chart values](https://github.com/yanpgwang/mango/blob/main/charts/mango/values.yaml)
also define per-role resource and temporary-storage limits.

From the repository root:

```sh
helm lint --strict charts/mango -f /secure/mango/values.yaml
helm install mango ./charts/mango -n mango \
  -f /secure/mango/values.yaml --wait --timeout 10m
kubectl -n mango get deployments,pods,services
kubectl -n mango port-forward service/mango-api 8080:8080
```

In another terminal, `curl -fsS http://127.0.0.1:8080/readyz` should succeed.
Protected `/v1` requests need your API credential. `/healthz` proves process
liveness; `/readyz` checks PostgreSQL writability. Neither certifies model,
S3, Temporal consumption or sandbox execution. A running orchestration Pod is
not a provider readiness signal; verify a real Agent turn after connecting
your worker.

## Connect your sandbox worker

Expose the API at an operator-managed URL reachable from both the supervisor
and its containers. A workstation's localhost port-forward is an inspection
path; it is not automatically reachable from a remote sandbox host.

Create an Environment with your Workspace credential as described in the
[Docker worker guide](self-hosted-worker.md#create-an-environment). Issue its
supervisor credential from the API Pod, using the owning Workspace:

```sh
kubectl -n mango exec deployment/mango-api -- mango api-key create \
  -workspace wrkspc_default -environment "$MANGO_ENVIRONMENT_ID" \
  -label docker-supervisor
```

Creation prints the key once. Store it on the trusted supervisor host as
`MANGO_ENVIRONMENT_KEY`. It is scoped to this Environment's queue; do not give
the supervisor your model or database credentials. Use the matching inspected
worker command and item image:

```sh
export MANGO_BASE_URL=https://your-mango.example.com
export MANGO_DOCKER_BASE_URL=https://your-mango.example.com
export MANGO_ENVIRONMENT_ID=env_replace_with_your_id
# Set MANGO_ENVIRONMENT_KEY through your secret manager.
mango-worker docker --image registry.example.com/team/mango-self-hosted-worker:0.1.0-alpha.2
```

The Docker host, workspace storage and execution policy belong to the operator.
The supervisor has Docker authority and stays outside Session containers.
Create a tool-capable Session using this Environment and send one Bash request
to verify the entire path. SDK installation remains in the [SDK guide](../sdk.md).

## Diagnose and preserve state

The pre-install Job runs only `mango migrate`, with database credentials and
existing image-pull Secrets. It retries within five minutes, deletes successful
hooks and retains failed hooks for up to an hour. Inspect a failed installation:

```sh
kubectl -n mango describe job mango-migrate
kubectl -n mango logs job/mango-migrate
kubectl -n mango describe pods
kubectl -n mango logs deployment/mango-api
kubectl -n mango logs deployment/mango-orchestrator
```

An unsupported schema version requires a fresh database or the matching older
binary; editing Goose ledger rows is not recovery. Missing Secrets show as
container configuration errors. A bucket unavailable at startup can disable
Files/Skills; fix it and restart the affected role. Model/Vault configuration
errors fail orchestration/startup explicitly. Original keyring changes also
require restarting both roles; do not discard keys referenced by saved data.

Removing the chart does not remove external PostgreSQL, Temporal history, S3
bytes or user sandbox directories. Keep all of those plus original encryption
keys for recovery. An initialized same-release restore may use
`migration.enabled=false`; both process roles still check the schema. Actual
cluster restart and consistent, quiesced restore acceptance remain pending in
the [alpha delivery](../design/kubernetes-alpha.md). Cross-version upgrades,
rollback and live multi-store snapshots are not supported.
