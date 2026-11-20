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
const transfer = (agent, task = 'count to 3') => calls(tool('transfer_task', { agent, task }));
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
    [{ ...opts, yaml: opts.yaml.replace('worker/child}', 'worker/child, toolsets: [{type: fetch, allowed_domains: ["10.0.0.0/8"]}]}') }, /domain pattern/],
    [{ ...opts, yaml: opts.yaml.replace('sub_agents:', 'readonly: true, toolsets: [{type: plan}], sub_agents:') }, /readonly/],
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
    if (method === 'abort') {
      assert.equal(req[2].messages.at(-4).content, 'abandoned');
      assert.equal(req[2].messages.at(-3).tool_calls[0].function.name, 'transfer_task');
      assert.deepEqual(req[2].messages.at(-2), {role:'tool', tool_call_id:'call-0', content:'The tool call was canceled by the user.'});
    } else assert.doesNotMatch(JSON.stringify(req[2].messages), /abandoned|transfer_task.*arguments/);
    assert.equal(req[4].messages.at(-1).content, '{"action":"accept"}');
    assert.equal(req[5].messages.at(-1).content, 'chosen');
  });
}

const stopped = n => `Execution stopped after reaching the configured max_iterations limit (${n}).`;
const callLimit = n => `hooks: {before_llm_call: {type: builtin, command: max_iterations, args: ['${n}']}}`;

test('the opening agent\'s cap spans handoffs; a handoff stopped by it is announced on the next send', async t => {
  const opts = options(`  root: {model: local/parent, max_iterations: 1, handoffs: [helper]}
  helper: {model: local/worker, max_iterations: 5, handoffs: [root]}`);
  const req = model(t, handoff('helper'), handoff('root'), text('fresh'));
  const events = [];
  const s = await api.createSession(opts, e => events.push(e));
  t.after(() => s.close());
  assert.equal((await s.send('one')).message.content, stopped(1));
  assert.deepEqual(events.slice(-2).map(e => e.type), ['delta', 'finish']);
  assert.equal(events.some(e => e.type === 'handoff'), false);
  const first = events.length;
  assert.equal((await s.send('two')).message.content, stopped(1));
  assert.deepEqual(events[first], { type: 'handoff', from: 'root', to: 'helper' });
  await s.restart();
  assert.equal((await s.send('three')).message.content, 'fresh');
  assert.deepEqual(req.map(r => r.model), ['parent', 'worker', 'parent']);
  assert.equal(events.filter(e => e.type === 'handoff').length, 1);
});

test('a delegate stopped by its own cap answers its caller, whichever reply ends the round', { timeout: 5000 }, async t => {
  const opts = options(`  root: {model: local/parent, max_iterations: 2, sub_agents: [helper]}
  helper: {model: local/worker, max_iterations: 1, toolsets: [{type: user_prompt}, {type: plan}]}
permissions: {ask: [write_plan], allow: ['*']}`);
  const req = model(t, transfer('helper'), calls(tool('user_prompt', { message: 'Which?' })),
    transfer('helper'), calls(tool('write_plan', { name: 'scope', content: 'plan' })));
  const events = [];
  const s = await api.createSession(opts, e => {
    events.push(e);
    if (e.type === 'elicitation') s.respondToElicitation({ id: e.id, action: 'accept' });
    if (e.type === 'tool_confirmation') s.confirm({ confirmationId: e.confirmationId, decision: 'approve' });
  });
  t.after(() => s.close());
  assert.equal((await s.send('go')).message.content, stopped(2));
  assert.deepEqual(req.map(r => r.model), ['parent', 'worker', 'parent', 'worker']);
  assert.equal(req[2].messages.at(-1).content, stopped(1));
  assert.deepEqual(events.filter(e => e.type === 'handoff').map(e => e.to), ['helper', 'root', 'helper', 'root']);
  assert.deepEqual(events.filter(e => e.type === 'tool_result' && e.name === 'transfer_task').map(e => e.output), [stopped(1), stopped(1)]);
  assert.equal(events.filter(e => e.type === 'finish').length, 1);
});

