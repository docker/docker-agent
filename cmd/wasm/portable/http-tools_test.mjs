import assert from 'node:assert/strict';
import test from 'node:test';
import { httpTools, toolFetch } from './http-tools.mjs';

const proxy = 'https://egress.example/route?tenant=a';
const start = 'https://api.example/start';
const relay = 'X-Docker-Agent-Location';
const redirect = (status, to) => new Response(null, { status, headers: { [relay]: to } });

// No request can reach a network, including the failure cases.
function network(t, handle = () => { throw new Error('unexpected request'); }) {
  const calls = [];
  t.mock.method(globalThis, 'fetch', async (raw, init) => {
    const wire = new URL(raw);
    const call = { wire, target: wire.searchParams.get('url') ?? wire.href, ...init, headers: new Headers(init.headers) };
    calls.push(call);
    assert.equal(call.credentials, 'omit');
    assert.equal(call.redirect, 'error');
    assert.equal(call.referrerPolicy, 'no-referrer');
    return handle(call);
  });
  return calls;
}

test('guard fails closed before any I/O, including on Node', async t => {
  const calls = network(t);
  await assert.rejects(toolFetch(start, {}, {}), /toolProxy/);
  for (const allowPrivateIPs of [1, 'true', {}, null]) await assert.rejects(toolFetch(start, {}, { allowPrivateIPs }), /boolean/);
  await assert.rejects(toolFetch(start, {}, Object.create({ allowPrivateIPs: true })), /toolProxy/);
  for (const toolProxy of ['http://proxy.example', 'https:///nohost', 'https://me:secret@proxy.example', 'https://proxy.example/#',
    'https://proxy.example/path#fragment', 'https://proxy.example/?%75rl=name', 'https://proxy.example/%zz']) {
    await assert.rejects(toolFetch(start, {}, { toolProxy }), e => !e.message.includes('secret'));
  }
  for (const target of ['data:,hi', 'file:///etc/hosts', 'ftp://example.org', 'rel', '//example.org', 'https:///path', 'http://u:p@test/']) {
    for (const policy of [{ toolProxy: proxy }, { allowPrivateIPs: true }]) await assert.rejects(toolFetch(target, {}, policy));
  }
  for (const init of [{ credentials: 'include' }, { redirect: 'follow' }, { headers: { Cookie: 'page' } },
    { headers: { Host: 'impostor.example' } }, { headers: { 'Proxy-Authorization': 'secret' } }, { agent: {} }, { method: 'CONNECT' }]) {
    await assert.rejects(toolFetch(start, init, { toolProxy: proxy }));
  }
  assert.equal(calls.length, 0);
});

test('guard scopes every proxied hop to the target, strips all custom headers on change of origin', async t => {
  const replies = [redirect(307, '../two?q=a#frag'), redirect(308, 'https://api.example:444/b'),
    redirect(302, start), new Response('ok', { headers: { [relay]: '/ignored' } })];
  const calls = network(t, () => replies.shift());
  const init = { method: 'POST', headers: { Authorization: 'Bearer secret', 'Mcp-Session-Id': 's', 'Content-Type': 'application/json' }, body: '{}' };
  const signal = new AbortController().signal;
  const res = await toolFetch(start + '#not-sent', init, { toolProxy: proxy }, signal);
  assert.ok(res instanceof Response);
  assert.equal(res.url, start);
  assert.equal(res.redirected, true);
  assert.equal(res.headers.has(relay), false);
  assert.equal(await res.text(), 'ok');
  assert.deepEqual(calls.map(c => c.target), [start, 'https://api.example/two?q=a', 'https://api.example:444/b', start]);
  assert.deepEqual(calls.map(c => c.method), ['POST', 'POST', 'POST', 'GET']);
  assert.deepEqual(calls.map(c => c.body), ['{}', '{}', '{}', undefined]);
  assert.deepEqual(calls.map(c => c.headers.get('Authorization')), ['Bearer secret', 'Bearer secret', null, null]);
  assert.deepEqual(calls.map(c => c.headers.get('Mcp-Session-Id')), ['s', 's', null, null]);
  assert.equal(init.headers.Authorization, 'Bearer secret');
  for (const { wire, signal: actual } of calls) {
    assert.equal(wire.origin + wire.pathname, 'https://egress.example/route');
    assert.deepEqual([...wire.searchParams.keys()], ['tenant', 'url']);
    assert.equal(wire.searchParams.get('tenant'), 'a');
    assert.equal(actual, signal);
  }
});

