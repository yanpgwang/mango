/** Native Blob or File, or a Blob with an explicit multipart filename. */
export type BinaryInput = Blob | { data: Blob; filename: string };

export interface ClientOptions {
  /** Server origin, optionally with a reverse-proxy prefix. Do not include /v1. */
  baseURL: string;
  /** Omit for public health/OpenAPI endpoints; protected routes then return 401. */
  apiKey?: string;
  /** JSON request deadline, including response body consumption. Default: 60s. Zero disables it. */
  timeoutMs?: number;
  /** Injectable fetch implementation for testing or runtime customization. */
  fetch?: typeof globalThis.fetch;
  headers?: HeadersInit;
}

export interface RequestOptions {
  signal?: AbortSignal;
  /** JSON deadline; for streaming operations this bounds connection setup only. */
  timeoutMs?: number;
  headers?: HeadersInit;
}

export class APIError extends Error {
  readonly status: number;
  readonly type: string;
  readonly requestId: string | undefined;
  /** The original API response; may be text for an upstream proxy error. */
  readonly body: unknown;
  /** Error response bodies are capped at 1 MiB. */
  readonly bodyTruncated: boolean;
  readonly headers: Headers;

  constructor(response: Response, body: unknown, bodyTruncated = false) {
    const envelope = isRecord(body) ? body : {};
    const error = isRecord(envelope.error) ? envelope.error : {};
    super(typeof error.message === 'string' ? error.message : `Mango API returned HTTP ${response.status}`);
    this.name = 'APIError';
    this.status = response.status;
    this.type = typeof error.type === 'string' ? error.type : 'http_error';
    this.requestId = response.headers.get('request-id') ??
      (typeof envelope.request_id === 'string' ? envelope.request_id : undefined);
    this.body = body;
    this.bodyTruncated = bodyTruncated;
    this.headers = response.headers;
  }
}

export class ProtocolError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = 'ProtocolError';
  }
}

/** JSON data and SSE metadata. These streams are live-only, not replay cursors. */
export interface SSEMessage<T> {
  event: string;
  data: T;
  id?: string;
  retry?: number;
}

/** An already-subscribed, single-consumer live stream. Close when no longer needed. */
export interface EventStream<T> extends AsyncIterableIterator<T> {
  close(): Promise<void>;
}

export interface Operation {
  readonly method: string;
  readonly path: string;
  readonly parameters: ReadonlyArray<{ readonly name: string; readonly in: string; readonly required: boolean }>;
  readonly body?: 'json' | 'multipart';
  readonly bodyRequired?: boolean;
  readonly response: 'json' | 'binary' | 'sse' | 'text' | 'empty';
  readonly public?: boolean;
  readonly pagination?: 'page' | 'files';
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

async function readErrorBody(response: Response): Promise<{ text: string; truncated: boolean }> {
  if (!response.body) return { text: '', truncated: false };
  const limit = 1024 * 1024;
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let length = 0;
  let text = '';
  let truncated = false;
  try {
    while (length < limit) {
      const chunk = await reader.read();
      if (chunk.done) break;
      const remaining = limit - length;
      const part = chunk.value.subarray(0, remaining);
      length += part.byteLength;
      text += decoder.decode(part, { stream: true });
      if (chunk.value.byteLength >= remaining) {
        // Cancel at the bound, including an exactly-sized body: reading another
        // chunk solely to prove EOF would permit a stalled error response.
        truncated = true;
        await reader.cancel();
        break;
      }
    }
    return { text: text + decoder.decode(), truncated };
  } finally { reader.releaseLock(); }
}

function multipart(body: unknown): FormData {
  if (!isRecord(body)) throw new TypeError('Multipart body must be an object');
  const form = new FormData();
  for (const [name, value] of Object.entries(body)) {
    if (value === undefined) continue;
    for (const part of Array.isArray(value) ? value : [value]) {
      if (part instanceof Blob) {
        form.append(name, part);
      } else if (isRecord(part) && part.data instanceof Blob && typeof part.filename === 'string') {
        form.append(name, part.data, part.filename);
      } else if (typeof part === 'string') {
        form.append(name, part);
      } else {
        throw new TypeError(`Invalid multipart value for ${name}: expected Blob, named Blob, or string`);
      }
    }
  }
  return form;
}

function deadline(signal: AbortSignal | undefined, timeoutMs: number) {
  if (!Number.isFinite(timeoutMs) || timeoutMs < 0) throw new TypeError('timeoutMs must be a finite non-negative number');
  const controller = new AbortController();
  const abort = () => controller.abort(signal?.reason);
  if (signal?.aborted) abort();
  else signal?.addEventListener('abort', abort, { once: true });
  const timer = timeoutMs > 0 ? setTimeout(() => {
    controller.abort(new DOMException('Mango request timed out', 'TimeoutError'));
  }, timeoutMs) : undefined;
  return {
    signal: controller.signal,
    abort(reason?: unknown) { controller.abort(reason); },
    clearTimer() { if (timer !== undefined) clearTimeout(timer); },
    dispose() {
      if (timer !== undefined) clearTimeout(timer);
      signal?.removeEventListener('abort', abort);
    },
  };
}

/**
 * Streaming SSE parser: handles arbitrary UTF-8 chunks, CR/LF, comments, multiline
 * data, and metadata. Incomplete events at EOF are discarded per the SSE format.
 * Closing the iterator cancels the reader and releases the HTTP connection.
 */
export async function* parseSSE<T>(body: ReadableStream<Uint8Array>): AsyncGenerator<SSEMessage<T>> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  const encoder = new TextEncoder();
  let pending = '';
  let scanned = 0;
  let data: string[] = [];
  let event = '';
  let id: string | undefined;
  let retry: number | undefined;
  let skipLF = false;
  let totalData = 0;
  const maxFrameBytes = 64 * 1024 * 1024;

