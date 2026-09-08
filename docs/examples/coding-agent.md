---
title: Coding agent
description: Repair failing tests and retrieve verified code through Mango.
---

# Coding agent

Give an agent a tiny Python invoice module with two bugs. Watch it run tests,
repair the implementation, and continue the same Session after its Docker worker
is replaced. The application then checks the result independently and downloads
the repaired source through Mango Files.

Source: [examples/coding-agent](https://github.com/yanpgwang/mango/tree/main/examples/coding-agent).
The application uses Mango's Go SDK and `mango-worker docker`; all Python
execution happens in Docker. The example enables the six local tools and leaves
Web tools disabled because the task uses only the supplied files. It needs no Python packages, GitHub account, or
hosted agent service. The fixture uses integer cents and Python `unittest`.

## Run

Start a Mango deployment with a [real model](../guides/model-configuration.md)
and [Files storage](../api/files.md). The [local stack](../getting-started.md)
provides PostgreSQL, Temporal, NATS and MinIO. Run these commands from the repo
root as your normal non-root user, with Go and a local Docker daemon available:

```bash
docker build -f deployments/self-hosted/docker/Dockerfile \
  -t mango-self-hosted-worker:local .

export MANGO_API_KEY=sk-mango-local-development
export MANGO_EXAMPLE_MODEL_ID=your-configured-model-id
export MANGO_EXAMPLE_BASE_URL=http://localhost:8080
export MANGO_DOCKER_BASE_URL=http://host.docker.internal:8080
make demo-coding-agent
```

The example starts and stops its own Docker supervisor; do not start another
supervisor for its Environment. The API URL must be reachable from the host,
and the Docker URL must be reachable from the worker containers. A host-native
API must listen on an interface reachable from Docker. The model credential
belongs to the running orchestration process, not to this client or its containers.

With the maintainer development environment configured, you can also use
`scripts/with-dev-env make demo-coding-agent`. The Make target takes its model ID
from `MANGO_EXAMPLE_MODEL_ID` or `MANGO_MODEL_ID`, then removes the model connection
variables before starting the application.

Optional settings:

| Variable | Purpose |
| --- | --- |
| `MANGO_WORKER_IMAGE` | Select an already-built worker image with Python 3. |
| `MANGO_EXAMPLE_OUTPUT_DIR` | Existing parent directory for a fresh result folder; defaults to the system temporary directory. Docker must be able to share this path. |

## What happens

1. Create an Agent, a self-hosted Environment, and a Session.
2. Upload two input files through Files and download them into a fresh
   `<workspace-root>/<session-id>` directory owned by the application.
3. Run the original tests in a fresh container: **4 tests, 3 failures**.
4. Start the Docker supervisor with `--workspace-root`, open the live event
   stream, and ask the agent to run tests and repair `invoice.py`.
5. Stop the supervisor and its container; start a replacement and ask the same
   Session to rerun tests and explain its existing code.
6. Stop the worker and run pristine tests against only the repaired source in
   a fresh read-only container: **4 tests pass**. Reject changed test files.
7. Upload the repaired source to Files, download it, and compare the bytes.

Tool calls and assistant replies print as the turns progress. If the stream
disconnects, the application polls paginated durable history, anchored to the
admitted input event, without resending the message. The observer waits through `requires_action` barriers for allowed worker tools;
other action requests fail the command. Each completed turn must include a
successful Bash result, and independent verification must report all four tests
passing without skips.

## Inspect and clean up

The printed result directory contains:

- `invoice-fixed.py`: downloaded repaired source.
- `before/`, `after/` and `before-tests.txt`, `after-tests.txt`: independently
  executed original/final source and pristine tests with captured results.
- `<session-id>/`: the worker's retained workspace.
- `events.json`, `resources.json`, `worker.log`: history, resource IDs and
  supervisor lifecycle evidence. No credentials are written to these files.

The example archives its Session, Agent and Environment on success, deletes its
three tutorial Files, and stops its supervisor. On failure it saves available
history, deletes the unfinished Session, archives the Agent/Environment, and
removes uploaded Files. Cleanup errors fail the command; inspect the recorded IDs
if an unavailable deployment prevents cleanup. Local artifacts remain for you to
inspect and can be removed afterward. A forced process kill can require manual
container/resource cleanup.

The original CMA iterate notebook uses hosted File mounts and output directories.
Mango provides the same debugging workflow through operator-owned staging and
explicit artifact transfer. This example demonstrates that workflow, without
claiming automatic mounts or complete CMA feature parity. See
[design provenance](../provenance.md#coding-agent-workflow-2026-09-08).
