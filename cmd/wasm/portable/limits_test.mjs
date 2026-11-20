import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { before, test } from 'node:test';
import boot from '../node/boot.js';

const reference = JSON.parse(readFileSync(new URL('./testdata/session-limits.json', import.meta.url)));
const provider = 'providers: {local: {provider: openai, base_url: "https://model.invalid/v1", token_key: KEY}}';
const config = (fields, models = '') => `${provider}\n${models}\nagents: {root: {model: local/test, ${fields}}}`;
const opts = yaml => ({ yaml, env: { KEY: 'test', OTHER: 'test' } });
const echo = '{type: api, allow_private_ips: true, api_config: {method: GET, name: echo, endpoint: "https://tool.invalid/echo"}}';
const call = (name, args = {}, id = 'a', index = 0) => ({index, id, type: 'function', function: {name, arguments: JSON.stringify(args)}});
let api;
before(async () => { api = await boot.bootDockerAgent(); });

function intercept(t, rounds, output = reference.source) {
  const requests = [];
  t.mock.method(globalThis, 'fetch', async (url, init) => {
    const req = new Request(url, init);
    if (req.url === 'https://tool.invalid/echo') {
      assert.equal(req.headers.has('Authorization'), false);
      return new Response(output);
    }
    assert.equal(req.url, 'https://model.invalid/v1/chat/completions');
    assert.equal(req.headers.get('Authorization'), 'Bearer test');
    requests.push(await req.json());
    assert.ok(rounds.length, 'unexpected model request');
    const calls = rounds.shift();
    if (typeof calls === 'number') return new Response('unavailable', {status: calls});
    const chunk = {choices: [{index: 0, delta: calls ? {tool_calls: calls} : {content: 'done'}, finish_reason: calls ? 'tool_calls' : 'stop'}]};
    const usage = {choices: [], usage: {prompt_tokens: 10, completion_tokens: 5}};
    return new Response(`data: ${JSON.stringify(chunk)}\n\ndata: ${JSON.stringify(usage)}\n\ndata: [DONE]\n\n`, {headers: {'Content-Type': 'text/event-stream'}});
  });
  return requests;
}

for (const oldBudget of [reference.max_old_tool_call_tokens, reference.max_tool_result_tokens]) {
  test(`agent stop commits tool results, history and usage (old budget ${oldBudget})`, async t => {
    const requests = intercept(t, [[call('echo')], null]);
    const events = [];
    const yaml = config(`toolsets: [${echo}], max_iterations: 1, max_tool_result_tokens: ${reference.max_tool_result_tokens}, max_old_tool_call_tokens: ${oldBudget}, hooks: {before_llm_call: {type: builtin, command: max_iterations, args: ['1']}}`);
    const s = await api.createSession(opts(yaml), e => events.push(e));
    try {
      assert.deepEqual(await s.send('first'), {message: {role: 'assistant', content: reference.stop}, usage: {input_tokens: 10, output_tokens: 5}});
      assert.equal(requests.length, 1);
      assert.equal(events.find(e => e.type === 'tool_result').output, reference.source);
      assert.equal(events.at(-2).content, reference.stop);
      assert.deepEqual(events.at(-1), {type: 'finish', reason: 'stop'});
      assert.equal((await s.send('again')).message.content, 'done');
      const tail = requests[1].messages.slice(-3);
      assert.deepEqual(tail.map(m => m.content), [oldBudget === reference.max_old_tool_call_tokens ? reference.projected_result : reference.stored_result, reference.stop, 'again']);
    } finally { await s.close(); }
  });
}

