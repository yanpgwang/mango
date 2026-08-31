// Used directly by the documentation. Run against the offline Compose stack.
// #region client
import { Mango } from '@mango-agents/sdk';

const client = new Mango({
  baseURL: process.env.MANGO_BASE_URL ?? 'http://localhost:8080',
  apiKey: process.env.MANGO_API_KEY!,
});
// #endregion client

if (!process.env.MANGO_API_KEY) throw new Error('Set MANGO_API_KEY before running this example');
let environmentID: string | undefined;
let agentID: string | undefined;
let sessionID: string | undefined;
try {
  // #region environment
  const environment = await client.createEnvironment({
    body: { name: 'Quickstart', config: { type: 'cloud' } },
  });
  // #endregion environment
  environmentID = environment.id;

  // #region agent
  const agent = await client.createAgent({
    body: { name: 'Assistant', model: 'offline-fake', system: 'Be concise.' },
  });
  // #endregion agent
  agentID = agent.id;

  // #region session
  const session = await client.createSession({
    body: { agent: agent.id, environment_id: environment.id, title: 'First session' },
  });
  // #endregion session
  sessionID = session.id;

  // #region stream
  // Subscribe before sending: the stream does not replay earlier events.
  const stream = await client.openSessionEvents(
    { session_id: session.id },
    { signal: AbortSignal.timeout(60_000) },
  );
  let completed = false;
  try {
    await client.sendSessionEvents({
      session_id: session.id,
      body: { events: [{ type: 'user.message', content: [{ type: 'text', text: 'Hello, Mango!' }] }] },
    });
    for await (const event of stream) {
      if (event.type === 'agent.message') console.log(event.content);
      if (event.type === 'session.status_idle') {
        if (event.stop_reason.type !== 'end_turn') throw new Error('The turn requires attention');
        completed = true;
        break;
      }
    }
    if (!completed) throw new Error('Stream ended before completion; reconcile persisted history');
  } finally {
    await stream.close();
  }
  // #endregion stream

  // #region history
  const history = [];
  for await (const event of client.listSessionEventsItems({
    session_id: session.id, order: 'asc', limit: 100,
  })) history.push(event);
  console.log(`Persisted events: ${history.length}`);
  // #endregion history
  if (!history.some(event => event.type === 'agent.message')) throw new Error('Missing persisted response');
  console.log('Quickstart completed');
} finally {
  // These are only the resources created by this invocation. Do not delete
  // unrelated sessions or reset the development database to clean up examples.
  try {
    if (sessionID) await client.deleteSession({ session_id: sessionID });
  } finally {
    try {
      if (agentID) await client.archiveAgent({ agent_id: agentID });
    } finally {
      if (environmentID) await client.deleteEnvironment({ environment_id: environmentID });
    }
  }
}
