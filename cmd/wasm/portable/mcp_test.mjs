import assert from 'node:assert/strict';
import { test } from 'node:test';
import { openMCP } from './mcp.mjs';

const endpoint = 'https://mcp.example.test/tools', proxy = 'https://guard.example.test/';
const config = { type: 'mcp', remote: { url: endpoint } };
const version = '2025-06-18';
const hello = { protocolVersion: version, capabilities: { tools: {} }, serverInfo: { name: 'test', version: '1' }, instructions: 'test only' };
const tool = { name: 'lookup', description: 'Find a record', inputSchema: { type: 'object', properties: { key: { type: 'string' } } }, annotations: { readOnlyHint: true } };
const result = (msg, result, headers) => Response.json({ jsonrpc: '2.0', id: msg.id, result }, { headers });
const ack = () => new Response(null, { status: 202 });

function server(overrides = {}) {
  const calls = [];
  const handlers = {
    'server/discover': msg => result(msg, { supportedVersions: [version] }),
    initialize: msg => result(msg, hello),
    'notifications/initialized': ack,
    'tools/list': msg => result(msg, { tools: [tool] }),
    'tools/call': msg => result(msg, { content: [{ type: 'text', text: 'hi' }] }),
    DELETE: () => new Response(null, { status: 204 }), ...overrides,
  };
  async function toolFetch(url, init, policy, signal) {
    signal.throwIfAborted();
    const msg = init.body && JSON.parse(init.body);
    const method = msg?.method ?? (msg ? 'reply' : 'DELETE');
    calls.push({ method, url, init, msg, policy, signal });
    assert.equal(init.method, method === 'DELETE' ? 'DELETE' : 'POST');
    assert.equal(init.redirect, 'error');
    assert.equal(init.credentials, 'omit');
    if (msg) assert.equal(msg.jsonrpc, '2.0');
    assert.ok(Object.hasOwn(handlers, method), `unexpected ${method}`);
    return handlers[method](msg, signal, init);
  }
  return { calls, toolFetch, open: (signal, ts = config) => openMCP(ts, {}, proxy, signal, { toolFetch }) };
}

const withID = msg => result(msg, hello, { 'MCP-Session-Id': 'opaque-session' });

test('guarded stateful handshake, negotiated version, pagination and teardown', async () => {
  const srv = server({ initialize: withID,
    'tools/list': msg => result(msg, msg.params.cursor ? { tools: [{ ...tool, name: 'write' }] } : { tools: [tool], nextCursor: 'next' }),
    'tools/call': msg => result(msg, { isError: true, content: [
      { type: 'text', text: 'unknown key' }, { type: 'text', text: 'See: ' },
      { type: 'resource_link', name: 'details]', uri: 'test:a)b' }, { type: 'resource', resource: { text: '\nfootnote' } },
    ] }),
  });
  const env = { HOST: 'mcp.example.test', TOKEN: 'explicit' };
  const s = await openMCP({ type: 'mcp', name: 'demo', allow_private_ips: true, remote: {
    url: 'https://${env.HOST}/tools', transport_type: 'streamable-http', headers: { Authorization: 'Bearer ${env.TOKEN}' },
  } }, env, proxy, undefined, { toolFetch: srv.toolFetch });
  assert.equal(s.instructions, hello.instructions);
  assert.deepEqual(s.tools, ['demo_lookup', 'demo_write'].map(name => ({ mcpStrict: true, type: 'function', function: { name, description: tool.description, parameters: { type: 'object', properties: { key: { type: ['string', 'null'] } }, required: ['key'], additionalProperties: false } } })));
  env.HOST = 'other.example.test'; // URL pinned at open; auth headers expanded per request.
  env.TOKEN = 'rotated';
  assert.deepEqual(await s.call('demo_write', '{"key":"a","skip":null,"nested":{"value":null}}'), {
    output: 'unknown key\nSee: [details\\]](test:a%29b)\nfootnote', is_error: true,
  });
  await Promise.all([s.close(), s.close()]);
  await assert.rejects(s.call('demo_write', '{}'), /closed/);
  const calls = srv.calls;
  assert.deepEqual(calls.map(c => c.method), ['server/discover', 'initialize', 'notifications/initialized', 'tools/list', 'tools/list', 'tools/call', 'DELETE']);
  assert.deepEqual(calls[1].msg.params.capabilities, {});
  assert.equal(calls[1].msg.params.protocolVersion, '2025-11-25');
  assert.equal('id' in calls[2].msg, false);
  assert.deepEqual(calls[3].msg.params, {});
  assert.deepEqual(calls[4].msg.params, { cursor: 'next' });
  assert.deepEqual(calls[5].msg.params, { name: 'write', arguments: { key: 'a', nested: { value: null } } });
  assert.equal(new Set(calls.filter(c => c.msg && 'id' in c.msg).map(c => c.msg.id)).size, 5);
  for (const [i, { url, policy, init }] of calls.entries()) {
    assert.equal(url, endpoint);
    assert.deepEqual(policy, { toolProxy: proxy, allowPrivateIPs: true });
    assert.equal(init.headers.get('Authorization'), i < 5 ? 'Bearer explicit' : 'Bearer rotated');
    assert.equal(init.headers.get('Accept'), 'application/json, text/event-stream');
    assert.equal(init.headers.get('Content-Type'), 'application/json');
    assert.equal(init.headers.get('MCP-Session-Id'), i > 1 ? 'opaque-session' : null);
    assert.equal(init.headers.get('MCP-Protocol-Version'), i === 0 ? '2026-07-28' : i === 1 ? null : version);
  }
});