test('transient failover spends one iteration and the next send starts fresh on primary', async t => {
  const requests = intercept(t, [503, [call('echo')], [call('echo', {}, 'b')]]);
  const yaml = config(`max_iterations: 1, session_compaction: false, fallback: {models: [local/backup], retries: -1}, toolsets: [${echo}]`);
  const s = await api.createSession(opts(yaml));
  try {
    assert.equal((await s.send('first')).message.content, reference.stop);
    assert.deepEqual(requests.map(r => r.model), ['test', 'backup']);
    assert.equal((await s.send('again')).message.content, reference.stop);
    assert.equal(requests.length, 3);
    assert.equal(requests[2].model, 'test');
    assert.deepEqual(requests[2].messages.slice(-3).map(m => m.content), [reference.source, reference.stop, 'again']);
  } finally { await s.close(); }
});

test('failed continuation retries the capped history, not the tool, and the next send trims the pair', async t => {
  const requests = intercept(t, [[call('echo')], 400, 400, null]);
  const yaml = config(`num_history_items: 3, max_tool_result_tokens: 4, session_compaction: false, fallback: {models: [local/backup], retries: -1}, toolsets: [${echo}]`);
  const events = [];
  const s = await api.createSession(opts(yaml), e => events.push(e));
  try {
    await assert.rejects(s.send('first'), /all models failed/);
    assert.deepEqual(events.filter(e => e.type === 'tool_result').map(e => e.output), [reference.source]);
    assert.equal(requests[1].messages.at(-1).content, reference.stored_result);
    assert.deepEqual(requests[2].messages, requests[1].messages);
    await s.send('again');
    assert.deepEqual(requests.map(r => r.model), ['test', 'test', 'backup', 'test']);
    assert.deepEqual(requests[3].messages.filter(m => m.role !== 'system'), [{role: 'user', content: 'first\nagain'}]);
  } finally { await s.close(); }
});

test('the maximum routed cap keeps eight tool results without a ninth model call', async t => {
  const calls = Array.from({length:8}, (_, i) => [call('think', {thought:`step ${i}`}, `id_${i}`)]);
  const requests = intercept(t, [503, ...calls, null]);
  const yaml = config('max_iterations: 8, session_compaction: false, fallback: {models: [local/backup], retries: -1}, toolsets: [{type: think}]');
  const s = await api.createSession(opts(yaml));
  try {
    assert.match((await s.send('start')).message.content, /max_iterations limit \(8\)/);
    assert.deepEqual(requests.map(r => r.model), ['test', 'backup', ...Array(7).fill('test')]);
    await s.send('new turn');
    assert.deepEqual(requests.at(-1).messages.filter(m => m.role === 'tool').map(m => m.tool_call_id),
      calls.map(([c]) => c.id));
  } finally { await s.close(); }
});

test('pending user_prompt is answered and capped before agent stop', async t => {
  const requests = intercept(t, [[call('user_prompt', {message:'ok?'})], null]);
  const yaml = config('toolsets: [{type: user_prompt}], max_iterations: 1, max_tool_result_tokens: 1');
  const s = await api.createSession(opts(yaml));
  try {
    assert.equal((await s.send('ask')).message.content, reference.stop);
    await s.send('again');
    assert.equal(requests[1].messages.at(-3).content, '{...');
  } finally { await s.close(); }
});

test('first matching toolset in model call order overrides exactly one round', async t => {
  const requests = intercept(t, [[call('echo'), call('calculate', {expression:'1+1'}, 'b', 1)], [call('think', {thought:'hi'})], null, null]);
  const routed = echo.replace('type: api', 'type: api, model: local/cheap');
  const yaml = config(`toolsets: [{type: calculator, model: local/other}, ${routed}, {type: think}]`);
  const s = await api.createSession(opts(yaml));
  try { await s.send('first'); await s.send('again'); } finally { await s.close(); }
  assert.deepEqual(requests.map(r => r.model), ['test', 'cheap', 'test', 'test']);
});

const seed = (content, parts) => [
  {role: 'assistant', tool_calls: [call('echo')]},
  {role: 'tool', tool_call_id: 'a', content, multi_content: parts},
  {role: 'user', content: 'go'},
];

