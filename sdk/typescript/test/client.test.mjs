import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { once } from 'node:events';
import { setTimeout as delay } from 'node:timers/promises';
import { Mango, APIError, ProtocolError, operations, parseSSE } from '../dist/index.js';

const json = (body, init = {}) => new Response(JSON.stringify(body), { ...init, headers: { 'content-type': 'application/json', ...init.headers } });
const encoder = new TextEncoder();
function bytes(...chunks) {
  return new ReadableStream({ start(controller) {
    for (const chunk of chunks) controller.enqueue(typeof chunk === 'string' ? encoder.encode(chunk) : chunk);
    controller.close();
  } });
}
async function collect(iterator) { const out = []; for await (const item of iterator) out.push(item); return out; }
async function serverFor(t, handler) {
  const server = createServer(handler);
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  t.after(() => { server.closeAllConnections(); server.close(); });
  return `http://127.0.0.1:${server.address().port}`;
}

test('every OpenAPI operation has a generated, callable method with correct wire routing', async () => {
  const manifest = JSON.parse(await readFile(new URL('../../operations.json', import.meta.url), 'utf8'));
  assert.equal(Object.keys(operations).length, 98);
  assert.deepEqual(Object.keys(operations).sort(), manifest.operations.map(op => op.id).sort());
  for (const op of manifest.operations) {
    let called = 0;
    const client = new Mango({ baseURL: 'http://localhost/proxy', apiKey: 'test-only', fetch: async (url, request) => {
      called++;
      let path = op.path;
      for (const p of op.parameters.filter(p => p.in === 'path')) path = path.replace(`{${p.name}}`, 'part%2F%25%3F%23');
      assert.equal(new URL(url).pathname, `/proxy${path}`, op.id);
      assert.equal(request.method, op.method, op.id);
      assert.equal(request.headers.get('authorization'), op.public ? null : 'Bearer test-only', op.id);
      assert.equal(request.redirect, 'manual');
      if (op.request_required) assert.ok(request.body !== undefined, op.id);
      if (op.response_content_type === 'text/event-stream') return new Response(bytes('data: {}\n\n'), { headers: { 'content-type': 'text/event-stream' } });
      if (!op.response_content_type) return new Response(null);
      if (op.response_content_type === 'application/json') return json({});
      return new Response('payload');
    } });
    const params = {};
    for (const p of op.parameters.filter(p => p.required)) params[p.name] = p.in === 'path' ? 'part/%?#' : 'test';
    if (op.request_content_type === 'multipart/form-data') params.body = op.id === 'uploadFile' ? { file: new File(['x'], 'x.txt') } : { files: [new File(['skill'], 'test/SKILL.md')] };
    else if (op.request_required) params.body = {};
    const result = client[op.id](params);
    if (op.response_content_type === 'text/event-stream') assert.deepEqual(await collect(result), [{}]);
    else {
      const response = await result;
      if (response instanceof Response) await response.arrayBuffer();
    }
    assert.equal(called, 1, op.id);
  }
});

test('query arrays repeat bracket keys; false and timestamp filters survive', async () => {
  const urls = [];
  const client = new Mango({ baseURL: 'http://localhost/prefix/', apiKey: 'test-only', fetch: async url => { urls.push(new URL(url)); return json({ data: [], next_page: null }); } });
  await client.listSessions({ 'statuses[]': ['idle', 'running'], include_archived: false, 'created_at[gte]': '2026-01-01T00:00:00Z', limit: 1 });
  assert.equal(urls[0].pathname, '/prefix/v1/sessions');
  assert.deepEqual(urls[0].searchParams.getAll('statuses[]'), ['idle', 'running']);
  assert.equal(urls[0].searchParams.get('include_archived'), 'false');
  assert.equal(urls[0].searchParams.get('created_at[gte]'), '2026-01-01T00:00:00Z');
  await client.listSessionEvents({ session_id: 'sesn_1', 'types[]': [] });
  assert.equal(urls[1].searchParams.has('types[]'), false);
});

