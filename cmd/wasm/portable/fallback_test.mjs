import assert from 'node:assert/strict';
import { before, test } from 'node:test';
import boot from '../node/boot.js';

let api;
before(async () => { api = await boot.bootDockerAgent(); });

const options = {
  yaml: `providers:
  p: {provider: openai, base_url: https://primary.invalid/v1, token_key: FIRST}
  b: {api_type: openai_responses, base_url: https://backup.invalid/v1, token_key: SECOND}
models:
  primary: {provider: p, model: primary, temperature: 0.25}
  backup: {provider: b, model: backup, temperature: 0, max_tokens: 23}
agents:
  root: {model: primary, instruction: Be concise, toolsets: [{type: think}], fallback: {models: [backup]}}
`,
  env: { FIRST: 'first-token', SECOND: 'second-token' },
};

function stream(chunks, chat = true) {
  const records = chunks.map(c => `data: ${JSON.stringify(c)}\n\n`).join('');
  return new Response(records + (chat ? 'data: [DONE]\n\n' : ''), { headers: { 'Content-Type': 'text/event-stream' } });
}
const piece = (delta, finish_reason = null) => ({ choices: [{ index: 0, delta, finish_reason }] });
const chat = () => stream([piece({ content: 'primary here' }, 'stop')]);
const responses = () => stream([
  { type: 'response.output_text.delta', output_index: 0, delta: 'backup here' },
  { type: 'response.completed', response: { id: 'r', status: 'completed', output: [] } },
], false);
const error = (status, message) => Response.json({ error: { message } }, { status });

function script(t, ...steps) {
  const requests = [];
  t.mock.method(globalThis, 'fetch', (url, init) => {
    requests.push({ url, auth: init.headers.Authorization, body: JSON.parse(init.body), redirect: init.redirect, credentials: init.credentials });
    return steps[requests.length - 1]();
  });
  return requests;
}

async function session(t, yaml = options.yaml, onEvent) {
  const events = [];
  const s = await api.createSession({ ...options, yaml }, e => { events.push(e); onEvent?.(e); });
  t.after(() => s.close());
  return { s, events };
}

test('reports a fallback, changes wire format and credentials, and pins only until cooldown expires', async t => {
  let now = 0;
  t.mock.method(performance, 'now', () => now);
  const requests = script(t, () => error(429, 'primary is down'), responses, responses, chat);
  const { s, events } = await session(t);
  assert.equal((await s.send('hi')).message.content, 'backup here');
  const [p, b] = requests;
  assert.deepEqual([p.url, p.auth, b.url, b.auth], ['https://primary.invalid/v1/chat/completions', 'Bearer first-token', 'https://backup.invalid/v1/responses', 'Bearer second-token']);
  for (const r of requests) assert.deepEqual([r.credentials, r.redirect], ['omit', 'error']);
  assert.equal(p.body.temperature, 0.25);
  assert.equal(b.body.temperature, 0);
  assert.equal(b.body.max_output_tokens, 23);
  assert.equal(b.body.messages, undefined);
  assert.equal(b.body.input[0].content[0].text, 'Be concise'); // Unmerged original system messages.
  assert.equal(b.body.input[1].role, 'system');
  assert.deepEqual(b.body.input[2], { role: 'user', content: 'hi' });
  assert.deepEqual(b.body.tools.map(t => t.name), p.body.tools.map(t => t.function.name));
  assert.deepEqual(events.map(e => e.type), ['fallback', 'delta', 'finish']);
  assert.deepEqual(events[0], { type: 'fallback', from: 'openai/primary', to: 'b/backup', attempt: 1, reason: 'model request failed: HTTP 429: primary is down' });
  now = 59_999;
  await s.send('again');
  assert.deepEqual(requests[2].body.input.slice(-2), [{ role: 'assistant', content: 'backup here' }, { role: 'user', content: 'again' }]);
  now = 60_000;
  await s.send('recovered');
  assert.deepEqual(requests.map(r => r.body.model), ['primary', 'backup', 'backup', 'primary']);
  assert.equal(events.filter(e => e.type === 'fallback').length, 1);
});