test('stateless session: reply in fragmented SSE, ping and explicit refusal of server requests', async () => {
  let cancelled = false;
  const srv = server({
    'tools/call': msg => {
      const frames = [': heartbeat\r\nid: prime\rdata:\r\r', 'event: unknown\ndata: invalid\n\n',
        ...[{ method: 'notifications/progress' }, { method: 'ping', id: 'p' }, { method: 'elicitation/create', id: 4 }]
          .map(m => `event: message\ndata: ${JSON.stringify({ jsonrpc: '2.0', ...m })}\n\n`),
        `data: {"id":${msg.id},"jsonrpc":"2.0",\r\ndata: "result":{"content":[{"type":"text","text":"café"}]}}\r\n\r\n`,
      ].join('');
      const bytes = new TextEncoder().encode(frames);
      let offset = 0;
      return new Response(new ReadableStream({
        pull(c) { if (offset < bytes.length) c.enqueue(bytes.subarray(offset, ++offset)); },
        cancel() { cancelled = true; },
      }), { headers: { 'Content-Type': 'text/event-stream; charset=UTF-8' } });
    }, reply: ack,
  });
  const s = await srv.open();
  assert.deepEqual(await s.call('lookup', ''), { output: 'café', is_error: false });
  await s.close();
  assert.ok(cancelled, 'must release the stream even if the server leaves it open');
  assert.deepEqual(srv.calls.filter(c => c.method === 'reply').map(c => c.msg), [
    { jsonrpc: '2.0', id: 'p', result: {} }, { jsonrpc: '2.0', id: 4, error: { code: -32602, message: 'client does not support elicitation' } },
  ]);
  assert.ok(srv.calls.every(c => !c.init.headers.has('MCP-Session-Id') && c.method !== 'DELETE' && c.policy.allowPrivateIPs === false));
});