test('allow_private_ips explicitly bypasses the proxy but never enables automatic redirects', async t => {
  const target = 'http://127.0.0.1/';
  const calls = network(t, () => new Response(null, { status: 301, headers: { Location: start } }));
  await assert.rejects(toolFetch(target, {}, { allowPrivateIPs: true, toolProxy: proxy }), /unexpected HTTP redirect/);
  assert.deepEqual(calls.map(c => c.wire.href), [target]);
});

test('proxy never follows a real Location, unsafe redirect target, or excessive chain', async t => {
  let reply, closed = false;
  const calls = network(t, () => reply ?? redirect(301, '/loop'));
  reply = new Response(new ReadableStream({ cancel() { closed = true; } }), { status: 301, headers: { Location: '/x', [relay]: '/x' } });
  await assert.rejects(toolFetch(start, {}, { toolProxy: proxy }), /unexpected HTTP redirect/);
  assert.ok(closed);
  reply = redirect(302, 'file:///tmp/x');
  await assert.rejects(toolFetch(start, {}, { toolProxy: proxy }), /invalid HTTP/);
  reply = new Response('moved?', { status: 302 });
  assert.equal(await (await toolFetch(start, {}, { toolProxy: proxy })).text(), 'moved?');
  reply = null;
  await assert.rejects(toolFetch(start, {}, { toolProxy: proxy }), /10 redirects/);
  assert.equal(calls.length, 13);
});

test('redirect method/body semantics and abort between hops', async t => {
  let status, controller, abort = false;
  const calls = network(t, ({ target }) => {
    if (abort) controller.abort();
    return target === start ? redirect(status, '/done') : new Response(null);
  });
  for (status of [301, 302, 303, 307, 308]) {
    for (const method of ['POST', 'HEAD']) {
      const body = method === 'POST' ? '{}' : undefined;
      await toolFetch(start, { method, body }, { toolProxy: proxy });
      assert.equal(calls.at(-1).method, status >= 307 || method === 'HEAD' ? method : 'GET');
      assert.equal(calls.at(-1).body, status >= 307 ? body : undefined);
    }
  }
  abort = true;
  status = 308;
  controller = new AbortController();
  await assert.rejects(toolFetch(start, {}, { toolProxy: proxy }, controller.signal), { name: 'AbortError' });
  assert.equal(calls.length, 21);
});

test('toolFetch streams the final body without buffering', async t => {
  let controller;
  network(t, () => new Response(new ReadableStream({ start(c) { controller = c; } }), { headers: { 'Content-Type': 'text/event-stream' } }));
  const res = await toolFetch(start, {}, { toolProxy: proxy });
  assert.equal(res.headers.get('Content-Type'), 'text/event-stream');
  controller.enqueue(new TextEncoder().encode('data: 1\n\n'));
  const reader = res.body.getReader();
  assert.equal(new TextDecoder().decode((await reader.read()).value), 'data: 1\n\n');
  await reader.cancel();
});

const apiConfig = { name: 'lookup', instruction: 'Look up a place.', method: 'GET', endpoint: start + '?who=${who}' };
function api(config = {}, extra = {}, env = {}) {
  return httpTools({ type: 'api', api_config: { ...apiConfig, ...config }, ...extra }, env, proxy);
}

