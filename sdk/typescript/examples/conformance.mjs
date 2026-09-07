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
  await client.system.health();
  environment = await client.environments.create({ name: 'TypeScript conformance', config: { type: 'cloud' } });
  for (let index = 0; index < 2; index++) agents.push(await client.agents.create({ name: `TypeScript conformance ${index}`, model: 'sdk-conformance' }));
  assert.equal((await client.agents.retrieve(agents[0].id)).id, agents[0].id);
  const listed = [];
  for await (const item of client.agents.listItems({ limit: 1 })) listed.push(item.id);
  for (const agent of agents) assert.ok(listed.includes(agent.id));
  const coordinator = await client.agents.create({
    name: 'TypeScript lead', model: 'sdk-conformance',
    multiagent: { type: 'coordinator', agents: [
      { type: 'agent', id: agents[0].id, version: 1 }, agents[1].id,
      { type: 'self' }, { type: 'advisor', model: 'review-model' },
    ] },
  });
  agents.push(coordinator);
  assert.deepEqual(coordinator.multiagent.agents[0], { type: 'agent', id: agents[0].id, version: 1 });
  assert.deepEqual(coordinator.multiagent.agents.at(-1), { type: 'advisor', model: 'review-model' });
  session = await client.sessions.create({ agent: coordinator.id, environment_id: environment.id, title: 'TypeScript conformance' });
  assert.equal((await client.sessions.retrieve(session.id)).id, session.id);
  const live = await client.sessions.events.stream(session.id, {}, { signal: AbortSignal.timeout(10_000) });
  let batch;
  try {
    batch = await client.sessions.events.send(session.id, { events: [{ type: 'user.message', content: [{ type: 'text', text: 'SDK conformance' }] }] });
    assert.equal(batch.data.length, 1);
    let received = false;
    for await (const event of live) {
      if (event.id === batch.data[0].id) { received = true; break; }
    }
    assert.equal(received, true, 'ready live subscription must receive submitted input');
  } finally { await live.close(); }
  const events = [];
  for await (const event of client.sessions.events.listItems(session.id, { limit: 1 })) events.push(event);
  assert.ok(events.some(event => event.id === batch.data[0].id));
  await assert.rejects(client.sessions.retrieve('sesn_sdk_missing'), error => error instanceof APIError && error.status === 404 && error.type === 'not_found_error' && !!error.requestId);
} finally {
  // Cleanup failures must fail conformance rather than silently leaving resources.
  if (session) await client.sessions.delete(session.id);
  if (environment) await client.environments.delete(environment.id);
  for (const agent of agents) await client.agents.archive(agent.id);
}
console.log('TypeScript SDK real-HTTP conformance passed (test fakes, no model call)');
