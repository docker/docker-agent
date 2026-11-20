import assert from 'node:assert/strict';
import { before, test } from 'node:test';
import boot from '../node/boot.js';

let api;
before(async () => { api = await boot.bootDockerAgent(); });

function mock(t, rounds) {
  const requests = [];
  t.mock.method(globalThis, 'fetch', async (url, init) => {
    const req = new Request(url, init);
    requests.push(await req.json());
    const call = rounds.shift();
    assert.notEqual(call, undefined, 'unexpected model request');
    const delta = call ? { tool_calls: [{ index: 0, id: 'one', type: 'function', function: call }] } : { content: 'ok' };
    const chunk = { id: 'test', object: 'chat.completion.chunk', model: 'test', choices: [{ index: 0, delta, finish_reason: call ? 'tool_calls' : 'stop' }] };
    return new Response(`data: ${JSON.stringify(chunk)}\n\ndata: [DONE]\n\n`, { headers: { 'Content-Type': 'text/event-stream' } });
  });
  return requests;
}

function options(extra = '') {
  return { yaml: `agents:\n  root:\n    model: openai/gpt-4o-mini\n    toolsets: [{type: think}, {type: user_prompt}]\n${extra}`, env: { OPENAI_API_KEY: 'test' } };
}

for (const enabled of [true, false]) {
  test(`redact_secrets ${enabled ? 'defaults on' : 'can be disabled'}`, async t => {
    const token = 'dckr_pat_' + 'A'.repeat(27);
    const expected = enabled ? '[REDACTED]' : token;
    const requests = mock(t, [{ name: 'think', arguments: JSON.stringify({ thought: token }) }, null]);
    const events = [];
    const session = await api.createSession(options(enabled ? '' : '    redact_secrets: false'), e => events.push(e));
    try { await session.send(token); } finally { await session.close(); }
    assert.equal(requests.length, 2);
    for (const body of requests) assert.equal(body.messages.find(m => m.role === 'user').content, expected);
    const result = events.find(e => e.type === 'tool_result');
    assert.equal(result.output, 'Thoughts:\n' + expected);
    assert.equal(JSON.parse(events.find(e => e.type === 'tool_call').args).thought, expected);
    assert.equal(events.some(e => e.type === 'warning' && e.message.includes('redact_secrets')), enabled);
    assert.equal(requests[1].messages.at(-1).content, result.output);
    assert.equal(JSON.parse(requests[1].messages.find(m => m.tool_calls).tool_calls[0].function.arguments).thought, expected);
  });
}

test('bounded model calls preserve completed elicitation, emit its result and reject before another fetch', async t => {
  const requests = mock(t, [{ name: 'user_prompt', arguments: '{"message":"ok?"}' }, null]);
  const events = [];
  const opts = options('    hooks:\n      before_llm_call: {type: builtin, command: max_iterations, args: ["1"]}');
  const session = await api.createSession(opts, async e => {
    events.push(e);
    if (e.type === 'elicitation') await session.respondToElicitation({ id: e.id, action: 'decline' });
  });
  try {
    await assert.rejects(session.send('ask'), /max_iterations builtin reached its limit of 1 model call/);
    assert.equal(requests.length, 1);
    assert.deepEqual(events.filter(e => ['tool_result', 'error', 'finish'].includes(e.type)).map(e => e.type), ['tool_result', 'error']);
    await session.send('new budget');
    assert.equal(requests[1].messages.some(m => m.role === 'tool'), true);
  } finally { await session.close(); }
});

