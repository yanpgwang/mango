# Mango TypeScript / JavaScript SDK

First-party client for Mango's current development API: all 98 OpenAPI operations
and 279 schema types. It uses native `fetch`, supports Node.js 22+, and has no
runtime dependencies. JavaScript uses the same package; TypeScript gets generated
request/response types and discriminated event unions.

The npm package is `mango-sdk`. This is an alpha SDK with no stable API contract.
Its contract is Mango's `/v1`; it does not use hosted agent services or provider SDKs. Deployment,
Memory, Files, Skills, Vaults, Webhooks, self-hosted Work, Threads, and all other
documented resources are included. Wrapping a route does not enable a server
capability that the operator has not configured.

## Install

For a published alpha version, install by its exact version:

```sh
npm install mango-sdk@0.1.0-alpha.1
```

The release uses the `alpha` dist-tag rather than `latest`. Registry packages
include compiled JavaScript and type declarations; consumers do not need to
compile the SDK. Use an SDK version built for your Mango server revision.

### Build and install from source

From this directory:

```sh
npm ci
npm run build
npm test
```

From your application's directory:

```sh
npm install /absolute/path/to/mango/sdk/typescript
```

Use ESM `import`. The package includes compiled JavaScript and declarations after
building. CommonJS `require` is not a supported entry point.

## Create an Agent and Session

```ts
import { Mango } from 'mango-sdk';

const client = new Mango({
  baseURL: process.env.MANGO_BASE_URL ?? 'http://localhost:8080',
  apiKey: process.env.MANGO_API_KEY!,
});

const environment = await client.createEnvironment({
  body: { name: 'Example', config: { type: 'cloud' } },
});
const agent = await client.createAgent({
  body: { name: 'Assistant', model: process.env.MANGO_MODEL_ID! },
});
const session = await client.createSession({
  body: { agent: agent.id, environment_id: environment.id },
});
await client.sendSessionEvents({
  session_id: session.id,
  body: { events: [{ type: 'user.message', content: [{ type: 'text', text: 'Hello!' }] }] },
});
```

Methods use OpenAPI `operationId` names, such as `getSession`, `createMemory`,
`heartbeatEnvironmentWork`, and `runDeployment`. The first argument combines
exact path/query parameter names and an optional `body`; the second contains
request options. Date/time fields remain ISO strings, opaque IDs remain strings,
and monetary decimal fields keep their wire representation.
`apiKey` can be omitted for public `health`, `readiness`, and `openAPI` calls;
protected routes then return the server's normal 401 response. No default key is
injected.

```ts
await client.updateAgent({ agent_id: agent.id, body: { system: null, tools: [] } });
// Omit a field (or use undefined in JavaScript) to leave it absent; null means
// explicit null, and [] remains an empty list. Server validation still applies.
```

## Pagination and errors

```ts
import { APIError } from 'mango-sdk';

for await (const item of client.listSessionsItems({
  'statuses[]': ['idle', 'running'],
  include_archived: false,
  limit: 50,
})) console.log(item.id);

// Every paginated list also exposes listXPages and the ordinary single-page listX.
for await (const page of client.listFilesPages({ limit: 20 })) console.log(page.data.length);

try {
  await client.getSession({ session_id: 'sesn_missing' });
} catch (error) {
  if (error instanceof APIError) console.error(error.status, error.type, error.requestId);
  else throw error;
}
```

Pagination keeps the original filters and follows `next_page` or Files'
`has_more`/`after_id` (or `before_id` for reverse traversal). It is lazy and stops
when you break the loop. Repeated cursors fail rather than loop indefinitely.
Array queries repeat exact bracket names; e.g. `types[]=...&types[]=...`.
Error response bodies are limited to 1 MiB and cancelled at that bound;
`APIError.bodyTruncated` indicates when its preserved body may be incomplete.

## Live events and cancellation