test('transient primary failure retries the identical request before switching, without pinning', async t => {
  const requests = script(t, () => error(503, 'down'), () => error(500, 'down'), () => error(408, 'slow'), responses, chat);
  const { s, events } = await session(t);
  await s.send('once');
  await s.send('twice');
  assert.deepEqual(requests.map(r => r.body.model), ['primary', 'primary', 'primary', 'backup', 'primary']);
  assert.deepEqual(requests[1], requests[0]);
  assert.deepEqual(requests[2], requests[0]);
  assert.equal(events.filter(e => e.type === 'fallback').length, 1);
});

test('defaults also retry transient transport failure on a single model', async t => {
  const requests = script(t, () => { throw new TypeError('Failed to fetch'); }, chat);
  const { s, events } = await session(t, options.yaml.replace(', fallback: {models: [backup]}', ''));
  assert.equal((await s.send('hi')).message.content, 'primary here');
  assert.deepEqual(requests[1], requests[0]);
  assert.deepEqual(events.map(e => e.type), ['delta', 'finish']);
});

test('pre-output stream errors advance an ordered named/inline chain; final error is reported once', async t => {
  const requests = script(t,
    () => stream([piece({ role: 'assistant' }), { error: { message: 'primary is down' } }]),
    () => stream([{ type: 'response.created', response: { id: 'r', output: [] } }, { type: 'response.failed', response: { error: { code: 'server_error', message: 'backup down' } } }], false),
    () => error(529, 'third down'));
  const { s, events } = await session(t, options.yaml.replace('[backup]', '[backup, p/third], retries: -1'));
  await assert.rejects(s.send('hi'), /all models failed: .*529: third down/);
  assert.deepEqual(requests.map(r => r.body.model), ['primary', 'backup', 'third']);
  assert.deepEqual(events.map(e => e.type), ['fallback', 'fallback', 'error']);
  assert.deepEqual(events.slice(0, 2).map(e => [e.from, e.to, e.attempt]), [['openai/primary', 'b/backup', 1], ['b/backup', 'openai/third', 1]]);
  assert.match(events[0].reason, /primary is down/);
  assert.match(events[1].reason, /backup down/);
});

for (const [name, delta] of Object.entries({ text: { content: 'partial' }, reasoning: { reasoning_content: 'partial' },
  tool: { tool_calls: [{ index: 0, function: { name: 'th' } }] } })) {
  test(`never retries after partial ${name}, even before an incomplete call is announced`, async t => {
    const requests = script(t, () => stream([piece(delta), { error: { message: '503' } }]));
    const { s, events } = await session(t);
    await assert.rejects(s.send('no replay'), /503/);
    assert.equal(requests.length, 1);
    assert.ok(!events.some(e => ['fallback', 'finish', 'tool_result'].includes(e.type)));
  });
}

test('a failure after a completed tool does not replay the model or tool and retains the result', async t => {
  const requests = script(t, () => stream([piece({ tool_calls: [{ id: 'call', index: 0, type: 'function', function: { name: 'think', arguments: '{"thought":"one"}' } }] }, 'tool_calls')]), () => error(500, 'followup failed'), chat);
  const { s, events } = await session(t);
  await assert.rejects(s.send('think'), /followup failed/);
  assert.equal(requests.length, 2);
  assert.deepEqual(events.filter(e => e.type === 'tool_result').map(e => e.output), ['Thoughts:\none']);
  assert.ok(!events.some(e => ['fallback', 'finish'].includes(e.type)));
  await s.send('recover');
  assert.deepEqual(requests[2].body.messages.filter(m => m.role === 'tool'), [requests[1].body.messages.at(-1)]);
});