test('JSON distinguishes omitted fields from explicit null, false, and empty lists', async () => {
  const bodies = [];
  const client = new Mango({ baseURL: 'http://localhost', apiKey: 'test-only', fetch: async (_, request) => { bodies.push(JSON.parse(request.body)); return json({}); } });
  await client.updateAgent({ agent_id: 'agent', body: { system: null, description: undefined, tools: [], metadata: {} } });
  assert.deepEqual(bodies[0], { system: null, tools: [], metadata: {} });
  await client.sendSessionEvents({ session_id: 'session', body: { events: [{ type: 'user.tool_result', tool_use_id: 'tool', is_error: false, content: [] }] } });
  assert.equal(bodies[1].events[0].is_error, false);
  await client.updateSession({ session_id: 'session', body: { budget: null } });
  assert.deepEqual(bodies[2], { budget: null });
});

test('typed errors preserve status/type/request id and writes are not retried', async () => {
  let calls = 0;
  const client = new Mango({ baseURL: 'http://localhost', apiKey: 'test-only', fetch: async () => {
    calls++;
    return json({ type: 'error', error: { type: 'conflict_error', message: 'Try later' }, request_id: 'req_body' }, { status: 409, headers: { 'request-id': 'req_header' } });
  } });
  await assert.rejects(client.createAgent({ body: { name: 'test', model: 'model' } }), error => {
    assert.ok(error instanceof APIError);
    assert.equal(error.message, 'Try later');
    assert.equal(error.status, 409);
    assert.equal(error.type, 'conflict_error');
    assert.equal(error.requestId, 'req_header');
    assert.ok(!String(error).includes('test-only'));
    return true;
  });
  assert.equal(calls, 1);
  const proxy = new Mango({ baseURL: 'http://localhost', apiKey: 'test-only', fetch: async () => new Response('upstream failed', { status: 502 }) });
  await assert.rejects(proxy.listAgents(), error => error instanceof APIError && error.type === 'http_error' && error.body === 'upstream failed');
});

test('error bodies stop at 1 MiB and cancel an unbounded upstream response', async () => {
  let cancelled = false;
  let pulls = 0;
  const client = new Mango({ baseURL: 'http://localhost', apiKey: 'test-only', fetch: async () => new Response(new ReadableStream({
    pull(controller) { pulls++; controller.enqueue(encoder.encode('x'.repeat(64 * 1024))); },
    cancel() { cancelled = true; },
  }), { status: 502 }) });
  await assert.rejects(client.listAgents(), error => {
    assert.ok(error instanceof APIError);
    assert.equal(error.status, 502);
    assert.equal(error.bodyTruncated, true);
    assert.equal(error.body.length, 1024 * 1024);
    return true;
  });
  assert.equal(cancelled, true);
  assert.ok(pulls <= 17, 'at most the bounded body and one stream prefetch chunk');
});

test('public operations work without a key; protected routes retain normal 401 errors', async () => {
  const client = new Mango({ baseURL: 'http://localhost', fetch: async (url, request) => {
    assert.equal(request.headers.has('authorization'), false);
    if (url.endsWith('/healthz') || url.endsWith('/readyz')) return new Response(null);
    if (url.endsWith('/openapi.yaml')) return new Response('openapi: 3.1.0');
    return json({ type: 'error', error: { type: 'authentication_error', message: 'A key is required' }, request_id: 'req_auth' }, { status: 401 });
  } });
  await client.health();
  await client.readiness();
  assert.equal(await client.openAPI(), 'openapi: 3.1.0');
  await assert.rejects(client.listAgents(), error => error instanceof APIError && error.status === 401);
});

test('multipart uploads preserve filenames, file bytes, and repeated skill file fields', async t => {
  const observed = [];
  const baseURL = await serverFor(t, async (request, response) => {
    try {
      const chunks = []; for await (const chunk of request) chunks.push(chunk);
      const body = await new Response(Buffer.concat(chunks), { headers: { 'content-type': request.headers['content-type'] } }).formData();
      observed.push(body);
      response.setHeader('content-type', 'application/json'); response.end('{}');
    } catch (error) { response.statusCode = 500; response.end(String(error)); }
  });
  const client = new Mango({ baseURL, apiKey: 'test-only' });
  await client.uploadFile({ body: { file: { data: new Blob(['a,b\n1,2'], { type: 'text/csv' }), filename: 'data.csv' } } });
  assert.equal(observed[0].get('file').name, 'data.csv');
  assert.equal(await observed[0].get('file').text(), 'a,b\n1,2');
  await client.createSkill({ body: { display_title: 'Review', files: [new File(['# Review'], 'review/SKILL.md'), new File(['x'], 'review/references/data.txt')] } });
  assert.equal(observed[1].get('display_title'), 'Review');
  assert.deepEqual(observed[1].getAll('files').map(file => file.name), ['review/SKILL.md', 'review/references/data.txt']);
});