test('only configured HTTP transports and session env; no transport or OAuth fallback', async t => {
  const raw = t.mock.method(globalThis, 'fetch', () => assert.fail('unexpected global fetch'));
  const srv = server();
  await assert.rejects(openMCP(config, {}, proxy), /guarded toolFetch/);
  async function refused(ts, error, options = {}, toolProxy = proxy) {
    await assert.rejects(openMCP(ts, {}, toolProxy, undefined, { toolFetch: srv.toolFetch, ...options }), error);
  }
  for (const key of ['command', 'args', 'ref', 'env', 'lifecycle', 'readonly', 'defer']) await refused({ ...config, [key]: true }, /unsupported MCP toolset/);
  for (const fields of [{ oauth: {} }, { transport_type: 'stdio' }, { standalone_sse: true }, { unsupported: true },
    { url: 'unix:///socket' }, { url: 'ftp://server' }, { url: 'https://user:pass@x/' }, { url: 'https://x/#secret' },
    { url: 'https://${env.ABSENT}/' }, { headers: { Token: '${headers.Authorization}' } }, { headers: { Token: '${something(${env.KEY})}' } },
    { headers: { 'mCp-seSSion-id': 'untrusted' } }, { headers: { 'Content-Type': 'not JSON' } }, { headers: { hi: 'a\r\nb' } }]) {
    await refused({ ...config, remote: { ...config.remote, ...fields } }, /MCP/);
  }
  await refused(config, /requires a trusted toolProxy/, {}, '');
  await refused({ ...config, allow_private_ips: 'true' }, /allow_private_ips/);
  await refused(config, /OAuth/, { oauthRedirectURI: endpoint });
  assert.equal(srv.calls.length, 0);
  assert.equal(raw.mock.callCount(), 0);
});

test('tool calls reject unknown names locally and distinguish RPC failures from empty/structured results', async () => {
  const replies = [msg => Response.json({ jsonrpc: '2.0', id: msg.id, error: { code: -32602, message: 'remote detail' } }),
    () => new Response(null, { status: 401, headers: { 'WWW-Authenticate': 'Bearer resource_metadata="https://other.test"' } }),
    msg => Response.json({ jsonrpc: '2.0', id: msg.id, error: { code: -32042, message: 'follow me', data: { url: 'https://other.test' } } }),
    msg => result(msg, { structuredContent: { ok: 2 }, content: [] }), msg => result(msg, { content: [] }),
    msg => result(msg, { content: [{ type: 'image', data: 'a' }] }), msg => result(msg, { content: [], isError: 'true' }),
    msg => result({ id: msg.id + 1 }, {}), () => new Response('an error page'),
  ];
  const srv = server({ 'tools/call': msg => replies.shift()(msg) });
  const s = await srv.open();
  for (const [name, args] of [['toString', '{}'], ['other', '{}'], ['lookup', '[]'], ['lookup', 'invalid']]) {
    assert.equal((await s.call(name, args)).is_error, true);
  }
  assert.equal(srv.calls.filter(c => c.method === 'tools/call').length, 0);
  for (const expected of [/-32602/, /401.*OAuth/, /URL elicitation.*not supported/, 'no output', 'no output', /non-text/, /invalid MCP tool result/, /mismatched/, /expected an MCP JSON/]) {
    const { output, is_error } = await s.call('lookup', '{}');
    assert.equal(is_error, typeof expected !== 'string');
    if (is_error) assert.match(output, expected);
    else assert.equal(output, expected);
  }
  assert.equal(srv.calls.length, 13, 'no auth URL followed and no tool invocation retried');
  await s.close();
});

test('invalidation and expiry do not reuse a stale catalog or replay a tool call', async () => {
  for (const expired of [false, true]) {
    const srv = server({ initialize: withID, 'tools/call': () => expired ? new Response(null, { status: 404 }) : new Response(
      'data: {"jsonrpc":"2.0","method":"notifications/tools/list_changed"}\n\n', { headers: { 'Content-Type': 'text/event-stream' } },
    ) });
    const s = await srv.open();
    const error = expired ? /expired/ : /catalog changed/;
    await assert.rejects(s.call('lookup', '{}'), error);
    await assert.rejects(s.call('lookup', '{}'), error);
    await s.close();
    assert.equal(srv.calls.filter(c => c.method === 'tools/call').length, 1);
    assert.equal(srv.calls.filter(c => c.method === 'DELETE').length, expired ? 0 : 1);
  }
});

