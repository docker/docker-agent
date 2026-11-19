import assert from 'node:assert/strict';
import { before, test } from 'node:test';
import boot from '../node/boot.js';

let api;
before(async () => { api = await boot.bootDockerAgent(); });
const providers = `providers:
  local: { base_url: https://parent.invalid, token_key: ROOT_KEY }
  worker: { base_url: https://child.invalid, api_type: openai_responses, token_key: CHILD_KEY }
`;
const env = { ROOT_KEY: 'root-only', CHILD_KEY: 'child-only' };
const options = (agents, extra) => ({ yaml: providers + 'agents:\n' + agents, env, ...extra });
const piece = (delta, finish_reason) => ({ choices: [{ index: 0, delta, finish_reason }] });
const text = content => piece({ content }, 'stop');
const tool = (name, args, index = 0) => ({ index, id: `call-${index}`, type: 'function', function: { name, arguments: JSON.stringify(args) } });
const calls = (...tool_calls) => piece({ tool_calls }, 'tool_calls');
const transfer = agent => calls(tool('transfer_task', { agent, task: 'count to 3' }));
const handoff = agent => calls(tool('handoff', { agent }));
const sse = chunks => new Response(chunks.map(c => `data: ${JSON.stringify(c)}\n\n`).join('') + 'data: [DONE]\n\n', { headers: { 'Content-Type': 'text/event-stream' } });

function model(t, ...steps) {
  const requests = [];
  t.mock.method(globalThis, 'fetch', (url, { body, headers }) => {
    const index = requests.length;
    requests.push({ url, headers, ...JSON.parse(body) });
    assert.ok(index < steps.length, `unexpected request #${index + 1}`);
    return sse([steps[index]]);
  });
  return requests;
}

test('cross-provider delegation and handoff use the selected route, credentials and protocol', async t => {
  const opts = options(`  root: {model: local/parent, sub_agents: [helper], handoffs: [helper], instruction: Parent-only rule}
  helper: {model: worker/child, description: Counts, instruction: Count carefully, handoffs: [helper]}`);
  const child = { type: 'response.completed', response: { status: 'completed', output: [{ id: 'm', type: 'message', role: 'assistant', content: [{ type: 'output_text', text: '  3  ' }] }] } };
  const requests = model(t, transfer('helper'), child, text('counted'), handoff('helper'), child, child);
  const events = [];
  const s = await api.createSession(opts, e => events.push(e));
  t.after(() => s.close());
  await s.send('keep this in the parent');
  const { input, tools } = requests[1];
  assert.equal(tools[0].name, 'handoff');
  assert.equal(tools[0].strict, true);
  assert.match(JSON.stringify(input), /<task>/);
  assert.doesNotMatch(JSON.stringify(input), /keep this in the parent|Parent-only rule/);
  assert.deepEqual(input.at(-1), { role: 'user', content: 'Please proceed.' });
  assert.deepEqual(requests[2].messages.at(-1), { role: 'tool', tool_call_id: 'call-0', content: '3' });
  assert.ok(!JSON.stringify(requests[2].messages).includes('Count carefully'));
  await s.send('pass it on');
  assert.match(JSON.stringify(requests[4].input), /keep this in the parent/);
  assert.doesNotMatch(JSON.stringify(requests[4].input), /Parent-only rule/);
  assert.equal(requests[4].input.at(-1).type, 'function_call_output');
  assert.equal(requests[4].input.at(-1).call_id, 'call-0');
  await s.restart();
  await s.send('fresh');
  assert.doesNotMatch(JSON.stringify(requests[5].input), /keep this in the parent|counted/);
  for (const r of requests) {
    const child = r.model === 'child';
    assert.equal(r.url, child ? 'https://child.invalid/responses' : 'https://parent.invalid/chat/completions');
    assert.equal(r.headers.Authorization, child ? 'Bearer child-only' : 'Bearer root-only');
    assert.doesNotMatch(JSON.stringify(r.input ?? r.messages), /root-only|child-only/);
  }
  assert.equal(requests[5].model, 'child');
  assert.deepEqual(events.filter(e => e.type === 'handoff').map(e => [e.from, e.to]), [['root', 'helper'], ['helper', 'root'], ['root', 'helper']]);
});

