import assert from 'node:assert/strict';
import { readdir, readFile } from 'node:fs/promises';
import { join, relative, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { load } from 'cheerio';
import { staticClient } from 'fumadocs-core/search/client/orama-static';
import { basePath, absoluteUrl } from '../src/lib/site.mjs';
import { createPreviewServer } from './serve.mjs';

async function listFiles(directory) {
  const files = [];
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const file = join(directory, entry.name);
    if (entry.isDirectory()) files.push(...await listFiles(file));
    else files.push(file);
  }
  return files;
}

export function resolveExportLink(href, pagePath, mount, files) {
  const url = new URL(href, `https://docs.invalid${mount}${pagePath}`);
  if (url.origin !== 'https://docs.invalid') return undefined;
  const pathname = decodeURIComponent(url.pathname);
  if (mount && pathname !== mount && !pathname.startsWith(`${mount}/`)) {
    throw new Error(`link escapes documentation base path: ${href}`);
  }
  const local = pathname.slice(mount.length).replace(/^\/+/, '');
  const candidates = [local, `${local.replace(/\/$/, '')}/index.html`.replace(/^\//, '')];
  const file = candidates.find(candidate => files.has(candidate));
  if (!file) throw new Error(`missing exported target: ${href}`);
  return { file, hash: decodeURIComponent(url.hash.slice(1)) };
}

export async function checkExport(directory = resolve('out')) {
  const all = await listFiles(directory);
  const files = new Set(all.map(file => relative(directory, file)));
  const pages = new Map();
  for (const file of files) {
    if (!file.endsWith('.html') || file === '404.html' || file.startsWith('404/') || file.startsWith('_not-found/')) continue;
    const html = load(await readFile(join(directory, file), 'utf8'));
    const path = `/${file.replace(/index\.html$/, '')}`;
    assert.equal(html('h1').length, 1, `${path}: exactly one document title`);
    assert.ok(html('title').text().includes('Mango'), `${path}: Mango page metadata`);
    assert.ok(!html('main').text().includes('::include['), `${path}: unresolved snippet include`);
    assert.equal(html('meta[property="og:title"]').attr('content'), html('h1').text(), `${path}: social title matches document`);
    assert.equal(html('link[rel="canonical"]').attr('href')?.replace(/\/$/, ''), absoluteUrl(path).replace(/\/$/, ''), `${path}: canonical URL`);
    pages.set(file, { html, path });
  }
  const errors = [];
  for (const { html, path } of pages.values()) {
    for (const element of html('a[href], link[href], script[src], img[src]').toArray()) {
      const href = html(element).attr('href') ?? html(element).attr('src');
      if (!href || /^(?:data:|mailto:|tel:)/.test(href)) continue;
      try {
        const target = resolveExportLink(href, path, basePath, files);
        if (target?.hash && pages.has(target.file)) {
          const ids = pages.get(target.file).html('[id]').toArray().map(node => node.attribs.id);
          assert.ok(ids.includes(target.hash), `missing anchor: ${href}`);
        }
      } catch (error) { errors.push(`${path}: ${error.message}`); }
    }
  }
  assert.deepEqual(errors, [], errors.join('\n'));
  // Existing routes and generated category landing pages must remain reachable.
  for (const path of ['index.html', 'getting-started/index.html', 'sdk/index.html', 'start/index.html', 'api/index.html', 'api/sessions/index.html', 'api/events/index.html', 'guides/index.html', 'examples/index.html', 'operations/index.html', 'project/index.html', 'sdk/go/index.html', 'sdk/python/index.html', 'sdk/typescript/index.html']) {
    assert.ok(files.has(path), `missing documentation route ${path}`);
  }
  for (const path of ['getting-started/index.html', 'api/agents/index.html', 'api/environments/index.html', 'api/sessions/index.html', 'api/events/index.html']) {
    const { html } = pages.get(path);
    const labels = html('[role="tab"]').toArray().map(node => html(node).text().trim());
    for (const language of ['TypeScript', 'Python', 'Go', 'HTTP']) {
      assert.ok(labels.includes(language), `${path}: missing ${language} tab`);
    }
  }
  const homeLinks = pages.get('index.html').html('a[href]').toArray().map(node => node.attribs.href.replace(/\/$/, ''));
  for (const path of ['/sdk', '/api', '/architecture']) {
    assert.ok(homeLinks.includes(`${basePath}${path}`), `missing overview navigation ${path}`);
  }
  const codingGuide = pages.get('examples/coding-agent-iterate/index.html').html('main').text();
  for (const snippet of ['AsyncMango(', 'client.upload_file(', 'client.stream_session_events(', 'client.download_file(', '--network=none']) {
    assert.ok(codingGuide.includes(snippet), `coding-agent guide: missing executable snippet ${snippet}`);
  }

  // Exercise the same static search client used in the browser, through a
  // loopback-only static server. No hosted search/API or browser automation.
  const server = createPreviewServer(directory);
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  try {
    const search = staticClient({ from: `${origin}${basePath}/search-index.json` });
    for (const query of ['Sessions', 'Python', 'Memory']) {
      const results = await search.search(query);
      assert.ok(results.length > 0, `static search has no results for ${query}`);
      for (const result of results) {
        const target = resolveExportLink(`${basePath}${result.url}`, '/', basePath, files);
        assert.ok(target, `invalid search result ${result.url}`);
      }
    }
    for (const path of ['/markdown/index.md', '/markdown/getting-started/index.md', '/markdown/api/sessions/index.md', '/markdown/examples/coding-agent-iterate/index.md', '/llms.txt']) {
      const response = await fetch(`${origin}${basePath}${path}`);
      assert.equal(response.status, 200, `${path}: exported Markdown available`);
      const markdown = await response.text();
      assert.ok(markdown.startsWith('#'), `${path}: Markdown content`);
      assert.ok(!markdown.includes('::include['), `${path}: unresolved Markdown snippet`);
      if (path === '/markdown/getting-started/index.md') {
        assert.ok(markdown.includes(absoluteUrl('/sdk#install-from-source')), 'Markdown links must work outside the source tree');
        for (const method of ['createSession', 'create_session', 'CreateSession', 'curl']) {
          assert.ok(markdown.includes(method), `${path}: missing ${method} example`);
        }
      }
      if (path === '/markdown/examples/coding-agent-iterate/index.md') {
        for (const snippet of ['AsyncMango(', 'client.upload_file(', 'client.download_file(', '--network=none']) {
          assert.ok(markdown.includes(snippet), `${path}: missing executable snippet ${snippet}`);
        }
      }
    }
    assert.equal((await fetch(`${origin}${basePath}/definitely-not-a-document/`)).status, 404);
  } finally {
    server.closeAllConnections();
    await new Promise(resolve => server.close(resolve));
  }
  console.log(`Static export verified: ${pages.size} pages, links/anchors/assets, language tabs, search and Markdown (${basePath || '/'})`);
}

if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
  await checkExport();
}