test('an instructions-only server has no callable tools', async () => {
  const srv = server({ initialize: msg => result(msg, { ...hello, capabilities: {} }) });
  const s = await srv.open();
  assert.deepEqual(s.tools, []);
  assert.equal(s.instructions, hello.instructions);
  assert.equal((await s.call('lookup', '{}')).is_error, true);
  await s.close();
  assert.deepEqual(srv.calls.map(c => c.method), ['server/discover', 'initialize', 'notifications/initialized']);
});

test('partial stateful sessions are deleted after invalid or excessive discovery responses', async () => {
  for (const [overrides, error] of [
    [{ initialize: msg => result(msg, { ...hello, protocolVersion: 'unknown' }, { 'MCP-Session-Id': 'opaque' }) }, /protocol version/],
    [{ 'notifications/initialized': () => new Response(null, { status: 401 }) }, /401.*OAuth/],
    [{ 'notifications/initialized': () => Response.json({}) }, /expected MCP acknowledgement/],
    [{ 'tools/list': msg => result(msg, { tools: [], nextCursor: 'cycle' }) }, /pagination/],
    [{ 'tools/list': msg => result(msg, { tools: [], nextCursor: String(msg.id) }) }, /excessive MCP pagination/],
    [{ 'tools/list': msg => result(msg, { tools: [tool, tool] }) }, /duplicate/],
    [{ 'tools/list': msg => result(msg, { tools: Array(257).fill(tool) }) }, /excessive/],
    [{ 'tools/list': msg => result(msg, { tools: [{ ...tool, inputSchema: [] }] }) }, /tool definition/],
    [{ 'tools/list': msg => result(msg, { tools: [], padding: 'x'.repeat(1 << 20) }) }, /too large/],
    [{ 'tools/list': msg => result(msg, { tools: [] }, { 'MCP-Session-Id': 'replacement' }) }, /unexpected MCP session ID/],
  ]) {
    const srv = server({ initialize: withID, ...overrides });
    await assert.rejects(srv.open(), error);
    assert.equal(srv.calls.at(-1).method, 'DELETE');
    assert.notEqual(srv.calls.at(-1).init.headers.get('MCP-Session-Id'), 'replacement');
  }
});

function hanging(started, aborted) {
  return (_, signal) => new Promise((_, reject) => {
    signal.addEventListener('abort', () => { aborted?.(); reject(signal.reason); }, { once: true });
    started.resolve();
  });
}

test('abort during discovery rolls back using an independent signal; pre-abort sends nothing', async () => {
  const c = new AbortController(), started = Promise.withResolvers();
  const srv = server({ initialize: withID, 'tools/list': hanging(started) });
  const open = srv.open(c.signal);
  await started.promise;
  c.abort();
  await assert.rejects(open, { name: 'AbortError' });
  assert.equal(srv.calls.at(-1).method, 'DELETE');
  assert.equal(srv.calls.at(-1).signal.aborted, false);
  const count = srv.calls.length;
  await assert.rejects(srv.open(c.signal), { name: 'AbortError' });
  assert.equal(srv.calls.length, count);
});

test('aborting a streamed call cancels the reader but does not poison later calls', async () => {
  const c = new AbortController(), reading = Promise.withResolvers();
  let count = 0, cancelled = 0;
  const srv = server({ 'tools/call': msg => ++count > 1 ? result(msg, { content: [] }) : new Response(new ReadableStream({
    pull() { reading.resolve(); }, cancel() { cancelled++; },
  }, { highWaterMark: 0 }), { headers: { 'Content-Type': 'text/event-stream' } }) });
  const s = await srv.open();
  const run = s.call('lookup', '{}', c.signal);
  await reading.promise;
  c.abort();
  await assert.rejects(run, { name: 'AbortError' });
  assert.equal(cancelled, 1);
  assert.deepEqual(await s.call('lookup', '{}'), { output: 'no output', is_error: false });
  await s.close();
});

test('close aborts in-flight work and attempts DELETE only once; HTTP 405 is permitted', async () => {
  const started = Promise.withResolvers();
  let aborted = false;
  const srv = server({ initialize: withID, 'tools/call': hanging(started, () => { aborted = true; }),
    DELETE: () => { assert.ok(aborted); return new Response(null, { status: 405 }); },
  });
  const s = await srv.open(), call = s.call('lookup', '{}');
  await started.promise;
  const done = s.close();
  assert.equal(s.close(), done);
  await assert.rejects(call, /closed/);
  await done;
  assert.equal(srv.calls.filter(c => c.method === 'DELETE').length, 1);
});

