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
    ['create_todo', '{"description":"masked","description":"blocked"}'], // The handler reads the last one.
    call('create_todo', {description:'refused'}), call('create_todo', {description:'approved'}), call('list_todos', {})], null]);
  const events = [];
  await api.chat({yaml, messages, autoApprove:true}, async e => {
    events.push(e);
    if (e.type === 'tool_confirmation') return JSON.parse(e.args).description === 'approved';
  });
  assert.deepEqual(events.filter(e => e.type === 'tool_confirmation').map(e => e.id), ['call-2', 'call-3']);
  assert.deepEqual(events.filter(e => e.type === 'tool_call').map(e => e.id), ['call-4', 'call-3']);
  assert.match(requests[1].messages.at(-5).content, /denied/);
  assert.match(requests[1].messages.at(-4).content, /denied/);
  assert.equal(JSON.parse(requests[1].messages.at(-3).content).todos.length, 0);
  assert.match(requests[1].messages.at(-2).content, /rejected/);
  assert.deepEqual(JSON.parse(lastOutput(requests[1])).all_todos, [{id:'todo_1',description:'approved',status:'pending'}]);
});

test('a folded field name or an overflowing number cannot dodge an argument rule', {timeout:5000}, async t => {
  const yaml = config([{type:'think'}, {type:'memory'}], {permissions:{deny:['think:thought=bad*'], allow:['delete_memory:id=safe*']}});
  // Go checks the exact key of a map, then lets each of these reach a handler that reads the other value.
  const requests = replay(t, [[['think', '{"Thought":"bad idea"}'], ['think', '{"thought":"ok","THOUGHT":"bad idea"}'],
    ['think', '{"thought":"bad idea","pad":1e400}'], ['delete_memory', '{"id":"safe1","ID":"other"}']], null]);
  const events = [];
  await api.chat({yaml, messages}, e => { events.push(e); if (e.type === 'tool_confirmation') return false; });
  assert.deepEqual(events.filter(e => e.type === 'tool_call'), []);
  assert.deepEqual(events.filter(e => e.type === 'tool_confirmation').map(e => e.id), ['call-3']);
  const outputs = requests[1].messages.slice(-4).map(m => m.content);
  for (const output of outputs.slice(0, 3)) assert.match(output, /^Tool 'think' is denied/);
  assert.match(outputs[3], /rejected/);
});

for (const mode of ['plain', 'thenable', 'session']) {
  test(`a ${mode} approval orders a batched write against parked loggers`, {timeout:5000}, async t => {
    const yaml = config([{type:'memory'}, {type:'think'}]);
    const requests = replay(t, [[call('nope', {}), call('delete_memory', {id:'m'}), call('think', {})], null]);
    const events = [];
    let session;
    function record(e) {
      events.push(e);
      if (e.type !== 'tool_confirmation') return;
      if (mode === 'session') return session.confirm(true);
      return mode === 'plain' ? true : {then(resolve) { resolve(true); }};
    }
    if (mode === 'session') {
      session = await api.createSession({yaml}, record);
      try { await session.send('go'); } finally { await session.close(); }
    } else await api.chat({yaml, messages}, record);
    const order = mode === 'plain' ? ['call-2','call-1','call-0'] : ['call-2','call-0','call-1'];
    assert.deepEqual(events.filter(e => e.type === 'tool_result').map(e => e.id), order);
    assert.deepEqual(requests[1].messages.slice(-3).map(m => m.tool_call_id), order);
  });
}

test('overlapping API calls commit in completion order, not launch order', {timeout:5000}, async t => {
  const started = [], release = Promise.withResolvers();
  const requests = replay(t, [[call('slow',{}),call('fast',{})], null], async req => {
    started.push(req.url);
    if (req.url.endsWith('/slow')) await release.promise;
    return new Response('ok');
  });
  const yaml = config([post('slow','slow'), post('fast','fast')]);
  await api.chat({yaml, messages, autoApprove:true}, e => {
    if (e.type === 'tool_result' && e.name === 'fast') release.resolve();
  });
  assert.deepEqual(started, ['https://tool.invalid/slow','https://tool.invalid/fast']);
  assert.deepEqual(requests[1].messages.slice(-2).map(m => m.tool_call_id), ['call-1','call-0']);
});

