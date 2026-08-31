# Coding-agent repair with the Python SDK

Upload the broken calculator and tests in `fixtures/`, observe a failed check
and repair, download the published source, and verify it against the original
tests in a separate restricted Docker container. All Mango calls use the
public async Python SDK; the Agent loop stays on the server.

From the repository root, with the repository's Go toolchain, Python 3.11+,
`uv`, and Docker installed:

```sh
uv sync --project sdk/python --frozen
docker pull python:3.12-alpine
docker compose -f deployments/local/compose.yaml up -d --wait postgres temporal nats minio
make test-coding-agent-example
```

The harness starts an isolated API/worker and executes `main.py` unchanged
against real services. Only inference is deterministic. It also checks stream
recovery, read-only client resume, and cleanup. To use a configured, potentially
billable model instead:

```sh
scripts/with-dev-env make test-coding-agent-example-live
```

For an existing Files-enabled deployment with a Python-capable sandbox, set
`MANGO_API_KEY` securely in your environment, then run:

```sh
MANGO_BASE_URL=http://localhost:8080 \
  sdk/python/.venv/bin/python examples/coding-agent/main.py \
  --model your-model-id --output-dir /tmp/mango-coding-result
```

The default local Compose API/worker use the local-process sandbox and cannot
run this File Resource workflow. See the
[complete tutorial](../../docs/examples/coding-agent-iterate.md) for deployment
requirements, executable SDK snippets, resource ownership, `--keep-resources`,
and read-only `--session-id` resume. Downloaded code is not executed on the
host; Docker verification is still not a hostile-code security guarantee.

Offline example checks: `make test-coding-agent-example-unit`.
