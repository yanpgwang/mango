// Standalone Mango application; does not run in the HTTP conformance harness.
import { Mango, type MultiagentRosterEntryInput } from 'mango-sdk';

const model = process.env.MANGO_MODEL_ID;
const environmentID = process.env.MANGO_ENVIRONMENT_ID;
if (!model || !environmentID || !process.env.MANGO_API_KEY) {
  throw new Error('Set MANGO_MODEL_ID, MANGO_ENVIRONMENT_ID and MANGO_API_KEY');
}
const client = new Mango({
  baseURL: process.env.MANGO_BASE_URL ?? 'http://localhost:8080',
  apiKey: process.env.MANGO_API_KEY,
});
const agentIDs: string[] = [];
let sessionID: string | undefined;
try {
  // #region team
  const researcher = await client.agents.create({
    name: 'researcher', model,
    system: 'Analyze the proposal and report concrete benefits and tradeoffs.',
  });
  agentIDs.push(researcher.id);
  const reviewer = await client.agents.create({
    name: 'reviewer', model,
    system: 'Review the proposal independently and identify risks and missing assumptions.',
  });
  agentIDs.push(reviewer.id);
  const roster: MultiagentRosterEntryInput[] = [
    { type: 'agent', id: researcher.id, version: researcher.version },
    { type: 'agent', id: reviewer.id, version: reviewer.version },
  ];
  if (process.env.MANGO_ADVISOR_MODEL) roster.push({ type: 'advisor', model: process.env.MANGO_ADVISOR_MODEL });
  const coordinator = await client.agents.create({
    name: 'lead', model,
    system: 'Delegate to both specialists, wait for their reports, then synthesize. For follow-ups, reuse the existing specialist threads.',
    multiagent: { type: 'coordinator', agents: roster },
  });
  agentIDs.push(coordinator.id);
  const session = await client.sessions.create({ agent: coordinator.id, environment_id: environmentID });
  sessionID = session.id;
  // #endregion team

  async function turn(text: string) {
    // #region observe
    const stream = await client.sessions.events.stream(session.id, {}, { signal: AbortSignal.timeout(180_000) });
    try {
      await client.sessions.events.send(session.id, { events: [{ type: 'user.message', content: [{ type: 'text', text }] }] });
      for await (const event of stream) {
        if (event.type === 'agent.message') console.log(event.content);
        if (event.type === 'session.status_idle') {
          if (event.stop_reason.type !== 'end_turn') throw new Error('Session needs attention; inspect its persisted events');
          return;
        }
        if (event.type === 'session.status_terminated' || event.type === 'session.deleted') throw new Error('Session terminated before completing the turn');
      }
      throw new Error('Stream disconnected; reconcile persisted events before resending');
    } finally { await stream.close(); }
    // #endregion observe
  }
  await turn(process.env.MANGO_TASK ?? 'Compare a monolith and microservices for a three-person team.');
  await turn('Ask the existing reviewer thread to challenge its earlier conclusion, then summarize.');
  // #region threads
  for await (const thread of client.sessions.threads.listItems(session.id)) {
    console.log(thread.id, thread.status);
    for await (const event of client.sessions.threads.events.listItems(session.id, thread.id)) {
      if (event.type === 'agent.message') console.log(event.content);
    }
  }
  // #endregion threads
} finally {
  try { if (sessionID) await client.sessions.delete(sessionID); }
  finally { for (const agentID of agentIDs.reverse()) await client.agents.archive(agentID); }
}