test('api_config schema, instructions, Go small endpoint and header expansion', async t => {
  const calls = network(t, () => new Response('pong'));
  const config = { args: { who: { type: 'string', description: 'Place' }, limit: { type: 'string' } }, required: ['who'],
    headers: { Authorization: 'Bearer ${ env.TOKEN }', 'X-Empty': '${env.MISSING}', 'X-Expr': '${f({v:"${env.TOKEN}"})}' } };
  const ts = api(config, {}, { TOKEN: 'key' });
  assert.equal(ts.tools[0].type, 'function');
  const def = ts.tools[0].function;
  assert.equal(def.name, 'lookup');
  assert.equal(ts.instructions, def.description);
  assert.deepEqual(def.parameters.required, ['limit', 'who']);
  assert.deepEqual(def.parameters.properties.limit.type, ['string', 'null']);
  assert.equal((await ts.call('wrong', '{}')).is_error, true);
  assert.equal((await ts.call('lookup', '{bad')).is_error, true);
  assert.equal((await ts.call('lookup', '{"limit":"1"}')).is_error, true);
  assert.equal((await ts.call('lookup', '{"who":2}')).is_error, true);
  assert.equal((await ts.call('lookup', '{"who":"a","surprise":1}')).is_error, true);
  assert.deepEqual(await ts.call('lookup', '{"who":"${env.TOKEN}","limit":null}'), { output: 'pong', is_error: false });
  assert.equal(calls.length, 1);
  assert.equal(new URL(calls[0].target).searchParams.get('who'), '${env.TOKEN}');
  assert.equal(calls[0].headers.get('Authorization'), 'Bearer key');
  assert.equal(calls[0].headers.get('X-Empty'), '');
  assert.equal(calls[0].headers.get('X-Expr'), '${f({v:"${env.TOKEN}"})}');
});

test('api POST sends JSON, handles error statuses and truncates/cancels response streams', async t => {
  let cancelled = false;
  const calls = network(t, () => new Response(new ReadableStream({
    start(c) { c.enqueue(new Uint8Array(31000).fill(65)); }, cancel() { cancelled = true; },
  }), { status: 403, statusText: 'Forbidden' }));
  const ts = api({ method: 'POST', endpoint: 'https://api.example/p/${env.PATH}', args: { count: { type: 'integer' } },
    headers: { 'Content-Type': 'text/plain' } }, {}, { PATH: 'resolved' });
  const result = await ts.call('lookup', '{"count":42}');
  assert.equal(calls[0].target, 'https://api.example/p/resolved');
  assert.equal(calls[0].method, 'POST');
  assert.equal(calls[0].headers.get('Content-Type'), 'application/json');
  assert.equal(calls[0].body, '{"count":42}');
  assert.equal(result.is_error, true);
  assert.equal(result.output, `API request failed with status 403 Forbidden: ${'A'.repeat(30000)}\n\n[Output truncated: exceeded 30,000 character limit]`);
  assert.ok(cancelled);
});

test('reject unsupported settings and ambiguous policies when registering tools', t => {
  const calls = network(t);
  for (const options of [{ method: 'DELETE' }, { output_schema: {} }, { args: { p: { type: 'object', properties: {} } } },
    { args: { p: { type: 'string', pattern: 'x' } } }, { endpoint: 'https://${who}/x' }, { required: ['unknown'] }]) {
    assert.throws(() => api(options));
  }
  assert.throws(() => api({}, { command: 'ignored?' }), /unsupported/);
  for (const extra of [{ escape_html: 'true' }, { allow_private_ips: 1 }, { timeout: 0 }, { headers: { Cookie: 'page' } },
    { allowed_domains: ['example.org'], blocked_domains: ['internal'] }, { blocked_domains: ['10.0.0.0/8'] }, { defer: true }]) {
    assert.throws(() => httpTools({ type: 'fetch', ...extra }));
  }
  assert.equal(calls.length, 0);
});