test('deadline covers call and cleanup; no retry after timeout', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const started = Promise.withResolvers(), deleting = Promise.withResolvers();
  const srv = server({ initialize: withID, 'tools/call': hanging(started), DELETE: hanging(deleting) });
  const s = await srv.open();
  const call = s.call('lookup', '{}');
  await started.promise;
  t.mock.timers.tick(30000);
  assert.deepEqual(await call, { is_error: true, output: 'MCP request timed out' });
  const close = s.close();
  await deleting.promise;
  t.mock.timers.tick(2500);
  await assert.rejects(close, /timed out/);
  assert.deepEqual(srv.calls.slice(4).map(c => c.method), ['tools/call', 'DELETE']);
});


const issuer = 'https://issuer.example', redirect = 'https://app.example/callback', tokenURL = 'https://tokens.example/token';
const invalidBearer = 'Bearer error="invalid_token"';
const unauthorized = challenge => new Response(null, { status: 401, headers: { 'WWW-Authenticate': challenge } });
const bearer = (value, extra) => Response.json({ token_type: 'Bearer', access_token: value, ...extra });

function protectedServer({ resource = endpoint, code, refresh, rpc } = {}) {
  const prompts = [], grants = [], metadata = [];
  const srv = server({ initialize: (msg, _, init) => init.headers.has('Authorization') ? withID(msg) : unauthorized('Bearer'), ...rpc });
  const options = {
    oauthRedirectURI: redirect,
    elicit(event) {
      prompts.push(event);
      const params = new URLSearchParams({ state: new URL(event.url).searchParams.get('state'), code: 'code', iss: issuer });
      return { action: 'accept', content: { callback_url: redirect + '?' + params } };
    },
    toolFetch(url, init, policy, signal) {
      assert.deepEqual(policy, { toolProxy: proxy, allowPrivateIPs: false });
      assert.equal(init.redirect, 'error');
      assert.equal(init.credentials, 'omit');
      if (url === endpoint) {
        assert.equal(init.headers.get('X-Private'), 'resource-only');
        return srv.toolFetch(url, init, policy, signal);
      }
      assert.deepEqual(Object.keys(init.headers).sort(), init.body ? ['Accept', 'Content-Type'] : ['Accept']);
      if (!init.body) {
        metadata.push(url);
        if (url === 'https://mcp.example.test/.well-known/oauth-protected-resource/tools') return Response.json({ resource, authorization_servers: [issuer] });
        assert.equal(url, issuer + '/.well-known/oauth-authorization-server');
        return Response.json({ issuer, authorization_endpoint: issuer + '/auth', token_endpoint: tokenURL, response_types_supported: ['code'],
          code_challenge_methods_supported: ['S256'], token_endpoint_auth_methods_supported: ['none'], authorization_response_iss_parameter_supported: true });
      }
      assert.equal(url, tokenURL);
      const form = Object.fromEntries(new URLSearchParams(init.body));
      grants.push(form);
      assert.equal(form.client_id, 'public-client');
      assert.equal(form.resource, endpoint);
      if (form.grant_type === 'authorization_code') return code?.(form) ?? bearer('old', { refresh_token: 'secret & 1', expires_in: 60 });
      assert.deepEqual(Object.keys(form).sort(), ['client_id', 'grant_type', 'refresh_token', 'resource']);
      assert.equal(form.grant_type, 'refresh_token');
      return refresh(form, signal);
    },
  };
  return { calls: srv.calls, prompts, grants, metadata, open: () => openMCP({ type: 'mcp', remote: {
    url: endpoint, headers: { 'X-Private': 'resource-only' }, oauth: { clientId: 'public-client' },
  } }, {}, proxy, undefined, options) };
}

