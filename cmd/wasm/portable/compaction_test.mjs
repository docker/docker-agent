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

const dedicated = `providers:
  primary: {provider: openai, base_url: https://primary.invalid/v1, token_key: LOCAL_KEY}
  briefs: {provider: openai, api_type: openai_responses, base_url: https://summary.invalid/v1, token_key: BACKUP_KEY}
models:
  main: {provider: primary, model: writer, context_window: 4096}
  brief: {provider: briefs, model: summarizer, context_window: 2048}
agents: {root: {model: main, compaction_model: brief, compaction_threshold: 0.5}}`;

test('dedicated summary routes validate both models and credentials before model I/O', async t => {
  const requests = intercept(t, () => assert.fail('unexpected model request'));
  for (const [yaml, reason] of [
    [dedicated.replace('compaction_model: brief', 'compaction_model: missing'), /must name a model/],
    [dedicated.replace('compaction_model: brief', 'compaction_model: primary/inline'), /must name a model/],
    [dedicated.replace(', context_window: 2048', ''), /context_window/],
    [dedicated.replace('model: summarizer', 'model: "a,b"'), /multi-model/],
    [dedicated.replace('compaction_threshold: 0.5', 'session_compaction: false'), /active compaction/],
    [dedicated.replace('model: main,', 'toolsets: [{type: think}], model: main,'), /tool-free/],
    [dedicated.replace('model: main,', 'structured_output: {mode: tool, name: answer, schema: {type: object, properties: {}}}, model: main,'), /tool-free/],
    [dedicated.replace('model: main,', 'max_iterations: 2, model: main,'), /iteration limits/],
    [dedicated.replace('model: main,', 'handoffs: [root], model: main,'), /routed compaction/],
    [dedicated.replace('model: summarizer,', 'model: summarizer, base_url: https://other.invalid/v1,'), /explicit model token_key/],
  ]) await assert.rejects(api.createSession({yaml, env}), reason);
  await assert.rejects(api.createSession({yaml: dedicated, env: {LOCAL_KEY: 'ordinary'}}), /BACKUP_KEY/);
  for (const [first, reason] of [
    [{role:'user', multi_content:[{type:'text',text:'seed'}]}, /multipart history/],
    [{role:'assistant', content:'seed', reasoning_content:'private'}, /reasoning history/],
  ]) await assert.rejects(api.chat({yaml: dedicated, env, messages:[first, {role:'user',content:'go'}]}), reason);
  assert.equal(requests.length, 0);
});

test('the dedicated route never handles ordinary inference; summary retries use only its credentials', async t => {
  const sent = [];
  t.mock.method(globalThis, 'fetch', (url, init) => {
    const body = JSON.parse(init.body);
    sent.push({url, auth:init.headers.Authorization, body});
    switch (sent.length) {
      case 1: return stream(answer('first answer'), usage(1024, 1));
      case 2: return new Response('unavailable', {status:503});
      case 3: return stream({type:'response.failed', response:{error:{code:'bad_token', message:`invalid bearer ${env.BACKUP_KEY}`}}});
      case 4: return stream(answer('unreported reply'), usage(4, 1));
      case 5: return stream(answer('recovered'), usage(4, 1));
      default: assert.fail('unexpected model request');
    }
  });
  const events = [];
  const s = await api.createSession({yaml: dedicated, env}, e => events.push(e));
  t.after(() => s.close());
  await s.send('old '.repeat(500));
  const boundary = events.length;
  await assert.rejects(s.send('continue'), /provider diagnostic contained credentials/);
  assert.deepEqual(events.slice(boundary).map(e => e.type), ['error']);
  assert.equal((await s.send('again')).message.content, 'recovered');
  assert.deepEqual(sent.map(r => [new URL(r.url).hostname, r.auth]), [
    ['primary.invalid','Bearer primary-only'], ['summary.invalid','Bearer backup-only'],
    ['summary.invalid','Bearer backup-only'], ['primary.invalid','Bearer primary-only'], ['primary.invalid','Bearer primary-only'],
  ]);
  assert.deepEqual(sent[1], sent[2]);
  assert.equal(sent[4].body.messages.at(-2).content, 'unreported reply');
  assert.doesNotMatch(JSON.stringify([events, ...sent.map(r => r.body)]), /backup-only|primary-only/);
});

