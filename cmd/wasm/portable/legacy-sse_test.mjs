import assert from 'node:assert/strict';
import { test } from 'node:test';
import { openMCP } from './mcp.mjs';
import { toolFetch as guardedFetch } from './http-tools.mjs';

const base = 'https://mcp.example.test/tools/sse?key=open-only';
const sessionPath = '../message?s=opaque', pinned = new URL(sessionPath, base).href;
const toolProxy = 'https://guard.example.test/';
const toolset = { type: 'mcp', remote: { transport_type: 'sse', url: base, headers: { Authorization: 'Bearer ${env.TOKEN}' } } };
const hello = { protocolVersion: '2024-11-05', capabilities: { tools: {} }, serverInfo: { name: 'legacy', version: '1' } };
const tool = { name: 'echo', description: 'Echo', inputSchema: { type: 'object' } };
const ack = () => new Response('Accepted', { status: 202 });
const encoder = new TextEncoder();

function server({ first = `event: endpoint\r\ndata: ${sessionPath}\r\n\r\n`, onGet, onPost } = {}) {
  const calls = [], cancelled = Promise.withResolvers();
  let stream;
  function write(text, fragmented) {
    const bytes = encoder.encode(text);
    if (fragmented) for (let i = 0; i < bytes.length; i++) stream.enqueue(bytes.subarray(i, i + 1));
    else stream.enqueue(bytes);
  }
  function event(message, fragmented) {
    write(`event: message\r\ndata: ${JSON.stringify({ jsonrpc: '2.0', ...message })}\r\n\r\n`, fragmented);
  }
  const toolFetch = async (url, init, policy, signal) => {
    const msg = init.body && JSON.parse(init.body);
    calls.push({ url, init, msg, policy, signal });
    if (init.method === 'GET') {
      const body = new ReadableStream({
        start(c) { stream = c; if (first) write(first); }, cancel: cancelled.resolve,
      });
      return onGet ? onGet(body, signal) : new Response(body, { headers: { 'Content-Type': 'text/event-stream; charset=utf-8' } });
    }
    assert.equal(init.method, 'POST');
    const res = onPost?.(msg, signal);
    if (res) return res;
    if (msg.method === 'initialize') event({ id: msg.id, result: hello });
    else if (msg.method === 'tools/list') event({ id: msg.id, result: { tools: [tool] } });
    else assert.equal(msg.method, 'notifications/initialized');
    return ack();
  };
  const env = { TOKEN: 'session-only' };
  return { calls, toolFetch, cancelled: cancelled.promise, env, event, write, end() { stream.close(); },
    open: (signal, options = {}, ts = toolset) => openMCP(ts, env, toolProxy, signal, { toolFetch, ...options }),
  };
}

test('one guarded GET; replies and notifications on SSE, including out-of-order concurrent calls', async t => {
  const raw = t.mock.method(globalThis, 'fetch', () => assert.fail('unexpected raw fetch'));
  const started = Promise.withResolvers(), ackSecond = Promise.withResolvers(), ids = [];
  const srv = server({ onPost(msg) {
    if (msg.method === 'tools/call') {
      ids.push(msg.id);
      if (ids.length === 2) { started.resolve(); return ackSecond.promise; }
      return ack();
    }
  } });
  const discovery = new AbortController();
  const s = await srv.open(discovery.signal, { elicit: () => assert.fail('unexpected prompt') });
  discovery.abort(); // The initial GET must survive the end of the discovery turn.
  srv.env.TOKEN = 'rotated';
  assert.deepEqual(s.tools, [{ mcpStrict: true, type: 'function', function: { name: tool.name, description: tool.description, parameters: {type: 'object', additionalProperties: false, required: [], properties: {}} } }]);
  assert.equal((await s.call('absent', {})).is_error, true);
  const a = s.call('echo', '{"n":1}'), b = s.call('echo', { n: 2 });
  await started.promise;
  srv.event({ method: 'notifications/progress', params: { progress: 1 } });
  srv.event({ id: ids[1], result: { content: [{ type: 'text', text: 'café' }] } }, true);
  srv.event({ id: ids[0], result: { content: [{ type: 'text', text: 'first' }] } });
  assert.deepEqual(await a, { output: 'first', is_error: false });
  ackSecond.resolve(ack());
  assert.deepEqual(await b, { output: 'café', is_error: false });
  const closed = s.close();
  assert.equal(s.close(), closed);
  await closed;
  await srv.cancelled;
  await assert.rejects(s.call('echo', {}), /closed/);
  assert.deepEqual(srv.calls.map(c => c.msg?.method ?? c.init.method), ['GET', 'initialize', 'notifications/initialized', 'tools/list', 'tools/call', 'tools/call']);
  assert.deepEqual(srv.calls[1].msg.params.capabilities, {});
  for (const [i, { url, msg, init, policy }] of srv.calls.entries()) {
    assert.deepEqual(policy, { toolProxy, allowPrivateIPs: false });
    assert.equal(init.redirect, 'error');
    assert.equal(init.credentials, 'omit');
    assert.equal(url, i ? pinned : base);
    assert.equal(init.headers.get('Authorization'), i < 4 ? 'Bearer session-only' : 'Bearer rotated');
    assert.equal(init.headers.get('Content-Type'), i ? 'application/json' : null);
    assert.equal(init.headers.get('MCP-Session-Id'), null);
    assert.equal(init.headers.get('MCP-Protocol-Version'), null);
    assert.equal(init.headers.get('Last-Event-ID'), null);
    if (i) {
      assert.equal(init.headers.get('Accept'), null);
      assert.equal(msg.jsonrpc, '2.0');
    } else assert.equal(init.headers.get('Accept'), 'text/event-stream');
  }
  assert.equal(raw.mock.callCount(), 0);
});