test('guarded initialize/list/call refresh before sending, rotating and retaining the refresh token', async t => {
  let now = 0;
  t.mock.method(Date, 'now', () => now);
  const replies = [bearer('second', { refresh_token: 'rotated!', expires_in: 60 }), bearer('third', { expires_in: 10 }), bearer('last')];
  const f = protectedServer({ refresh: () => replies.shift(), rpc: { 'notifications/initialized': () => { now = 60000; return ack(); } } });
  const s = await f.open();
  assert.deepEqual(f.calls.map(c => [c.method, c.init.headers.get('Authorization')]), [
    ['initialize', null], ['initialize', 'Bearer old'], ['notifications/initialized', 'Bearer old'], ['tools/list', 'Bearer second'],
  ]);
  for (const [time, token] of [[89999, 'second'], [90001, 'third'], [100002, 'last'], [86400000, 'last']]) {
    now = time;
    assert.deepEqual(await s.call('lookup', '{}'), { output: 'hi', is_error: false });
    assert.equal(f.calls.at(-1).init.headers.get('Authorization'), 'Bearer ' + token);
  }
  await s.close();
  assert.equal(f.calls.at(-1).method, 'DELETE');
  assert.equal(f.calls.at(-1).init.headers.get('Authorization'), 'Bearer last');
  assert.deepEqual(f.grants.slice(1).map(g => g.refresh_token), ['secret & 1', 'rotated!', 'rotated!']);
  assert.equal(f.prompts.length, 1);
  assert.equal(f.metadata.length, 2);
});

test('a mismatched resource cannot receive a grant or authorize replay', async () => {
  const f = protectedServer({ resource: endpoint + '/other' });
  await assert.rejects(f.open(), /resource identifier mismatch/);
  assert.deepEqual(f.prompts, []);
  assert.deepEqual(f.grants, []);
  assert.deepEqual(f.calls.map(c => c.method), ['initialize']);
});

test('late parallel invalid_token responses reuse one refresh and replay each rejected RPC once', async () => {
  const late = Promise.withResolvers(), started = Promise.withResolvers();
  const f = protectedServer({ refresh: () => bearer('replaced'), rpc: {
    'tools/call': async (msg, _, init) => {
      if (init.headers.get('Authorization') === 'Bearer replaced') return result(msg, { content: [] });
      if (msg.params.arguments.key === 'late') { started.resolve(); await late.promise; }
      return unauthorized(invalidBearer + ', resource_metadata="https://other.example/discovery"');
    },
  } });
  const s = await f.open();
  const slow = s.call('lookup', '{"key":"late"}');
  await started.promise;
  assert.equal((await s.call('lookup', '{}')).is_error, false);
  late.resolve();
  assert.equal((await slow).is_error, false);
  const calls = f.calls.filter(c => c.method === 'tools/call');
  assert.equal(calls.length, 4);
  assert.deepEqual(calls.map(c => c.init.headers.get('Authorization')), ['Bearer old', 'Bearer old', 'Bearer replaced', 'Bearer replaced']);
  assert.equal(calls[0].init.body, calls[3].init.body);
  assert.equal(calls[1].init.body, calls[2].init.body);
  assert.equal(f.grants.length, 2);
  assert.equal(f.prompts.length, 1);
  await s.close();
});

test('one cancelled caller does not abort another caller’s refresh; close cancels remaining work without replay', async t => {
  let now = 0;
  t.mock.method(Date, 'now', () => now);
  for (const close of [true, false]) {
    now = 0;
    const started = Promise.withResolvers(), send = Promise.withResolvers();
    const f = protectedServer({ refresh: (_, signal) => { started.resolve(signal); return send.promise; } });
    const s = await f.open(), c = new AbortController();
    now = 61000;
    const first = s.call('lookup', '{}', c.signal);
    const signal = await started.promise;
    const second = s.call('lookup', '{}');
    c.abort();
    await assert.rejects(first, { name: 'AbortError' });
    assert.equal(signal.aborted, false);
    if (close) {
      await s.close();
      await assert.rejects(second, /closed/);
      assert.equal(signal.aborted, true);
      assert.equal(f.calls.at(-1).init.headers.has('Authorization'), false);
    }
    send.resolve(bearer('new'));
    if (!close) {
      assert.equal((await second).is_error, false);
      assert.equal(f.calls.at(-1).init.headers.get('Authorization'), 'Bearer new');
      await s.close();
    }
    assert.equal(f.grants.length, 2);
    assert.equal(f.prompts.length, 1);
    assert.equal(f.calls.filter(c => c.method === 'tools/call').length, close ? 0 : 1);
    assert.equal(f.calls.filter(c => c.method === 'DELETE').length, 1);
  }
});