  function line(text: string): SSEMessage<T> | undefined {
    if (text === '') {
      if (data.length === 0) { event = ''; retry = undefined; return; }
      let parsed: T;
      try { parsed = JSON.parse(data.join('\n')) as T; }
      catch (cause) { throw new ProtocolError('SSE frame contains invalid JSON', { cause }); }
      const message: SSEMessage<T> = { event: event || 'message', data: parsed };
      if (id !== undefined) message.id = id;
      if (retry !== undefined) message.retry = retry;
      data = []; event = ''; retry = undefined; totalData = 0;
      return message;
    }
    if (text.startsWith(':')) return;
    const split = text.indexOf(':');
    const field = split < 0 ? text : text.slice(0, split);
    let value = split < 0 ? '' : text.slice(split + 1);
    if (value.startsWith(' ')) value = value.slice(1);
    if (field === 'data') {
      totalData += encoder.encode(value).byteLength + (data.length ? 1 : 0);
      if (totalData > maxFrameBytes) throw new ProtocolError('SSE frame exceeds 64 MiB');
      data.push(value);
    } else if (field === 'event') event = value;
    else if (field === 'id' && !value.includes('\0')) id = value;
    else if (field === 'retry' && /^\d+$/.test(value)) retry = Number(value);
  }

  try {
    while (true) {
      const chunk = await reader.read();
      if (chunk.done) break;
      pending += decoder.decode(chunk.value, { stream: true });
      let start = 0;
      // Resume after the previously inspected partial line. Large persisted
      // events arrive in many chunks and must not rescan their entire prefix.
      for (let i = scanned; i < pending.length; i++) {
        const char = pending[i];
        if (skipLF) {
          skipLF = false;
          if (char === '\n') { start = i + 1; continue; }
        }
        if (char !== '\r' && char !== '\n') continue;
        const message = line(pending.slice(start, i));
        start = i + 1;
        skipLF = char === '\r';
        if (message) yield message;
      }
      pending = pending.slice(start);
      scanned = pending.length;
      if (pending.length > maxFrameBytes) throw new ProtocolError('SSE line exceeds 64 MiB');
    }
  } finally {
    try { await reader.cancel(); } finally { reader.releaseLock(); }
  }
}

export class Transport {
  private readonly baseURL: string;
  private readonly apiKey: string | undefined;
  private readonly timeoutMs: number;
  private readonly fetcher: typeof globalThis.fetch;
  private readonly headers: Headers;