test('downloads return streaming bodies and preserve content metadata', async () => {
  let cancelled = false;
  const source = new ReadableStream({ start(controller) { controller.enqueue(encoder.encode('first')); }, cancel() { cancelled = true; } });
  const client = new Mango({ baseURL: 'http://localhost', apiKey: 'test-only', fetch: async () => new Response(source, { headers: { 'content-type': 'application/zip', 'content-disposition': 'attachment; filename=skill.zip' } }) });
  const response = await client.downloadSkillVersion({ skill_id: 'skill_x', version: '1' });
  assert.equal(response.headers.get('content-type'), 'application/zip');
  const reader = response.body.getReader();
  assert.equal(new TextDecoder().decode((await reader.read()).value), 'first');
  await reader.cancel();
  assert.equal(cancelled, true);
});

test('SSE parses every UTF-8 byte boundary, CRLF, comments, multiline data, and metadata', async () => {
  const wire = encoder.encode(': ping\r\nevent: event_delta\r\nid: evt_1\r\nretry: 100\r\ndata: {"text":\r\ndata: "记忆"}\r\n\r\ndata: {"next":true}\n\ndata: {"incomplete":true}');
  const stream = bytes(...Array.from(wire, byte => new Uint8Array([byte])));
  const messages = await collect(parseSSE(stream));
  assert.deepEqual(messages, [
    { event: 'event_delta', id: 'evt_1', retry: 100, data: { text: '记忆' } },
    { event: 'message', id: 'evt_1', data: { next: true } },
  ]);
});

test('SSE accepts valid data larger than the former 16 MiB cap', async () => {
  const length = 17 * 1024 * 1024;
  const wire = encoder.encode(`data: ${JSON.stringify({ text: 'x'.repeat(length) })}\n\n`);
  const chunks = [];
  for (let offset = 0; offset < wire.length; offset += 64 * 1024) chunks.push(wire.subarray(offset, offset + 64 * 1024));
  const messages = await collect(parseSSE(bytes(...chunks)));
  assert.equal(messages.length, 1);
  assert.equal(messages[0].data.text.length, length);
});

test('eager stream is ready before input submission and closes without iteration', async t => {
  let subscribed = false;
  let streamResponse;
  const baseURL = await serverFor(t, (request, response) => {
    if (request.url.endsWith('/stream')) {
      subscribed = true;
      streamResponse = response;
      response.writeHead(200, { 'content-type': 'text/event-stream' });
      response.flushHeaders();
    } else if (request.method === 'POST') {
      assert.equal(subscribed, true);
      streamResponse.write('data: {"type":"agent.message","content":[]}\n\n');
      response.setHeader('content-type', 'application/json');
      response.end('{"data":[]}');
    }
  });
  const client = new Mango({ baseURL, apiKey: 'test-only' });
  const events = await client.openSessionEvents({ session_id: 'sesn_1' });
  assert.equal(subscribed, true);
  await client.sendSessionEvents({ session_id: 'sesn_1', body: { events: [{ type: 'user.message', content: [{ type: 'text', text: 'Hello' }] }] } });
  assert.equal((await events.next()).value.type, 'agent.message');
  await events.close();
  assert.equal((await events.next()).done, true);

  let cancelled = false;
  const fake = new Mango({ baseURL: 'http://localhost', fetch: async () => new Response(new ReadableStream({ cancel() { cancelled = true; } }), { headers: { 'content-type': 'text/event-stream' } }) });
  const unopened = await fake.openSessionThreadEvents({ session_id: 'sesn_1', thread_id: 'sthr_1' });
  await unopened.close();
  assert.equal(cancelled, true);
  assert.equal((await unopened.next()).done, true);
});