test('failed refresh is not an authorization prompt or permission to replay', async t => {
  let now = 0;
  t.mock.method(Date, 'now', () => now);
  for (const expired of [true, false]) {
    now = 0;
    let attempts = 0;
    const f = protectedServer({ refresh: () => {
      if (++attempts === 1) throw new Error('request failure: secret & 1');
      return bearer('new');
    }, rpc: { 'tools/call': (msg, _, init) => init.headers.get('Authorization') === 'Bearer new' ? result(msg, { content: [] }) : unauthorized(invalidBearer) } });
    const s = await f.open();
    if (expired) now = 60000;
    assert.deepEqual(await s.call('lookup', '{}'), { output: 'MCP OAuth transport request failed', is_error: true });
    assert.equal(f.calls.filter(c => c.method === 'tools/call').length, expired ? 0 : 1);
    // Only a separate caller-initiated RPC may try again.
    assert.equal((await s.call('lookup', '{}')).is_error, false);
    assert.equal(f.calls.filter(c => c.method === 'tools/call').length, expired ? 1 : 2);
    assert.equal(f.grants.length, 3);
    assert.equal(f.prompts.length, 1);
    await s.close();
  }
});

test('ambiguous or unauthorized tool failures never replay; a second invalid_token ends recovery', async () => {
  for (const [response, error, count] of [
    [() => { throw new Error('connection lost'); }, /transport/, 1],
    [() => new Response(null, { status: 502 }), /Bad Gateway/, 1],
    [() => new Response('truncated', { headers: { 'Content-Type': 'application/json' } }), /invalid MCP JSON/, 1],
    [msg => Response.json({ jsonrpc: '2.0', id: msg.id, error: { code: -32001, message: 'invalid_token' } }), /JSON-RPC/, 1],
    [() => new Response(null, { status: 403 }), /403/, 1],
    [() => unauthorized('Bearer'), /401/, 1], [() => unauthorized('Bearer error="insufficient_scope"'), /401/, 1],
    [() => unauthorized(invalidBearer), /401/, 2],
  ]) {
    const f = protectedServer({ refresh: () => bearer('replacement'), rpc: { 'tools/call': response } });
    const s = await f.open();
    const { output, is_error } = await s.call('lookup', '{}');
    assert.equal(is_error, true);
    assert.match(output, error);
    assert.equal(f.calls.filter(c => c.method === 'tools/call').length, count);
    assert.equal(f.grants.length, count);
    assert.equal(f.prompts.length, 1);
    await s.close();
  }
});

test('expiry without a refresh grant does not send a tool unauthenticated; reopen starts a new session', async t => {
  let now = 0, keys = 0;
  t.mock.method(Date, 'now', () => now);
  const f = protectedServer({ code: () => bearer('key' + ++keys, { expires_in: 10 }) });
  const s = await f.open();
  now = 10000;
  assert.equal((await s.call('lookup', '{}')).is_error, false);
  assert.deepEqual(f.calls.filter(c => c.method === 'tools/call').map(c => c.init.headers.get('Authorization')), ['Bearer key2']);
  await s.close();
  const second = await f.open();
  assert.deepEqual(f.calls.filter(c => c.method === 'initialize').map(c => c.init.headers.get('Authorization')), [null, 'Bearer key1', null, 'Bearer key3']);
  assert.equal(f.metadata.length, 4);
  assert.equal(f.prompts.length, 3);
  await second.close();
});