test('a missing credential, unsafe target route, ambiguous todo scope or unsupported policy fails before fetch', async t => {
  const fetch = t.mock.method(globalThis, 'fetch', () => assert.fail('unexpected model request'));
  const opts = options(`  root: {model: local/parent, sub_agents: [helper]}
  helper: {model: worker/child}`);
  for (const [o, error] of [
    [{ ...opts, env: { ROOT_KEY: 'key' } }, /CHILD_KEY/],
    [{ ...opts, yaml: opts.yaml.replace('https://child.invalid', 'http://child.invalid') }, /model endpoint/],
    [{ ...opts, yaml: opts.yaml.replace('worker/child}', 'worker/child, toolsets: [{type: todo}, {type: todo, shared: true}]}') }, /todo/],
    [{ ...opts, yaml: opts.yaml.replace('worker/child}', 'worker/child, permissions: {deny: [think]}}') }, /permissions/],
    [{ ...opts, yaml: opts.yaml.replace('worker/child}', 'worker/child, toolsets: [{type: fetch}]}') }, /host toolsets/],
    [{ ...opts, yaml: opts.yaml.replace('sub_agents:', 'toolsets: [{type: plan}], sub_agents:') }, /host toolsets/],
    [{ ...opts, yaml: opts.yaml.replace('worker/child}', 'worker/child, force_handoff: missing}') }, /force_handoff/],
    [{ ...opts, yaml: opts.yaml.replace('[helper]', '[docker.io/remote]') }, /external agent/],
  ]) await assert.rejects(api.createSession(o), error);
  assert.equal(fetch.mock.callCount(), 0);
});

test('checks the immediate caller’s edge list and refuses recursive transfers as tool errors', async t => {
  const opts = options(`  root: {model: local/parent, sub_agents: [helper]}
  helper: {model: local/worker, sub_agents: [root], handoffs: [helper]}
  outsider: {model: local/other}`);
  const req = model(t, transfer('helper'), handoff('root'), transfer('outsider'), transfer('root'), text('worker done'), text('done'));
  const events = [];
  const result = await api.chat({ ...opts, messages: [{ role: 'user', content: 'go' }] }, e => events.push(e));
  assert.equal(result.message.content, 'done');
  assert.deepEqual(req.map(r => r.model), ['parent', 'worker', 'worker', 'worker', 'worker', 'parent']);
  const errors = events.filter(e => e.type === 'tool_result' && e.is_error);
  assert.equal(errors.length, 3);
  assert.match(errors[0].output, /not in handoffs list/);
  assert.match(errors[1].output, /not in sub-agents list/);
  assert.match(errors[2].output, /cycle detected: root -> helper -> root/);
  for (const [i, e] of errors.entries()) assert.equal(req[2 + i].messages.at(-1).content, e.output);
});

test('a team switch cannot be batched with another call, including a mutating todo', async t => {
  const opts = options(`  root: {model: local/parent, handoffs: [helper], toolsets: [{type: todo}]}
  helper: {model: local/worker}`);
  const req = model(t, calls(tool('create_todo', { description: 'must not persist' }), tool('handoff', { agent: 'helper' }, 1)), calls(tool('list_todos', {})), text('done'));
  const events = [];
  const s = await api.createSession(opts, e => events.push(e));
  t.after(() => s.close());
  await assert.rejects(s.send('mixed'), /must be called alone/);
  await s.send('list');
  assert.equal(req.length, 3);
  assert.equal(events.some(e => e.type === 'handoff'), false);
  assert.doesNotMatch(JSON.stringify(req.at(-1).messages), /mixed|must not persist/);
  assert.deepEqual(JSON.parse(req.at(-1).messages.at(-1).content).todos, []);
});

