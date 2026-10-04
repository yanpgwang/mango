---
title: Files
description: Upload and download immutable Workspace files.
slug: /api/files
---

# Files

The Files API stores immutable client uploads and generated MCP text results in configured S3-compatible
storage. PostgreSQL owns metadata and crash-recoverable upload/delete intents;
the object store owns bytes.

```text
POST   /v1/files
GET    /v1/files
GET    /v1/files/{file_id}
GET    /v1/files/{file_id}/content
DELETE /v1/files/{file_id}
```

Set `MANGO_FILE_S3_BUCKET` and the corresponding endpoint, region, and
credential variables before using these routes. All routes use Mango's standard
bearer authentication; uploads require `multipart/form-data`.

## Upload and list

Upload one multipart part named `file`. The maximum file size is 500 MB.
Uploads stream through bounded temporary storage rather than being buffered in
Go memory.

Lists use the `after_id` or `before_id` direction, an optional `limit`,
and a `data`/`has_more` envelope. The two direction parameters
cannot be combined.

Every ready File can be downloaded with `GET /v1/files/{file_id}/content`
using an API key for its Workspace. The response streams the immutable bytes
with content type, length, `Content-Disposition: attachment`, and `nosniff`.
Metadata includes `checksum_sha256`, the hex SHA-256 of complete bytes.
Missing, deleting, and cross-Workspace Files return 404. Active scoped Work
credentials may GET metadata/content only for generated MCP output Files owned
by their Session. They cannot list, upload, delete, or read other Files; ordinary
input and workspace deliverable transfers remain application-owned.

```sh
curl "$MANGO_BASE_URL/v1/files/$FILE_ID/content" \
  -H "Authorization: Bearer $MANGO_API_KEY" \
  --output report.csv
```

## Outcome rubrics

A ready client upload can be reused as an outcome rubric by sending
`{"type":"file","file_id":"file_..."}` in `user.define_outcome`. Admission
validates the File as bounded text independently of its binary download endpoint.
Mango reads at most the largest valid UTF-8 encoding of 262,144 characters,
checks the stored byte count and SHA-256, rejects empty, invalid UTF-8,
over-limit, deleting, missing, and cross-Workspace Files, and
durably snapshots the resulting text with the admitted event.

The event returned to clients retains only the File reference. Deleting the
source after admission does not change the working-agent or grader input.

## Message content

A ready client upload can be referenced by a `user.message` document:

```json
{
  "type": "document",
  "source": {"type": "file", "file_id": "file_..."}
}
```

This text-only implementation accepts declared text formats, JSON/XML
variants, common textual application formats, and generic
`application/octet-stream` uploads whose bytes are valid UTF-8 and contain no
NUL. It does not parse PDF, OCR images, or send provider-native document
blocks. Mango reads and verifies the object before event admission, stores an
immutable private snapshot with the event, and sends the model an ordinary
text block containing filename/media metadata plus the File content. The
public event retains the original `file_id`, and deleting the source File does
not change replay or later conversation turns.

Each File and the aggregate resolved File content in one admission are limited
to 262,144 characters. Empty, oversized, corrupt, non-UTF-8, non-text,
missing, deleting, and cross-Workspace Files fail before the
Session or event is committed. File-sourced images and File documents inside
tool results remain unsupported.

## Generated MCP output

When projected MCP text exceeds 100,000 characters and fits within 32 MiB,
Mango publishes it as a `text/plain` File. `agent.mcp_tool_result.file_id`
references it, and the preview identifies `.mango-tool-results/{file_id}.txt`
relative to the Session workspace root. Native Go workers verify and materialize
it before dispatching local tools, including after reattachment. A custom worker
can GET the File metadata and content with its active Work token and implement
the same preparation step.

A generated output File cannot be deleted while its Session exists: DELETE
returns 409. Delete the Session first to release this pin, then delete the File
normally. Session deletion preserves the ready File and its bytes for operators.
This prevents storage retries from resurrecting deleted results. Generated
pending uploads are recovered from their durable MCP receipt; startup cleanup
waits until their owning Session is gone. Storage must be configured on both the
API and orchestrator processes. Missing storage or projected text over 32 MiB
produces an explicit tool error and preview.

Publication retries keep the same File ID and immutable receipt but use separate
internal object keys. An older write cannot publish over a replacement. Durable
cleanup records collect superseded writes across process restarts and after
Session/File deletion; an unknown remote outcome keeps its record for later
scans. The current uploading or ready File's bytes are protected from cleanup.
Recovery retries File publication without repeating the completed MCP call.

## Worker files

Files created in a self-hosted sandbox remain in the operator-owned workspace.
The operator or application reads selected deliverables from that workspace
and uploads them through `POST /v1/files`, then retains the returned File IDs.
Those uploads can be downloaded through the authenticated content endpoint.
The application owns any association between File IDs and Sessions. Mango does
not automatically mount inputs or publish outputs from a workspace.

## Lifecycle and limits

- Metadata becomes visible only after the object write completes.
- Delete hides metadata before deleting bytes; startup and periodic reconciliation finish
  interrupted operations.
- If an object write fails and its cleanup cannot delete the object, Mango
  retains the hidden upload record. Reconciliation retries that
  deletion after storage recovers; failed cleanup does not discard the record.
  Independent object cleanup guards retain late writes even after the
  original upload metadata is gone. Unknown remote writes keep a guard until
  writer completion can be confirmed.
- Files are accepted as bounded UTF-8 outcome rubrics and text-only
  `user.message` document content.
- Worker workspace files remain private to the operator unless an application
  uploads them explicitly.
- File metadata and object keys are Workspace-scoped. A one-minute internal
  database lease, renewed every 20 seconds during ordinary uploads, protects
  active requests from another API process's cleanup. Completion checks lease
  ownership; cleanup atomically claims only ended or expired uploads. API
  processes retry cleanup every 20 seconds, including after a restart before
  lease expiry. Lease loss cancels the upload; applications may retry the request.

See [Session Resources](session-resources.md) for supported Memory Store inputs.
