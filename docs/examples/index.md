---
title: Examples
description: Runnable examples with explicit verification boundaries.
---

# Examples

- [SDK quickstart](../getting-started.md): Go, Python, TypeScript, or HTTP against the offline stack.
- [Terminal UI](terminal-ui.md): inspect and interact with Sessions in the terminal.
- [Coding-agent iteration](coding-agent-iterate.md): use the Python SDK to upload failing tests, follow a repair, download the result, and independently verify it.
- [Human-in-the-loop gate](hitl-gate.md): persist a custom-tool approval barrier.
- [Specialist team](multi-agent-team.md): coordinate a team, consult an Advisor, and synthesize reports.

Start with the SDK quickstart for an offline first turn. The coding, human-gate,
and specialist-team examples connect to a running Mango deployment with a real
model; they do not start the runtime or invoke its system tests. Each guide
explains its prerequisites, expected results, cleanup, and limits. A successful
example is evidence for that workflow, not a production-readiness guarantee.
