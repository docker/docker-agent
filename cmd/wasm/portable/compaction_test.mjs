import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { before, test } from 'node:test';
import boot from '../node/boot.js';

const yaml = `providers: {local: {api_type: openai_chatcompletions, base_url: 'https://fixture.invalid/v1'}}
models: {chosen: {provider: local, model: test, context_window: 2048, max_tokens: 2000, compaction_threshold: 0.5}}
agents: {root: {model: chosen, instruction: 'keep this policy', session_compaction: true, redact_secrets: false, compaction_threshold: 1, toolsets: [{type: think}]}}`;
const answer = (content, finish_reason = 'stop') => ({ choices: [{ index: 0, delta: { content }, finish_reason }] });
const usage = (prompt_tokens, completion_tokens) => ({ choices: [], usage: { prompt_tokens, completion_tokens } });
function stream(...chunks) {
  return new Response(chunks.map(c => `data: ${JSON.stringify(c)}\n\n`).join('') + 'data: [DONE]\n\n', { headers: { 'Content-Type': 'text/event-stream' } });
}
function intercept(t, handler) {
  const requests = [];
  t.mock.method(globalThis, 'fetch', (_, init) => {
    const request = JSON.parse(init.body);
    requests.push(request);
    return handler(request, requests.length, init.signal);
  });
  return requests;
}
let api;
before(async () => { api = await boot.bootDockerAgent(); });

test('explicit unsupported strategies, thresholds and unknown windows reject before fetch', async t => {
  const requests = intercept(t, () => { throw new Error('unexpected fetch'); });
  for (const config of [
    yaml.replace('context_window: 2048, ', ''),
    yaml.replace('window: 2048', 'window: 4294967296'),
    yaml.replace('threshold: 0.5', 'threshold: 0'),
    yaml.replace('threshold: 0.5', 'threshold: 1.01'),
    yaml.replace('threshold: 0.5', 'threshold: .nan'),
    yaml.replace('session_compaction: true', 'session_compaction: {strategy: truncate}'),
    yaml.replace('model: chosen,', 'model: chosen, compaction_model: chosen,'),
    yaml.replace('provider: local,', 'provider: local, provider_opts: {native_compaction: true},'),
    yaml.replace('model: chosen,', 'model: chosen, hooks: {before_compaction: []},'),
    yaml + '\n---\nagents: {root: {model: local/test}}',
  ]) await assert.rejects(api.createSession({ yaml: config }), undefined, config);
  assert.equal(requests.length, 0);
});

const chain = readFileSync(new URL('./testdata/fallback-compaction.yaml', import.meta.url), 'utf8');
const env = { MODEL_URL: 'https://primary.invalid/v1', LOCAL_KEY: 'primary-only', BACKUP_KEY: 'backup-only' };

test('incompatible or tool-using compaction routes reject before model I/O', async t => {
  const requests = intercept(t, () => assert.fail('unexpected model request'));
  for (const yaml of [
    chain.replace('model: spare, context_window: 2048', 'model: spare, context_window: 8192'),
    chain.replace('openai_chatcompletions', 'openai_responses'),
    chain.replace('    fallback:', '    toolsets: [{type: think}]\n    fallback:'),
    chain.replace('name: answer', 'mode: tool, name: answer'),
    chain.replace('    fallback:', '    handoffs: [other]\n    fallback:') + '\n  other: {model: main}',
  ]) await assert.rejects(api.createSession({ yaml, env }), /compaction/);
  await assert.rejects(api.chat({ yaml: chain, env, messages: [
    { role: 'user', multi_content: [{ type: 'text', text: 'earlier' }] }, { role: 'user', content: 'go' },
  ] }), /multipart history with routed compaction/);
  assert.equal(requests.length, 0);
});

