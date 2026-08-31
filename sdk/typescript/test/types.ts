import { Mango, type Agent, type AgentCreateRequest, type SessionEvent, type FileUploadRequest, type EventStream, type EventStreamFrame } from '../src/index.js';

const client = new Mango({ baseURL: 'http://localhost', apiKey: 'test-only' });
const body: AgentCreateRequest = { name: 'analyst', model: 'model', system: null, tools: [] };
const agent: Promise<Agent> = client.createAgent({ body });
void agent;
const live: Promise<EventStream<EventStreamFrame>> = client.openSessionEvents({ session_id: 'session' });
void live;
new Mango({ baseURL: 'http://localhost' }).health();
client.updateSession({ session_id: 'session', body: { budget: null, metadata: { remove: null } } });
client.listSessionEvents({ session_id: 'session', 'types[]': ['agent.message', 'session.status_idle'] });
const upload: FileUploadRequest = { file: new Blob(['x']) };
client.uploadFile({ body: upload });

// @ts-expect-error Agent creation requires a model.
client.createAgent({ body: { name: 'no-model' } });
// @ts-expect-error A Session ID is required.
client.getSession({});
// @ts-expect-error Creation name is not nullable.
client.createAgent({ body: { name: null, model: 'model' } });
// @ts-expect-error Unknown Session event discriminants must not typecheck.
client.sendSessionEvents({ session_id: 'session', body: { events: [{ type: 'unknown.event' }] } });
// @ts-expect-error JSON binary strings are not multipart uploads.
client.uploadFile({ body: { file: 'not-a-blob' } });
// @ts-expect-error Nullable response fields remain nullable.
const date: string = ({} as Agent).archived_at;
void date;
function eventContent(event: SessionEvent) {
  if (event.type === 'agent.message') return event.content[0]?.text;
  return undefined;
}
void eventContent;
