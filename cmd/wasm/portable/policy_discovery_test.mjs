import assert from 'node:assert/strict';
import { before, test } from 'node:test';
import boot from '../node/boot.js';

const modelURL = 'https://model.invalid/v1/chat/completions';
const post = (name, path, options = {}) => ({type:'api', allow_private_ips:true, ...options,
  api_config:{name, method:'POST', endpoint:`https://tool.invalid/${path}`}});
const messages = [{role:'user', content:'go'}];
const call = (name, args) => [name, JSON.stringify(args)];
const names = body => (body.tools ?? []).map(t => t.function.name);
const lastOutput = body => body.messages.at(-1).content;
let api;
before(async () => { api = await boot.bootDockerAgent(); });

function config(toolsets, fields = {}, agent = {}) {
  return JSON.stringify({providers:{local:{base_url:'https://model.invalid/v1'}},
    agents:{root:{model:'local/test', toolsets, ...agent}}, ...fields});
}

// Reuse model call IDs across rounds. IDs supplied by a provider are not approval tokens.
function replay(t, steps, toolFetch) {
  const requests = [];
  t.mock.method(globalThis, 'fetch', async (url, init) => {
    if (String(url) !== modelURL) {
      assert.ok(toolFetch, `unexpected non-model request ${url}`);
      return toolFetch(new Request(url, init));
    }
    const body = JSON.parse(init.body);
    const calls = steps[requests.length];
    assert.notEqual(calls, undefined, 'extra model step');
    requests.push(body);
    const delta = calls ? {tool_calls:calls.map(([name, args], index) => ({index, type:'function', id:`call-${index}`, function:{name, arguments:args}}))} : {content:'done'};
    const chunk = {choices:[{index:0, delta, finish_reason:calls ? 'tool_calls' : 'stop'}]};
    return new Response(`data: ${JSON.stringify(chunk)}\n\ndata: [DONE]\n\n`, {headers:{'content-type':'text/event-stream'}});
  });
  return requests;
}

test('mandatory asks gate pure Rust writes; argument denies survive overlapping allows and autoApprove', {timeout:5000}, async t => {
  const yaml = config([{type:'todo'}], {permissions:{deny:['create_todo:description=blocked*'], ask:['create_todo'], allow:['*']}});
  const requests = replay(t, [[call('create_todo', {description:'blocked'}),
    ['create_todo', '{"description":"blocked","description":"masked"}'],
    call('create_todo', {description:'refused'}), call('create_todo', {description:'approved'}), call('list_todos', {})], null]);
  const events = [];
  await api.chat({yaml, messages, autoApprove:true}, async e => {
    events.push(e);
    if (e.type === 'tool_confirmation') return JSON.parse(e.args).description === 'approved';
  });
  assert.deepEqual(events.filter(e => e.type === 'tool_confirmation').map(e => e.id), ['call-2', 'call-3']);
  assert.deepEqual(events.filter(e => e.type === 'tool_call').map(e => e.id), ['call-3', 'call-4']);
  assert.match(requests[1].messages.at(-5).content, /denied/);
  assert.match(requests[1].messages.at(-4).content, /denied/);
  assert.match(requests[1].messages.at(-3).content, /rejected/);
  assert.deepEqual(JSON.parse(lastOutput(requests[1])).todos, [{id:'todo_1',description:'approved',status:'pending'}]);
});

test('a stale decision cannot resume a later turn with the same model call ID', {timeout:5000}, async t => {
  const make = text => [call('think', {thought:text})];
  const requests = replay(t, [make('abandoned'), make('kept'), null, make('refused'), null]);
  const yaml = config([{type:'think'}], {runtime:{safety:'strict'}});
  let prompt = Promise.withResolvers();
  const s = await api.createSession({yaml, autoApprove:true}, e => { if (e.type === 'tool_confirmation') prompt.resolve(e); });
  t.after(() => s.close());
  const aborted = assert.rejects(s.send('first'), /aborted/);
  const old = await prompt.promise;
  await s.restart();
  await aborted;
  prompt = Promise.withResolvers();
  const turn = s.send('second');
  const current = await prompt.promise;
  assert.equal(current.id, old.id);
  assert.notEqual(current.confirmationId, old.confirmationId);
  await assert.rejects(s.confirm({confirmationId:old.confirmationId, decision:'approve'}), /match/);
  await assert.rejects(s.confirm(true), /confirmationId/);
  await s.confirm({confirmationId:current.confirmationId, decision:'approve_tool', toolName:'think'});
  await turn;
  assert.equal(lastOutput(requests[2]), 'Thoughts:\nkept');
  prompt = Promise.withResolvers();
  const again = s.send('third');
  const repeated = await prompt.promise;
  await s.confirm({confirmationId:repeated.confirmationId, decision:'reject'});
  await again;
  assert.match(lastOutput(requests[4]), /rejected/);
});