test('seeded text results are capped on the actual request (Go len/4 and UTF-8 cuts)', async t => {
  const marker = '\n[...tool result truncated: middle omitted...]\n';
  const cases = [
    [1, 'ééé', 'ééé'], [1, '🌏🌏', '...'], [4, reference.source, reference.stored_result],
    [20, '🌏'.repeat(80), '🌏'.repeat(4) + marker + '🌏'.repeat(4)],
  ];
  const requests = intercept(t, cases.map(() => null));
  for (const [tokens, content, expected] of cases) {
    await api.chat({...opts(config(`max_tool_result_tokens: ${tokens}`)), messages: seed(content)});
    assert.equal(requests.at(-1).messages.find(m => m.role === 'tool').content, expected);
  }
});

test('unsafe projections and model routes reject before any request', async t => {
  const requests = intercept(t, []);
  for (const field of ['num_history_items: 1', 'max_tool_result_tokens: 1', 'max_old_tool_call_tokens: 1', 'toolsets: [{type: think, model: local/other}]']) {
    await assert.rejects(api.chat({...opts(config(field)), messages: seed('secret', [{type:'text', text:'duplicate'}])}), /text-only history/);
  }
  for (const yaml of [
    config('max_iterations: 9'), config('max_iterations: -1'), config('max_tool_result_tokens: -2'), config('num_history_items: -1'),
    config('toolsets: [{type: session_context}], max_old_tool_call_tokens: 1'),
    config('max_iterations: 1, toolsets: [{type: think, model: local/cheap}], sub_agents: [root]'),
    config('toolsets: [{type: think, model: local/cheap}], fallback: {models: [local/other]}'),
    config('max_tool_result_tokens: 4', 'models: {local/test: {provider: local, model: test, context_window: 2048}}'),
    ...['missing', 'openai/test', 'local/gpt-5', 'local/a,local/b'].map(model => config(`toolsets: [{type: think, model: '${model}'}]`)),
    ...['base_url: https://elsewhere.invalid', 'token_key: OTHER', 'context_window: 2048'].map(extra => config('toolsets: [{type: think, model: cheap}]', `models: {cheap: {provider: local, model: cheap, ${extra}}}`)),
  ]) await assert.rejects(api.createSession(opts(yaml)));
  const token = 'dckr_pat_' + 'A'.repeat(27);
  await assert.rejects(api.chat({...opts(config('max_tool_result_tokens: 4')), messages: seed(token)}), /cannot safely truncate/);
  assert.equal(requests.length, 0);
});

test('fallback history limits reject incompatible configurations and seeds before I/O', async t => {
  const requests = intercept(t, []);
  const route = 'session_compaction: false, fallback: {models: [local/backup]}';
  const fields = `num_history_items: 3, ${route}`;
  for (const [yaml, reason] of [
    [config(fields.replace('session_compaction: false, ', '')), /requires session_compaction: false/],
    [config(fields.replace('false', 'true'), 'models: {local/test: {provider: local, model: test, context_window: 4096}}'), /require session_compaction: false/],
    [config(`${fields}, handoffs: [root]`), /history limits with local teams/],
    [config(`${fields}, toolsets: [{type: session_context}]`), /session_context/],
    [config(`${fields}, toolsets: [{type: think, model: local/cheap}]`), /toolset model overrides/],
    [config(`${fields}, structured_output: {name: out, mode: tool, schema: {type: object, properties: {}}}`), /tool-mode structured output/],
    [config(`${fields}, hooks: {before_llm_call: {type: builtin, command: max_iterations, args: ['2']}}`), /history limits with an iteration limit/],
    [config(fields.replace('[local/backup]', '[local/backup, local/gpt-5]')), /Chat Completions on every target/],
  ]) await assert.rejects(api.createSession(opts(yaml)), reason);
  for (const field of ['num_history_items: 1', 'max_tool_result_tokens: 4', 'max_old_tool_call_tokens: 3']) {
    const options = opts(config(`${field}, ${route}`));
    await assert.rejects(api.chat({...options, messages: seed('text', [{type:'text', text:'part'}])}), /text-only history/);
  }
  assert.equal(requests.length, 0);
});

