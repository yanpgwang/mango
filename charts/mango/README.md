# Mango Helm chart candidate

This chart candidate installs the API and Temporal orchestration roles against
operator-provided PostgreSQL, Temporal, NATS and S3. Sandbox execution stays on
user infrastructure. It does not install state services, mount a Docker socket,
create Session Pods or generate credentials.

Chart/appVersion: `0.1.0-alpha.2`. Public image/chart publication is pending;
isolated Kubernetes 1.37.0 install, replacement and same-release quiesced restore
are tested. Use an existing inspected candidate
image. The [Kubernetes guide](https://yanpgwang.github.io/mango/guides/kubernetes)
contains prerequisites, existing-Secret creation, installation, external-worker
connection and diagnosis. The repository copy is
[`docs/guides/kubernetes.md`](../../docs/guides/kubernetes.md).
The [matched candidate guide](https://yanpgwang.github.io/mango/guides/release-candidates)
explains chart packaging and actual release-artifact installation/recovery checks.

Required values:

| Values | Role |
| --- | --- |
| `database.existingSecret.name/key` | PostgreSQL URL for serving and orchestration |
| `database.migrationSecret.name/key` (optional) | Separate schema-owner URL for the initial hook |
| `auth.existingSecret.name/key` | API bootstrap key; API only |
| `temporal.address/namespace` | Registered namespace dedicated to this logical installation |
| `nats.existingSecret.name/key` | NATS URL, including credentials if needed |
| `files.endpoint/region/bucket/pathStyle` | Existing S3-compatible bucket |
| `files.existingSecret.name/accessKey/secretKey` | Selected S3 credential keys |
| `model.baseURL/id/auth` | Messages endpoint, model ID, `bearer` or `x-api-key` |
| `model.existingSecret.name/key` | Model credential; orchestration only |
| `vault.enabled/existingSecret.name/key` (optional) | Original JSON keyring for Vaults/Webhooks |

`image.repository/tag` select the versioned image; `image.digest` takes
precedence. Existing `image.pullSecrets` work for every role, including the
pre-install hook. `latest` is rejected. All roles use UID/GID/fsGroup 65532, a
read-only root, bounded writable `/tmp`, resource requests/limits, dropped
capabilities and no mounted service-account token.

Resource names follow the release name or `fullnameOverride`. A digit-leading
name receives a `mango-` prefix, and long names are bounded with a hash suffix;
no relaxed Kubernetes Service naming feature gate is required.

The migration hook initializes the fresh timestamp baseline `20261005000001`;
old development version `1` is rejected without data reset. Same-release
retries preserve data. `migration.enabled=false` is available for an initialized
same-release restore; cross-version upgrades/rollback are unsupported. Removing
the chart keeps operator state stores and sandbox working directories.

Verify locally with Helm 4.3.0:

```sh
make chart-check HELM=helm
```
