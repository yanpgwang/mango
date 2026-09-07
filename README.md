<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/mango-logo-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="assets/mango-logo.svg">
    <img src="assets/mango-logo.png" alt="Mango" width="420">
  </picture>
</p>

<p align="center">
  <strong>The open-source, self-hosted alternative to Claude Managed Agents.</strong>
</p>

<p align="center">
  <a href="https://yanpgwang.github.io/mango/">Documentation</a> ·
  <a href="https://yanpgwang.github.io/mango/getting-started">Quickstart</a> ·
  <a href="https://yanpgwang.github.io/mango/api">API reference</a> ·
  <a href="https://yanpgwang.github.io/mango/examples">Examples</a>
</p>

<p align="center">
  <img alt="Status: Alpha" src="https://img.shields.io/badge/status-alpha-orange">
  <a href="https://github.com/yanpgwang/mango/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/yanpgwang/mango/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://github.com/yanpgwang/mango/actions/workflows/pages.yml"><img alt="Documentation" src="https://github.com/yanpgwang/mango/actions/workflows/pages.yml/badge.svg"></a>
  <a href="LICENSE"><img alt="Apache 2.0 license" src="https://img.shields.io/github/license/yanpgwang/mango"></a>
</p>

Define an Agent, start a Session, and send it work through an API.
Mango runs the model-and-tool loop,
persists the conversation, and coordinates execution and recovery. Build your
application with Go, Python, TypeScript/JavaScript, or the HTTP API.

## Why Mango?

- **Delegate execution.** Configure instructions and tools, then let Mango
  drive the agent loop, manage context, and coordinate tool calls.
- **Own the infrastructure.** Self-host the API, orchestration, state, and
  tool workers. The first-party Docker launcher runs shell and file operations
  on your infrastructure; model calls use your configured endpoint.
- **Keep work running.** Persist accepted input, conversation history, and
  action waits across API and orchestration-worker restarts.
- **Stay in control.** Stream responses, send follow-ups, interrupt a turn, or
  wait for a person or your application to return a tool result.
- **Build beyond one agent.** Coordinate persistent specialists,
  reuse Skills and Memory, schedule Sessions, and subscribe to lifecycle Webhooks.

> [!IMPORTANT]
> Mango is alpha. APIs may change, and production deployment is not yet supported.
> See [capabilities and limits](https://yanpgwang.github.io/mango/capabilities)
> for the supported scope of each workflow.

## Quickstart

You need Git, Docker with Compose, and `make`. Start the local stack with its
built-in offline model; no model credentials are needed:

```bash
git clone https://github.com/yanpgwang/mango.git
cd mango
export MANGO_API_KEY=sk-mango-local-development
MANGO_MODEL_BASE_URL= MANGO_MODEL_API_KEY= MANGO_MODEL_ID= \
  docker compose -f deployments/local/compose.yaml up -d --build
make local-health
```

With `curl` and `jq` installed, run a complete first Session:

```bash
bash examples/sdk-quickstart.sh
```

The example creates an Environment, Agent, and Session, sends a message, reads
the persisted reply, and cleans up its resources. Look for `Quickstart completed`.
This is a text-only walkthrough; running shell or file tools requires a separate
[Environment worker](https://yanpgwang.github.io/mango/guides/self-hosted-worker).
The local key above is for development only.

Prefer an SDK? The [Quickstart](https://yanpgwang.github.io/mango/getting-started)
walks through the same flow in TypeScript, Python, Go, and HTTP. **The current
resource-based SDKs must be installed from source**; published alpha 1 packages
use an earlier interface. Follow the [SDK installation guide](https://yanpgwang.github.io/mango/sdk).

Stop the stack while keeping its data:

```bash
make local-down
```

## Explore Mango

| I want to… | Start here |
| --- | --- |
| Understand Agents, Sessions, and workers | [Core concepts](https://yanpgwang.github.io/mango/concepts) |
| Connect a model endpoint | [Model configuration](https://yanpgwang.github.io/mango/guides/model-configuration) |
| Run sandboxed shell and file tools | [Docker worker guide](https://yanpgwang.github.io/mango/guides/self-hosted-worker) |
| Add human input or coordinate a team | [Runnable examples](https://yanpgwang.github.io/mango/examples) |
| Inspect Sessions in a terminal | [Terminal UI](https://yanpgwang.github.io/mango/examples/terminal-ui) |
| Integrate an API operation | [API reference](https://yanpgwang.github.io/mango/api) |
| Understand deployment and recovery | [Deployment](https://yanpgwang.github.io/mango/deployment) · [Architecture](https://yanpgwang.github.io/mango/architecture) |

Mango is an independent project, unaffiliated with Anthropic. Claude Managed
Agents is a design reference; Mango owns its API and does not require or proxy a
hosted agent service. See [design provenance](https://yanpgwang.github.io/mango/provenance)
for adopted concepts and intentional differences.

## Contributing

Bug reports, documentation fixes, and focused pull requests are welcome.
Start with [CONTRIBUTING.md](CONTRIBUTING.md) for setup and the relevant tests.

```bash
make verify       # lint, unit tests, race tests, and vet
make docs-check   # check and build the documentation site
```

Report vulnerabilities privately through the process in [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE).