```ts
const abort = new AbortController();
// This Promise resolves after successful subscription headers, not after the
// first event. Input can now be sent without waiting for an event to arrive.
const events = await client.openSessionEvents(
  { session_id: session.id, 'event_deltas[]': ['agent.message'] },
  { signal: abort.signal },
);
try {
  await client.sendSessionEvents({
    session_id: session.id,
    body: { events: [{ type: 'user.message', content: [{ type: 'text', text: 'Hello!' }] }] },
  });
  for await (const frame of events) {
    if (frame.type === 'agent.message') console.log(frame.content);
    if (frame.type === 'session.status_idle') break;
  }
} finally {
  await events.close(); // Also releases an opened stream that was never iterated.
}
```

Streams are **live-only**. `openSessionEvents` and `openSessionThreadEvents` are
eager, returning a ready async-iterator handle with `close()`. The lower-level
`streamSessionEvents` and `streamSessionThreadEvents` async generators are lazy:
they connect on the first `next()`/`for await` iteration, **not** when constructed
or awaited. Use the eager helpers above to establish subscription before input.
The SDK does not reconnect, send `Last-Event-ID`, or promise replay.
Use `listSessionEvents` for durable history; applications that combine live and
history views must deduplicate persisted event IDs. Preview deltas are ephemeral.
`streamSessionEventsMessages` (and the thread equivalent) also exposes SSE
`event`, `id`, and `retry` metadata; these are not a replay guarantee.
JSON data within each SSE frame is limited to 64 MiB; larger frames fail with
`ProtocolError` and cancel the connection. This includes ordinary persisted
events, which can be much larger than text deltas.

Normal JSON requests have a 60-second deadline, including the body. Change it on
the client or per request with `timeoutMs`; zero disables it. Streaming/download
timeouts apply to connection setup only. The caller's `AbortSignal` remains
effective during body consumption. Breaking an SSE loop cancels its reader.

The client never retries automatically, including failed writes: a lost response
does not prove the server rejected a submission. Resolve ambiguous writes using
the application's durable history instead of blindly re-submitting. Redirects
are rejected; a bearer key is not forwarded to another destination.

## Files and Skills

```ts
const file = await client.uploadFile({
  body: { file: new File(['name,value\nexample,42'], 'data.csv', { type: 'text/csv' }) },
});
await client.createSkill({
  body: {
    display_title: 'Reviewer',
    files: [new File(['---\nname: reviewer\ndescription: Review inputs\n---\nCheck facts.'], 'reviewer/SKILL.md')],
  },
});
```

Uploads accept `Blob`, `File`, or `{ data: Blob, filename: string }`; Skill files
preserve relative filenames. Fetch creates the multipart boundary. Downloads
(`downloadFile`, `downloadSkillVersion`) return a native `Response` with streaming
`body` and content headers; consume or cancel the body to release the connection.
Only Files marked downloadable can be downloaded; the server enforces this.

```ts
const response = await client.downloadFile({ file_id: 'file_downloadable_output' });
const bytes = await response.arrayBuffer(); // Optional buffering; body is a stream.
```

Do not embed a Workspace API key in browser-delivered code. Put this SDK in a
trusted backend. Optional `fetch` injection supports deterministic tests; custom
implementations must honor the supplied signal and redirect policy.

## Maintaining the contract

```sh
# From repository root, after an intentional Mango OpenAPI change:
go run ./scripts/sdk-contract
cd sdk/typescript
npm run generate
npm test
```

`npm run generate:check` checks deterministic generated source without writing.
Tests cover all operations' dispatch, types/nullability, query encoding,
multipart/files, errors, cancellation/deadlines, SSE, and pagination. They use
local fakes/loopback HTTP and do not require credentials or a model. The separate
`examples/conformance.mjs` is run by the repository's cross-language test harness
against real Mango HTTP handlers with explicitly simulated backend dependencies;
it is not a live-model verification.
