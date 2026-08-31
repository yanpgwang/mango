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
anchors, assets, language tabs, static search, Markdown exports, and existing
landing-page URLs. `npm run serve` serves that exact artifact on loopback only
at `http://127.0.0.1:4175/mango/`; override `DOCS_PORT` if needed. Unknown routes
return 404 rather than falling back to the home page.

Search runs in the browser against the exported `search-index.json`; it needs
no hosted search service. Copy Markdown uses static `/markdown/.../index.md`
resources, and `llms.txt` indexes them. The built site needs no Node server,
model credentials, provider requests, or remote font downloads.

## Write documentation

- Keep page content in `docs/*.md` (or `.mdx` when JSX is required). Frontmatter
  `title` is required; `description` and a root-relative `slug` are optional.
- The source H1 remains readable on GitHub and is rendered once by the site.
- Organize navigation with `meta.json`. Use `pagesIndex` for a folder landing
  page, including an existing sibling such as `../sdk`.
- Use relative `.md` links for documentation. Code/source links outside `docs/`
  should point to the corresponding GitHub file. Put images in `public/`.
- Mermaid fences render diagrams locally. Admonitions use `:::warning[Title]`
  (or `info`, `danger`, etc.) and a closing `:::`.

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
For the coding-agent guide, also run `make test-coding-agent-example-unit` and
`make test-coding-agent-example`. The latter executes its exact Python program
against real backing services with deterministic inference; see the guide for
dependencies and the explicitly opt-in live-model target.

## Publishing

The `.github/workflows/pages.yml` workflow builds and deploys the static site
after documentation changes land on `main`, and it can also be started manually.
Configure the repository's Pages publishing source as **GitHub Actions** before
the first deployment.
The workflow uploads `website/out`, including `.nojekyll`. Changes to included
SDK examples also trigger a rebuild. Nothing is deployed merely by opening a PR.
