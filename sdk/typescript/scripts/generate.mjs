import { readFile, writeFile, mkdir } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { createHash } from 'node:crypto';

// The shared snapshot is exported from Mango's own OpenAPI. This generator does
// not import server internals or any hosted-provider SDK.
const root = new URL('../../', import.meta.url);
const specBytes = await readFile(new URL('openapi.json', root), 'utf8');
const spec = JSON.parse(specBytes);
const manifest = JSON.parse(await readFile(new URL('operations.json', root), 'utf8'));
const outputURL = new URL('../src/generated.ts', import.meta.url);
const schemas = spec.components.schemas;
const operations = [...manifest.operations].sort((a, b) => a.id.localeCompare(b.id, 'en'));
const knownOperations = new Map();
for (const item of Object.values(spec.paths)) {
  for (const op of Object.values(item)) if (op?.operationId) knownOperations.set(op.operationId, op);
}
if (knownOperations.size !== operations.length || operations.some(op => !knownOperations.has(op.id))) {
  throw new Error('Operation manifest does not match OpenAPI; run go run ./scripts/sdk-contract');
}

const quote = JSON.stringify;
const pascal = name => name[0].toUpperCase() + name.slice(1);
const comment = text => text ? `/** ${text.replaceAll('*/', '* /').replaceAll('\n', '\n * ')} */\n` : '';

function deref(schema) {
  if (!schema?.$ref) return schema;
  if (!schema.$ref.startsWith('#/components/schemas/')) throw new Error(`Unsupported schema reference ${schema.$ref}`);
  const name = schema.$ref.split('/').at(-1);
  if (!(name in schemas)) throw new Error(`Unknown schema ${name}`);
  return schemas[name];
}

function type(schema) {
  if (schema === true || schema === undefined || schema === null) return 'unknown';
  if (schema === false) return 'never';
  if (schema.$ref) {
    deref(schema); // fail fast on stale references
    return schema.$ref.split('/').at(-1);
  }
  if (Object.hasOwn(schema, 'const')) return quote(schema.const);
  if (schema.enum) return schema.enum.map(quote).join(' | ') || 'never';
  if (schema.oneOf || schema.anyOf) return `(${(schema.oneOf ?? schema.anyOf).map(type).join(' | ')})`;
  if (schema.allOf) return `(${schema.allOf.map(type).join(' & ')})`;
  if (Array.isArray(schema.type)) return schema.type.map(kind => type({ ...schema, type: kind })).join(' | ');
  if (schema.format === 'binary') return 'BinaryInput';
  switch (schema.type) {
    case 'null': return 'null';
    case 'string': return 'string';
    case 'integer':
    case 'number': return 'number';
    case 'boolean': return 'boolean';
    case 'array': return `Array<${type(schema.items)}>`;
    case 'object': return objectType(schema);
    case undefined: return schema.properties || schema.additionalProperties ? objectType(schema) : 'unknown';
    default: throw new Error(`Unsupported schema type ${schema.type}`);
  }
}

function objectType(schema) {
  if (schema.maxProperties === 0) return 'Record<string, never>';
  const fields = Object.entries(schema.properties ?? {}).sort(([a], [b]) => a.localeCompare(b, 'en'));
  const required = new Set(schema.required ?? []);
  const parts = fields.map(([name, field]) => {
    return comment(field.description) + `${quote(name)}${required.has(name) ? '' : '?'}: ${type(field)};`;
  });
  if (schema.additionalProperties === true) parts.push('[key: string]: unknown;');
  else if (schema.additionalProperties && typeof schema.additionalProperties === 'object') parts.push(`[key: string]: ${type(schema.additionalProperties)};`);
  if (!parts.length) return schema.additionalProperties === false ? 'Record<string, never>' : 'Record<string, unknown>';
  return `{\n${parts.map(part => part.split('\n').map(line => '  ' + line).join('\n')).join('\n')}\n}`;
}

function responseKind(op) {
  if (!op.response_content_type) return 'empty';
  if (op.response_content_type === 'text/event-stream') return 'sse';
  if (op.response_content_type === 'application/json') return 'json';
  if (op.response_content_type === 'application/yaml' || op.response_content_type.startsWith('text/')) return 'text';
  return 'binary';
}

function pagination(op) {
  const shape = deref(op.response_schema);
  if (!shape?.properties?.data) return undefined;
  if (shape.properties.next_page && op.parameters.some(p => p.name === 'page')) return 'page';
  if (shape.properties.has_more && op.parameters.some(p => p.name === 'after_id')) return 'files';
}