  constructor(options: ClientOptions) {
    const url = new URL(options.baseURL);
    if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) {
      throw new TypeError('baseURL must be an HTTP(S) URL without credentials, query, or fragment');
    }
    if (options.apiKey !== undefined && typeof options.apiKey !== 'string') throw new TypeError('apiKey must be a string');
    this.baseURL = url.href.replace(/\/+$/, '');
    this.apiKey = options.apiKey;
    this.timeoutMs = options.timeoutMs ?? 60_000;
    this.fetcher = options.fetch ?? globalThis.fetch;
    this.headers = new Headers(options.headers);
  }

  private async open(operation: Operation, params: Record<string, unknown>, options: RequestOptions, streaming: boolean) {
    let path = operation.path;
    const query = new URLSearchParams();
    const headers = new Headers(this.headers);
    new Headers(options.headers).forEach((value, key) => headers.set(key, value));
    headers.set('Accept', operation.response === 'sse' ? 'text/event-stream' : operation.response === 'binary' ? '*/*' : 'application/json');
    // Authentication is controlled only by the client key, never caller-supplied headers.
    headers.delete('Authorization');
    if (!operation.public && this.apiKey) headers.set('Authorization', `Bearer ${this.apiKey}`);
    for (const parameter of operation.parameters) {
      const value = params[parameter.name];
      if (value === undefined) {
        if (parameter.required) throw new TypeError(`Missing required parameter ${parameter.name}`);
        continue;
      }
      if (value === null) throw new TypeError(`Parameter ${parameter.name} cannot be null`);
      if (parameter.in === 'path') {
        const stringValue = String(value);
        // URL parsers normalize dot segments even when percent encoded.
        if (stringValue === '.' || stringValue === '..' || stringValue === '') throw new TypeError(`Invalid path parameter ${parameter.name}`);
        path = path.replace(`{${parameter.name}}`, encodeURIComponent(stringValue));
      } else if (parameter.in === 'query') {
        for (const item of Array.isArray(value) ? value : [value]) query.append(parameter.name, String(item));
      } else if (parameter.in === 'header') headers.set(parameter.name, String(value));
    }
    if (operation.bodyRequired && params.body === undefined) throw new TypeError('Request body is required');
    let body: BodyInit | undefined;
    if (params.body !== undefined) {
      if (operation.body === 'multipart') {
        headers.delete('Content-Type'); // fetch must add its own boundary.
        body = multipart(params.body);
      } else if (operation.body === 'json') {
        headers.set('Content-Type', 'application/json');
        body = JSON.stringify(params.body);
      }
    }
    const url = this.baseURL + path + (query.size ? `?${query}` : '');
    const lease = deadline(options.signal, options.timeoutMs ?? this.timeoutMs);
    try {
      const request: RequestInit = { method: operation.method, headers, redirect: 'manual', signal: lease.signal };
      if (body !== undefined) request.body = body;
      const response = await this.fetcher(url, request);
      if (!response.ok) {
        const body = await readErrorBody(response);
        let error: unknown = body.text;
        try { error = JSON.parse(body.text); } catch { /* Proxy responses need not be JSON. */ }
        throw new APIError(response, error, body.truncated);
      }
      // Leave caller cancellation connected throughout stream consumption, but do
      // not kill a healthy long-running stream at the normal JSON request deadline.
      if (streaming) lease.clearTimer();
      return { response, lease };
    } catch (error) { lease.dispose(); throw error; }
  }

  protected async request<T>(operation: Operation, params: object, options: RequestOptions = {}): Promise<T> {
    const { response, lease } = await this.open(operation, params as Record<string, unknown>, options, false);
    try {
      if (response.status === 204 || operation.response === 'empty') {
        await response.body?.cancel();
        return undefined as T;
      }
      if (operation.response === 'text') return await response.text() as T;
      try { return await response.json() as T; }
      catch (cause) {
        if (lease.signal.aborted) throw lease.signal.reason;
        throw new ProtocolError('Mango API returned invalid JSON', { cause });
      }
    } finally { lease.dispose(); }
  }

  protected async download(operation: Operation, params: object, options: RequestOptions = {}): Promise<Response> {
    const { response, lease } = await this.open(operation, params as Record<string, unknown>, options, true);
    if (!response.body) { lease.dispose(); return response; }
    const reader = response.body.getReader();
    // Tie listener cleanup to body consumption/cancellation rather than headers.
    const body = new ReadableStream<Uint8Array>({
      async pull(controller) {
        try {
          const item = await reader.read();
          if (item.done) { lease.dispose(); reader.releaseLock(); controller.close(); }
          else controller.enqueue(item.value);
        } catch (error) { lease.dispose(); reader.releaseLock(); controller.error(error); }
      },
      async cancel(reason) { try { await reader.cancel(reason); } finally { lease.dispose(); reader.releaseLock(); } },
    });
    return new Response(body, { status: response.status, statusText: response.statusText, headers: response.headers });
  }

  private async readyStream<T, Output>(operation: Operation, params: object, options: RequestOptions, select: (message: SSEMessage<T>) => Output): Promise<EventStream<Output>> {
    const { response, lease } = await this.open(operation, params as Record<string, unknown>, options, true);
    try {
      if (!response.headers.get('content-type')?.toLowerCase().includes('text/event-stream')) {
        await response.body?.cancel();
        throw new ProtocolError('Expected a text/event-stream response');
      }
      if (!response.body) throw new ProtocolError('SSE response has no body');
    } catch (error) { lease.dispose(); throw error; }
    const body = response.body;
    const parser = parseSSE<T>(body);
    let ended = false;
    let closing: Promise<void> | undefined;
    const done: IteratorReturnResult<undefined> = { done: true, value: undefined };
    const close = (): Promise<void> => {
      if (closing) return closing;
      if (ended) return Promise.resolve();
      ended = true;
      // Abort first so a parser blocked in reader.read() does not make return()
      // wait indefinitely for another event. This also handles close-before-next.
      lease.abort(new DOMException('Mango event stream closed', 'AbortError'));
      closing = (async () => {
        try {
          if (body.locked) await parser.return(undefined);
          else await body.cancel();
        } catch (error) {
          if (!lease.signal.aborted) throw error;
        } finally { lease.dispose(); }
      })();
      return closing;
    };
    const result: EventStream<Output> = {
      [Symbol.asyncIterator]() { return this; },
      async next() {
        if (ended) return done;
        try {
          const item = await parser.next();
          if (item.done) { ended = true; lease.dispose(); return done; }
          return { done: false, value: select(item.value) };
        } catch (error) {
          const wasClosed = ended;
          ended = true;
          lease.dispose();
          if (wasClosed) return done;
          throw error;
        }
      },
      async return() { await close(); return done; },
      close,
    };
    return result;
  }

  protected openFrames<T>(operation: Operation, params: object, options: RequestOptions = {}): Promise<EventStream<T>> {
    return this.readyStream<T, T>(operation, params, options, message => message.data);
  }

  protected async *stream<T>(operation: Operation, params: object, options: RequestOptions = {}): AsyncGenerator<SSEMessage<T>> {
    yield* await this.readyStream<T, SSEMessage<T>>(operation, params, options, message => message);
  }

  protected async *frames<T>(operation: Operation, params: object, options: RequestOptions = {}): AsyncGenerator<T> {
    for await (const message of this.stream<T>(operation, params, options)) yield message.data;
  }

  protected async *pages<T>(operation: Operation, params: object, options: RequestOptions = {}): AsyncGenerator<T> {
    const next: Record<string, unknown> = { ...params };
    const seen = new Set<string>();
    const startingCursor = operation.pagination === 'files' ? (next.before_id ?? next.after_id) : next.page;
    if (typeof startingCursor === 'string') seen.add(startingCursor);
    while (true) {
      const page = await this.request<T>(operation, next, options);
      if (!isRecord(page)) throw new ProtocolError('Pagination response is not an object');
      if (!Array.isArray(page.data)) throw new ProtocolError('Pagination response has no data array');
      yield page;
      if (operation.pagination === 'files') {
        if (!page.has_more) return;
        const backwards = next.before_id !== undefined;
        const cursor = backwards ? page.first_id : page.last_id;
        if (typeof cursor !== 'string' || cursor === '' || seen.has(cursor)) throw new ProtocolError('Files pagination did not advance');
        seen.add(cursor);
        delete next[backwards ? 'after_id' : 'before_id'];
        next[backwards ? 'before_id' : 'after_id'] = cursor;
      } else {
        const cursor = page.next_page;
        if (cursor === null || cursor === undefined) return;
        if (typeof cursor !== 'string' || cursor === '' || seen.has(cursor)) throw new ProtocolError('Pagination did not advance');
        seen.add(cursor);
        next.page = cursor;
      }
    }
  }
}
