import { Mango, type Agent, type AgentCreateRequest, type SessionEvent, type FileUploadRequest, type EventStream, type EventStreamFrame } from 'mango-sdk';

const client = new Mango({ baseURL: 'http://localhost', apiKey: 'test-only' });
const body: AgentCreateRequest = { name: 'analyst', model: 'model', system: null, tools: [] };
const agent: Promise<Agent> = client.agents.create({ ...body });
void agent;
const live: Promise<EventStream<EventStreamFrame>> = client.sessions.events.stream('session');
void live;
new Mango({ baseURL: 'http://localhost' }).system.health();
client.sessions.update('session', { budget: null, metadata: { remove: null } });
client.sessions.events.list('session', { types: ['agent.message', 'session.status_idle'] });
const upload: FileUploadRequest = { file: new Blob(['x']) };
client.files.upload({ ...upload });

// @ts-expect-error Agent creation requires a model.
client.agents.create({ name: 'no-model' });
// @ts-expect-error A Session ID is required.
client.sessions.retrieve();
// @ts-expect-error Creation name is not nullable.
client.agents.create({ name: null, model: 'model' });
// @ts-expect-error Unknown Session event discriminants must not typecheck.
client.sessions.events.send('session', { events: [{ type: 'unknown.event' }] });
// @ts-expect-error JSON binary strings are not multipart uploads.
client.files.upload({ file: 'not-a-blob' });
// @ts-expect-error Nullable response fields remain nullable.
const date: string = ({} as Agent).archived_at;
void date;
function eventContent(event: SessionEvent) {
  if (event.type === 'agent.message') return event.content[0]?.text;
  return undefined;
}
void eventContent;
