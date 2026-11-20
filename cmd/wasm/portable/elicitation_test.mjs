import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { before, test } from 'node:test';
import boot from '../node/boot.js';

const catalog = JSON.parse(readFileSync(new URL('./src/tool_catalog.json', import.meta.url)));
const options = { yaml: `providers: {local: {provider: openai, base_url: 'http://localhost:9000/v1'}}
agents: {root: {model: local/test, instruction: Hello, toolsets: [{type: user_prompt}, {type: think}]}}` };
const chatOptions = { ...options, messages: [{ role: 'user', content: 'go' }] };
let api;
before(async () => { api = await boot.bootDockerAgent(); });

const schema = { type: 'string', enum: ['red', 'green'] };
function prompt(index = 0, args = { message: 'Which?', title: 'Color', schema }) {
  return { index, type: 'function', id: `call_${index}`, function: { name: 'user_prompt', arguments: JSON.stringify(args) } };
}

function model(t, calls = [prompt()]) {
  const requests = [];
  t.mock.method(globalThis, 'fetch', (_, init) => {
    const body = JSON.parse(init.body);
    requests.push(body);
    const asking = body.messages.at(-1).role === 'user';
    const delta = asking ? { tool_calls: calls } : { content: 'done' };
    const chunk = { choices: [{ index: 0, delta, finish_reason: asking ? 'tool_calls' : 'stop' }] };
    return new Response(`data: ${JSON.stringify(chunk)}\n\ndata: [DONE]\n\n`, { headers: { 'Content-Type': 'text/event-stream' } });
  });
  return requests;
}

function lastAnswer(requests) {
  const message = requests.at(-1).messages.at(-1);
  assert.equal(message.role, 'tool');
  return JSON.parse(message.content);
}

test('session elicitation uses explicit replies and replays an ordered batch of tool results', { timeout: 5000 }, async t => {
  const requests = model(t, [prompt(),
    { index: 1, id: 'thought', type: 'function', function: { name: 'think', arguments: '{"thought":"plan"}' } },
    prompt(2, { message: 'More?' }),
  ]);
  const first = Promise.withResolvers(), answeredSecond = Promise.withResolvers();
  const events = [];
  const s = await api.createSession(options, event => {
    events.push(event);
    if (event.type !== 'elicitation') return;
    if (event.message === 'Which?') {
      first.resolve(event);
      return 'decline'; // Sessions ignore even usable callback replies.
    }
    s.respondToElicitation({ id: event.id, action: 'cancel' }).then(answeredSecond.resolve, answeredSecond.reject);
  });
  t.after(() => s.close());
  const turn = s.send('choose');
  const { id, ...event } = await first.promise;
  await answeredSecond.promise;
  assert.notEqual(id, 'call_0');
  assert.deepEqual(event, { type: 'elicitation', message: 'Which?', mode: '', meta: { 'cagent/title': 'Color' }, url: '', server_elicitation_id: '', schema });
  await assert.rejects(s.respondToElicitation({ action: 'accept' }), /id is required/);
  await assert.rejects(s.respondToElicitation({ id: 'call_0', action: 'accept' }), /pending/);
  await assert.rejects(s.respondToElicitation({ id, action: 'ok' }), /unknown elicitation action/);
  await assert.rejects(s.respondToElicitation({ id, action: 'accept', content: { text: 'x'.repeat(65536) } }), /size limit/);
  assert.equal(requests.length, 1);
  await s.respondToElicitation({ id, action: 'accept', content: { color: 'green' } });
  await assert.rejects(s.respondToElicitation({ id, action: 'decline' }), /pending/);
  assert.equal((await turn).message.content, 'done');
  assert.deepEqual(events.map(e => e.type), [
    'tool_call_delta', 'tool_call_delta', 'tool_call_delta', 'tool_call',
    'tool_call', 'tool_result', 'tool_call', 'elicitation', 'elicitation', 'tool_result', 'tool_result', 'delta', 'finish',
  ]);
  assert.deepEqual(events.filter(e => e.type === 'tool_result').map(e => e.is_error), [false, true, false]);
  const second = events.find(e => e.message === 'More?');
  assert.notEqual(second.id, id);
  assert.deepEqual([second.meta, second.schema], [null, null]);
  assert.equal(requests.length, 2);
  for (const request of requests) {
    assert.equal(request.tool_choice, 'auto');
    assert.deepEqual(request.tools, [...catalog.user_prompt.tools, ...catalog.think.tools]);
    assert.equal(request.messages[0].content, `Hello\n${catalog.user_prompt.instructions}\n${catalog.think.instructions}`);
  }
  const [assistant, think, cancelled, accepted] = requests[1].messages.slice(-4);
  assert.equal('content' in assistant, false);
  assert.deepEqual(assistant.tool_calls.map(c => c.id), ['call_0', 'thought', 'call_2']);
  assert.deepEqual(accepted, { role: 'tool', tool_call_id: 'call_0', content: '{"action":"accept","content":{"color":"green"}}' });
  assert.deepEqual(think, { role: 'tool', tool_call_id: 'thought', content: 'Thoughts:\nplan' });
  assert.deepEqual(cancelled, { role: 'tool', tool_call_id: 'call_2', content: '{"action":"cancel"}' });
});

