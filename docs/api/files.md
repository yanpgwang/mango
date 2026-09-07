---
title: Files
description: Upload immutable files for application and text-message workflows.
slug: /api/files
---

# Files

The Files API stores immutable client uploads in configured S3-compatible
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

Lists currently use the `after_id` or `before_id` direction, an optional
`scope_id`, and a `data`/`has_more` envelope. The two direction parameters
cannot be combined.

Client uploads have `scope: null` and `downloadable: false`; their content
endpoint is intentionally unavailable. Mango may read validated text uploads
internally for supported message and outcome workflows.

## Outcome rubrics

A ready top-level client upload can be reused as an outcome rubric by sending
`{"type":"file","file_id":"file_..."}` in `user.define_outcome`. This is an
internal admission read and does not make the File publicly downloadable.
Mango reads at most the largest valid UTF-8 encoding of 262,144 characters,
checks the stored byte count and SHA-256, rejects empty, invalid UTF-8,
over-limit, deleting, missing, cross-Workspace, and Session-scoped Files, and
durably snapshots the resulting text with the admitted event.

The event returned to clients retains only the File reference. Deleting the
source after admission does not change the working-agent or grader input.

## Message content

A ready top-level client upload can be referenced by a `user.message` document:

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
Session-scoped, missing, deleting, and cross-Workspace Files fail before the
Session or event is committed. File-sourced images and File documents inside
tool results remain unsupported.

## Worker files

Files created in a self-hosted sandbox remain in the operator-owned workspace.
Mango does not automatically publish a directory as Session-scoped Files.
Applications that need downloadable artifacts should upload them explicitly or
implement that transfer in their launcher under their own storage policy.

## Lifecycle and limits

- Metadata becomes visible only after the object write completes.
- Delete hides metadata before deleting bytes; startup reconciliation finishes
  interrupted operations.
- Top-level Files are accepted as bounded UTF-8 outcome rubrics and text-only
  `user.message` document content.
- Worker workspace files remain private to the operator unless an application
  uploads them explicitly.
- File metadata and object keys are Workspace-scoped. Startup reconciliation
  currently assumes one Files-enabled API process.

See [Session Resources](session-resources.md) for supported Memory Store inputs.
