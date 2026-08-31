// For the repository's isolated real-HTTP conformance harness, not a live model demo.
import assert from 'node:assert/strict';
import { Mango, APIError } from '../dist/index.js';

const baseURL = process.env.MANGO_SDK_TEST_URL;
const apiKey = process.env.MANGO_SDK_TEST_KEY;
if (!baseURL || !apiKey) throw new Error('MANGO_SDK_TEST_URL and MANGO_SDK_TEST_KEY are required');
const client = new Mango({ baseURL, apiKey, timeoutMs: 10_000 });
const agents = [];
let environment;
let session;
try {
  await client.health();
  environment = await client.createEnvironment({ body: { name: 'TypeScript conformance', config: { type: 'cloud' } } });
  for (let index = 0; index < 2; index++) agents.push(await client.createAgent({ body: { name: `TypeScript conformance ${index}`, model: 'sdk-conformance' } }));
  assert.equal((await client.getAgent({ agent_id: agents[0].id })).id, agents[0].id);
  const listed = [];
  for await (const item of client.listAgentsItems({ limit: 1 })) listed.push(item.id);
  for (const agent of agents) assert.ok(listed.includes(agent.id));
  session = await client.createSession({ body: { agent: agents[0].id, environment_id: environment.id, title: 'TypeScript conformance' } });
  assert.equal((await client.getSession({ session_id: session.id })).id, session.id);
  const live = await client.openSessionEvents({ session_id: session.id }, { signal: AbortSignal.timeout(10_000) });
  let batch;
  try {
    batch = await client.sendSessionEvents({ session_id: session.id, body: { events: [{ type: 'user.message', content: [{ type: 'text', text: 'SDK conformance' }] }] } });
    assert.equal(batch.data.length, 1);
    let received = false;
    for await (const event of live) {
      if (event.id === batch.data[0].id) { received = true; break; }
    }
    assert.equal(received, true, 'ready live subscription must receive submitted input');
  } finally { await live.close(); }
  const events = [];
  for await (const event of client.listSessionEventsItems({ session_id: session.id, limit: 1 })) events.push(event);
  assert.ok(events.some(event => event.id === batch.data[0].id));
  await assert.rejects(client.getSession({ session_id: 'sesn_sdk_missing' }), error => error instanceof APIError && error.status === 404 && error.type === 'not_found_error' && !!error.requestId);
} finally {
  // Cleanup failures must fail conformance rather than silently leaving resources.
  if (session) await client.deleteSession({ session_id: session.id });
  if (environment) await client.deleteEnvironment({ environment_id: environment.id });
  for (const agent of agents) await client.archiveAgent({ agent_id: agent.id });
}
console.log('TypeScript SDK real-HTTP conformance passed (test fakes, no model call)');