test('reject unsafe endpoint announcements before issuing any POST', async () => {
  for (const raw of ['https://elsewhere.test/', '//elsewhere.test/', 'http://mcp.example.test/', 'https://mcp.example.test:8443/',
    'https://u:p@mcp.example.test/', '//@mcp.example.test/', 'https:////mcp.example.test/', '/path#fragment', '', '/a\\b',
    'blob:https://mcp.example.test/foo', 'data:text/plain,foo', 'https://mcp.example.test.attacker.test/']) {
    const srv = server({ first: `event: endpoint\ndata: ${raw}\n\n` });
    await assert.rejects(srv.open(), /invalid or cross-origin MCP SSE endpoint/);
    await srv.cancelled;
    assert.deepEqual(srv.calls.map(c => c.url), [base]);
  }
});

test('two sessions through the real egress guard keep their POST endpoints and tokens separate', async t => {
  const servers = new Map(), opens = [];
  const network = t.mock.method(globalThis, 'fetch', (url, init) => {
    const proxy = new URL(url);
    assert.equal(proxy.origin, new URL(toolProxy).origin);
    assert.equal(init.redirect, 'error');
    assert.equal(init.credentials, 'omit');
    const target = proxy.searchParams.get('url');
    assert.equal(init.headers.get('Authorization'), `Bearer ${new URL(target).pathname.split('/')[1]}`);
    return servers.get(target).toolFetch(target, init, {}, init.signal);
  });
  for (const id of ['one', 'two']) {
    const get = `https://same.test/${id}/stream`, post = `https://same.test/${id}/msg`;
    const srv = server({ first: 'event: endpoint\ndata: msg\n\n', onPost(msg) {
      if (msg.method === 'tools/call') {
        srv.event({ id: msg.id, result: { content: [{ type: 'text', text: id }] } });
        return ack();
      }
    } });
    servers.set(get, srv); servers.set(post, srv);
    srv.env.TOKEN = id;
    opens.push(srv.open(undefined, { toolFetch: guardedFetch }, { ...toolset, remote: { ...toolset.remote, url: get } }));
  }
  const sessions = await Promise.all(opens);
  try {
    assert.deepEqual(await Promise.all(sessions.map(s => s.call('echo', {}))), [
      { output: 'one', is_error: false }, { output: 'two', is_error: false },
    ]);
  } finally { await Promise.all(sessions.map(s => s.close())); }
  assert.equal(network.mock.callCount(), 10);
});

test('a second endpoint, bad ID, unsupported server request, catalog change or EOF retires the stream', async () => {
  for (const [send, error] of [
    [srv => srv.write('event: endpoint\ndata: /replacement\n\n'), /endpoint changed/],
    [srv => srv.write('event: endpoint\ndata: https://elsewhere.test/\n\n'), /endpoint changed/],
    [srv => srv.event({ id: 'other', result: {} }), /unexpected MCP SSE response ID/],
    [srv => srv.event({ method: 'elicitation/create', id: 1, params: { message: 'ask?' } }), /server requests are not supported/],
    [srv => srv.event({ method: 'notifications/tools/list_changed' }), /catalog changed/],
    [srv => srv.end(), /stream ended/],
    [srv => srv.write('data: ' + 'x'.repeat(1 << 20)), /too large/],
  ]) {
    const started = Promise.withResolvers();
    const srv = server({ onPost(msg) { if (msg.method === 'tools/call') { started.resolve(); return ack(); } } });
    const s = await srv.open();
    const call = s.call('echo', {});
    await started.promise;
    send(srv);
    await assert.rejects(call, error);
    await assert.rejects(s.call('echo', {}), error);
    await s.close();
    assert.deepEqual(srv.calls.map(c => c.url), [base, pinned, pinned, pinned, pinned]);
  }
});

