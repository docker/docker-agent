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
    return handlers[method](msg, signal);
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
  assert.deepEqual(s.tools, ['demo_lookup', 'demo_write'].map(name => ({ type: 'function', function: { name, description: tool.description, parameters: tool.inputSchema } })));
  env.HOST = 'other.example.test'; // URL pinned at open; auth headers expanded per request.
  env.TOKEN = 'rotated';
  assert.deepEqual(await s.call('demo_write', '{"key":"a","skip":null,"nested":{"value":null}}'), {
    output: 'unknown key\nSee: [details\\]](test:a%29b)\nfootnote', is_error: true,
  });
  await Promise.all([s.close(), s.close()]);
  await assert.rejects(s.call('demo_write', '{}'), /closed/);
  const calls = srv.calls;
  assert.deepEqual(calls.map(c => c.method), ['initialize', 'notifications/initialized', 'tools/list', 'tools/list', 'tools/call', 'DELETE']);
  assert.deepEqual(calls[0].msg.params.capabilities, {});
  assert.equal(calls[0].msg.params.protocolVersion, '2025-11-25');
  assert.equal('id' in calls[1].msg, false);
  assert.deepEqual(calls[2].msg.params, {});
  assert.deepEqual(calls[3].msg.params, { cursor: 'next' });
  assert.deepEqual(calls[4].msg.params, { name: 'write', arguments: { key: 'a', nested: { value: null } } });
  assert.equal(new Set(calls.filter(c => c.msg && 'id' in c.msg).map(c => c.msg.id)).size, 4);
  for (const [i, { url, policy, init }] of calls.entries()) {
    assert.equal(url, endpoint);
    assert.deepEqual(policy, { toolProxy: proxy, allowPrivateIPs: true });
    assert.equal(init.headers.get('Authorization'), i < 4 ? 'Bearer explicit' : 'Bearer rotated');
    assert.equal(init.headers.get('Accept'), 'application/json, text/event-stream');
    assert.equal(init.headers.get('Content-Type'), 'application/json');
    assert.equal(init.headers.get('MCP-Session-Id'), i ? 'opaque-session' : null);
    assert.equal(init.headers.get('MCP-Protocol-Version'), i ? version : null);
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
    { jsonrpc: '2.0', id: 'p', result: {} }, { jsonrpc: '2.0', id: 4, error: { code: -32601, message: 'Method not supported' } },
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
  for (const expected of [/-32602/, /401.*OAuth/, /URL elicitation.*not supported/, '{"ok":2}', 'no output', /non-text/, /invalid MCP tool result/, /mismatched/, /expected an MCP JSON/]) {
    const { output, is_error } = await s.call('lookup', '{}');
    assert.equal(is_error, typeof expected !== 'string');
    if (is_error) assert.match(output, expected);
    else assert.equal(output, expected);
  }
  assert.equal(srv.calls.length, 12, 'no auth URL followed and no tool invocation retried');
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
  assert.deepEqual(srv.calls.map(c => c.method), ['initialize', 'notifications/initialized']);
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
  assert.deepEqual(srv.calls.slice(3).map(c => c.method), ['tools/call', 'DELETE']);
});

test('401 during initialization prompts once and retries only the bound resource with its bearer', async () => {
  const issuer = 'https://issuer.example', redirect = 'https://app.example/callback';
  const attempts = [], discoveryHeaders = [];
  let prompts = 0;
  const toolFetch = async (url, init, policy) => {
    assert.deepEqual(policy,{toolProxy:proxy,allowPrivateIPs:false});
    if (url !== endpoint) {
      discoveryHeaders.push(new Headers(init.headers));
      if (url.endsWith('/.well-known/oauth-protected-resource/tools')) return Response.json({resource:endpoint,authorization_servers:[issuer]});
      if (url === issuer+'/.well-known/oauth-authorization-server') return Response.json({issuer,
        authorization_endpoint:issuer+'/auth',token_endpoint:issuer+'/token',response_types_supported:['code'],
        authorization_response_iss_parameter_supported:true, token_endpoint_auth_methods_supported:['none'],code_challenge_methods_supported:['S256']});
      if (url === issuer+'/token') return Response.json({access_token:'session-only',token_type:'Bearer'});
      assert.fail('unexpected discovery or token URL');
    }
    attempts.push(init.headers.get('Authorization'));
    if (!attempts.at(-1)) return new Response(null,{status:401,headers:{'WWW-Authenticate':'Bearer'}});
    assert.equal(attempts.at(-1),'Bearer session-only');
    assert.equal(init.headers.get('X-Private'),'resource-only');
    const msg=JSON.parse(init.body);
    if (msg.method === 'initialize') return result(msg,hello);
    if (msg.method === 'notifications/initialized') return ack();
    if (msg.method === 'tools/list') return result(msg,{tools:[]});
    assert.fail('unexpected RPC');
  };
  const session = await openMCP({type:'mcp',remote:{url:endpoint,headers:{'X-Private':'resource-only'},oauth:{clientId:'public-client'}}}, {}, proxy, undefined, {
    toolFetch, oauthRedirectURI:redirect, elicit: async event => {
      prompts++;
      const state=new URL(event.url).searchParams.get('state');
      return {action:'accept',content:{callback_url:redirect+'?'+new URLSearchParams({code:'test',state,iss:issuer})}};
    },
  });
  await session.close();
  assert.equal(prompts,1);
  assert.equal(attempts.length,4);
  assert.ok(discoveryHeaders.length >= 3 && discoveryHeaders.every(h=>!h.has('Authorization') && !h.has('X-Private')));
});