test('closing an eager stream unblocks a pending read with no next event', { timeout: 2000 }, async t => {
  const baseURL = await serverFor(t, (_, response) => {
    response.writeHead(200, { 'content-type': 'text/event-stream' });
    response.flushHeaders();
  });
  const client = new Mango({ baseURL });
  const events = await client.openSessionEvents({ session_id: 'sesn_1' });
  const next = events.next();
  await events.close();
  assert.equal((await next).done, true);
});

test('SSE stops cleanly when consumer breaks and rejects invalid frames/content types', async () => {
  let cancelled = false;
  const client = new Mango({ baseURL: 'http://localhost', apiKey: 'test-only', fetch: async (_, request) => {
    assert.equal(request.headers.has('last-event-id'), false);
    return new Response(new ReadableStream({ start(controller) { controller.enqueue(encoder.encode('data: {"type":"agent.message"}\n\n')); }, cancel() { cancelled = true; } }), { headers: { 'content-type': 'text/event-stream' } });
  } });
  for await (const frame of client.streamSessionEvents({ session_id: 'sesn_1' })) { assert.equal(frame.type, 'agent.message'); break; }
  assert.equal(cancelled, true);
  await assert.rejects(collect(parseSSE(bytes('data: not-json\n\n'))), ProtocolError);
  const wrong = new Mango({ baseURL: 'http://localhost', apiKey: 'test-only', fetch: async () => json({}) });
  await assert.rejects(collect(wrong.streamSessionEvents({ session_id: 'sesn_1' })), ProtocolError);
});

test('streaming outlives JSON deadline, caller cancellation remains active', async t => {
  const baseURL = await serverFor(t, (request, response) => {
    if (request.url.includes('/stream')) {
      response.writeHead(200, { 'content-type': 'text/event-stream' });
      response.flushHeaders();
      const timer = setTimeout(() => response.write('data: {"type":"agent.message"}\n\n'), 70);
      request.on('close', () => clearTimeout(timer));
    }
  });
  const client = new Mango({ baseURL, apiKey: 'test-only', timeoutMs: 40 });
  const controller = new AbortController();
  const stream = client.streamSessionEvents({ session_id: 'sesn_1' }, { signal: controller.signal });
  assert.equal((await stream.next()).value.type, 'agent.message');
  controller.abort();
  await assert.rejects(stream.next(), error => error.name === 'AbortError');
  await assert.rejects(client.getSession({ session_id: 'sesn_1' }), error => error.name === 'TimeoutError');
});

test('JSON deadline includes delayed body and already-aborted calls do not dispatch', async t => {
  let requests = 0;
  const baseURL = await serverFor(t, (request, response) => { requests++; response.writeHead(200, { 'content-type': 'application/json' }); response.flushHeaders(); });
  const client = new Mango({ baseURL, apiKey: 'test-only', timeoutMs: 30 });
  await assert.rejects(client.listAgents(), error => error.name === 'TimeoutError');
  const controller = new AbortController(); controller.abort();
  await assert.rejects(client.listAgents({}, { signal: controller.signal }), error => error.name === 'AbortError');
  await delay(10);
  assert.equal(requests, 1);
});

test('redirects never forward bearer credentials to another origin', async t => {
  let destinationCalls = 0;
  const destination = await serverFor(t, (_, response) => { destinationCalls++; response.end('{}'); });
  const baseURL = await serverFor(t, (_, response) => { response.writeHead(302, { location: destination }); response.end(); });
  const client = new Mango({ baseURL, apiKey: 'test-only' });
  await assert.rejects(client.listAgents(), error => error instanceof APIError && error.status === 302);
  assert.equal(destinationCalls, 0);
});