test('session elicitation at delivery does not overtake a parked logger', async t => {
  const requests = replay(t, [[call('unknown',{}),call('user_prompt',{message:'Choose'}),call('think',{})], null]);
  const yaml = config([{type:'user_prompt'}, {type:'think'}]);
  const s = await api.createSession({yaml}, e => {
    if (e.type === 'elicitation') return s.respondToElicitation({id:e.id, action:'decline'});
  });
  try { await s.send('go'); } finally { await s.close(); }
  assert.deepEqual(requests[1].messages.slice(-3).map(m => m.tool_call_id), ['call-2','call-0','call-1']);
});

test('network batches with competing interactive waits retain sequential dispatch', async t => {
  const events = [];
  const requests = replay(t, [[call('delete_memory',{id:'m'}), call('save',{}), call('think',{})], null], () => new Response('ok'));
  const yaml = config([{type:'memory'}, post('save','save'), {type:'think'}]);
  await api.chat({yaml, messages}, async e => {events.push(e); return true;});
  assert.deepEqual(events.filter(e => e.type === 'tool_confirmation').map(e => e.id), ['call-0','call-1']);
  assert.deepEqual(requests[1].messages.slice(-3).map(m => m.tool_call_id), ['call-0','call-1','call-2']);
});

test('a serial fallback still delivers sibling elicitations without waiting for the first answer', {timeout:5000}, async t => {
  const requests = replay(t, [[call('user_prompt',{message:'first'}), call('user_prompt',{message:'second'}), call('save',{})], null], () => new Response('ok'));
  const yaml = config([{type:'user_prompt'}, post('save','save')]);
  const first = Promise.withResolvers();
  await api.chat({yaml, messages, autoApprove:true}, e => {
    if (e.type !== 'elicitation') return;
    if (e.message === 'first') return first.promise;
    first.resolve('decline');
    return 'accept';
  });
  assert.deepEqual(requests[1].messages.slice(-3).map(m => m.tool_call_id), ['call-0','call-1','call-2']);
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

test('deferred API, allow-list and readonly filters precede search and activation; a denied tool is found but never runs', {timeout:5000}, async t => {
  const yaml = config([post('save','wrong',{tools:['excluded'],defer:true}), post('save','right',{defer:true,toon:'^save$'}),
    post('unsafe','readonly',{readonly:true,defer:true}), post('private','hidden',{defer:true})],
  {permissions:{ask:['save'],deny:['private']}});
  const requests = replay(t, [[call('search_tool',{query:''})], [call('add_tool',{name:'unsafe'}),call('add_tool',{name:'private'}),call('add_tool',{name:'save'})],
    [call('save',{})], [call('private',{}),call('save',{})], null], req => {
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
  // Like Go, a denial does not hide the tool: it is refused at dispatch.
  assert.deepEqual(JSON.parse(lastOutput(requests[1]).split('\n')[1]).map(t => t.name), ['private','save']);
  const [unsafe, hidden] = requests[2].messages.slice(-3,-1).map(m => m.content);
  assert.equal(unsafe, "Tool 'unsafe' not found.");
  assert.match(hidden, /^Tool 'private' has been activated/);
  assert.deepEqual(names(requests[2]), ['search_tool','add_tool','private','save']);
  assert.deepEqual(confirmations, ['save','save']);
  assert.deepEqual(calls, ['call-0']);
  assert.deepEqual(posts, ['https://tool.invalid/right']);
  assert.equal(lastOutput(requests[3]), 'records[2]{id}:\n  1\n  2');
  assert.equal(requests[4].messages.at(-2).content, "Tool 'private' is denied by permissions configuration.");
  assert.match(lastOutput(requests[4]), /rejected/);
});

test('guessing a deferred tool in its activation step cannot run it', async t => {
  const requests = replay(t, [[call('add_tool',{name:'save'}),call('save',{})], null]);
  const events = [];
  await api.chat({yaml:config([post('save','unused',{defer:true})]), messages, autoApprove:true}, e => { events.push(e); });
  assert.deepEqual(names(requests[0]), ['search_tool','add_tool']);
  assert.equal(lastOutput(requests[1]), "Tool 'save' is not available. You can only use the tools provided to you.");
  assert.deepEqual(names(requests[1]), ['search_tool','add_tool','save']);
  assert.deepEqual(events.filter(e => e.type === 'tool_call').map(e => e.name), ['add_tool']);
  assert.deepEqual(events.filter(e => e.type === 'tool_result').map(e => e.is_error), [false, true]);
});

test('malformed arguments still need approval, then come back as the decoder\'s error; a config cannot waive approval', {timeout:5000}, async t => {
  const requests = replay(t, [[['add_memory', '{"memory": "x'], ['list_plans', '{"x": oops'], ['add_memory', '[1]']], [call('add_memory', {memory:'kept'})], null]);
  const yaml = config([{type:'memory'}, {type:'plan'}], {runtime:{safety:'autonomous'}});
  const events = [];
  const result = await api.chat({yaml, messages}, e => { events.push(e); return e.type === 'tool_confirmation' ? true : undefined; });
  assert.equal(result.message.content, 'done');
  assert.deepEqual(events.filter(e => e.type === 'tool_confirmation').map(e => e.args), ['{"memory": "x', '[1]', '{"memory":"kept"}']);
  assert.deepEqual(requests[1].messages.slice(-3).map(m => m.content), [
    '{"plans":[]}',
    'Error calling tool: unexpected end of JSON input',
    'Error calling tool: json: cannot unmarshal array into Go value of type memory.AddMemoryArgs',
  ]);
  assert.match(lastOutput(requests[2]), /^Memory added successfully/);
  assert.equal(events.filter(e => e.type === 'tool_call').length, 4);
});

test('a hallucinated tool gets a correctable error without running, prompting or failing the send', async t => {
  const requests = replay(t, [[call('think',{thought:'kept'}), call('shell',{cmd:'rm -rf /'})], [call('shell',{cmd:'again'})], null]);
  const events = [];
  const yaml = config([{type:'think'}], {permissions:{ask:['shell'], deny:['shell']}});
  const result = await api.chat({yaml, messages}, e => { events.push(e); });
  assert.equal(result.message.content, 'done');
  const unavailable = "Tool 'shell' is not available. You can only use the tools provided to you.";
  assert.deepEqual(requests[1].messages.slice(-2).map(m => m.content), ['Thoughts:\nkept', unavailable]);
  assert.equal(lastOutput(requests[2]), unavailable);
  assert.deepEqual(events.filter(e => e.type === 'tool_confirmation' || e.type === 'tool_call').map(e => e.name), ['think']);
  assert.equal(events.filter(e => e.type === 'tool_call_delta' && e.name === 'shell').length, 2);
});

test('MCP read-only claims do not authorize a dynamic call or make it pass readonly discovery', {timeout:5000}, async t => {
  const mcp = {type:'mcp', allow_private_ips:true, remote:{url:'https://mcp.invalid/rpc',transport_type:'streamable'}};
  const rpcCalls = [];
  const requests = replay(t, [[call('save',{})], null, [call('search_tool',{query:''})], null], async req => {
    assert.equal(req.url,'https://mcp.invalid/rpc');
    const m = await req.json();
    rpcCalls.push(m.method);
    if (m.method === 'server/discover') return new Response(null, {status: 404});
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

test('a filtered builtin is not offered and a denied one is refused, even under autoApprove', async t => {
  const requests = replay(t, [[call('create_todo',{description:'no'})], [call('list_todos',{})], null, [call('create_todo',{description:'no'})], null]);
  const events = [];
  for (const yaml of [config([{type:'todo',tools:['list_todos']}]), config([{type:'todo'}],{permissions:{deny:['create_todo']}})]) {
    await api.chat({yaml,messages,autoApprove:true},e => { events.push(e); });
  }
  assert.ok(requests.slice(0, 3).every(body => !names(body).includes('create_todo')));
  assert.ok(requests.slice(3).every(body => names(body).includes('create_todo'))); // Go offers a denied tool.
  assert.ok(events.every(e => !['tool_call','tool_confirmation'].includes(e.type) || e.name === 'list_todos'));
  const unavailable = "Tool 'create_todo' is not available. You can only use the tools provided to you.";
  assert.deepEqual(events.filter(e => e.type === 'tool_result' && e.is_error).map(e => e.output),
    [unavailable, "Tool 'create_todo' is denied by permissions configuration."]);
  assert.deepEqual(JSON.parse(lastOutput(requests[2])).todos, []);
});

test('same-name MCP tools keep their profile’s discovery, grants, transport and credentials across switches', {timeout:5000}, async t => {
  const proxy = 'https://guard.invalid/';
  const mcp = agent => ({defer:true, tools:['record'], allow_private_ips:agent === 'root',
    remote:{url:`https://${agent}.invalid/rpc`, headers:{Authorization:`Bearer \${env.${agent}}`}}});
  const agent = name => ({model:`local/${name}`, toolsets:[{type:'mcp',ref:name}]});
  const mcps = Object.fromEntries(['root','child','idle'].map(name => [name,mcp(name)]));
  const yaml = config([], {mcps, agents:{root:{...agent('root'), handoffs:['child','idle']},
    child:{...agent('child'), sub_agents:['root']}, idle:agent('idle')}});
  const env = {root:'root-only', child:'child-only', idle:'unused'};
  const secret = 'dckr_pat_' + 'A'.repeat(27);
  const write = [call('record',{note:secret})], add = [call('add_tool',{name:'record'})];
  const steps = [add, write, [call('handoff',{agent:'child'})], [call('search_tool',{query:''})], add, write, write,
    [call('transfer_task',{agent:'root', task:'write'})], write, null, null,
    write, [call('transfer_task',{agent:'root', task:'try again'})], write, null, null];
  const rpc = [], records = [], closed = [];
  const req = replay(t, steps, async request => {
    const url = new URL(request.url);
    const who = url.origin === 'https://root.invalid' ? 'root' : 'child';
    if (who === 'child') {
      assert.equal(url.origin + '/', proxy);
      assert.equal(url.searchParams.get('url'), 'https://child.invalid/rpc');
    } else assert.equal(request.url, 'https://root.invalid/rpc');
    assert.equal(request.headers.get('Authorization'), 'Bearer ' + env[who]);
    const session = request.headers.get('Mcp-Session-Id');
    if (request.method === 'DELETE') {
      closed.push([who, session]);
      return new Response(null,{status:204});
    }
    const msg = await request.json();
    rpc.push([who, msg.method]);
    if (msg.method === 'server/discover') return new Response(null, {status: 404});
    assert.equal(session, msg.method === 'initialize' ? null : who + '-session');
    if (msg.method === 'notifications/initialized') return new Response(null,{status:202});
    let result;
    if (msg.method === 'initialize') {
      result = {protocolVersion:'2025-06-18', capabilities:{tools:{}}, serverInfo:{name:who,version:'1'}};
    } else if (msg.method === 'tools/list') {
      result = {tools:['record','hidden'].map(name => ({name, description:who+' '+name, inputSchema:{type:'object'}}))};
    } else {
      assert.equal(msg.method, 'tools/call');
      assert.deepEqual(msg.params, {name:'record', arguments:{note:'[REDACTED]'}});
      records.push(who);
      result = {content:[{type:'text',text:secret}]};
    }
    return Response.json({jsonrpc:'2.0', id:msg.id, result}, {headers:{'Mcp-Session-Id':who+'-session'}});
  });
  let prompt = Promise.withResolvers();
  const s = await api.createSession({yaml, env, toolProxy:proxy}, e => {
    if (e.type === 'tool_confirmation') prompt.resolve(e);
  });
  t.after(() => s.close());
  async function decide(who, decision, stale) {
    const e = await prompt.promise;
    prompt = Promise.withResolvers();
    assert.equal(e.agent, who);
    assert.equal(e.id, 'call-0');
    assert.deepEqual(JSON.parse(e.args), {note:'[REDACTED]'});
    if (stale) await assert.rejects(s.confirm({confirmationId:stale, decision:'approve'}), /match/);
    await s.confirm({confirmationId:e.confirmationId, decision, toolName:'record'});
    return e.confirmationId;
  }
  assert.deepEqual(rpc, []);
  const run = s.send('switch');
  const first = await decide('root','approve_tool');
  assert.ok(rpc.every(([who]) => who === 'root'));
  // The child has to discover, activate and approve its own record despite the parent's grant.
  const rejected = await decide('child','reject',first);
  assert.deepEqual(records, ['root']);
  const granted = await decide('child','approve_tool',rejected);
  await run;
  assert.deepEqual(records, ['root','child','root']);
  assert.deepEqual(names(req[0]), ['handoff','search_tool','add_tool']);
  assert.deepEqual(names(req[3]), ['transfer_task','search_tool','add_tool']);
  assert.deepEqual(JSON.parse(lastOutput(req[4]).split('\n')[1]), [{name:'record',description:'child record'}]);
  assert.match(lastOutput(req[6]), /rejected/);
  assert.equal(lastOutput(req[7]), '[REDACTED]');
  for (const r of req) {
    const descriptions = (r.tools ?? []).filter(t => t.function.name === 'record').map(t => t.function.description);
    if (descriptions.length) assert.deepEqual(descriptions, [r.model+' record']);
    assert.ok(!JSON.stringify(r).includes(secret));
    assert.doesNotMatch(JSON.stringify(r), /root-only|child-only/);
  }
  await s.restart();
  const restarted = s.send('fresh');
  const again = await decide('child','reject',granted);
  await decide('root','reject',again);
  await restarted;
  assert.deepEqual(records, ['root','child','root']);
  assert.equal(req[11].model, 'child');
  assert.doesNotMatch(JSON.stringify(req[11].messages), /handed off|The user rejected/);
  assert.deepEqual(rpc.filter(([,m]) => m === 'initialize'), [['root','initialize'],['child','initialize']]);
  await s.close();
  assert.deepEqual(closed.sort(), [['child','child-session'],['root','root-session']]);
});

test('restart during child MCP discovery closes the partial session and retries from the parent', {timeout:5000}, async t => {
  const transfer = [call('transfer_task', {agent:'child',task:'list'})];
  const yaml = config([], {agents:{root:{model:'local/root',sub_agents:['child']},
    child:{model:'local/child',toolsets:[{type:'mcp',allow_private_ips:true,remote:{url:'https://child.invalid/rpc'}}]}}});
  const listing = Promise.withResolvers(), closed = [];
  let generation = 0;
  const requests = replay(t, [transfer, transfer, null, null], async req => {
    assert.equal(req.url,'https://child.invalid/rpc');
    const session = req.headers.get('Mcp-Session-Id');
    if (req.method === 'DELETE') {
      closed.push(session);
      return new Response(null,{status:204});
    }
    const msg = await req.json();
    if (msg.method === 'server/discover') return new Response(null, {status: 404});
    if (msg.method === 'initialize') {
      assert.equal(session,null);
      return Response.json({jsonrpc:'2.0',id:msg.id,result:{protocolVersion:'2025-06-18',capabilities:{tools:{}},serverInfo:{name:'test',version:'1'}}},
        {headers:{'Mcp-Session-Id':String(++generation)}});
    }
    assert.equal(session,String(generation));
    if (msg.method === 'notifications/initialized') return new Response(null,{status:202});
    assert.equal(msg.method,'tools/list');
    if (generation === 1) {
      listing.resolve();
      return new Promise((_, reject) => req.signal.addEventListener('abort', () => reject(req.signal.reason), {once:true}));
    }
    return Response.json({jsonrpc:'2.0',id:msg.id,result:{tools:[{name:'fresh',inputSchema:{type:'object'}}]}});
  });
  const s = await api.createSession({yaml});
  t.after(() => s.close());
  const abandoned = assert.rejects(s.send('abandoned'), /aborted/);
  await listing.promise;
  await s.restart();
  await abandoned;
  assert.deepEqual(closed,['1']);
  await s.send('retry');
  assert.deepEqual(requests.map(r => r.model), ['root','root','child','root']);
  assert.deepEqual(names(requests[2]), ['fresh']);
  assert.doesNotMatch(JSON.stringify(requests[1].messages), /abandoned/);
  await s.close();
  assert.deepEqual(closed,['1','2']);
});

const outputName = '__structured_output__';
const output = { mode: 'tool', name: 'report', strict: true,
  schema: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'], additionalProperties: false } };

test('only the internal output call bypasses filters, approval and tool transforms', async t => {
  const secret = 'dckr_pat_' + 'A'.repeat(27);
  const final = call(outputName, {text:secret});
  const requests = replay(t, [[final, final], [[outputName, '{']], [final, call('think', {thought:'ask'})], [final]]);
  const yaml = config([{type:'think'}], {permissions:{deny:[outputName], ask:['think']}},
    {structured_output:output, readonly:true});
  const events = [];
  const result = await api.chat({yaml, messages}, e => {
    events.push(e);
    if (e.type === 'tool_confirmation') return false;
  });
  assert.deepEqual(names(requests[0]), ['think', outputName]);
  assert.ok(requests.every(r => !r.response_format));
  assert.deepEqual(events.filter(e => e.type === 'tool_confirmation').map(e => e.name), ['think']);
  const replies = events.filter(e => e.type === 'tool_result' && e.name === outputName);
  assert.deepEqual(replies.map(e => e.is_error), [true,true,true,true,false]);
  assert.match(replies[2].output, /not valid JSON/);
  assert.equal(replies.at(-1).output, final[1]);
  assert.equal(result.message.content, final[1]);
  assert.equal(events.filter(e => e.type === 'finish').length, 1);
  // before_llm_call redaction still applies to replayed messages, not to the local final answer.
  assert.ok(!JSON.stringify(requests.slice(1)).includes(secret));
});

test('reserved output name cannot be reintroduced through host discovery', async t => {
  const requests = replay(t, []);
  for (const settings of [{}, {defer:true}, {readonly:true}]) {
    const yaml = config([post(outputName, 'wrong', settings)], {}, {structured_output:output});
    await assert.rejects(api.chat({yaml, messages}), /reserved for tool-mode structured output/);
  }
  assert.deepEqual(requests, []);
});

test('unsupported tool schemas fail before fetch even on unselected agents', async t => {
  const fetch = t.mock.method(globalThis, 'fetch', () => assert.fail('schema or model fetch'));
  const nested = items => ({type:'object', properties:{nested:{type:'array',items}}});
  for (const schema of [nested({$ref:'https://schema.invalid'}), nested({$ref:'#'}), nested({type:'string',pattern:'x{99999999}'}),
    nested({type:'string', format:'uri'}), {...nested({type:'string'}), $defs:{unused:{$ref:'file:///schema'}}},
    {...nested({$ref:'#/$defs/loop'}), $defs:{loop:{$ref:'#/$defs/loop'}}}]) {
    const agents = {root:{model:'local/test'}, nested:{model:'local/test', structured_output:{...output, schema}}};
    await assert.rejects(api.createSession({yaml:config([], {agents})}), /structured_output/);
  }
  const agents = {root:{model:'local/test', handoffs:['child']}, child:{model:'local/test', structured_output:output}};
  const handoff = await api.createSession({yaml:config([], {agents})});
  await handoff.close();
  agents.root = {model:'local/test', sub_agents:['child']};
  await assert.rejects(api.createSession({yaml:config([], {agents})}), /tool-mode structured output inside transfer_task/);
  for (const field of [{max_iterations:1}, {max_tool_result_tokens:1}, {num_history_items:1}, {toolsets:[{type:'think',model:'local/cheap'}]}]) {
    await assert.rejects(api.createSession({yaml:config([], {}, {structured_output:output,...field})}), /tool-mode structured output with history limits/);
  }
  assert.equal(fetch.mock.callCount(), 0);
});

test('native Claude schemas reject ambiguous values and seeds before model I/O', async t => {
  const fetch = t.mock.method(globalThis, 'fetch', () => assert.fail('unexpected fetch'));
  const yaml = config([], {}, { fallback:{models:['local/claude-test']}, structured_output:{name:'report',
    schema:{type:'object', properties:{rows:{type:'array', items:{oneOf:[{const:'value'}]}}}}} });
  for (const literal of ['0.125', '1.0', '-0.0', '1e-7', '1_000', '010', '!!int "0x11"', '"a\\tb"']) {
    await assert.rejects(api.createSession({yaml:yaml.replace('"value"', literal)}), /Claude schema/);
  }
  for (const seed of [
    {role:'system', content:'fallback', multi_content:[{type:'text', text:'leading system'}]},
    {role:'system', content:'fallback', multi_content:[{type:'image_url', image_url:{url:'https://media.invalid/pic'}}]},
    {role:'user', name:'caller', content:'previous'},
  ]) {
    await assert.rejects(api.chat({yaml, messages:[seed, ...messages]}), /unnamed text-only history/);
  }
  assert.equal(fetch.mock.callCount(), 0);
});