for (const method of ['abort', 'restart', 'close']) {
  test(`${method} during child elicitation unwinds the transfer and invalidates its pending reply`, { timeout: 5000 }, async t => {
    const opts = options(`  root: {model: local/parent, sub_agents: [helper]}
  helper: {model: local/worker, toolsets: [{type: user_prompt}]}`);
    const ask = calls(tool('user_prompt', { message: 'Which?' }));
    const req = model(t, transfer('helper'), ask, transfer('helper'), ask, text('chosen'), text('done'));
    let question = Promise.withResolvers();
    const s = await api.createSession(opts, e => { if (e.type === 'elicitation') question.resolve(e.id); });
    t.after(() => s.close());
    const abandoned = assert.rejects(s.send('abandoned'), /aborted/);
    const old = await question.promise;
    await s[method]();
    await abandoned;
    await assert.rejects(s.respondToElicitation({ id: old, action: 'accept' }), /pending|closed/);
    assert.equal(req.length, 2);
    if (method === 'close') return;
    question = Promise.withResolvers();
    const fresh = s.send('fresh');
    const id = await question.promise;
    assert.notEqual(id, old);
    await assert.rejects(s.respondToElicitation({ id: old, action: 'accept' }), /pending/);
    await s.respondToElicitation({ id, action: 'accept' });
    assert.equal((await fresh).message.content, 'done');
    assert.equal(req[2].model, 'parent');
    assert.doesNotMatch(JSON.stringify(req[2].messages), /abandoned|transfer_task.*arguments/);
    assert.equal(req[4].messages.at(-1).content, '{"action":"accept"}');
    assert.equal(req[5].messages.at(-1).content, 'chosen');
  });
}

test('shared todos follow the team, scratchpads follow the conversation and agent', async t => {
  const opts = options(`  root: {model: local/parent, sub_agents: [helper], toolsets: [{type: think}, {type: todo, shared: true}]}
  helper: {model: local/worker, toolsets: [{type: think}, {type: todo, shared: true}]}`);
  const think = thought => calls(tool('think', { thought }));
  const list = calls(tool('list_todos', {}));
  const req = model(t, calls(tool('create_todo', { description: 'shared' })), think('root only'), transfer('helper'), think('first'), list,
    text('first child'), transfer('helper'), think('second'), text('second child'), think('resumed'), text('done'), list, text('empty'));
  const s = await api.createSession(opts);
  t.after(() => s.close());
  await s.send('go');
  const last = n => req[n].messages.at(-1).content;
  assert.equal(last(4), 'Thoughts:\nfirst');
  assert.equal(last(8), 'Thoughts:\nsecond');
  assert.equal(last(10), 'Thoughts:\nroot only\nresumed');
  assert.equal(JSON.parse(last(5)).todos[0].description, 'shared');
  const other = await api.createSession(opts);
  t.after(() => other.close());
  await other.send('list');
  assert.deepEqual(JSON.parse(last(12)).todos, []);
});

test('a transfer at depth ten succeeds and a transfer beyond it returns a tool error', async t => {
  const names = ['root', ...Array.from({ length: 11 }, (_, i) => `child${i + 1}`)];
  const opts = options(names.map((name, i) => `  ${name}: {model: local/${name}, sub_agents: [${names[i + 1] ?? name}]}`).join('\n'));
  const req = model(t, ...names.slice(1).map(transfer), ...names.slice(1).map(() => text('done')));
  const events = [];
  const result = await api.chat({ ...opts, messages: [{ role: 'user', content: 'go' }] }, e => events.push(e));
  assert.equal(result.message.content, 'done');
  assert.equal(req.some(r => r.model === 'child11'), false);
  assert.match(events.find(e => e.type === 'tool_result' && e.is_error).output, /depth limit exceeded/);
  assert.equal(events.filter(e => e.type === 'handoff').length, 20);
});

test('the global switch budget stops repeated nested fan-out and the session recovers', async t => {
  const opts = options(`  root: {model: local/root, sub_agents: [worker]}
  worker: {model: local/worker, sub_agents: [leaf]}
  leaf: {model: local/leaf}`);
  const steps = [];
  for (let i = 0; i < 4; i++) {
    steps.push(transfer('worker'));
    const count = i === 3 ? 5 : 8;
    for (let j = 0; j < count; j++) steps.push(transfer('leaf'), text('leaf'));
    steps.push(text('worker'));
  }
  // At switch #33 the guard fires before a leaf gets a request.
  const req = model(t, ...steps.slice(0, -2), text('recovered'));
  const s = await api.createSession(opts);
  t.after(() => s.close());
  await assert.rejects(s.send('fan out'), /team switch limit/);
  assert.equal((await s.send('retry')).message.content, 'recovered');
  assert.equal(req.at(-1).model, 'root');
  assert.doesNotMatch(JSON.stringify(req.at(-1).messages), /fan out/);
});
