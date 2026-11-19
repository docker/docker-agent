import assert from 'node:assert/strict';
import { readFileSync, statSync, readdirSync } from 'node:fs';
import { createServer } from 'node:http';
import { join, dirname } from 'node:path';
import { test, before } from 'node:test';
import boot from '../node/boot.js';

const artifact = boot.wasmPath;
let api;
before(async () => { api = await boot.bootDockerAgent(); });

function config(endpoint) {
  return `providers:
  local:
    provider: openai
    base_url: ${endpoint}/v1
    token_key: LOCAL_KEY
agents:
  root:
    model: local/test
    instruction: 'Hello \${env.WHO}'
    toolsets:
      - type: think
`;
}

function chunk(delta, finish_reason = null) {
  return { id: 'chatcmpl-test', object: 'chat.completion.chunk', model: 'test', choices: [{ index: 0, delta, finish_reason }] };
}

async function mockModel(t, script) {
  const requests = [];
  const server = createServer(async (req, res) => {
    try {
      assert.equal(req.url, '/v1/chat/completions');
      assert.equal(req.method, 'POST');
      assert.equal(req.headers.authorization, 'Bearer per-session-only');
      const buffers = [];
      for await (const part of req) buffers.push(part);
      const request = JSON.parse(Buffer.concat(buffers));
      requests.push(request);
      res.writeHead(200, { 'Content-Type': 'text/event-stream; charset=utf-8' });
      const events = script(request, requests.length);
      for (const event of events) res.write(`data: ${JSON.stringify(event)}\r\n\r\n`);
      res.end('data: [DONE]\r\n\r\n');
    } catch (e) { res.destroy(e); }
  });
  await new Promise((resolve, reject) => server.once('error', reject).listen(0, '127.0.0.1', resolve));
  t.after(() => { server.closeAllConnections(); return new Promise(resolve => server.close(resolve)); });
  const options = { yaml: config(`http://127.0.0.1:${server.address().port}`), env: { LOCAL_KEY: 'per-session-only', WHO: 'Ada' } };
  return { options, requests };
}

test('the complete runtime is under 2 MiB and has no Go, WASI or other host imports', async () => {
  const bytes = readFileSync(artifact);
  assert.deepEqual(WebAssembly.Module.imports(await WebAssembly.compile(bytes)), []);
  const size = bytes.length + readdirSync(dirname(artifact)).filter(f => f.endsWith('.mjs')).reduce((n, f) => n + statSync(join(dirname(artifact), f)).size, 0);
  assert.ok(size < 2 * 1024 * 1024, `${size} bytes including the JS host`);
});

test('streams a tool turn, tracks usage and history, then restarts cleanly', async t => {
  const usage = { choices: [], usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 } };
  const { options, requests } = await mockModel(t, (_, n) => n === 1 ? [
    chunk({ tool_calls: [{ index: 0, id: 'call_1', type: 'function', function: { name: 'think', arguments: '{"thou' } }] }),
    chunk({ tool_calls: [{ index: 0, function: { arguments: 'ght":"plan"}' } }] }, 'tool_calls'), usage,
  ] : [chunk({ role: 'assistant', content: 'Hello' }), chunk({ content: ' 🌍' }, 'stop'), usage]);
  const events = [];
  const session = await api.createSession(options, ev => events.push(ev));
  t.after(() => session.close());
  const result = await session.send('hi');
  assert.deepEqual(result, { message: { role: 'assistant', content: 'Hello 🌍' }, usage: { input_tokens: 6, output_tokens: 4 } });
  assert.equal(events.filter(e => e.type === 'finish').length, 1);
  assert.equal(events.filter(e => e.type === 'usage').length, 2);
  assert.equal(events.find(e => e.type === 'tool_result').output, 'Thoughts:\nplan');
  assert.equal(events.filter(e => e.type === 'delta').map(e => e.content).join(''), 'Hello 🌍');
  assert.equal(requests[0].model, 'test');
  assert.match(requests[0].messages[0].content, /Hello Ada/);
  assert.deepEqual(requests[0].tools.map(x => x.function.name), ['think']);
  assert.equal(requests[1].messages.at(-1).tool_call_id, 'call_1');
  await session.send('again');
  assert.equal(requests[2].messages.at(-2).content, 'Hello 🌍');
  await session.restart();
  await session.send('fresh');
  assert.deepEqual(requests[3].messages.filter(m => m.role === 'user').map(m => m.content), ['fresh']);
});