test('dynamic flags and equivalent hooks add one transient system message per source', async t => {
  const requests = mock(t, [{ name: 'think', arguments: '{"thought":"ok"}' }, null, null]);
  const opts = options('    add_date: true\n    add_environment_info: true\n    hooks:\n      turn_start: [{type: builtin, command: add_date}]\n      session_start: {type: builtin, command: add_environment_info}');
  const session = await api.createSession(opts);
  try {
    await session.send('first');
    await session.restart();
    await session.send('second');
  } finally { await session.close(); }
  assert.equal(requests.length, 3);
  for (const { messages } of requests) {
    const env = messages.filter(m => m.content?.includes('Operating System: js'));
    const dates = messages.filter(m => /^Today's date: \d{4}-\d{2}-\d{2}$/.test(m.content));
    assert.equal(env.length, 1);
    assert.equal(dates.length, 1);
    assert.equal(dates[0].role, 'system');
    assert.equal(env[0].role, 'system');
    assert.ok(messages.indexOf(env[0]) < messages.indexOf(dates[0]));
    assert.ok(messages.indexOf(dates[0]) < messages.findIndex(m => m.role === 'user'));
  }
});

test('date is scoped to a model round across retries and sessions, including limited tool history', async t => {
  const day = n => Date.UTC(2030, 4, n, 12) + new Date().getTimezoneOffset() * 60_000;
  t.mock.timers.enable({ apis: ['Date'], now: day(2) });
  const yaml = `providers: {local: {provider: openai, base_url: https://model.invalid/v1, token_key: KEY}}
agents:
  root:
    model: local/main
    session_compaction: false
    num_history_items: 4
    fallback: {models: [local/backup], retries: 1}
    hooks: {turn_start: {type: builtin, command: add_date}}
    toolsets: [{type: think}]`;
  const opts = {yaml, env:{KEY:'key'}};
  const requests = [];
  const result = (delta, finish_reason = 'stop') => new Response(`data: ${JSON.stringify({choices:[{index:0, delta, finish_reason}]})}\n\ndata: [DONE]\n\n`,
    {headers:{'Content-Type':'text/event-stream'}});
  const other = await api.createSession(opts);
  const s = await api.createSession(opts);
  t.after(() => Promise.all([s.close(), other.close()]));
  t.mock.method(globalThis, 'fetch', async (_, init) => {
    requests.push(JSON.parse(init.body));
    switch (requests.length) {
      case 1:
        t.mock.timers.setTime(day(3));
        await other.send('independent');
        return new Response(null, {status:503});
      case 3: return new Response(null, {status:401});
      case 4:
        t.mock.timers.setTime(day(4));
        return result({tool_calls:[{id:'a', index:0, type:'function', function:{name:'think',arguments:'{"thought":"once"}'}}]}, 'tool_calls');
      default: return result({content:'ok'});
    }
  });
  const secret = 'dckr_pat_' + 'A'.repeat(27);
  await s.send(secret);
  t.mock.timers.setTime(day(5));
  await s.send('again');
  assert.deepEqual(requests.map(r => r.model), ['main', 'main', 'main', 'backup', 'backup', 'backup']);
  assert.deepEqual(requests.map(r => /Today's date: (\d{4}-\d{2}-\d{2})/.exec(r.messages[0].content)?.[1]),
    ['2030-05-02', '2030-05-03', '2030-05-02', '2030-05-02', '2030-05-04', '2030-05-05']);
  assert.deepEqual(requests[0].messages, requests[2].messages);
  assert.deepEqual(requests[2].messages, requests[3].messages);
  assert.equal(requests[3].messages.at(-1).content, '[REDACTED]');
  assert.equal(JSON.stringify(requests).includes(secret), false);
  assert.equal(requests[4].messages.at(-1).content, 'Thoughts:\nonce');
});

test('explicit output redaction protects elicitation events and model history even with flag off', async t => {
  const token = 'dckr_pat_' + 'B'.repeat(27);
  const requests = mock(t, [{ name: 'user_prompt', arguments: '{"message":"choose"}' }, null]);
  const events = [];
  const opts = options('    redact_secrets: false\n    hooks:\n      tool_response_transform: {matcher: "user_prompt", hooks: {type: builtin, command: redact_secrets}}');
  const s = await api.createSession(opts, async e => {
    events.push(e);
    if (e.type === 'elicitation') await s.respondToElicitation({ id: e.id, action: 'accept', content: { answer: token } });
  });
  try { await s.send(token); } finally { await s.close(); }
  assert.equal(requests[0].messages.at(-1).content, token);
  const output = events.find(e => e.type === 'tool_result').output;
  assert.equal(JSON.parse(output).content.answer, '[REDACTED]');
  assert.equal(requests[1].messages.at(-1).content, output);
});
