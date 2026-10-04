// For the repository's isolated real-HTTP conformance harness, not a live model demo.
import assert from 'node:assert/strict';
import { Mango, APIError } from '../dist/index.js';

const baseURL = process.env.MANGO_SDK_TEST_URL;
const apiKey = process.env.MANGO_SDK_TEST_KEY;
if (!baseURL || !apiKey) throw new Error('MANGO_SDK_TEST_URL and MANGO_SDK_TEST_KEY are required');
const client = new Mango({ baseURL, apiKey, timeoutMs: 10_000 });
const agents = [];
const skillInput = { type: 'custom', skill_id: 'skill_reports', version: 'latest' };
const resolvedSkill = { type: 'custom', skill_id: 'skill_reports', version: '1759178010641129' };
const skillConfig = { skills: [skillInput], tools: [{ type: 'agent_toolset_20260401', default_config: { enabled: false }, configs: [{ name: 'read', enabled: true, permission_policy: { type: 'auto' } }] }] };
let environment;
let session;
try {
  await client.system.health();
  const mcpPage = await client.sessions.events.list("sesn_mcp_fixture");
  assert.equal(mcpPage.data[0].type, "agent.mcp_tool_result");
  assert.equal(mcpPage.data[0].file_id, "file_mcp_full");
  const autoPage = await client.sessions.events.list("sesn_auto_fixture");
  assert.equal(autoPage.data.length, 6);
  for (const [index, event] of autoPage.data.entries()) {
    assert.equal(event.type, index < 3 ? "agent.tool_use" : "agent.mcp_tool_use");
    const expected = ["allow", "ask", "deny"][index % 3];
    assert.equal(event.evaluated_permission, expected);
    assert.equal(event.evaluation.type, "auto");
    assert.equal(event.evaluation.evaluated_permission.type, expected);
    if (expected !== "allow") assert.equal(event.evaluation.evaluated_permission.reason_code, expected === "ask" ? "indeterminate" : "high_risk");
  }
  const payload = new Uint8Array([109, 97, 110, 103, 111, 0, 255]);
  const uploaded = await client.files.upload({ file: new File([payload], 'result.bin', { type: 'application/octet-stream' }) });
  try {
    assert.deepEqual(Object.keys(uploaded).sort(), ['checksum_sha256', 'created_at', 'filename', 'id', 'mime_type', 'size_bytes', 'type']);
    assert.equal(uploaded.size_bytes, payload.length);
    const response = await client.files.download(uploaded.id);
    assert.deepEqual(new Uint8Array(await response.arrayBuffer()), payload);
  } finally { await client.files.delete(uploaded.id); }
  environment = await client.environments.create({ name: 'TypeScript conformance', config: { type: 'self_hosted' } });
  const check = await client.environments.work.create(environment.id, { data: { type: 'healthcheck' } });
  assert.deepEqual(check.data, { type: 'healthcheck' });
  assert.ok(check.expires_at);
  assert.equal(check.result, null);
  const done = await client.environments.work.complete(environment.id, check.id, { status: 'failed', message: 'conformance diagnostic' });
  assert.deepEqual(done.result, { status: 'failed', message: 'conformance diagnostic' });
  const checks = await client.environments.work.list(environment.id);
  assert.deepEqual(checks.data[0].data, { type: 'healthcheck' });
  for (let index = 0; index < 2; index++) agents.push(await client.agents.create({ name: `TypeScript conformance ${index}`, model: 'sdk-conformance', ...skillConfig }));
  assert.equal((await client.agents.retrieve(agents[0].id)).id, agents[0].id);
  assert.deepEqual(agents[0].tools[0].configs[0].permission_policy, { type: "auto" });
  for (const agent of agents) {
    assert.deepEqual(agent.skills, [resolvedSkill]);
    assert.deepEqual((await client.agents.retrieve(agent.id)).skills, [resolvedSkill]);
  }

  const listed = [];
  for await (const item of client.agents.listItems({ limit: 1 })) listed.push(item.id);
  for (const agent of agents) assert.ok(listed.includes(agent.id));
  const coordinator = await client.agents.create({
    name: 'TypeScript lead', model: 'sdk-conformance', ...skillConfig,
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
  assert.deepEqual(session.agent.skills, [resolvedSkill]);
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
