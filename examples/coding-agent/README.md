# Coding-agent repair with the Python SDK

Upload the broken calculator and tests in `fixtures/`, observe a failed check
and repair, download the published source, and verify it against the original
tests in a separate restricted Docker container. All Mango calls use the
public async Python SDK; the Agent loop stays on the server.

From the repository root, with Python 3.11+, `uv`, and Docker installed:

```sh
uv sync --project sdk/python --frozen
docker pull python:3.12-alpine
```

For an existing Files-enabled deployment with a Python-capable sandbox and a
tool-capable model, set `MANGO_API_KEY` securely in your environment, then run:

```sh
MANGO_BASE_URL=http://localhost:8080 \
  sdk/python/.venv/bin/python examples/coding-agent/main.py \
  --model your-model-id --output-dir /tmp/mango-coding-result
```

Model credentials stay on the server; real-model requests may incur provider
charges. The example does not start the server or invoke Mango's test suite.
Its fixtures are self-contained, and its result verifier is part of the
application workflow, not a system-test harness.

The default local Compose API/worker use the local-process sandbox and cannot
run this File Resource workflow. See the
[complete tutorial](../../docs/examples/coding-agent-iterate.md) for deployment
requirements, executable SDK snippets, resource ownership, `--keep-resources`,
and read-only `--session-id` resume. Downloaded code is not executed on the
host; Docker verification is still not a hostile-code security guarantee.