test('summary retries stay on the primary with its bearer; continuation selects a fresh route', async t => {
  const yaml = chain.replace('model: spare,', 'model: spare, base_url: https://backup.invalid/v1, token_key: BACKUP_KEY,');
  const sent = [];
  let now = 0;
  t.mock.method(performance, 'now', () => now);
  t.mock.method(globalThis, 'fetch', (url, init) => {
    const body = JSON.parse(init.body);
    sent.push(body);
    const backup = body.model === 'spare';
    assert.equal(new URL(url).hostname, backup ? 'backup.invalid' : 'primary.invalid');
    assert.equal(init.headers.Authorization, 'Bearer ' + (backup ? env.BACKUP_KEY : env.LOCAL_KEY));
    switch (sent.length) {
      case 1: return stream({ error: { message: 'primary unavailable' } });
      case 2: return stream(answer('first answer'), usage(1024, 1));
      case 3: return stream(answer('discard this fragment', null), { error: { code: 503, message: 'retry summary' } });
      case 4:
        now = 3_600_001; // The old pin expires while the summary is running.
        return stream(answer('key facts'), usage(20, 1));
      case 5: return stream({ error: { message: 'primary still unavailable' } });
      case 6: return stream(answer('done'), usage(20, 2));
      default: assert.fail('extra model request');
    }
  });
  const events = [];
  const s = await api.createSession({ yaml, env }, e => events.push(e));
  t.after(() => s.close());
  await s.send('old '.repeat(500));
  assert.equal((await s.send('continue')).message.content, 'done');
  assert.deepEqual(sent.map(b => b.model), ['main', 'spare', 'main', 'main', 'main', 'spare']);
  assert.deepEqual(sent[2], sent[3]);
  assert.match(sent[3].messages[0].content, /summarize conversations/);
  assert.equal(sent[3].response_format, undefined);
  assert.equal(sent[3].temperature, 0.25);
  assert.equal(sent[5].temperature, 0.75);
  assert.deepEqual(sent[5].messages, sent[4].messages);
  assert.match(sent[4].messages[0].content, /Session Summary: key facts/);
  assert.equal(events.filter(e => e.type === 'fallback').length, 2);
  assert.doesNotMatch(JSON.stringify([sent, events]), /discard this fragment/);
});

test('summary overflow or malformed output never drains an uncompressed fallback request', async t => {
  let response;
  const requests = intercept(t, (_, n) => n === 1 ? stream({ error: { message: 'primary unavailable' } })
    : n === 2 ? stream(answer('first'), usage(1024, 1)) : stream(...response));
  const s = await api.createSession({ yaml: chain, env });
  t.after(() => s.close());
  await s.send('old '.repeat(500));
  response = [{ error: { message: 'context_length_exceeded' } }];
  await assert.rejects(s.send('overflow'), /context overflow/);
  response = [answer('partial', 'length')];
  await assert.rejects(s.send('malformed'), /complete, non-empty text summary/);
  assert.deepEqual(requests.map(r => r.model), ['main', 'spare', 'main', 'main']);
  assert.equal(requests.at(-1).messages.some(m => m.content?.includes('overflow')), false);
});

test('disabled compaction still reports the window and clamps the ordinary completion cap', async t => {
  const requests = intercept(t, () => stream(answer('answer'), usage(1500, 3)));
  const events = [];
  const s = await api.createSession({ yaml: yaml.replace('session_compaction: true', 'session_compaction: false') }, e => events.push(e));
  t.after(() => s.close());
  await s.send('first');
  await s.send('second');
  assert.equal(requests.length, 2);
  assert.equal(requests[1].max_tokens, 1024);
  assert.equal(requests[1].messages[1].content, 'first');
  assert.equal(events.filter(e => e.type === 'usage').at(-1).context_limit, 2048);
});

test('summary failures cannot apply partial output, execute tools, or poison a retry', async t => {
  const failures = [
    [answer('  ')], [answer('bad', null)], [answer('cut', 'length')], [answer('filtered', 'content_filter')],
    [{ error: { message: 'unavailable' } }],
    [{ choices: [{ delta: { tool_calls: [{ index: 0, id: 'bad', type: 'function', function: { name: 'think', arguments: '{"thought":"bad"}' } }] }, finish_reason: 'tool_calls' }] }],
  ];
  const requests = intercept(t, (_r, n) => {
    if (n === 1) return stream(answer('old answer'), usage(1024, 1));
    if (n <= failures.length + 1) return stream(...failures[n - 2]);
    return stream(answer(n === failures.length + 2 ? '  key facts  ' : 'done'), usage(20, 2));
  });
  const events = [];
  const s = await api.createSession({ yaml }, e => events.push(e));
  t.after(() => s.close());
  await s.send('old question');
  for (let i = 0; i < failures.length; i++) await assert.rejects(s.send(`bad turn ${i}`));
  assert.equal((await s.send('retry')).message.content, 'done');
  for (let i = 1; i < requests.length - 1; i++) {
    const r = requests[i];
    assert.equal(r.tools, undefined);
    assert.equal(r.max_tokens, 512);
    assert.match(r.messages[0].content, /summarize conversations/);
    assert.equal(r.messages[1].content, 'old question');
    assert.equal(r.messages[2].content, 'old answer');
    assert.match(r.messages.at(-1).content, new RegExp(`^${i <= failures.length ? `bad turn ${i - 1}` : 'retry'}\nProvide a summary`));
  }
  assert.deepEqual(requests.at(-1).messages.map(m => m.role), ['system', 'user']);
  assert.equal(requests.at(-1).messages[1].content, 'Session Summary: key facts');
  assert.deepEqual(events.filter(e => e.type === 'delta').map(e => e.content), ['old answer', 'done']);
  assert.equal(events.filter(e => e.type === 'tool_call').length, 0);
});