test('the acting agent\'s call limit counts the whole send and keeps the handoff it interrupted', async t => {
  const opts = options(`  root: {model: local/parent, ${callLimit(2)}, handoffs: [helper]}
  helper: {model: local/worker, ${callLimit(1)}, handoffs: [root]}`);
  const req = model(t, handoff('helper'), handoff('root'), text('done'));
  const events = [];
  const s = await api.createSession(opts, e => events.push(e));
  t.after(() => s.close());
  await assert.rejects(s.send('one'), /limit of 1 model call/);
  assert.deepEqual(events.slice(-2).map(e => e.type), ['handoff', 'error']);
  assert.equal(req.length, 1);
  assert.equal((await s.send('two')).message.content, 'done');
  assert.deepEqual(req.map(r => r.model), ['parent', 'worker', 'parent']);
  assert.deepEqual(req[1].messages.filter(m => m.role === 'user').map(m => m.content), ['one', 'two']);
});

test("a blank reply warns, joins no history and answers with the last reply kept, even a delegate's", async t => {
  const opts = options(`  root: {model: local/parent, sub_agents: [helper]}
  helper: {model: local/worker}`);
  const req = model(t, text(' first \n'), text(''), transfer('helper'), text('from the delegate'),
    piece({ reasoning_content: 'thinking' }, 'stop'), text('  '), text('  '));
  const events = [];
  const s = await api.createSession(opts, e => events.push(e));
  t.after(() => s.close());
  const replies = [];
  for (const prompt of ['one', 'two', 'three', 'four']) replies.push((await s.send(prompt)).message.content);
  assert.deepEqual(replies, ['first', 'first', 'from the delegate', 'from the delegate']);
  await s.restart();
  assert.equal((await s.send('five')).message.content, '');
  assert.deepEqual(events.filter(e => e.type === 'warning').map(e => e.message.replace(/^Model \S+\/parent | \(stop reason: stop\)\..*$/g, '')),
    ['returned an empty response', 'produced only reasoning and no reply', 'returned an empty response', 'returned an empty response']);
  assert.ok(req.every(r => r.messages.every(m => m.role !== 'assistant' || m.tool_calls || m.content.trim())));
  assert.doesNotMatch(JSON.stringify(req), /thinking/);
  assert.equal(req[1].messages.find(m => m.role === 'assistant').content, ' first \n');
});

test('tool budgets and the loop guard stay with the opening agent; the trim count follows the acting agent', async t => {
  const opts = options(`  root: {model: local/parent, max_tool_result_tokens: 4, max_consecutive_tool_calls: 2, handoffs: [helper], toolsets: [{type: think}]}
  helper: {model: local/worker, max_tool_result_tokens: 100, num_history_items: 3, toolsets: [{type: think}]}`);
  const named = (id, name, args) => calls({ index: 0, id, type: 'function', function: { name, arguments: JSON.stringify(args) } });
  const repeat = id => named(id, 'think', { thought: 'same' });
  const req = model(t, named('a', 'think', { thought: 'x'.repeat(80) }), named('b', 'handoff', { agent: 'helper' }), repeat('c'), repeat('d'));
  const s = await api.createSession(opts);
  t.after(() => s.close());
  await assert.rejects(s.send('one'), /detected 2 consecutive identical calls to think/);
  assert.deepEqual(req.map(r => r.model), ['parent', 'parent', 'worker', 'worker']);
  assert.match(req[1].messages.at(-1).content, /^.{7}\.\.\..{6}$/s);
  const afterHandoff = req[2].messages.filter(m => m.role !== 'system');
  assert.deepEqual(afterHandoff.map(m => m.role), ['user', 'assistant', 'tool']);
  assert.equal(afterHandoff[1].tool_calls[0].id, 'b');
  assert.match(afterHandoff[2].content, /^The age\.\.\.agent\.$/);
  assert.deepEqual(req[3].messages.filter(m => m.role !== 'system').map(m => m.tool_call_id ?? m.tool_calls?.[0].id), [undefined, 'c', 'c']);
});

