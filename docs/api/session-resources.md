---
title: Session Resources
slug: /api/session-resources
---

# Session Resources

Session Resources attach File copies, Memory Stores, or frozen public Git
repositories to a Session without injecting their contents into model context.

```text
POST   /v1/sessions/{session_id}/resources
GET    /v1/sessions/{session_id}/resources
GET    /v1/sessions/{session_id}/resources/{resource_id}
DELETE /v1/sessions/{session_id}/resources/{resource_id}
```

## File attachments

A File attachment creates an independent downloadable copy scoped to the
Session. An explicit path is normalized beneath `/mnt/session/uploads`.
OpenSandbox materializes a permission-hardened sandbox-local copy as a current
backend limitation;
changing it does not change the S3-backed source or downloadable Session File.
Write a modified deliverable beneath `/mnt/session/outputs` when output
publication is available.
Deleting the source upload does not break the copy. Detach the Session Resource
to delete it from the sandbox.

Runtime add/delete commits desired state in PostgreSQL and is reconciled before
the next sandbox tool. A Session may hold up to 500 active resources and 500 MB
of aggregate File/repository snapshot bytes.

## Memory Store attachments

Up to eight Stores may be attached when a Session is created. Each attachment
is `read_only` or `read_write` and is mounted beneath
`/mnt/memory/<store-slug>/`. Memory attachments cannot currently be added or
removed after Session creation.

## Git repository attachments

A `git_repository` attachment is accepted when creating a Session directly or
as a Deployment resource template. Mango clones an anonymous public HTTPS
remote in the control plane, resolves the requested checkout once per Session,
and stores a bounded tar snapshot containing the worktree and `.git` metadata.
The sandbox restores that immutable source as an independent writable
directory; Agent edits never change the stored snapshot or the upstream
repository.

```json
{
  "agent": "agent_coder",
  "environment_id": "env_cloud",
  "resources": [
    {
      "type": "git_repository",
      "url": "https://github.com/acme/widgets.git",
      "checkout": {"type": "branch", "name": "main"},
      "mount_path": "/workspace/widgets"
    }
  ]
}
```

`checkout` may select a branch or a full 40-character commit SHA. When it is
omitted, Mango uses the remote's advertised default branch. The response
retains the requested checkout and exposes `resolved_commit`, the exact commit
frozen into the Session. Omitting `mount_path` produces
`/workspace/<repository-name>`.

Repository code is part of the Session's trust boundary. Mango rejects URL
credentials, query strings, fragments, non-HTTPS transports, mount paths
outside `/workspace`, paths overlapping `/workspace/skills`, special archive
entries, and symlinks escaping the repository. Outbound clone connections use
the same public-network-only dialer as other tenant-configured endpoints.

Deployment resources retain only the requested URL, checkout, and mount path.
Every Run resolves a branch or default checkout again and freezes a new Session
snapshot; an explicit commit stays fixed. Updating a Deployment changes future
Runs only.

The current slice intentionally does not support private repositories, raw
tokens, push/PR credentials, recursive submodule checkout, Git LFS object
download, repository Skill discovery, runtime attach, runtime detach, or
in-place Session updates. Submodule directories remain uninitialized and LFS
files remain pointer files. Deleting the Session removes the stored snapshot
through the normal crash-recoverable File lifecycle.

## Availability

File, Git, and Memory mounts require a cloud Environment using OpenSandbox.
GitHub repository resources and update-time token rotation are not Mango API
variants. Repository restoration requires a POSIX `tar` executable in the
sandbox image; the local default image satisfies this contract.

See [Files](files.md) and [Memory](memory.md).
