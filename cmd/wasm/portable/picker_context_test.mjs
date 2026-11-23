import assert from 'node:assert/strict';
import { test } from 'node:test';
import boot from '../node/boot.js';

const yaml = `providers: {local: {provider: openai, api_type: openai_chatcompletions, base_url: 'https://model.invalid/v1', token_key: KEY}}
models: {B: {provider: local, model: careful, max_tokens: 24, temperature: 0}}
agents: {root: {model: local/fast, instruction: private-instruction, toolsets: [{type: model_picker, models: [B]}, {type: session_context}]}}`;

test('restart exposes only this handle’s previous conversation and retains its selected model', async t => {
  const api = await boot.bootDockerAgent();
  const records = [];
  let archivedID;
  const responses = [['change_model', {model:'B'}], null, ['list_sessions', {}], ['read_session', {session_id:'-2'}], ['revert_model', {}], null,
    ['list_sessions', {}], ['read_session', () => ({session_id: archivedID})], null];
  t.mock.method(globalThis, 'fetch', async (input, init) => {
    const req = new Request(input, init);
    assert.equal(req.url, 'https://model.invalid/v1/chat/completions');
    assert.equal(req.headers.get('Authorization'), 'Bearer this-handle-only');
    const body = await req.json();
    const turn = responses[records.length];
    assert.notEqual(turn, undefined);
    records.push(body);
    const [name, args] = turn ?? [];
    const delta = name ? {tool_calls:[{id:'call', index:0, type:'function', function:{name, arguments:JSON.stringify(typeof args === 'function' ? args() : args)}}]} : {content:'answer'};
    const data = {id:'response', model:body.model, choices:[{index:0, delta, finish_reason:name ? 'tool_calls' : 'stop'}]};
    return new Response(`data: ${JSON.stringify(data)}\n\ndata: [DONE]\n\n`, {headers:{'content-type':'text/event-stream'}});
  });
  const options = {yaml, env:{KEY:'this-handle-only'}};
  const s = await api.createSession(options);
  const isolated = await api.createSession(options);
  t.after(() => Promise.all([s.close(), isolated.close()]));
  await s.send('private-first-turn');
  await s.restart();
  await s.send('archive');
  assert.deepEqual(records.map(r => [r.model, r.max_tokens, r.temperature]), [
    ['fast', undefined, undefined], ...Array(4).fill(['careful', 24, 0]), ['fast', undefined, undefined],
  ]);
  const summaries = JSON.parse(records[3].messages.at(-1).content);
  assert.equal(summaries.length, 1);
  const [{id, title, created_at, num_messages}] = summaries;
  archivedID = id;
  assert.match(id, /^[\da-f-]{36}$/);
  assert.equal(title, '');
  assert.match(created_at, /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(Z|[+-]\d\d:\d\d)$/);
  assert.equal(num_messages, 4);
  assert.equal(records[4].messages.at(-1).content, `# Session ${id} — (untitled)\nCreated: ${created_at}\nMessages: 4\n\n## user\nprivate-first-turn\n\n## assistant\n→ called tool \`change_model\` with {"model":"B"}\n\n## tool result\nModel changed to B\n\n## assistant\nanswer\n`);
  await isolated.send('list');
  assert.equal(records[6].model, 'fast');
  assert.equal(records[7].messages.at(-1).content, '[]');
  assert.equal(records[8].messages.at(-1).content, `session "${id}" not found`);
  assert.doesNotMatch(JSON.stringify(records.slice(6)), /private-first-turn/);
});
