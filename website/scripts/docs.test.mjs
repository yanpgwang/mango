import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { normalizeBasePath } from '../src/lib/site.mjs';
import { remarkDocumentTitle, remarkRelativeDocLinks } from '../src/lib/markdown.mjs';
import { exportMarkdown } from '../src/lib/markdown-export.mjs';
import { resolveExportLink } from './check-export.mjs';
import { createPreviewServer } from './serve.mjs';

test('normalize root and nested base paths, reject URL injection', () => {
  assert.equal(normalizeBasePath('/'), '');
  assert.equal(normalizeBasePath('/mango/'), '/mango');
  assert.equal(normalizeBasePath('/team/docs'), '/team/docs');
  for (const value of ['https://example.com', '//evil', '/a/../b', '/a?key=x', '/a#x']) {
    assert.throws(() => normalizeBasePath(value));
  }
});

test('one title without removing later content headings', () => {
  const tree = { children: [{ type: 'heading', depth: 1 }, { type: 'heading', depth: 2 }] };
  remarkDocumentTitle()(tree);
  assert.deepEqual(tree.children, [{ type: 'heading', depth: 2 }]);
  const other = { children: [{ type: 'paragraph' }, { type: 'heading', depth: 1 }] };
  remarkDocumentTitle()(other);
  assert.equal(other.children.length, 2);
  const withImport = { children: [{ type: 'mdxjsEsm' }, { type: 'heading', depth: 1 }, { type: 'paragraph' }] };
  remarkDocumentTitle()(withImport);
  assert.deepEqual(withImport.children, [{ type: 'mdxjsEsm' }, { type: 'paragraph' }]);
});

test('resolve bare Markdown links without rewriting other URL types', () => {
  const urls = ['sdk.md', 'api/sessions.md#history', 'guide.mdx?view=full#stream', '../intro.md', './api.md', '/api/', '#next', 'https://example.com/a.md', '//example.com/a.md', 'mailto:a.md', 'image.svg'];
  const tree = { children: [{ type: 'paragraph', children: urls.map(url => ({ type: 'link', url })) }, { type: 'definition', url: 'sdk/python.md' }] };
  remarkRelativeDocLinks()(tree);
  assert.deepEqual(tree.children[0].children.map(node => node.url), [
    './sdk.md', './api/sessions.md#history', './guide.mdx?view=full#stream', ...urls.slice(3),
  ]);
  assert.equal(tree.children[1].url, './sdk/python.md');
});

test('export links cover assets, root hosting, base paths, and anchors', () => {
  const files = new Set(['index.html', 'api/sessions/index.html', 'img/mark.svg']);
  assert.deepEqual(resolveExportLink('/mango/api/sessions/#create', '/', '/mango', files), { file: 'api/sessions/index.html', hash: 'create' });
  assert.equal(resolveExportLink('/api/sessions/', '/', '', files).file, 'api/sessions/index.html');
  assert.equal(resolveExportLink('/mango/img/mark.svg', '/', '/mango', files).file, 'img/mark.svg');
  assert.equal(resolveExportLink('https://github.com/yanpgwang/mango', '/', '/mango', files), undefined);
  assert.throws(() => resolveExportLink('/api/sessions', '/', '/mango', files));
  assert.throws(() => resolveExportLink('/mango/missing/', '/', '/mango', files));
});

test('Markdown exports resolve links without rewriting code or mutating the page', () => {
  const tree = { type: 'root', children: [
    { type: 'paragraph', children: [{ type: 'link', url: './sdk.md', children: [{ type: 'text', value: 'SDK' }] }] },
    { type: 'code', lang: 'md', value: '[SDK](./sdk.md)' },
    { type: 'mdxjsEsm', value: 'export const title = "SDK";' },
  ] };
  const markdown = exportMarkdown(tree, () => 'https://docs.example.com/mango/sdk/');
  assert.ok(markdown.includes('[SDK](https://docs.example.com/mango/sdk/)'));
  assert.ok(markdown.includes('```md\n[SDK](./sdk.md)\n```'));
  assert.ok(!markdown.includes('export const'));
  assert.equal(tree.children[0].children[0].url, './sdk.md');
});

test('static preview serves only the mounted export, with no SPA fallback', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'mango-docs-test-'));
  await mkdir(join(directory, 'api'), { recursive: true });
  await writeFile(join(directory, 'index.html'), '<h1>Mango</h1>');
  await writeFile(join(directory, 'api/index.html'), '<h1>API</h1>');
  const server = createPreviewServer(directory, '/mango');
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  try {
    assert.equal((await fetch(`${origin}/mango/`)).status, 200);
    assert.equal(await (await fetch(`${origin}/mango/api/`)).text(), '<h1>API</h1>');
    assert.equal((await fetch(`${origin}/mango/missing/`)).status, 404);
    assert.equal((await fetch(`${origin}/api/`)).status, 404);
    assert.equal((await fetch(`${origin}/mango/%2e%2e%2foutside`)).status, 404);
    assert.equal((await fetch(`${origin}/mango/`, { method: 'POST' })).status, 405);
    assert.equal(await (await fetch(`${origin}/mango/`, { method: 'HEAD' })).text(), '');
  } finally {
    server.closeAllConnections();
    await new Promise(resolve => server.close(resolve));
    await rm(directory, { recursive: true, force: true });
  }
});