const descriptors = operations.map(op => {
  const desc = {
    method: op.method,
    path: op.path,
    parameters: op.parameters.map(({ name, in: location, required }) => ({ name, in: location, required: !!required })),
    response: responseKind(op),
  };
  if (op.request_content_type) desc.body = op.request_content_type === 'multipart/form-data' ? 'multipart' : 'json';
  if (op.request_required) desc.bodyRequired = true;
  if (op.public) desc.public = true;
  if (pagination(op)) desc.pagination = pagination(op);
  return `${quote(op.id)}: ${JSON.stringify(desc, null, 2)}`;
});

let source = `// Generated by scripts/generate.mjs from Mango OpenAPI. Do not edit.\n`;
source += `// Contract SHA-256: ${createHash('sha256').update(specBytes).digest('hex')}\n`;
source += `import { Transport, type BinaryInput, type ClientOptions, type EventStream, type Operation, type RequestOptions, type SSEMessage } from './transport.js';\n\n`;
for (const name of Object.keys(schemas).sort()) source += comment(schemas[name].description) + `export type ${name} = ${type(schemas[name])};\n\n`;
source += `export const operations = {\n${descriptors.join(',\n')}\n} as const satisfies Record<string, Operation>;\n\n`;
source += `export type OperationId = keyof typeof operations;\n\n`;

for (const op of operations) {
  const name = pascal(op.id);
  const properties = Object.fromEntries(op.parameters.map(p => [p.name, { ...p.schema, description: p.description }]));
  const required = op.parameters.filter(p => p.required).map(p => p.name);
  if (op.request_content_type) {
    properties.body = op.request_schema;
    if (op.request_required) required.push('body');
  }
  source += `export type ${name}Params = ${objectType({ type: 'object', properties, required, additionalProperties: false })};\n`;
  const kind = responseKind(op);
  const response = kind === 'empty' ? 'void' : kind === 'binary' ? 'Response' : type(op.response_schema);
  source += `export type ${name}Response = ${response};\n\n`;
}

source += `/** All documented Mango operations. No automatic retries or hidden runtime delegation. */\nexport class Mango extends Transport {\n  constructor(options: ClientOptions) { super(options); }\n\n`;
for (const op of operations) {
  const name = pascal(op.id);
  const required = op.request_required || op.parameters.some(p => p.required);
  const params = `params: ${name}Params${required ? '' : ' = {}'}, options: RequestOptions = {}`;
  const desc = `operations.${op.id}`;
  const kind = responseKind(op);
  source += comment(knownOperations.get(op.id).summary).split('\n').filter(Boolean).map(line => `  ${line}\n`).join('');
  if (kind === 'sse') {
    source += `  open${op.id.slice('stream'.length)}(${params}): Promise<EventStream<${name}Response>> {\n    return this.openFrames<${name}Response>(${desc}, params, options);\n  }\n\n`;
    source += `  ${op.id}(${params}): AsyncGenerator<${name}Response> {\n    return this.frames<${name}Response>(${desc}, params, options);\n  }\n\n`;
    source += `  /** Same live stream with SSE event/id metadata preserved. No automatic reconnect. */\n  ${op.id}Messages(${params}): AsyncGenerator<SSEMessage<${name}Response>> {\n    return this.stream<${name}Response>(${desc}, params, options);\n  }\n\n`;
  } else {
    source += `  ${op.id}(${params}): Promise<${name}Response> {\n    return this.${kind === 'binary' ? 'download' : `request<${name}Response>`}(${desc}, params, options);\n  }\n\n`;
  }
  if (pagination(op)) {
    const itemType = type(deref(op.response_schema).properties.data.items);
    source += `  /** Lazily fetch pages; stopping iteration prevents further requests. */\n  ${op.id}Pages(${params}): AsyncGenerator<${name}Response> {\n    return this.pages<${name}Response>(${desc}, params, options);\n  }\n\n`;
    source += `  /** Lazily traverse all items using the API's opaque cursor. */\n  async *${op.id}Items(${params}): AsyncGenerator<${itemType}> {\n    for await (const page of this.${op.id}Pages(params, options)) yield* page.data;\n  }\n\n`;
  }
}
source += '}\n';

if (process.argv.includes('--check')) {
  const existing = await readFile(outputURL, 'utf8').catch(() => '');
  if (existing !== source) {
    process.stderr.write('TypeScript SDK is stale; run npm run generate in sdk/typescript\n');
    process.exitCode = 1;
  } else process.stdout.write(`TypeScript SDK is current: ${operations.length} operations, ${Object.keys(schemas).length} schemas\n`);
} else {
  await mkdir(new URL('../src/', import.meta.url), { recursive: true });
  await writeFile(outputURL, source);
  process.stdout.write(`Generated ${fileURLToPath(outputURL)} (${operations.length} operations, ${Object.keys(schemas).length} schemas)\n`);
}
