---
title: Files
description: Upload and download immutable Workspace files.
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

Lists use the `after_id` or `before_id` direction, an optional `limit`,
and a `data`/`has_more` envelope. The two direction parameters
cannot be combined.

Every ready File can be downloaded with `GET /v1/files/{file_id}/content`
using an API key for its Workspace. The response streams the immutable bytes
with content type, length, `Content-Disposition: attachment`, and `nosniff`.
There are no download-eligibility or Session-scope fields. Missing, deleting,
and cross-Workspace Files return 404. Scoped Work credentials cannot use the
Files API; the trusted application or operator performs explicit transfers.

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

## Worker files

Files created in a self-hosted sandbox remain in the operator-owned workspace.
The operator or application reads selected deliverables from that workspace
and uploads them through `POST /v1/files`, then retains the returned File IDs.
Those uploads can be downloaded through the authenticated content endpoint.
The application owns any association between File IDs and Sessions. Mango does
not automatically mount inputs or publish outputs from a workspace.

## Lifecycle and limits

- Metadata becomes visible only after the object write completes.
- Delete hides metadata before deleting bytes; startup reconciliation finishes
  interrupted operations.
- Files are accepted as bounded UTF-8 outcome rubrics and text-only
  `user.message` document content.
- Worker workspace files remain private to the operator unless an application
  uploads them explicitly.
- File metadata and object keys are Workspace-scoped. Startup reconciliation
  currently assumes one Files-enabled API process.

See [Session Resources](session-resources.md) for supported Memory Store inputs.