test('page and Files cursor helpers are lazy, preserve filters, and reject loops', async () => {
  const urls = [];
  const client = new Mango({ baseURL: 'http://localhost', apiKey: 'test-only', fetch: async url => {
    const parsed = new URL(url); urls.push(parsed);
    if (parsed.pathname.endsWith('/files')) return json(parsed.searchParams.has('after_id') ? { data: [{ id: 'file2' }], has_more: false, first_id: 'file2', last_id: 'file2' } : { data: [{ id: 'file1' }], has_more: true, first_id: 'file1', last_id: 'file1' });
    return json(parsed.searchParams.has('page') ? { data: [{ id: 'agent2' }], next_page: null } : { data: [{ id: 'agent1' }], next_page: 'opaque+/=' });
  } });
  const items = client.listAgentsItems({ limit: 1, include_archived: false });
  assert.equal(urls.length, 0);
  assert.deepEqual(await collect(items), [{ id: 'agent1' }, { id: 'agent2' }]);
  assert.equal(urls[1].searchParams.get('page'), 'opaque+/=');
  assert.equal(urls[1].searchParams.get('include_archived'), 'false');
  assert.deepEqual(await collect(client.listFilesItems({ scope_id: 'sesn_1' })), [{ id: 'file1' }, { id: 'file2' }]);
  assert.equal(urls[3].searchParams.get('after_id'), 'file1');
  assert.equal(urls[3].searchParams.get('scope_id'), 'sesn_1');
  const loop = new Mango({ baseURL: 'http://localhost', apiKey: 'test-only', fetch: async () => json({ data: [], next_page: 'same' }) });
  await assert.rejects(collect(loop.listAgentsPages()), ProtocolError);
  let startingCalls = 0;
  const startingLoop = new Mango({ baseURL: 'http://localhost', apiKey: 'test-only', fetch: async () => { startingCalls++; return json({ data: [], next_page: 'start' }); } });
  await assert.rejects(collect(startingLoop.listAgentsPages({ page: 'start' })), ProtocolError);
  assert.equal(startingCalls, 1);
});

test('Files before_id traversal uses first_id and stopping pages does not prefetch', async () => {
  const urls = [];
  const client = new Mango({ baseURL: 'http://localhost', apiKey: 'test-only', fetch: async url => {
    urls.push(new URL(url));
    return json(urls.length === 1
      ? { data: [{ id: 'file_b' }], has_more: true, first_id: 'file_b', last_id: 'file_b' }
      : { data: [{ id: 'file_a' }], has_more: false, first_id: 'file_a', last_id: 'file_a' });
  } });
  assert.deepEqual(await collect(client.listFilesItems({ before_id: 'file_c' })), [{ id: 'file_b' }, { id: 'file_a' }]);
  assert.equal(urls[1].searchParams.get('before_id'), 'file_b');
  assert.equal(urls[1].searchParams.has('after_id'), false);
  let requests = 0;
  const lazy = new Mango({ baseURL: 'http://localhost', apiKey: 'test-only', fetch: async () => { requests++; return json({ data: [], next_page: 'next' }); } });
  for await (const page of lazy.listAgentsPages()) { assert.deepEqual(page.data, []); break; }
  assert.equal(requests, 1);
});

test('download cancellation still works after the connection deadline', async t => {
  const baseURL = await serverFor(t, (request, response) => {
    response.writeHead(200, { 'content-type': 'application/octet-stream' });
    response.write('head');
  });
  const client = new Mango({ baseURL, apiKey: 'test-only', timeoutMs: 50 });
  const controller = new AbortController();
  const response = await client.downloadFile({ file_id: 'file_1' }, { signal: controller.signal });
  const reader = response.body.getReader();
  assert.equal(new TextDecoder().decode((await reader.read()).value), 'head');
  await delay(70);
  controller.abort();
  await assert.rejects(reader.read(), error => error.name === 'AbortError');
});

test('path dot segments and malformed base URLs cannot redirect resource access', async () => {
  for (const baseURL of ['file:///tmp/test', 'https://user:pass@example.com', 'https://example.com?key=1', 'https://example.com#frag']) assert.throws(() => new Mango({ baseURL, apiKey: 'test-only' }), TypeError);
  let calls = 0;
  const client = new Mango({ baseURL: 'http://localhost', apiKey: 'test-only', fetch: async () => { calls++; return json({}); } });
  await assert.rejects(client.getSession({ session_id: '..' }), TypeError);
  await assert.rejects(client.getSession({ session_id: '' }), TypeError);
  assert.equal(calls, 0);
});