test('a legacy session never discovers OAuth, reconnects, or replays a rejected POST', async () => {
  const opts = { oauthRedirectURI: 'https://app.test/cb', elicit: () => assert.fail('unexpected prompt') };
  const anonymous = { ...toolset, remote: { ...toolset.remote, headers: {} } };
  const body = { status: 401, headers: { 'WWW-Authenticate': 'Bearer resource_metadata="https://elsewhere.test/discovery"' } };
  const get = server({ onGet: stream => new Response(stream, body) });
  await assert.rejects(get.open(undefined, opts, { ...toolset, remote: { ...toolset.remote, oauth: {} } }), /OAuth is not supported/);
  assert.equal(get.calls.length, 0);
  await assert.rejects(get.open(undefined, opts, anonymous), /HTTP 401/);
  await get.cancelled;
  assert.deepEqual(get.calls.map(c => c.url), [base]);
  for (const [response, error] of [[() => new Response(null, body), /HTTP 401/], [() => Response.redirect('https://elsewhere.test/'), /HTTP 302/],
    [() => { throw new Error('failed to POST ' + pinned); }, /^Error: MCP transport request failed$/]]) {
    const srv = server({ onPost(msg) { if (msg.method === 'tools/call') return response(); } });
    const s = await srv.open(undefined, opts, anonymous);
    await assert.rejects(s.call('echo', {}), error);
    await s.close();
    await srv.cancelled;
    assert.deepEqual(srv.calls.map(c => c.url), [base, pinned, pinned, pinned, pinned]);
  }
});

test('invalid stream, first event or POST acknowledgement rolls back discovery', async () => {
  for (const [setup, failure] of [
    [{ first: 'data: {}\n\n' }, /expected MCP SSE endpoint event/],
    [{ onGet: stream => new Response(stream, { headers: { 'Content-Type': 'application/json' } }) }, /event-stream response/],
    [{ onGet: stream => Object.defineProperty(new Response(stream), 'redirected', { value: true }) }, /unexpected MCP redirect/],
    [{ onPost: msg => msg.method === 'initialize' && Response.json({ id: msg.id, result: hello }) }, /expected MCP SSE acknowledgement/],
  ]) {
    const srv = server(setup);
    await assert.rejects(srv.open(), failure);
    await srv.cancelled;
    assert.deepEqual(srv.calls.map(c => c.url), setup.onPost ? [base, pinned] : [base]);
  }
});

test('cancel and deadline cover endpoint discovery; a pre-aborted open never fetches', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  for (const timeout of [false, true]) {
    const started = Promise.withResolvers(), controller = new AbortController();
    const srv = server({ first: ': connecting\n\n', onGet(body) {
      started.resolve(); return new Response(body, { headers: { 'Content-Type': 'text/event-stream' } });
    } });
    const open = srv.open(controller.signal);
    await started.promise;
    if (timeout) t.mock.timers.tick(30000);
    else controller.abort();
    await assert.rejects(open, { name: timeout ? 'TimeoutError' : 'AbortError' });
    await srv.cancelled;
    assert.ok(srv.calls[0].signal.aborted);
    await assert.rejects(srv.open(AbortSignal.abort()), { name: 'AbortError' });
    assert.deepEqual(srv.calls.map(c => c.url), [base]);
  }
});

test('cancel, timeout and close release pending calls and the persistent GET', async t => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  for (const action of ['cancel', 'timeout', 'close']) {
    const started = Promise.withResolvers(), controller = new AbortController();
    const srv = server({ onPost(msg) { if (msg.method === 'tools/call') { started.resolve(); return ack(); } } });
    const s = await srv.open();
    const call = s.call('echo', {}, controller.signal);
    await started.promise;
    if (action === 'cancel') controller.abort();
    if (action === 'timeout') t.mock.timers.tick(30000);
    if (action === 'close') s.close();
    await assert.rejects(call, /abort|timed out|closed/i);
    await s.close();
    await srv.cancelled;
    assert.ok(srv.calls[0].signal.aborted);
    assert.deepEqual(srv.calls.map(c => c.url), [base, pinned, pinned, pinned, pinned]);
  }
});