test('iteration limits that Go would enforce inside a delegated task or on a team fallback chain are refused', async t => {
  const fetch = t.mock.method(globalThis, 'fetch', () => assert.fail('unexpected model request'));
  for (const [agents, reason] of [
    [`  root: {model: local/parent, sub_agents: [helper]}\n  helper: {model: local/worker, ${callLimit(1)}}`, /"helper", which can run inside transfer_task/],
    [`  root: {model: local/parent, sub_agents: [helper]}\n  helper: {model: local/worker, handoffs: [other]}\n  other: {model: local/other, ${callLimit(1)}}`, /"other", which can run inside transfer_task/],
    [`  root: {model: local/parent, sub_agents: [helper]}\n  helper: {model: local/worker, force_handoff: other}\n  other: {model: local/other, ${callLimit(1)}}`, /"other", which can run inside transfer_task/],
    [`  root: {model: local/parent, ${callLimit(2)}, handoffs: [helper]}\n  helper: {model: local/worker, sub_agents: [root]}`, /"root", which can run inside transfer_task/],
    [`  root: {model: local/parent, handoffs: [helper]}\n  helper: {model: local/worker, max_iterations: 2, session_compaction: false, fallback: {models: [local/backup]}}`, /fallback chain within local teams/],
    [`  root: {model: local/parent, handoffs: [helper]}\n  helper: {model: local/worker, ${callLimit(2)}, session_compaction: false, fallback: {models: [local/backup]}}`, /fallback chain within local teams/],
    [`  root: {model: local/parent, max_iterations: 9, handoffs: [helper]}\n  helper: {model: local/worker}`, /tool round limit/],
    [`  root: {model: local/parent, handoffs: [helper]}\n  helper: {model: local/worker, num_history_items: 2, session_compaction: false, fallback: {models: [local/backup]}}`, /fallback chain within local teams/],
  ]) await assert.rejects(api.createSession(options(agents)), reason);
  const limited = options(`  root: {model: local/parent, handoffs: [helper]}\n  helper: {model: local/worker, num_history_items: 2}`);
  for (const message of [{ role: 'user', name: 'caller', content: 'named' }, { role: 'assistant', content: 'kept', reasoning_content: 'private' },
    { role: 'user', content: 'image', multi_content: [{ type: 'text', text: 'part' }] }]) {
    await assert.rejects(api.chat({ ...limited, messages: [message, { role: 'user', content: 'go' }] }), /unnamed history without reasoning|text-only history/);
  }
  assert.equal(fetch.mock.callCount(), 0);
});

test('only one-shot chat may cross a foreground handoff with redaction disabled', async t => {
  const secret = 'dckr_pat_' + 'S'.repeat(27);
  const opts = options(`  root: {model: local/parent, force_handoff: helper, redact_secrets: false}
  helper: {model: local/worker}`);
  const requests = model(t, text(secret), text('done'));
  await assert.rejects(api.createSession(opts), /foreground handoffs.*one-shot chat/);
  const messages = [{role: 'user', content: secret}];
  for (const seed of [{role: 'user', name: secret, content: 'seed'},
    {role: 'user', multi_content: [{type: 'text', text: secret}]}]) {
    await assert.rejects(api.chat({...opts, messages: [seed, ...messages]}), /unnamed text-only history/);
  }
  await assert.rejects(api.chat({...opts, yaml: opts.yaml.replace('force_handoff:', 'max_tool_result_tokens: 4, force_handoff:'), messages}), /split a secret/);
  const explicit = opts.yaml.replace('redact_secrets: false', 'redact_secrets: false, hooks: {before_llm_call: {type: builtin, command: redact_secrets}}');
  await assert.rejects(api.chat({...opts, yaml: explicit, messages: [{role: 'user', name: secret, content: 'seed'}, ...messages]}), /unnamed text-only history/);
  assert.equal(requests.length, 0);
  assert.equal((await api.chat({...opts, messages})).message.content, 'done');
  assert.ok(JSON.stringify(requests[0].messages).includes(secret));
  assert.ok(!JSON.stringify(requests[1].messages).includes(secret));
  assert.deepEqual(requests[1].messages.filter(m => m.content === '[REDACTED]').map(m => m.role), ['user', 'assistant']);
});

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
  const req = model(t, ...names.slice(1).map(n => transfer(n)), ...names.slice(1).map(() => text('done')));
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
    for (let j = 0; j < count; j++) steps.push(transfer('leaf', `part ${j}`), text('leaf'));
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

