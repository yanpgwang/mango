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

// The wire retains snake_case and bracket filters. SDK query arguments use
// plain identifiers so callers do not have to quote HTTP parameter names.
const argumentName = name => name.replaceAll(/[^a-zA-Z0-9]+/g, '_').replaceAll(/^_|_$/g, '');
const camel = name => name.replaceAll(/_([a-z])/g, (_, char) => char.toUpperCase());
const methodName = name => name === 'open_api' ? 'openAPI' : camel(name);
const resourceName = resource => resource.split('.').map(part => pascal(camel(part))).join('') + 'Resource';
const resources = [...new Set(operations.flatMap(op => op.sdk_resource.split('.').map((_, i, parts) => parts.slice(0, i + 1).join('.'))))].sort();
function bodySchema(op) {
  const schema = deref(op.request_schema);
  if (!schema) return undefined;
  if (schema.type !== 'object') throw new Error(`Object request required: ${op.id}`);
  return schema;
}
function argumentsSchema(op) {
  const body = bodySchema(op);
  const properties = { ...(body?.properties ?? {}) };
  const required = op.request_required ? [...(body?.required ?? [])] : [];
  for (const p of op.parameters.filter(p => p.in === 'query')) {
    const name = argumentName(p.name);
    if (Object.hasOwn(properties, name)) throw new Error(`Ambiguous argument ${op.id}.${name}`);
    properties[name] = { ...p.schema, description: p.description };
    if (p.required) required.push(name);
  }
  return { type: 'object', properties, required, additionalProperties: false };
}
for (const op of operations) {
  const name = pascal(op.id);
  source += `export type ${name}Params = ${objectType(argumentsSchema(op))};\n`;
  const kind = responseKind(op);
  const response = kind === 'empty' ? 'void' : kind === 'binary' ? 'Response' : type(op.response_schema);
  source += `export type ${name}Response = ${response};\n\n`;
}

function children(resource) {
  return resources.filter(r => r.split('.').slice(0, -1).join('.') === resource);
}
function fields(resource) {
  return children(resource).map(r => `  readonly ${camel(r.split('.').at(-1))}: ${resourceName(r)};\n`).join('');
}
function assignments(resource) {
  return children(resource).map(r => `    this.${camel(r.split('.').at(-1))} = new ${resourceName(r)}(transport);\n`).join('');
}
source += `/** Mango resource services share one transport. Writes are never retried automatically. */\nexport class Mango {\n`;
source += fields('') + `  constructor(options: ClientOptions) {\n    const transport = new Transport(options);\n` + assignments('') + `  }\n}\n\n`;
for (const resource of resources) {
  source += `export class ${resourceName(resource)} {\n` + fields(resource);
  source += `  constructor(private readonly transport: Transport) {\n` + assignments(resource) + `  }\n\n`;
  for (const op of operations.filter(op => op.sdk_resource === resource)) {
    const name = pascal(op.id);
    const method = methodName(op.sdk_method);
    const schema = argumentsSchema(op);
    const pathArgs = op.parameters.filter(p => p.in === 'path').map(p => argumentName(p.name));
    const hasParams = Object.keys(schema.properties).length > 0;
    const params = [...pathArgs.map(p => `${p}: string`), ...(hasParams ? [`params: ${name}Params${schema.required.length ? '' : ' = {}'}`] : []), 'options: RequestOptions = {}'].join(', ');
    const forwarded = [...pathArgs, ...(hasParams ? ['params'] : []), 'options'].join(', ');
    const wire = op.parameters.map(p => `${quote(p.name)}: ${p.in === 'path' ? argumentName(p.name) : `params.${argumentName(p.name)}`}`);
    const body = bodySchema(op);
    if (body) {
      const bodyFields = Object.keys(body.properties ?? {});
      let value = `{ ${bodyFields.map(key => `${quote(key)}: params.${key}`).join(', ')} }`;
      if (!op.request_required) value = `(${bodyFields.map(key => `params.${key} !== undefined`).join(' || ') || 'false'} ? ${value} : undefined)`;
      wire.push(`body: ${value}`);
    }
    const wireParams = `{ ${wire.join(', ')} }`;
    const desc = `operations.${op.id}`;
    const kind = responseKind(op);
    source += comment(knownOperations.get(op.id).summary).split('\n').filter(Boolean).map(line => `  ${line}\n`).join('');
    if (kind === 'sse') {
      source += `  /** Resolves after subscription. Close the stream when finished; no automatic reconnect. */\n  ${method}(${params}): Promise<EventStream<${name}Response>> {\n    return this.transport.openFrames<${name}Response>(${desc}, ${wireParams}, options);\n  }\n\n`;
      source += `  /** Lazy raw SSE iteration, including event/id metadata. */\n  ${method}Messages(${params}): AsyncGenerator<SSEMessage<${name}Response>> {\n    return this.transport.stream<${name}Response>(${desc}, ${wireParams}, options);\n  }\n\n`;
    } else {
      source += `  ${method}(${params}): Promise<${name}Response> {\n    return this.transport.${kind === 'binary' ? 'download' : `request<${name}Response>`}(${desc}, ${wireParams}, options);\n  }\n\n`;
    }
    if (pagination(op)) {
      const itemType = type(deref(op.response_schema).properties.data.items);
      source += `  /** Lazily fetch pages; stopping iteration prevents further requests. */\n  ${method}Pages(${params}): AsyncGenerator<${name}Response> {\n    return this.transport.pages<${name}Response>(${desc}, ${wireParams}, options);\n  }\n\n`;
      source += `  /** Lazily traverse all items using the API's opaque cursor. */\n  async *${method}Items(${params}): AsyncGenerator<${itemType}> {\n    for await (const page of this.${method}Pages(${forwarded})) yield* page.data;\n  }\n\n`;
    }
  }
  source += '}\n\n';
}

source = source.trimEnd() + '\n';

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
