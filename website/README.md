# Documentation site

Fumadocs with Next.js static export, the Docs layout, a neutral light/dark theme,
and Mango orange accents. The repository-level `docs/` directory is the single
source of documentation; it is read directly, not copied into a second tree.

```bash
npm ci
npm start
```

Node.js 22+ is required. The default development URL is
`http://localhost:3000/mango/`. To choose a local port:

```sh
npm start -- --hostname 127.0.0.1 --port 4175
```

The production defaults target GitHub Pages at
`https://yanpgwang.github.io/mango/`. Override `DOCS_URL` and
`DOCS_BASE_URL` when building for another host. They are build-time settings;
use the same base URL for build and preview. For a root-hosted site:

```sh
DOCS_URL=https://docs.example.com DOCS_BASE_URL=/ npm run build
DOCS_BASE_URL=/ npm run serve
```

## Checks and static preview

```sh
npm run typecheck
npm test
npm run build
npm run serve
```

`npm run build` writes `out/` and checks page metadata, internal links and
anchors, assets, language tabs, static search, Markdown exports, and the main
reader paths. `npm run serve` serves that exact artifact on loopback only
at `http://127.0.0.1:4175/mango/`; override `DOCS_PORT` if needed. Unknown routes
return 404 rather than falling back to the home page.

Search runs in the browser against the exported `search-index.json`; it needs
no hosted search service. Copy Markdown uses static `/markdown/.../index.md`
resources, and `llms.txt` indexes them. The built site needs no Node server,
model credentials, provider requests, or remote font downloads.

## Write documentation

- Keep page content in `docs/*.md` (or `.mdx` when JSX is required). Frontmatter
  `title` and a one-sentence `description` are required by Mango's editorial
  convention; a root-relative `slug` is optional.
- The source H1 remains readable on GitHub and is rendered once by the site.
- Organize navigation with `meta.json`. Use `pagesIndex` for a folder landing
  page, including an existing sibling such as `../sdk`; do not also list that
  index as a child. Do not use `sidebar_label` or `sidebar_position`.
- Use relative `.md` or `.mdx` links for documentation, including literal
  relative `href` values on native Fumadocs `Card` components. Code/source links outside `docs/`
  should point to the corresponding GitHub file. Put images in `public/`.
- Mermaid fences render diagrams locally. Admonitions use `:::warning[Title]`
  (or `info`, `danger`, etc.) and a closing `:::`.

### Content responsibilities

The README and docs home are independent entry points: both introduce Mango
as an open-source, self-hosted alternative to Claude Managed Agents and explain
what its runtime manages. Lead with that product category and operator control;
durability supports the execution promise. Detailed tutorials need not repeat
the positioning. The README shows how to start Mango and points to the docs;
the docs home helps readers choose a path. Core concepts explain the public
resource model; the quickstart completes one offline, text-only Session with
the current source SDKs. Model setup and tool-worker setup have their own
guides. Tutorials state prerequisites, expected results, and cleanup; API
reference pages describe requests, responses, and lifecycle constraints.
Architecture explains implementation and recovery. Capability status and
design provenance each have one authoritative home.

Keep these boundaries when editing: a new reader must be able to get the
repository, start the stack, run one example, recognize success, and stop it
without reading a design record or the contributor test matrix. Keep alpha
status and task-specific security limits visible, but link to their full
explanation instead of repeating policy paragraphs. SDK examples must match
the server checkout and state whether their packages are published.

Use the standard Docs layout, page title, description, TOC, code tabs, callouts,
and cards. Avoid additional navigation layers or custom visual components when
these suffice. A docs rewrite changes presentation, not runtime, storage, or
API semantics. Validate the static export, search, Markdown links, and the
desktop/mobile reading path before delivery.

### Multi-language examples

Keep snippets in the runnable examples under `sdk/*/examples/quickstart*` and
`examples/sdk-quickstart.sh`. Name regions with `# region` / `# endregion`
(Python/shell) or `// #region` / `// #endregion` (Go/TypeScript).
The coding-agent guide also includes regions from
`examples/coding-agent/main.py` and `verify.py`.

Include regions in Markdown rather than copying code:

```md
::include[../sdk/typescript/examples/quickstart.ts#session]{lang="ts" meta='tab="TypeScript" tab-group="mango-language"'}

::include[../sdk/python/examples/quickstart.py#session]{lang="python" meta='tab="Python" tab-group="mango-language"'}
```

Adjacent code blocks/includes become a tab group. Use the same
`tab-group="mango-language"` so the user's selection carries across examples.
For standalone code fences, use the same `tab` and `tab-group` metadata.

After modifying SDK examples, run `make sdk-test` and `make sdk-conformance`
from the repository root. The latter runs the exact rendered example files
against real HTTP handlers with test-only repositories and model behavior;
it is not production, recovery-service, or live-model evidence.
Cookbook-style examples such as the coding-agent guide are separate applications,
not system-test entrypoints. Run them against a configured Mango deployment as
described in their guides. Documentation builds resolve their source snippets
without executing the applications.

## Publishing

The `.github/workflows/pages.yml` workflow builds and deploys the static site
after documentation changes land on `main`, and it can also be started manually.
Configure the repository's Pages publishing source as **GitHub Actions** before
the first deployment.
The workflow uploads `website/out`, including `.nojekyll`. Changes to included
SDK examples also trigger a rebuild. Nothing is deployed merely by opening a PR.
