---
title: Skills
slug: /api/skills
---

# Skills

Custom Skills are versioned zip bundles stored in S3-compatible storage. An
Agent or Session resolves every reference to an immutable Skill Version before
execution.

```text
POST   /v1/skills
GET    /v1/skills
GET    /v1/skills/{skill_id}
DELETE /v1/skills/{skill_id}
POST   /v1/skills/{skill_id}/versions
GET    /v1/skills/{skill_id}/versions
GET    /v1/skills/{skill_id}/versions/{version}
GET    /v1/skills/{skill_id}/versions/{version}/content
DELETE /v1/skills/{skill_id}/versions/{version}
```

Skills routes require configured Files storage and Mango's standard bearer
authentication. Create and Version uploads require `multipart/form-data`.

## Bundle contract

Create and Version uploads accept a zip archive or path-qualified multipart
files smaller than 30 MB. A bundle contains one top-level directory and a root
`SKILL.md`; validation rejects traversal, absolute paths, links, duplicate
paths, and invalid frontmatter metadata.

Agent references use the documented custom union. An omitted Version or
`latest` is replaced by a concrete ready Version before the Agent Version or
Session snapshot is stored. Active Agent and Session pins prevent deleting an
archive that is still executable.

## Runtime behavior

Docker, E2B, CubeSandbox, OpenSandbox, and Daytona Sessions initially expose
only Skill name, description, and instruction path metadata. A private `Skill`
dispatcher selects the immutable bundle, returns `Launching skill: <name>`,
and injects the complete main instruction file on demand. Supporting files and
scripts remain available through ordinary sandbox tools. Docker presents a
read-only bind mount; remote adapters present a permission-hardened local copy
and preserve the canonical archive in Mango storage.

Primary and `self` Agent bundles use `/workspace/skills/<name>/`; external
roster Agents use isolated namespaces below `/workspace/skills/.agents/`.

External managed catalogs, repository auto-loading, and Environment Worker
runtime activation are not implemented. Session creation returns `422` when
the effective primary Agent or any resolved roster member has custom Skills
but the selected Environment cannot execute them. This includes every
self-hosted Environment and cloud Environments using a sandbox adapter without
Skill support. Session overrides are applied before this check; Skills storage
and Agent definitions do not depend on the configured sandbox capability.

See [Environment Work](environment-work.md) for the external worker boundary
and [Sandbox backends](../sandboxes.md#custom-skill-mounts) for the remote-copy
limitation.