test('fetch uses Go single/multi result shapes, caches robots per origin per call, respects prefix rules', async t => {
  const calls = network(t, ({ target }) => new URL(target).pathname === '/robots.txt'
    ? new Response('User-agent: *\nDisallow: /\nUser-agent: Cagent\nDisallow: /secret\nAllow: /secret/share')
    : new Response('<é>', { status: 200, statusText: 'OK', headers: { 'Content-Type': 'text/html' } }));
  const ts = httpTools({ type: 'fetch', escape_html: true, headers: { 'X-Token': '${env.TOKEN}' }, allowed_domains: ['example'] }, { TOKEN: 'key' }, proxy);
  assert.deepEqual(await ts.call('fetch', { urls: [start], format: 'html' }), {
    output: `Successfully fetched ${start} (Status: 200, Length: 4 bytes):\n\n<é>`, is_error: false,
  });
  assert.equal(calls.length, 2);
  assert.deepEqual(calls.map(c => c.headers.get('X-Token')), ['key', 'key']);
  assert.equal(calls[1].headers.get('Accept'), 'text/html;q=1.0, text/plain;q=0.8, */*;q=0.1');
  const urls = ['https://api.example/%73ecret', 'https://api.example/secret/share', 'https://badexample/path'];
  const multi = await ts.call('fetch', JSON.stringify({ urls, format: 'html', timeout: 1 }));
  assert.equal(multi.is_error, false);
  assert.match(multi.output, /\\u003c/);
  const [denied, ok, blocked] = JSON.parse(multi.output);
  assert.match(denied.error, /blocked by robots.txt/);
  assert.equal(ok.body, '<é>');
  assert.equal(ok.contentLength, 4);
  assert.equal(ok.status, '200 OK');
  assert.match(blocked.error, /allowed_domains/);
  assert.deepEqual(calls.map(c => c.target), ['https://api.example/robots.txt', start, 'https://api.example/robots.txt', urls[1]]);
});

test('fetch checks domains on robots, content and every redirect, including mapped IPv4 and FQDN', async t => {
  let failRobots = true;
  const calls = network(t, ({ target }) => {
    if (target.endsWith('/robots.txt')) return failRobots ? redirect(302, 'https://secret.example./robots.txt') : new Response(null, { status: 404 });
    return redirect(302, 'http://[::ffff:7f00:1]/forbidden');
  });
  const ts = httpTools({ type: 'fetch', blocked_domains: ['127.0.0.1', 'secret.example'] }, {}, proxy);
  for (const raw of ['http://[::ffff:127.0.0.1]/', 'http://SECRET.EXAMPLE./', start]) {
    assert.match((await ts.call('fetch', { urls: [raw] })).output, /blocked_domains/);
  }
  failRobots = false;
  assert.match((await ts.call('fetch', { urls: [start] })).output, /blocked_domains/);
  assert.deepEqual(calls.map(c => c.target), ['https://api.example/robots.txt', 'https://api.example/robots.txt', start]);
});

test('fetch limits bodies and fails closed on unsupported HTML conversion and robots files', async t => {
  let robots = '', cancelled = false;
  network(t, ({ target }) => target.endsWith('/robots.txt') ? new Response(robots) : new Response(new ReadableStream({
    start(c) { c.enqueue(new Uint8Array((1 << 20) + 1).fill(97)); }, cancel() { cancelled = true; },
  }), { headers: { 'Content-Type': 'text/html' } }));
  const ts = httpTools({ type: 'fetch' }, {}, proxy);
  const large = await ts.call('fetch', { urls: [start], format: 'html' });
  assert.match(large.output, /Length: 1048576 bytes/);
  assert.ok(cancelled);
  const text = await ts.call('fetch', { urls: [start], format: 'text' });
  assert.equal(text.is_error, true);
  assert.match(text.output, /HTML conversion/);
  robots = 'User-agent: *\nDisallow: /foo$';
  assert.match((await ts.call('fetch', { urls: [start] })).output, /unsupported robots/);
  robots = ' '.repeat(65537);
  assert.match((await ts.call('fetch', { urls: [start] })).output, /robots.txt too large/);
});

test('invalid args, absent proxy, and pre-aborted signal never issue a request', async t => {
  const calls = network(t);
  const ts = httpTools({ type: 'fetch' });
  assert.match((await ts.call('fetch', { urls: [start] })).output, /SSRF-guarded HTTP/);
  for (const args of [{ urls: [] }, { urls: [start], method: 'POST' }, { urls: [start], format: 'pdf' }, { urls: [start], timeout: 301 }]) {
    assert.equal((await ts.call('fetch', args)).is_error, true);
  }
  assert.equal((await api().call('lookup', '{}', AbortSignal.abort())).is_error, true);
  assert.equal(calls.length, 0);
});