test('routed iteration cap rejects incompatible settings before any request', async t => {
  const requests = intercept(t, []);
  const fields = 'max_iterations: 1, session_compaction: false, fallback: {models: [local/backup]}';
  for (const [yaml, reason] of [
    [config(fields.replace('max_iterations: 1', 'max_iterations: 9')), /tool round limit/],
    [config(fields.replace('session_compaction: false, ', '')), /requires session_compaction: false/],
    [config(`fallback: {models: [local/backup]}, hooks: {before_llm_call: {type: builtin, command: max_iterations, args: ['2']}}`), /requires session_compaction: false/],
    [config(`${fields}, hooks: {before_llm_call: {type: builtin, command: max_iterations, args: ['9']}}`), /portable tool round limit/],
    [config(`${fields.replace('max_iterations: 1, ', '')}, hooks: {before_llm_call: {type: builtin, command: max_iterations, args: ['2']}}, structured_output: {name: out, mode: tool, schema: {type: object, properties: {}}}`), /tool-mode structured output/],
    [config(`${fields}, sub_agents: [root]`), /max_iterations with local teams/],
    [config(`${fields}, num_history_items: 1`), /history limits/],
    [config(`${fields}, toolsets: [{type: think, model: local/cheap}]`), /toolset model overrides/],
    [config(`${fields}, structured_output: {name: out, mode: tool, schema: {type: object, properties: {}}}`), /tool-mode structured output/],
  ]) await assert.rejects(api.createSession(opts(yaml)), reason);
  assert.equal(requests.length, 0);
});

test('old tool budget is newest first, uses floor(bytes/4), and never rewrites arguments', async t => {
  const requests = intercept(t, [null]);
  const exchanges = ['abc', 'four', 'abcdefgh', '😀', 'hi'].flatMap((text, i) => [
    {role: 'assistant', tool_calls: [call('echo', {source: i}, `tool-${i}`)]},
    {role: 'tool', tool_call_id: `tool-${i}`, content: text},
  ]);
  await api.chat({...opts(config('max_old_tool_call_tokens: 2')), messages: [...exchanges, {role:'user', content:'go'}]});
  const messages = requests[0].messages;
  assert.deepEqual(messages.filter(m => m.role === 'tool').map(m => m.content), ['abc', '[content truncated]', '[content truncated]', '😀', 'hi']);
  assert.deepEqual(messages.filter(m => m.tool_calls).map(m => JSON.parse(m.tool_calls[0].function.arguments).source), [0,1,2,3,4]);
});

test('before-LLM-only redaction cannot be bypassed by capping a live host tool result', async t => {
  const token = 'dckr_pat_' + 'B'.repeat(27);
  const requests = intercept(t, [[call('echo')], null], token);
  const yaml = config(`toolsets: [${echo}], max_tool_result_tokens: 4, redact_secrets: false, hooks: {before_llm_call: {type: builtin, command: redact_secrets}}`);
  const s = await api.createSession(opts(yaml));
  try {
    await assert.rejects(s.send('call echo'), /cannot safely truncate/);
    assert.equal(requests.length, 1);
    await s.send('after error');
    assert.equal(requests[1].messages.some(m => m.role === 'tool'), false);
  } finally { await s.close(); }
});


test('finish is the last event even when the only pending host call is first in a batch', async t => {
  intercept(t, [[call('echo'), call('calculate', {expression:'1'}, 'b', 1)]]);
  const events = [];
  const s = await api.createSession(opts(config(`toolsets: [${echo}, {type: calculator}], max_iterations: 1`)), e => events.push(e));
  try { assert.equal((await s.send('go')).message.content, reference.stop); } finally { await s.close(); }
  assert.equal(events.filter(e => e.type === 'tool_result').length, 2);
  assert.deepEqual(events.slice(-2).map(e => e.type), ['delta', 'finish']);
});
