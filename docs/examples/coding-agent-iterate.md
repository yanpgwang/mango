---
title: Repair failing tests with the Python SDK
slug: /examples/coding-agent-iterate
---

# Repair failing tests with the Python SDK

Upload a broken calculator and its tests, let an Agent repair a writable copy,
then download the result and run the original tests independently. An Agent
saying "done" is not the acceptance check.

The [complete program](https://github.com/yanpgwang/mango/tree/main/examples/coding-agent)
uses Mango's async Python SDK for every Mango operation. The code blocks below
are included directly from that executable source; they share variables and
run inside async functions, rather than being separate copy-and-paste scripts.

## Run the example

From a repository checkout, install the Python SDK and prepare the local
verification image. Python 3.11+, `uv`, and a running Docker daemon are required:

```sh
uv sync --project sdk/python --frozen
docker pull python:3.12-alpine
```

### Reproducible local run

Use the repository's Go toolchain and backing services to run the exact Python
program through an isolated Mango API and worker:

```sh
docker compose -f deployments/local/compose.yaml up -d --wait postgres temporal nats minio
make test-coding-agent-example
```

This uses real authenticated HTTP, PostgreSQL, Temporal, NATS, MinIO, and Docker.
Only inference is deterministic; no model credentials are needed. The harness
creates isolated test state and cleans up its resources and downloaded files.
It also disconnects the event stream and resumes a kept Session from a new
client process, checking that neither recovery path sends the task again.

To run the same program with your configured model, first configure the private
development environment as described in [Getting started](../getting-started.md),
then explicitly opt in to potentially billable requests:

```sh
scripts/with-dev-env make test-coding-agent-example-live
```

The live harness starts its own API and worker; it does not change an already
running stack. The model credential stays server-side. The Python program
receives only a Mango API key and model ID.

### Connect to your Mango deployment

The deployment must have [Files enabled](../deployment.md), a Python-capable
sandbox supporting File Resources and Session Outputs, and a tool-capable
model. For the Docker provider, select `MANGO_SANDBOX=docker` on **both** the API
and worker, and `MANGO_SANDBOX_IMAGE=python:3.12-alpine` on the worker. They must
share PostgreSQL, Temporal, NATS, and object-store configuration. The worker's
Docker daemon must be able to bind its resource staging directory; see
[Deployment model](../deployment.md) and [Sandbox backends](../sandboxes.md).

The default `make local-up` stack uses the local-process sandbox, which rejects
File Resources and Session Outputs. Its `offline-fake` model is a text demo,
not a coding model. Starting that stack alone is not sufficient for this example.

With a capable deployment running, provide its Workspace key through your
environment, then run:

```sh
# Set MANGO_API_KEY securely in your shell; do not use the model provider key.
MANGO_BASE_URL=http://localhost:8080 \
  sdk/python/.venv/bin/python examples/coding-agent/main.py \
  --model your-model-id --output-dir /tmp/mango-coding-result
```

Alternatively, `make demo-coding-agent` uses `MANGO_BASE_URL`, `MANGO_API_KEY`,
and `MANGO_EXAMPLE_MODEL_ID`. It does not start or reconfigure a server.
The default job deadline is 300 seconds; override it with `--timeout`.
Cleanup requests have separate bounded timeouts.

Tool sequences vary with the model. A successful run includes a failed check,
subsequent tool work, a downloaded artifact, and this final acceptance result:

```text
Session: session_...
[tool] bash
[result] failed — agent can revise
...
Downloaded: /tmp/mango-coding-result/repaired_calc.py
Independent verification passed. Coding-agent example completed.
```

## 1. Connect and configure the Agent

The SDK handles authentication, typed requests, pagination, and streaming
transport. Mango's worker owns model calls and the durable Agent loop:

::include[../../examples/coding-agent/main.py#client]{lang="python"}

Enable only the coding tools this task needs. Web Search/Fetch and other tools
remain disabled:

::include[../../examples/coding-agent/main.py#agent]{lang="python"}

This is a trusted development task. Disabling web tools is not a network
sandbox: `bash` still has the selected provider's network access. The prompt's
"no network" instruction is not an enforced egress policy.

## 2. Upload source and original checks

The shared fixtures contain an off-by-one addition bug and a missing
divide-by-zero guard. Checks use Python's standard-library `unittest`; no
package installation is needed inside the sandbox.

::include[../../examples/coding-agent/main.py#upload]{lang="python"}

An upload is a reusable File object. Session creation takes independent copies
at the requested mount paths; with Docker those input mounts are read-only.
The Agent copies them to `/workspace/iterate` before editing.

## 3. Create the Environment and Session

`cloud` means Mango's worker manages execution through the operator-selected
sandbox. It does not delegate execution to a hosted agent service:

::include[../../examples/coding-agent/main.py#environment]{lang="python"}

Pin the Agent Version and attach the inputs when creating a dedicated Session:

::include[../../examples/coding-agent/main.py#session]{lang="python"}

## 4. Follow failure and repair

Open the event stream before sending the task so a fast turn cannot finish
before the client subscribes:

::include[../../examples/coding-agent/main.py#stream]{lang="python"}

The observer prints tool calls/results and requires an observed failing check
before accepting `session.status_idle` with `end_turn`. A custom-tool or
approval barrier, error, or termination is not completion. It then verifies
the actual artifact, not the Agent's final prose.

After a lost stream, the program opens a new stream, reads paginated persisted
history, and deduplicates history/live events by ID. Recovery only reads: it
never resends the task. A failed task-submission request is reported instead
of blindly retried because its acceptance may be uncertain.

## 5. Download and independently verify

Before idle, Mango publishes regular files written under
`/mnt/session/outputs`. Select the Session-scoped `repaired_calc.py` output:

::include[../../examples/coding-agent/main.py#download]{lang="python"}

The output has a different name from the input: Session-scoped Files also
contain downloadable input copies. The client requires one matching artifact,
bounds the download to 64 KiB, checks the byte count, and never overwrites an
existing local file.

The verifier combines the downloaded source with the **original local tests**
in a disposable Docker container. Generated code is never imported on the
host, and neither credentials nor the repository are mounted into the container:

::include[../../examples/coding-agent/verify.py#verify]{lang="python"}

This independent check has no network, read-only inputs/root filesystem, an
unprivileged user, resource limits, and a timeout. Docker remains a development
isolation boundary, not a hostile multi-tenant security guarantee. The
verification image must already exist locally; use `--verifier-image` to select
another suitable Python image.

## Cleanup and read-only resume

By default the program deletes its Session (releasing its sandbox and scoped
Files), deletes its uploads and Environment, and archives its Agent, including
after a failure. The downloaded file remains on disk; a failed check means it
must not be treated as a verified result. Agent archival retains history.
Cleanup failures print the affected IDs and exit unsuccessfully.

Pass `--keep-resources` to retain the resources and print their IDs for inspection.
Resume observation/download with a fresh output directory:

```sh
sdk/python/.venv/bin/python examples/coding-agent/main.py \
  --session-id session_... --output-dir /tmp/mango-coding-resumed
```

Resume requires the dedicated example Session's metadata. It does not create a
new turn, repair a failed turn, or delete existing resources. Kept resources
must be cleaned up explicitly. If a create request loses its response, the
client cannot know that resource's ID; inspect Mango before creating another
job rather than assuming the write failed.

## Verification tiers and design boundary

`make test-coding-agent-example-unit` checks event interpretation, bounded
recovery, artifact rejection, cleanup, Python typing, and lint without services.
`make test-coding-agent-example` adds the real-service SDK journey and runs in
CI; `make test-coding-agent-example-live` adds explicitly configured real-model
inference. The existing `make test-coding-agent` internal durable scenario
continues to share the same fixtures.

The broken-calculator scenario is adapted from Anthropic's public CMA iterate
cookbook. Mango adopts the user problem and lifecycle lessons, not a hosted
runtime dependency or field-level compatibility promise. See
[Design provenance](../provenance.md#coding-agent-scenario-fixtures) for what
was adopted, changed, and rejected. This client is a single-job tutorial, not
a general-purpose worker or a new SDK runner abstraction.