test('deferred API, allow-list, readonly and deny filters precede search, activation and dispatch', {timeout:5000}, async t => {
  const yaml = config([post('save','wrong',{tools:['excluded'],defer:true}), post('save','right',{defer:true,toon:'^save$'}),
    post('unsafe','readonly',{readonly:true,defer:true}), post('private','hidden',{defer:true})],
  {permissions:{ask:['save'],deny:['private']}});
  const requests = replay(t, [[call('search_tool',{query:''})], [call('add_tool',{name:'unsafe'}),call('add_tool',{name:'private'}),call('add_tool',{name:'save'})],
    [call('save',{})], [call('save',{})], null], req => {
    posts.push(req.url);
    assert.equal(req.method, 'POST');
    return Response.json({records:[{id:1},{id:2}]});
  });
  const posts = [], confirmations = [], calls = [];
  await api.chat({yaml, autoApprove:true, messages}, e => {
    if (e.type === 'tool_confirmation') {
      confirmations.push(e.name);
      return confirmations.length === 1 ? {decision:'approve_tool',toolName:'save'} : false;
    }
    if (e.type === 'tool_call' && e.name === 'save') {
      calls.push(e.id);
      e.args = '{"mutated":true}'; // A UI event is not the invocation.
    }
  });
  assert.deepEqual(names(requests[0]), ['search_tool','add_tool']);
  assert.deepEqual(names(requests[1]), names(requests[0]));
  assert.deepEqual(JSON.parse(lastOutput(requests[1]).split('\n')[1]).map(t => t.name), ['save']);
  assert.deepEqual(requests[2].messages.slice(-3,-1).map(m => m.content), ["Tool 'unsafe' not found.", "Tool 'private' not found."]);
  assert.deepEqual(names(requests[2]), ['search_tool','add_tool','save']);
  assert.deepEqual(confirmations, ['save','save']);
  assert.deepEqual(calls, ['call-0']);
  assert.deepEqual(posts, ['https://tool.invalid/right']);
  assert.equal(lastOutput(requests[3]), 'records[2]{id}:\n  1\n  2');
  assert.match(lastOutput(requests[4]), /rejected/);
});

test('guessing a deferred tool in its activation step cannot run it', async t => {
  const requests = replay(t, [[call('add_tool',{name:'save'}),call('save',{})]]);
  await assert.rejects(api.chat({yaml:config([post('save','unused',{defer:true})]), messages, autoApprove:true}), /unsupported tool name/);
  assert.deepEqual(names(requests[0]), ['search_tool','add_tool']);
});

test('MCP read-only claims do not authorize a dynamic call or make it pass readonly discovery', {timeout:5000}, async t => {
  const mcp = {type:'mcp', allow_private_ips:true, remote:{url:'https://mcp.invalid/rpc',transport_type:'streamable'}};
  const rpcCalls = [];
  const requests = replay(t, [[call('save',{})], null, [call('search_tool',{query:''})], null], async req => {
    assert.equal(req.url,'https://mcp.invalid/rpc');
    const m = await req.json();
    rpcCalls.push(m.method);
    if (m.method === 'notifications/initialized') return new Response(null,{status:202});
    const result = m.method === 'initialize' ? {protocolVersion:'2025-06-18',capabilities:{tools:{}},serverInfo:{name:'fixture',version:'1'}}
      : m.method === 'tools/list' ? {tools:[{name:'save',inputSchema:{type:'object'},annotations:{readOnlyHint:true}}]} : assert.fail(m.method);
    return Response.json({jsonrpc:'2.0', id:m.id, result});
  });
  let asked = 0;
  await api.chat({messages, yaml:config([mcp])}, e => { if (e.type === 'tool_confirmation') { asked++; return false; } });
  await api.chat({messages, yaml:config([{...mcp,readonly:true,defer:true}])});
  assert.equal(asked,1);
  assert.match(lastOutput(requests[1]), /rejected/);
  assert.deepEqual(names(requests[2]), ['search_tool','add_tool']);
  assert.match(lastOutput(requests[3]), /No deferred tools/);
  assert.ok(!rpcCalls.includes('tools/call'));
});