test('abort during or just after summarization restores history, and other sessions and restart start clean', async t => {
  const waiting = [Promise.withResolvers(), Promise.withResolvers()];
  const requests = intercept(t, (_r, n, signal) => {
    if (n === 1) return stream(answer('old answer'), usage(1024, 2));
    if (n === 2 || n === 5) {
      waiting[n === 2 ? 0 : 1].resolve();
      return new Promise((_, reject) => signal.addEventListener('abort', () => reject(signal.reason), { once: true }));
    }
    return stream(answer(`reply ${n}`), usage(10, 2));
  });
  const s = await api.createSession({ yaml });
  const other = await api.createSession({ yaml });
  t.after(() => Promise.all([s.close(), other.close()]));
  await s.send('old question');
  const aborted = s.send('discard me');
  await waiting[0].promise;
  await other.send('isolated');
  s.abort();
  await assert.rejects(aborted, /aborted/);
  const after = s.send('discard this too');
  await waiting[1].promise;
  s.abort();
  await assert.rejects(after, /aborted/);
  await s.send('commit');
  for (const i of [1, 3, 5]) {
    assert.match(requests[i].messages[0].content, /summarize conversations/);
    assert.equal(requests[i].messages[1].content, 'old question');
    assert.equal(requests[i].messages[2].content, 'old answer');
  }
  assert.deepEqual(requests[2].messages.slice(1), [{ role: 'user', content: 'isolated' }]);
  assert.equal(requests[4].messages[1].content, 'Session Summary: reply 4');
  assert.equal(requests[6].messages[1].content, 'Session Summary: reply 6');
  await s.restart();
  await s.send('fresh');
  assert.deepEqual(requests[7].messages.slice(1), [{ role: 'user', content: 'fresh' }]);
});

test('never replace a prefix too large for the summary request, including after an elicitation', { timeout: 5000 }, async t => {
  const requests = intercept(t, () => stream({ choices: [{ index: 0, delta: { tool_calls: [{ index: 0, type: 'function', id: 'q', function: { name: 'user_prompt', arguments: '{"message":"Answer?"}' } }] }, finish_reason: 'tool_calls' }] }, usage(1, 1)));
  const result = api.chat({ yaml: yaml.replace('type: think', 'type: user_prompt'), messages: [{ role: 'user', content: 'ask' }] }, event => {
    if (event.type === 'elicitation') return { action: 'accept', content: { answer: 'x'.repeat(8000) } };
  });
  await assert.rejects(result, /prefix does not fit/);
  assert.equal(requests.length, 1);
});

test('compaction on a picked model uses its parameters, redacted prefix and no user output schema', async t => {
  const secret = 'dckr_pat_' + 'S'.repeat(27);
  const cfg = `providers: {p: {base_url: https://fixture.invalid}}
models:
  start: {provider: p, model: start, context_window: 2048}
  picked: {provider: p, model: picked, context_window: 2048, temperature: 0.25}
agents:
  root:
    model: start
    toolsets: [{type: model_picker, models: [picked]}]
    structured_output: {name: answer, schema: {type: object, properties: {}}}`;
  const pick = { choices: [{ delta: { tool_calls: [{ id: 'pick', index: 0, type: 'function', function: { name: 'change_model', arguments: '{"model":"picked"}' } }] }, finish_reason: 'tool_calls' }] };
  const requests = intercept(t, (_, n) => n === 1 ? stream(pick) : stream(answer(n === 3 ? secret : 'ok'), usage(n === 2 ? 1900 : 1, 1)));
  const s = await api.createSession({ yaml: cfg });
  t.after(() => s.close());
  await s.send(secret);
  await s.send('next');
  assert.deepEqual(requests.map(r => r.model), ['start', 'picked', 'picked', 'picked']);
  const [, , summary, resume] = requests;
  assert.equal(summary.temperature, 0.25);
  assert.equal(summary.response_format, undefined);
  assert.equal(summary.tools, undefined);
  assert.match(summary.messages[0].content, /summarize conversations/);
  assert.equal(resume.response_format.json_schema.name, 'answer');
  assert.match(JSON.stringify(resume.messages), /Session Summary: \[REDACTED\]/);
  assert.equal(JSON.stringify(requests).includes(secret), false);
});
