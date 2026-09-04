# Docker self-hosted worker

This is Mango's first reference launcher for a `self_hosted` Environment. The
trusted host process polls and acknowledges Environment Work with
`MANGO_API_KEY`; each acknowledged item runs in a separate Docker container
with only its short-lived `MANGO_WORK_SECRET`.

Build the sandbox image from the repository root:

```sh
docker build \
  -f deployments/self-hosted/docker/Dockerfile \
  -t mango-self-hosted-worker:local \
  .
```

Run the host supervisor:

```sh
export MANGO_BASE_URL=http://localhost:8080
export MANGO_DOCKER_BASE_URL=http://host.docker.internal:8080
export MANGO_API_KEY=replace-with-a-workspace-key
export MANGO_ENVIRONMENT_ID=env_replace_me

go run ./cmd/mango-worker docker
```

`MANGO_DOCKER_BASE_URL` must be reachable from the sandbox container. The
launcher adds Docker's `host-gateway` mapping for
`host.docker.internal`; when Mango and the worker share a user-defined Docker
network, use Mango's network DNS name instead and pass `--network`.

The launcher follows the same lifecycle split as the public CMA self-hosted
examples:

1. the host owns Poll and Ack;
2. a foreground, per-Work container owns first heartbeat, continuous lease
   renewal, Session event recovery, tool execution, result submission, and
   forced Stop;
3. containers are removed on exit, while one named `/workspace` volume remains
   per Session so later activations see the same working tree.

Mango makes CMA's narrower per-Session credential path mandatory rather than
retaining the SDK's Environment-key fallback or the Docker cookbook script's
broader handoff. The Workspace key is never placed in the container. The Work
secret is passed only to that item process, decoded there into the per-Session
token, and scrubbed from `bash` subprocesses. It is never written to a Docker
label or volume. Launcher errors report an exit code and container identity
without automatically embedding untrusted container logs.

The reference image runs as uid/gid `65532`, drops all Linux capabilities, sets
`no-new-privileges`, uses a read-only root filesystem, gives `/tmp` a bounded
tmpfs, and defaults to 1 CPU, 1 GiB memory, and 256 processes. Docker bridge
egress is still unrestricted. Configure firewall or Docker network policy for
the destinations your tools require; container isolation alone is not a
hostile multi-tenant guarantee.

The initial reference executes `bash`, `read`, `write`, `edit`, `glob`, and
`grep` inside `/workspace`. Self-hosted Skill and Memory preparation and
automatic Session output publication remain explicit follow-up work; Session
admission continues to reject those unsupported combinations until the worker
can honor them.