test('stateless chat preserves history and aborts synchronously', async t => {
  const { options, requests } = await mockModel(t, () => [chunk({ content: 'ok' }, 'stop')]);
  const messages = [{ role: 'user', content: 'first' }, { role: 'assistant', content: 'yes' }, { role: 'user', content: 'again' }];
  const aborted = api.chat({ ...options, messages });
  api.abort();
  await assert.rejects(aborted, /aborted/);
  assert.equal(requests.length, 0);
  const result = await api.chat({ ...options, messages });
  assert.equal(result.message.content, 'ok');
  assert.deepEqual(requests[0].messages.slice(-3), messages);
});

test('rejects unsupported developer turns and final-prompt attachments before fetch', async t => {
  const { options, requests } = await mockModel(t, () => [chunk({ content: 'unexpected' }, 'stop')]);
  const prompt = { role: 'user', content: 'go' };
  await assert.rejects(api.chat({ ...options, messages: [{ role: 'developer', content: 'rule' }, prompt] }), /unsupported history role/);
  await assert.rejects(api.chat({ ...options, messages: [{ ...prompt, multi_content: [{ type: 'text', text: 'extra' }] }] }), /final user prompt is text-only/);
  assert.equal(requests.length, 0);
});

test('rejects missing credentials and unsupported capabilities before fetch', async () => {
  const yaml = config('https://example.invalid');
  await assert.rejects(api.createSession({ yaml }), /LOCAL_KEY/);
  for (const type of ['mcp', 'shell', 'filesystem', 'openapi']) {
    await assert.rejects(api.createSession({ yaml: yaml.replace('type: think', `type: ${type}`), env: { LOCAL_KEY: 'k' } }));
  }
  await assert.rejects(api.createSession({ yaml: yaml + 'permissions: {}\n', env: { LOCAL_KEY: 'k' } }));
});

test('a broken stream rejects, emits an error, and releases the turn', async t => {
  const { options } = await mockModel(t, (_, n) => n === 1 ? [chunk({ content: 'partial' })] : [chunk({ content: 'ok' }, 'stop')]);
  const events = [];
  const s = await api.createSession(options, e => events.push(e));
  t.after(() => s.close());
  await assert.rejects(s.send('broken'));
  assert.ok(events.some(e => e.type === 'error'));
  assert.ok(!events.some(e => e.type === 'finish'));
  assert.equal((await s.send('retry')).message.content, 'ok');
});


test('an in-flight fetch can be stopped from the delta callback', async t => {
  const server = createServer((req, res) => {
    req.resume();
    res.writeHead(200, { 'Content-Type': 'text/event-stream' });
    res.write(`data: ${JSON.stringify(chunk({ content: 'tick' }))}\n\n`);
    // Keep the stream open until abort cancels the fetch.
  });
  await new Promise((resolve, reject) => server.once('error', reject).listen(0, '127.0.0.1', resolve));
  t.after(() => { server.closeAllConnections(); return new Promise(resolve => server.close(resolve)); });
  const yaml = config(`http://127.0.0.1:${server.address().port}`);
  const events = [];
  const session = await api.createSession({ yaml, env: { LOCAL_KEY: 'k' } }, ev => {
    events.push(ev.type);
    if (ev.type === 'delta') session.abort();
  });
  const turn = session.send('slow');
  await assert.rejects(session.send('again'), /already active/);
  await assert.rejects(turn, /aborted/);
  assert.ok(events.includes('delta'));
  assert.ok(!events.includes('finish'));
  await session.close();
  await session.close();
  await assert.rejects(session.send('late'), /closed/);
});