test('abort while sleeping releases the turn without starting a retry', async t => {
  const requests = script(t, () => error(503, 'down'), chat);
  const { s, events } = await session(t);
  const sleeping = Promise.withResolvers();
  const timer = globalThis.setTimeout;
  t.mock.method(globalThis, 'setTimeout', (fn, ms, ...args) => {
    if (ms >= 180 && ms <= 220) sleeping.resolve();
    return timer(fn, ms, ...args);
  });
  const result = s.send('cancel');
  await sleeping.promise;
  s.abort();
  await assert.rejects(result, /aborted/);
  assert.equal(requests.length, 1);
  assert.deepEqual(events, []);
  await s.send('fresh');
  assert.deepEqual(requests[1].body.messages.filter(m => m.role === 'user').map(m => m.content), ['fresh']);
});

test('abort from the fallback callback does not dispatch the selected backup', async t => {
  const requests = script(t, () => error(401, 'bad key'));
  const { s, events } = await session(t, options.yaml, e => { if (e.type === 'fallback') s.abort(); });
  await assert.rejects(s.send('cancel'), /aborted/);
  assert.equal(requests.length, 1);
  assert.deepEqual(events.map(e => e.type), ['fallback']);
});

test('validates every fallback target before issuing a request', async t => {
  const requests = script(t);
  for (const [yaml, expected] of [
    [options.yaml.replace('[backup]', '[absent]'), /not found/],
    [options.yaml.replace('[backup]', '[anthropic/test]'), /unsupported provider/],
    [options.yaml.replace('key: SECOND', 'key: MISSING'), /MISSING/],
    [options.yaml.replace('https://backup.invalid', 'http://backup.invalid'), /HTTPS/],
    [options.yaml.replace('model: backup,', 'base_url: https://elsewhere.invalid, model: backup,'), /cross-origin/],
    [options.yaml.replace('model: backup,', 'frequency_penalty: 0.5, model: backup,'), /does not support/],
    [options.yaml.replace('[backup]', '[backup], retries: -2'), /retries/],
    [options.yaml.replace('[backup]', '[backup], cooldown: 1d'), /cooldown/],
    [options.yaml.replace('[backup]', '[backup], cooldown_ns: 1000'), /unknown field/],
  ]) await assert.rejects(api.createSession({ ...options, yaml }), expected);
  assert.equal(requests.length, 0);
});

test('cross-protocol failover rejects history that Responses cannot replay', async t => {
  const requests = script(t, () => error(404, 'missing'));
  const messages = [{ role: 'user', content: 'first', name: 'Ada' }, { role: 'user', content: 'again' }];
  await assert.rejects(api.chat({ ...options, messages }), /cannot replay/);
  assert.equal(requests.length, 1);
  assert.deepEqual(requests[0].body.messages.slice(-2), messages);
});

test('a keyless backup receives neither the primary key nor another provider key', async t => {
  const requests = script(t, () => error(403, 'not allowed'), responses);
  const { s } = await session(t, options.yaml.replace(', token_key: SECOND', ''));
  await s.send('hi');
  assert.deepEqual(requests.map(r => r.auth), ['Bearer first-token', undefined]);
});

test('a Responses tool fragment blocks failover even if nothing was announced', async t => {
  const requests = script(t, () => stream([
    { type: 'response.function_call_arguments.delta', item_id: 'unannounced', delta: '{' },
    { type: 'response.failed', response: { error: { code: '500', message: 'down' } } },
  ], false));
  const { s, events } = await session(t, options.yaml.replace('root: {model: primary', 'root: {model: backup').replace('[backup]', '[primary]'));
  await assert.rejects(s.send('hi'), /down/);
  assert.equal(requests.length, 1);
  assert.deepEqual(events.map(e => e.type), ['error']);
});

test('HTTP and streaming diagnostics cannot expose a target credential in fallback or error events', async t => {
  script(t, () => error(401, 'invalid bearer first-token'), () => stream([
    { type: 'response.failed', response: { error: { message: 'invalid bearer second-token' } } },
  ], false));
  const { s, events } = await session(t);
  await assert.rejects(s.send('go'), /all models failed: provider diagnostic contained credentials/);
  assert.deepEqual(events.map(e => e.type), ['fallback', 'error']);
  assert.doesNotMatch(JSON.stringify(events), /first-token|second-token/);
});