test('discovery never adopts a session ID or follows a redirect, and is size-limited', async () => {
  for (const probe of [msg => result(msg, {supportedVersions: ['2026-07-28'], capabilities: {tools: {}}}, {'MCP-Session-Id': 'not-initialized'}),
    () => new Response(null, {status: 307, headers: {Location: 'https://other.test'}}),
    () => new Response('old server without JSON discovery')]) {
    const srv = server({'server/discover': probe});
    const s = await srv.open();
    await s.close();
    assert.deepEqual(srv.calls.map(c => c.method), ['server/discover', 'initialize', 'notifications/initialized', 'tools/list']);
    assert.ok(srv.calls.every(c => c.url === endpoint && !c.init.headers.has('MCP-Session-Id')));
  }
  const srv = server({'server/discover': msg => result(msg, {padding: 'a'.repeat(1 << 20)})});
  await assert.rejects(srv.open(), /too large/);
  assert.deepEqual(srv.calls.map(c => c.method), ['server/discover']);
});

test('schema normalization visits schema positions, not ref targets or enum and example data', async () => {
  const node = () => ({type: 'object', properties: {q: {type: 'string', format: 'email'}}});
  const raw = {type: 'object', $defs: {x: node()}, definitions: {x: node()}, patternProperties: {x: node()},
    not: node(), if: node(), then: node(), else: node(), contains: node(), propertyNames: node(),
    additionalProperties: node(), items: node(), anyOf: [node()], oneOf: [node()], allOf: [node()], prefixItems: [node()],
    properties: JSON.parse('{"__proto__":{"$ref":"#"},"10":{"enum":[{"format":"keep"}]},"2":{"type":"array"}}'),
    examples: [{format: 'keep'}]};
  const srv = server({'tools/list': msg => result(msg, {tools: [{...tool, inputSchema: raw}]})});
  const s = await srv.open();
  try {
    const p = s.tools[0].function.parameters;
    for (const child of [p.$defs.x, p.definitions.x, p.patternProperties.x, p.not, p.if, p.then, p.else,
      p.contains, p.propertyNames, p.additionalProperties, p.items, ...p.anyOf, ...p.oneOf, ...p.allOf, ...p.prefixItems]) {
      assert.deepEqual(child, {type: 'object', properties: {q: {type: ['string','null']}}, required: ['q'], additionalProperties: false});
    }
    assert.deepEqual(p.required, ['10', '2', '__proto__']);
    assert.deepEqual(p.properties.__proto__, {anyOf: [{$ref: '#'}, {type: 'null'}]});
    assert.deepEqual(p.properties['10'].anyOf[0].enum, [{format: 'keep'}]);
    assert.deepEqual(p.properties['2'].anyOf[0].items, {type: 'object'});
    assert.deepEqual(p.examples, [{format: 'keep'}]);
    assert.equal(s.tools[0].mcpStrict, undefined);
  } finally { await s.close(); }
});

test('a declined server elicitation never transmits content; accepted content uses the host JSON view', async () => {
  let action = 'decline';
  const content = {private: 'do not send', toJSON() { return {answer: 'blue'}; }};
  const srv = server({'tools/call': msg => {
    const frames = [{id: 10, method: 'elicitation/create', params: {message: 'Choose'}}, {id: msg.id, result: {content: []}}];
    return new Response(frames.map(f => `data: ${JSON.stringify({jsonrpc: '2.0', ...f})}\n\n`).join(''), {headers: {'Content-Type': 'text/event-stream'}});
  }, reply: ack});
  const s = await openMCP(config, {}, proxy, undefined, {toolFetch: srv.toolFetch, elicit: () => ({action, content})});
  try {
    for (action of ['decline', 'cancel', 'accept']) {
      assert.deepEqual(await s.call('lookup', '{}'), {output: 'no output', is_error: false});
      assert.deepEqual(srv.calls.at(-1).msg.result, action === 'accept' ? {action, content: {answer: 'blue'}} : {action});
    }
  } finally { await s.close(); }
});
