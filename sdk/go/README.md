# Mango Go SDK

A standalone, standard-library-only client for all **98 operations** in Mango's
current `/v1` OpenAPI contract, including health/readiness and the OpenAPI route.
It does not depend on Mango server packages, Temporal, or a hosted agent service.
Go **1.24+** is required for the JSON omission semantics used by typed inputs.

The SDK is pre-release and has not been published to a package registry or tagged
for independent releases. To use this checkout from another Go module:

```sh
go mod edit -require=github.com/yanpgwang/mango/sdk/go@v0.0.0
go mod edit -replace=github.com/yanpgwang/mango/sdk/go=/absolute/path/to/managed-agent-go/sdk/go
go mod tidy
```

## Quick start

```go
import (
    "context"
    "os"

    mango "github.com/yanpgwang/mango/sdk/go"
)

client, err := mango.New(mango.Config{
    BaseURL: "http://localhost:8080", // reverse-proxy path prefixes are supported
    APIKey:  os.Getenv("MANGO_API_KEY"),
})
if err != nil { panic(err) }
ctx := context.Background()

agent, err := client.CreateAgent(ctx, mango.AgentCreateRequest{
    Name: "assistant",
    Model: mango.ModelInput{String: mango.Ptr("your-configured-model")},
    System: mango.SomePtr("You are a helpful assistant."),
})
if err != nil { panic(err) }

environment, err := client.CreateEnvironment(ctx, mango.EnvironmentCreateRequest{
    Name: "default", // defaults to a cloud Environment
})
if err != nil { panic(err) }

session, err := client.CreateSession(ctx, mango.SessionCreateRequest{
    Agent: mango.SessionAgentInput{String: mango.Ptr(agent.ID)},
    EnvironmentID: environment.ID,
    Title: mango.Some("First session"),
})
if err != nil { panic(err) }

_, err = client.SendSessionEvents(ctx, session.ID, mango.SendSessionEventsRequest{
    Events: []mango.ClientSessionEventInput{{
        UserMessageEventInput: &mango.UserMessageEventInput{
            Type: "user.message",
            Content: []mango.MessageContentInput{{
                TextBlockInput: &mango.TextBlockInput{Type: "text", Text: "Hello!"},
            }},
        },
    }},
})
if err != nil { panic(err) }
```

Methods use the OpenAPI operation ID with an uppercase first letter, for example
`CreateAgent`, `PollEnvironmentWork`, `ListMemoryVersions`, `CreateWebhook`, and
`OpenAPI`. Path identifiers are positional strings; query filters are typed
`<Operation>Params`; JSON or multipart bodies use named request types. Check
[`operations_generated.go`](operations_generated.go) and
[`types_generated.go`](types_generated.go), or run `go doc . Client`.

## Inputs, unions, and errors

- Leave an `Optional[T]` field at its zero value to omit it.
- `Some(false)`, `Some(int64(0))`, and `Some("")` send those exact values.
- `Null[T]()` sends JSON `null`. The server rejects null on non-nullable fields.
- Nullable strings have type `Optional[*string]`: use `SomePtr("text")` or
  `Null[*string]()`. Required nullable response fields are pointers.
- Wire unions expose concrete variant pointers: set exactly one. On reads,
  recognized variants are decoded into their named type; unknown variants remain
  in `Raw` as `json.RawMessage`. There is no extra union envelope on the wire.
- Open-ended JSON Schema fields retain additional properties. Server constraints
  such as allowed model IDs, lengths, regexes, and cross-field invariants are not
  duplicated as client-side validation.

Use `errors.As(err, &apiError)` with `var apiError *mango.APIError` to inspect
`StatusCode`, `Type`, `Message`, and `RequestID`. `APIError.Body` may contain
sensitive data; avoid logging it indiscriminately. Non-JSON failures still return
a typed HTTP error.

## Pagination

```go
items := client.ListAgentsAutoPaging(ctx, mango.ListAgentsParams{
    Limit: mango.Some(int64(100)),
})
for items.Next() {
    agent := items.Value()
    _ = agent
}
if err := items.Err(); err != nil { panic(err) }
```

Every list endpoint exposing `next_page` has an `AutoPaging` helper. Files uses
its own `has_more` and `after_id`/`before_id` convention. Filters and direction are
preserved; repeated cursors fail instead of looping forever. Manual one-page
methods remain available.

## Live events and recovery

```go
stream, err := client.StreamSessionEvents(ctx, session.ID, mango.StreamSessionEventsParams{})
if err != nil { panic(err) }
defer stream.Close()
for stream.Next() {
    event := stream.Event()
    var frame mango.EventStreamFrame
    if err := event.Decode(&frame); err != nil { panic(err) }
    _ = frame
}
if err := stream.Err(); err != nil { panic(err) }
```

The iterator supports fragmented reads, LF/CRLF/CR lines, comments, multiline
data, an initial UTF-8 BOM, IDs, and retry metadata. Lines and accumulated event
data have a 64 MiB safety limit. It is **live-only**, does not reconnect, and does
not claim lossless delivery. The request context controls cancellation. To recover
durable events, open a stream, buffer incoming events, read paginated
`ListSessionEvents` or `ListSessionThreadEvents` history, and deduplicate persisted
events by their IDs before continuing. Best-effort preview deltas are not durable.

SSE and binary downloads are not cut off by `RequestTimeout` or the supplied
`http.Client.Timeout`. Finite requests default to 60 seconds; override
`RequestTimeout` or pass a context deadline as needed. No automatic retries are
added, especially for writes whose response may be lost after admission. Redirects
are never followed, so bearer credentials cannot be forwarded to redirect targets.

## Files and Skills

```go
source, err := os.Open("report.csv")
if err != nil { panic(err) }
defer source.Close()
file, err := client.UploadFile(ctx, mango.FileUploadRequest{
    File: mango.Upload{Filename: "report.csv", ContentType: "text/csv", Reader: source},
})
if err != nil { panic(err) }

download, err := client.DownloadFile(ctx, file.ID)
if err != nil { panic(err) }
defer download.Close()
// io.Copy(destination, download) streams without buffering the entire file.
```

`CreateSkill` and `CreateSkillVersion` take `[]Upload`; filenames such as
`references/data.csv` retain their relative Skill path in multipart headers.
Upload readers remain caller-owned. They should respond to cancellation when
they perform blocking work; closing the HTTP request cannot interrupt arbitrary
custom reader code.

## Development and verification

From the repository root:

```sh
python3 sdk/go/generate.py
python3 sdk/go/generate.py --check
cd sdk/go
go test -race ./...
go vet ./...
```

Generation uses only Python's standard library and `gofmt`; runtime uses only Go's
standard library. The shared `sdk/openapi.json` snapshot is exported from Mango's
actual OpenAPI document by `go run ./scripts/sdk-contract`. Changes to the API must
regenerate both the shared contract and language bindings.

The tests use deterministic local HTTP servers. Repository-level
`make sdk-conformance` additionally runs `examples/conformance` against Mango's
actual HTTP handlers with test repositories. Neither tier calls a real model or
claims verification of a deployed service, storage provider, or paid model.