test('agents share plan and memory storage, but not tool visibility or mandatory approvals', {timeout:5000}, async t => {
  const opts = options(`  root: {model: local/parent, sub_agents: [helper], toolsets: [{type: plan}, {type: memory}]}
  helper: {model: local/worker, readonly: true, toolsets: [{type: memory}, {type: plan}]}
permissions: {ask: [write_plan], allow: ['*']}`, {autoApprove:true});
  const write = calls(tool('write_plan', {name:'scope', content:'shared plan'}));
  const read = calls(tool('read_plan', {name:'scope'}), tool('get_memories', {}, 1));
  const req = model(t, write, write, calls(tool('add_memory', {memory:'shared memory'})), transfer('helper'), read,
    text('read both'), text('done'), transfer('helper'), read, text('read again'), text('done'), read, text('empty'));
  let prompts = 0;
  const s = await api.createSession(opts, e => {
    if (e.type === 'tool_confirmation') {
      assert.equal(e.name, 'write_plan');
      return s.confirm({confirmationId:e.confirmationId, decision:++prompts === 1 ? 'reject' : 'approve'});
    }
  });
  t.after(() => s.close());
  await s.send('store');
  assert.equal(prompts, 2);
  assert.match(req[1].messages.at(-1).content, /rejected/);
  await s.restart();
  await s.send('read again');
  for (const i of [4, 8]) {
    const names = req[i].tools.map(t => t.function.name);
    assert.ok(!names.includes('add_memory') && !names.includes('write_plan'));
    const [plan, memories] = req[i + 1].messages.slice(-2).map(m => JSON.parse(m.content));
    assert.equal(plan.content, 'shared plan');
    assert.equal(plan.revision, 1);
    assert.equal(memories[0].memory, 'shared memory');
  }
  const other = await api.createSession({...opts, agentName:'helper'});
  t.after(() => other.close());
  await other.send('isolated');
  assert.match(req.at(-1).messages.at(-2).content, /not found/);
  assert.equal(req.at(-1).messages.at(-1).content, 'null');
});

test('an approve_tool grant from a delegate stays in that task', async t => {
  const opts = options(`  root: {model: local/root, sub_agents: [helper], toolsets: [{type: memory}]}
  helper: {model: local/worker, toolsets: [{type: memory}]}`);
  const remove = calls(tool('delete_memory', {id: 'absent'}));
  model(t, transfer('helper'), remove, remove, text('first'), transfer('helper'), remove, text('second'), remove, text('done'));
  const asked = [];
  const s = await api.createSession(opts, e => {
    if (e.type === 'tool_confirmation') {
      asked.push(e.agent);
      return s.confirm({decision: 'approve_tool', confirmationId: e.confirmationId, toolName: e.name});
    }
  });
  t.after(() => s.close());
  await s.send('delegate twice');
  assert.deepEqual(asked, ['helper', 'helper', 'root']);
});

test('a loop in a nested transfer stops without running a hidden continuation', async t => {
  const opts = options(`  root: {model: local/root, sub_agents: [helper], toolsets: [{type: think}]}
  helper: {model: local/worker, sub_agents: [leaf], max_consecutive_tool_calls: 1}
  leaf: {model: local/leaf}`);
  const req = model(t, calls(tool('think', {thought: 'keep'})), transfer('helper'), transfer('leaf'), text('from leaf'), text('recovered'));
  const events = [];
  const s = await api.createSession(opts, e => events.push(e));
  t.after(() => s.close());
  await assert.rejects(s.send('first'), /consecutive identical calls to transfer_task/);
  assert.deepEqual(req.map(r => r.model), ['root', 'root', 'worker', 'leaf']);
  const stopped = events.length;
  await s.send('again');
  assert.deepEqual(events[stopped], {type: 'handoff', from: 'helper', to: 'root'});
  assert.deepEqual(req[4].messages.filter(m => m.role === 'tool').map(m => m.content), ['Thoughts:\nkeep', 'Delegated task failed']);
  assert.doesNotMatch(JSON.stringify(req[4].messages), /from leaf/);
});
