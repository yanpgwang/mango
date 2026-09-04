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

For self-hosted Environments, the Go Environment Worker downloads the frozen
primary and roster Agent pins before starting tool dispatch. Primary Skills use
`<workdir>/skills/<name>` and external roster Agents use the same stable scoped
layout as the Agent loop. The worker accepts only Mango's canonical zip shape,
bounds compressed and expanded content, rejects path escapes and non-regular
members, atomically publishes each directory, and removes its downloaded
directories when the Work item ends. Model-visible paths are relative
`skills/...` paths rooted at that `workdir`, so a launcher may choose a location
other than `/workspace`. A setup failure fails closed.

External managed catalogs and repository auto-loading are not implemented.
Cloud Session creation still returns `422` when its transitional sandbox
adapter cannot execute custom Skills. Session overrides are applied before
this check; Skills storage and Agent definitions do not depend on the
configured sandbox capability.

See [Environment Work](environment-work.md) for the external worker boundary
and [Sandbox backends](../sandboxes.md#custom-skill-mounts) for the remote-copy
limitation.
