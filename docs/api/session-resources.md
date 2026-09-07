---
title: Session resources
description: Attach Memory Stores to self-hosted Sessions.
---

# Session resources

Mango's self-hosted API supports Memory Stores as Session Resources. Attach
them when creating a Session or use them as Deployment templates:

```json
{
  "resources": [
    {
      "type": "memory_store",
      "memory_store_id": "memstore_123",
      "access": "read_write",
      "instructions": "Keep durable project decisions here."
    }
  ]
}
```

The Session response includes the resolved Store name, description,
instructions, access, and mount path. The external worker synchronizes the
Store into its sandbox before tool execution and writes back authorized changes
through scoped Session APIs.

File and Git Resources belong to CMA's hosted-sandbox workflow and are not
accepted by Mango's self-hosted Session contract. The operator should stage
files or repositories while launching the sandbox, or let the agent obtain them
inside the worker under the operator's network and credential policy.

There are no post-creation `/sessions/{id}/resources` mutation routes. This
avoids promising a control-plane mount lifecycle for filesystems Mango does not
own.

See [Memory](memory.md), [Sessions](sessions.md), and
[Self-hosted workers](../architecture/self-hosted-workers.md).