test('incompatible or tool-using compaction routes reject before model I/O', async t => {
  const requests = intercept(t, () => assert.fail('unexpected model request'));
  for (const yaml of [
    chain.replace('model: spare, context_window: 2048', 'model: spare, context_window: 8192'),
    chain.replace('models:\n', '  r: {provider: openai, api_type: openai_responses, base_url: $MODEL_URL, token_key: LOCAL_KEY}\nmodels:\n')
      .replace('spare: {provider: local', 'spare: {provider: r'),
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

test('Responses summary retries discard stale output snapshots before accepting a new one', async t => {
  const delta = content => ({ type: 'response.output_text.delta', item_id: 'msg', output_index: 0, delta: content });
  const snapshot = text => ({ type: 'response.output_text.done', item_id: 'msg', output_index: 0, text });
  const finished = input_tokens => ({ type: 'response.completed', response: { status: 'completed', output: [],
    usage: { input_tokens, output_tokens: 1 } } });
  const failure = message => ({ type: 'response.failed', response: { status: 'failed', error: { code: 'failed', message } } });
  const requests = intercept(t, (_, n) => {
    switch (n) {
      case 1: return stream(failure('primary unavailable'));
      case 2: return stream(delta('first answer'), finished(1024));
      case 3: return stream(delta('discard'), snapshot('discard'), failure('service unavailable'));
      case 4: return stream(delta('key'), snapshot('key facts'), finished(10));
      case 5: return stream(delta('done'), finished(10));
      default: assert.fail('extra model request');
    }
  });
  const s = await api.createSession({ yaml: chain.replace('openai_chatcompletions', 'openai_responses'), env });
  t.after(() => s.close());
  await s.send('old '.repeat(500));
  assert.equal((await s.send('continue')).message.content, 'done');
  assert.deepEqual(requests.map(r => r.model), ['main', 'spare', 'main', 'main', 'spare']);
  assert.deepEqual(requests[2], requests[3]);
  assert.equal(requests[4].input[0].content, 'Session Summary: key facts');
});

test('summary overflow retries a smaller request on the primary; malformed retries never drain a fallback', async t => {
  const overflow = { error: { message: 'context_length_exceeded' } };
  const requests = intercept(t, (_, n) => {
    switch (n) {
      case 1: return stream({ error: { message: 'primary unavailable' } });
      case 2: return stream(answer('first'), usage(1000, 25));
      case 3: case 5: return stream(answer('discard', null), overflow);
      case 4: return stream(answer('partial', 'length'));
      case 6: return stream(answer('key facts'), usage(40, 2));
      case 7: return stream(answer('done'));
      default: assert.fail('unexpected model request');
    }
  });
  const events = [];
  const s = await api.createSession({ yaml: chain, env }, e => events.push(e));
  t.after(() => s.close());
  await s.send('first');
  await assert.rejects(s.send('malformed'), /complete, non-empty text summary/);
  assert.equal(requests.length, 4);
  assert.equal((await s.send('retry')).message.content, 'done');
  assert.deepEqual(requests.map(r => r.model), ['main', 'spare', 'main', 'main', 'main', 'main', 'spare']);
  for (const i of [3, 5]) {
    assert.equal(requests[i].messages.length, 2);
    assert.match(requests[i].messages[1].content, /^(malformed|retry)\nProvide a summary/);
  }
  assert.equal(requests[6].messages[0].content, 'Session Summary: key facts');
  assert.doesNotMatch(JSON.stringify(requests.slice(4)), /malformed/);
  assert.doesNotMatch(JSON.stringify([events, requests]), /discard|partial/);
});

test('after shrinking, preserve the raw stream error and silently keep the ordinary reply', async t => {
  const diagnostic = '{ "message": "bad input", "code": "invalid" }';
  const requests = intercept(t, (_, n) => {
    switch (n) {
      case 1: return stream(answer('first'), usage(1000, 25));
      case 2: return stream({error: {message: 'reduce your prompt'}});
      case 3: return new Response(`data: {"error":${diagnostic}}\n\n`, {headers:{'content-type':'text/event-stream'}});
      case 4: return stream(answer('silent'), usage(10, 2));
      case 5: return stream(answer('done'));
      default: assert.fail('unexpected request');
    }
  });
  const events = [];
  const s = await api.createSession({yaml: yaml.replace(', toolsets: [{type: think}]', '')}, e => events.push(e));
  t.after(() => s.close());
  await s.send('first');
  const message = `run compaction agent: model failed: error receiving from stream: received error while streaming: ${diagnostic}`;
  await assert.rejects(s.send('next'), {message});
  assert.equal(events.at(-1).message, message);
  await s.send('afterwards');
  assert.equal(requests.at(-1).messages.at(-2).content, 'silent');
  assert.doesNotMatch(JSON.stringify(events), /silent/);
});

test('plain compaction drains a text reply after overflow, never a silent tool action', async t => {
  const overflow = {error: {message: 'prompt too long'}};
  const call = {choices: [{delta:{tool_calls:[{id:'hidden', index:0, type:'function',
    function:{name:'think', arguments:'{"thought":"secret"}'}}]}, finish_reason:'tool_calls'}]};
  const requests = intercept(t, (_, n) => {
    switch (n) {
      case 1: case 4: return stream(answer('first'), usage(1024, 1));
      case 2: case 5: case 6: return stream(overflow);
      case 3: return stream(answer('silent'), usage(10, 2));
      case 7: return stream(call);
      default: assert.fail('unexpected model request');
    }
  });
  const events = [];
  const s = await api.createSession({yaml}, e => events.push(e));
  t.after(() => s.close());
  await s.send('old '.repeat(500));
  await assert.rejects(s.send('again'), /run compaction agent: The conversation has exceeded/);
  await s.send('old '.repeat(500));
  assert.equal(requests[3].messages.at(-2).content, 'silent');
  await assert.rejects(s.send('again'), /run compaction agent: The conversation has exceeded/);
  assert.equal(requests.length, 7);
  assert.ok(requests.at(-1).tools.length);
  assert.doesNotMatch(JSON.stringify(events), /silent|hidden|secret/);
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
  const [primary, picked, summary, resume] = requests;
  assert.equal(primary.response_format.json_schema.name, 'answer');
  assert.equal(picked.response_format, undefined);
  assert.equal(summary.temperature, 0.25);
  assert.equal(summary.response_format, undefined);
  assert.equal(summary.tools, undefined);
  assert.match(summary.messages[0].content, /summarize conversations/);
  assert.equal(resume.response_format, undefined);
  assert.match(JSON.stringify(resume.messages), /Session Summary: \[REDACTED\]/);
  assert.equal(JSON.stringify(requests).includes(secret), false);
});
