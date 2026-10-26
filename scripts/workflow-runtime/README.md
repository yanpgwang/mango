# Workflow program feasibility study

This is independently authored research for the
[program recovery proposal](../../docs/design/workflow-program-runtime.md),
not a Mango executor, public API, sandbox service, release dependency or example
application. Neither candidate is approved for executing production programs.

The selected problem is narrower than the complete feasibility gate: establish
whether fresh, disposable JS evaluators can reproduce sequential and parallel
Agent orchestration from a PostgreSQL command **and delivery** trace. Temporal
does not supply this semantic replay automatically. A runnable failure fixture
is more useful evidence than assuming that cached Promise results are enough.

## Run

```sh
make test-workflow-runtime
```

Requires Go and Docker with Linux containers. The first image build downloads
checksum-pinned QuickJS 2026-06-04 and digest-pinned base images. Subsequent test
execution needs only these images and `postgres:17.5-alpine`; it requires no
model, CMA credentials, external network service or reference checkout. Default
Go/race tests run IPC validation and skip the explicit engine study. To focus
after building the images:

```sh
MANGO_TEST_WORKFLOW_RUNTIME=1 go test -count=1 -v ./scripts/workflow-runtime \
  -run 'TestEvaluatorStudy/quickjs/ordered_deliveries_and_callbacks'
```

The fixture starts its own loopback-only PostgreSQL container and creates
random Run identities. Every pump starts a new container. Tests remove their
owned containers and database; no existing local Mango stack is used. Research
images remain cached under `mango-workflow-study:{quickjs,node}`.

## Acceptance for this study

- Freeze source/JSON inputs in SQL; repeated identical calls have distinct
  invocation ordinals, while replay matches kind, ordinal and canonical args.
- Preserve callback and host-call order when B finishes before A and callbacks
  create dependent calls, even when both outcomes are cached after restart.
- Commit command/result-delivery/return before acknowledgement. Kill the real
  evaluator after every boundary of an independently specified trace; retain
  one invocation intent per logical call and one committed terminal value.
- Replay a rejected admission and catch/fallback after live policy changes;
  separate queued concurrency from a rejection; keep siblings running when
  `Promise.all` rejects; exercise result-driven loops.
- Use real SQL locking for epoch replacement, pause/stop fences and a receipt
  arriving during wait handoff. Paused work may settle; stopped work may not
  deliver a successful business result.
- Probe restricted capabilities, orphan Promises, outstanding work at return,
  malformed IPC, runaway loops/microtasks/allocations and bounded output.
- Record actual image/platform identities and bounded replay wall time. Keep
  all generated code inside disposable containers with no host mounts, network,
  capabilities, credentials or Docker socket.

The SQL adapter is a test fixture, not a candidate product schema. Its Agent
invocations are durable SQL intents with deterministic supplied results, **not
model calls or real Thread creation**. It has no Session authorization, real
outbox relay, Temporal coordinator, provider output-schema adapter or physical
tool cancellation. Those are distinct required product/gate tests. Docker
containment and a few realm probes do not prove resistance to arbitrary engine
or kernel vulnerabilities. Only the observed Linux platform is measured; no
cross-platform trace equality or production scale is claimed.

The wrapper accepts an async function body with JSON `args`, `agent(prompt,
{agent:"researcher"})`, native `await`, `Promise.all`, pure transforms and
result-driven control flow. It removes ambient time/randomness/direct I/O,
dynamic code generation and uncertified `race`/`any`. Values must be bounded
acyclic JSON. This is a research profile, not a published script contract.
No source, prompt, result or stack is printed by engine-failure diagnostics.

Both engines share the same independently written realm fixture. QuickJS uses
a minimal C embedder with no `std`/`os` registration and explicit job draining.
Node uses `node:vm` with `microtaskMode: afterEvaluate` and no host function in
the realm. `node:vm` is not treated as a security boundary; container limits and
the outer timeout own containment/cancellation for both candidates. Identical
fixtures test both, but are not a proof that their whole JS languages match.

The research limits are 128 MiB container memory, one CPU, a 3-second CPU limit,
32 processes, a 2 MiB hard OS stack, no core dumps and an 8-second host pump
deadline. QuickJS has a 48 MiB allocator budget latched in trusted C and a
500 ms/job-count quantum; Node has a 48 MiB old-space limit and 500 ms VM
quantum. Engine stack guards are configured above the OS boundary (disabled in
QuickJS, 8 MiB in V8) so actual stack exhaustion terminates the process. This
relationship must be retested on each engine/platform/profile, not assumed.
Ordinary deterministic `RangeError` remains catchable. Resource tests inspect
container exit status; a syntax/startup error alone is not accepted evidence.

The returned program Promise determines success or failure. Node's trusted
host suppresses process-wide unhandled-rejection termination: a direct or
derived Promise can reject before the program reaches its later `await`.
Tests exercise this in one live evaluator across an event-loop turn and again
after replacement. Catching a failed child or ignoring a rejected derived
Promise does not cancel siblings; every admitted host invocation must settle
before a successful final return. This host policy matches the QuickJS fixture
and is part of the pinned research profile, not an inference about CMA.

## Bridge to actual Run and Thread components

The separate `make test-workflow-quickjs-integration` research tier uses this
same QuickJS image, the actual Run pump/PG frontier/outboxes and real Thread
runner. Its JS source drives two model-fixture calls, replays committed receipt
delivery order after loss and persists a program-return candidate. It does not
use this directory's toy SQL journal or execute the external cookbook/SDK.
See the [proof and limits](../../docs/design/workflow-runtime-study.md#real-quickjs-runthread-proof).
Default tests and application startup do not enable the adapter; no production
engine choice or complete Run lifecycle is implied.