const imageHistory = [
  { role: 'user', multi_content: [{ type: 'document', document: { name: 'small.gif', mime_type: 'image/gif', source: { inline_data: 'R0lGODlhAQABAAAAACwAAAAAAQABAAA=' } } }] },
  { role: 'user', content: 'describe the image' },
];

function visionConfig(backup, protocol = 'openai_chatcompletions') {
  return `providers:
  p: {provider: openai, base_url: https://primary.invalid/v1, token_key: FIRST}
  b: {provider: openai, base_url: https://backup.invalid/v1, token_key: SECOND, api_type: ${protocol}}
models:
  first: {provider: p, model: vision, capabilities: {image: true}}
  second: {provider: b, model: vision-backup, capabilities: ${backup}}
agents: {root: {model: first, fallback: {models: [second]}}}`;
}

test('compatible fallback replays the image bytes but never the primary credentials', async t => {
  const requests = script(t, () => error(429, 'unavailable'), () => stream([piece({ content: 'backup described it' }, 'stop')]));
  const events = [];
  const result = await api.chat({ yaml: visionConfig('{image: true}'), env: options.env, messages: imageHistory }, e => events.push(e));
  assert.equal(result.message.content, 'backup described it');
  assert.deepEqual(requests.map(r => [r.body.model, r.auth, r.url]), [
    ['vision', 'Bearer first-token', 'https://primary.invalid/v1/chat/completions'],
    ['vision-backup', 'Bearer second-token', 'https://backup.invalid/v1/chat/completions'],
  ]);
  assert.deepEqual(requests[1].body.messages, requests[0].body.messages);
  assert.ok(requests[1].body.messages.some(m => Array.isArray(m.content) && m.content.some(p => p.image_url?.url === 'data:image/gif;base64,R0lGODlhAQABAAAAACwAAAAAAQABAAA=')));
  assert.equal(events.filter(e => e.type === 'fallback').length, 1);
});

test('a later capable backup gets the original image bytes even after two text-only attempts', async t => {
  const yaml = visionConfig('{image: true}', 'openai_responses')
    .replace('model: vision, capabilities: {image: true}', 'model: vision, capabilities: {image: false}')
    .replace('[second]', '[p/text-only, second]');
  const requests = script(t, () => error(401, 'unavailable'), () => error(401, 'unavailable'), responses);
  await api.chat({ yaml, env: options.env, messages: imageHistory });
  assert.deepEqual(requests.map(r => [r.body.model, r.auth]), [
    ['vision', 'Bearer first-token'], ['text-only', 'Bearer first-token'], ['vision-backup', 'Bearer second-token'],
  ]);
  for (const req of requests.slice(0, 2)) assert.doesNotMatch(JSON.stringify(req.body), /image_url|R0lGOD/);
  assert.ok(requests[2].body.input.some(m => Array.isArray(m.content) && m.content.some(p =>
    p.type === 'input_image' && p.image_url === 'data:image/gif;base64,R0lGODlhAQABAAAAACwAAAAAAQABAAA=')));
});

test('checks malformed or unreplayable seed history before any model or discovery request', async t => {
  const requests = script(t);
  const yaml = visionConfig('{image: true}', 'openai_responses')
    .replace('model: vision, capabilities: {image: true}', 'model: vision, capabilities: {image: false}')
    .replace('model: first,', 'model: first, toolsets: [{type: mcp, remote: {url: https://mcp.invalid}}],');
  for (const [first, reason] of [
    [{ ...imageHistory[0], name: 'Alice' }, /named or reasoning/],
    [{ ...imageHistory[0], reasoning_content: 'internal' }, /named or reasoning/],
    [{ ...imageHistory[0], provider_state: {} }, /provider state/],
    [{ role: 'user', multi_content: [{ type: 'image_url', image_url: { url: 'file:///tmp/image.png' } }] }, /image URL/],
    [{ role: 'user', multi_content: [{ type: 'document', document: { name: 'bad.gif', mime_type: 'image/gif', source: {inline_data:'AA=='} } }] }, /signature/],
  ]) await assert.rejects(api.chat({ yaml, env: options.env, messages: [first, imageHistory.at(-1)] }), reason);
  assert.equal(requests.length, 0);
});