test('session grants from a different profile cannot authorize the same API name', {timeout:5000}, async t => {
  const posts = [];
  replay(t, [[call('save',{})],null,[call('save',{})],null,[call('save',{})],null], req => { posts.push(req.url); return new Response('ok'); });
  const yaml = config([], {agents:{root:{model:'local/test',toolsets:[post('save','granted')]}, other:{model:'local/test',toolsets:[post('save','separate')]}}});
  let granted = 0;
  const s = await api.createSession({yaml}, e => {
    if (e.type === 'tool_confirmation') { granted++; return s.confirm({decision:'approve_tool',toolName:e.name}); }
  });
  t.after(() => s.close());
  await s.send('grant');
  await s.send('again');
  await api.chat({messages,yaml,agentName:'other'});
  assert.equal(granted,1);
  assert.deepEqual(posts,['https://tool.invalid/granted','https://tool.invalid/granted']);
});

test('TOON applies to approved pure results, including a resumed elicitation', async t => {
  const yaml = config([{type:'user_prompt',toon:'user_prompt'}], {permissions:{ask:['user_prompt']}});
  const requests = replay(t, [[call('user_prompt',{message:'Choose'})],null]);
  const events = [];
  await api.chat({yaml,messages}, e => {
    events.push(e);
    if (e.type === 'tool_confirmation') return true;
    if (e.type === 'elicitation') return {action:'accept',content:{answer:'blue'}};
  });
  assert.deepEqual(events.filter(e => ['tool_confirmation','tool_call','elicitation','tool_result'].includes(e.type)).map(e => e.type),
    ['tool_confirmation','tool_call','elicitation','tool_result']);
  assert.equal(lastOutput(requests[1]), 'action: accept\ncontent:\n  answer: blue');
  assert.equal(events.find(e => e.type === 'tool_result').output,lastOutput(requests[1]));
});

test('unsupported policy and filter shapes fail before opening any transport', async t => {
  const fetch = t.mock.method(globalThis,'fetch',() => assert.fail('network access during policy validation'));
  for (const yaml of [config([{type:'todo',tools:null}]), config([{type:'todo',defer:'all'}]), config([{type:'todo',toon:'['}]),
    config([], {permissions:{deny:['mcp:server:*']}}), config([], {runtime:null}), config([], {runtime:{safety:'unknown'}})]) {
    await assert.rejects(api.createSession({yaml,autoApprove:true}));
  }
  for (const options of [{safety:'strict'}, {permissions:{deny:['*']}}]) {
    await assert.rejects(api.createSession({yaml:config([]), ...options}), /YAML/);
  }
  assert.equal(fetch.mock.callCount(),0);
});

test('a guessed filtered or unconditionally denied builtin cannot run, even under autoApprove', async t => {
  const requests = replay(t, [[call('create_todo',{description:'no'})], [call('create_todo',{description:'no'})]]);
  const events = [];
  for (const yaml of [config([{type:'todo',tools:['list_todos']}]), config([{type:'todo'}],{permissions:{deny:['create_todo']}})]) {
    await assert.rejects(api.chat({yaml,messages,autoApprove:true},e => events.push(e)),/(?:unsupported|invalid) tool name/);
  }
  assert.ok(requests.every(body => !names(body).includes('create_todo')));
  assert.ok(events.every(e => !['tool_call','tool_confirmation','tool_result'].includes(e.type)));
});