test('sessions without a handler decline; stateless chat awaits only elicitation replies', { timeout: 5000 }, async t => {
  const requests = model(t);
  const session = await api.createSession(options);
  try { await session.send('go'); } finally { await session.close(); }
  assert.deepEqual(lastAnswer(requests), { action: 'decline' });
  for (const [callback, expected] of [
    [undefined, { action: 'decline' }], [() => Promise.reject(new Error('no')), { action: 'decline' }],
    [() => { throw new Error('no'); }, { action: 'decline' }], [() => ({}), { action: 'decline' }],
    [() => 'cancel', { action: 'cancel' }], [() => true, { action: 'accept' }],
    [() => Promise.resolve({ action: 'accept', content: { n: 0, ok: false } }), { action: 'accept', content: { n: 0, ok: false } }],
  ]) {
    await api.chat(chatOptions, callback && (ev => ev.type === 'elicitation' ? callback() : new Promise(() => {})));
    assert.deepEqual(lastAnswer(requests), expected);
  }
});

for (const method of ['abort', 'restart', 'close']) {
  test(`${method} wakes a pending session and invalidates its elicitation ID`, { timeout: 5000 }, async t => {
    const requests = model(t);
    let question = Promise.withResolvers();
    const session = await api.createSession(options, e => { if (e.type === 'elicitation') question.resolve(e.id); });
    t.after(() => session.close());
    const abandoned = assert.rejects(session.send('old'), /aborted/);
    const id = await question.promise;
    const stopped = session[method]();
    await assert.rejects(session.respondToElicitation({ id, action: 'accept' }), /pending|closed/);
    await stopped;
    await abandoned;
    assert.equal(requests.length, 1);
    if (method === 'close') return;

    question = Promise.withResolvers();
    const fresh = session.send('fresh');
    const current = await question.promise;
    assert.notEqual(current, id);
    assert.deepEqual(requests[1].messages.filter(m => m.role === 'user').map(m => m.content), ['fresh']);
    await assert.rejects(session.respondToElicitation({ id, action: 'accept' }), /pending/);
    await session.respondToElicitation({ id: current, action: 'decline' });
    await fresh;
    assert.deepEqual(lastAnswer(requests), { action: 'decline' });
  });
}

test('replacing or aborting stateless chat releases an unresolved callback and ignores its late reply', { timeout: 5000 }, async t => {
  const requests = model(t);
  for (const replace of [false, true]) {
    const question = Promise.withResolvers(), reply = Promise.withResolvers();
    const start = requests.length;
    const aborted = assert.rejects(api.chat(chatOptions, e => {
      if (e.type === 'elicitation') { question.resolve(); return reply.promise; }
    }), /aborted/);
    await question.promise;
    if (replace) await api.chat(chatOptions, () => 'cancel');
    else api.abort();
    await aborted;
    reply.resolve('accept');
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(requests.length, start + (replace ? 3 : 1));
    if (replace) assert.deepEqual(lastAnswer(requests), { action: 'cancel' });
  }
});
