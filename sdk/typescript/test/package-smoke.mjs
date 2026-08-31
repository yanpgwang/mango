// Feed this file to Node on stdin from a fresh directory with the tarball installed.
import assert from 'node:assert/strict';
import { Mango, APIError, operations } from 'mango-sdk';

assert.equal(Object.keys(operations).length, 98);
assert.equal(typeof APIError, 'function');
let calls = 0;
const client = new Mango({
  baseURL: 'https://mango.invalid/proxy',
  apiKey: 'package-test-only',
  fetch: async (url, request) => {
    calls++;
    assert.equal(new URL(url).pathname, '/proxy/v1/agents');
    assert.equal(request.method, 'POST');
    assert.equal(request.headers.get('authorization'), 'Bearer package-test-only');
    assert.equal(request.redirect, 'manual');
    assert.deepEqual(JSON.parse(request.body), { name: 'test', model: 'test' });
    return Response.json({ id: 'agent_package_test' });
  },
});
assert.equal((await client.createAgent({ body: { name: 'test', model: 'test' } })).id, 'agent_package_test');
assert.equal(calls, 1);
console.log('Installed mango-sdk: public import, 98 operations, and authenticated request verified');
