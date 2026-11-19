// Run in a fresh browser page per implementation. Keep request/response scripts
// independent of the client: an extra model call is a failure, not a new answer.
async function boot() {
  if (location.pathname === '/rust') {
    const { loadDockerAgent } = await import('/agent.mjs');
    return loadDockerAgent();
  }
  await import('/wasm_exec.js');
  const go = new Go();
  const { instance } = await WebAssembly.instantiateStreaming(fetch('/go.wasm'), go.importObject);
  go.run(instance);
  return globalThis.dockerAgent;
}

const piece = (delta, finish_reason = null) => ({ choices: [{ index: 0, delta, finish_reason }] });
const usage = { choices: [], usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 } };
const answer = [piece({ role: 'assistant', content: 'hello ' }), piece({ content: '🌍' }, 'stop'), usage];
function tool(name, args) {
  const text = JSON.stringify(args);
  return [piece({ role: 'assistant', tool_calls: [{ id: 'call_1', index: 0, type: 'function', function: { name, arguments: text.slice(0, 4) } }] }),
    piece({ tool_calls: [{ index: 0, function: { arguments: text.slice(4) } }] }, 'tool_calls'), usage];
}
const config = `providers:
  local: { provider: openai, base_url: ${location.origin}/v1, token_key: LOCAL_KEY }
agents:
  root: { model: local/test, instruction: 'Hello \${env.WHO}' }
`;

async function replay(api, name, yaml, replies, drive, endpoint = '/v1/chat/completions', service) {
  const trace = { requests: [], events: [], results: [] };
  const ids = new Map();
  function correlation(value) {
    if (typeof value !== 'string' || !value) throw new Error('missing correlation ID');
    if (!ids.has(value)) ids.set(value, `id:${ids.size}`);
    return ids.get(value);
  }
  function record(event) {
    const e = { ...event };
    // Rename generated IDs by first occurrence, preserving all relationships.
    if ('message_id' in e) e.message_id = correlation(e.message_id);
    if (e.type === 'elicitation') e.id = correlation(e.id);
    trace.events.push(e);
  }
  const originalFetch = globalThis.fetch;
  let unexpected = '';
  globalThis.fetch = async (input, init) => {
    const req = new Request(input, init);
    if (service && req.url !== new URL(endpoint, location.origin).href) return service(req);
    if (req.url !== new URL(endpoint, location.origin).href || req.method !== 'POST' || !replies[trace.requests.length]) {
      unexpected = `unexpected ${req.method} ${req.url} (call ${trace.requests.length + 1})`;
      throw new Error(unexpected);
    }
    const chunks = replies[trace.requests.length];
    trace.requests.push({ method: req.method, path: new URL(req.url).pathname,
      authorization: req.headers.get('Authorization'), body: await req.json() });
    const isChat = endpoint.endsWith('/chat/completions');
    const stream = chunks.map(c => `data: ${JSON.stringify(isChat ? { id: 'completion', object: 'chat.completion.chunk', model: 'test', ...c } : c)}\r\n\r\n`).join('');
    return new Response(stream + (isChat ? 'data: [DONE]\r\n\r\n' : ''), { headers: { 'Content-Type': 'text/event-stream' } });
  };
  try {
    await drive({ api, options: { yaml, env: { LOCAL_KEY: 'fixture', WHO: 'Ada' } }, record, trace });
    if (unexpected || trace.requests.length !== replies.length) throw new Error(unexpected || `expected ${replies.length} requests; got ${trace.requests.length}`);
    return trace;
  } catch (e) { throw new Error(`${name}: ${e.message}`); }
  finally { globalThis.fetch = originalFetch; }
}

async function run() {
  const api = await boot();
  const tests = {};
  async function turns({ api, options, record, trace }) {
    const s = await api.createSession(options, record);
    try {
      for (const prompt of ['first', 'again']) trace.results.push(await s.send(prompt));
    } finally { await s.close(); }
  }
  const picker = config.replace('agents:', 'models: {careful: {provider: local, model: precise, max_tokens: 33, temperature: 0.2}}\nagents:')
    .replace('model: local/test', 'model: local/test, toolsets: [{type: model_picker, models: [careful, local/fast]}, {type: session_context}]');
  tests.modelPicker = await replay(api, 'modelPicker', picker, [tool('change_model', { model: 'careful' }), answer,
    tool('change_model', { model: 'off-list' }), tool('list_sessions', {}), tool('read_session', { session_id: '-1' }),
    tool('change_model', { model: 'local/fast' }), tool('revert_model', {}), answer], turns);
  tests.plain = await replay(api, 'plain', config, [answer, answer], turns);
  const team = config.replace('model: local/test', 'model: local/test, sub_agents: [helper], handoffs: [helper]') + '  helper: {model: local/worker, description: Helps, instruction: Be precise}\n';
  tests.transfer = await replay(api, 'transfer', team, [tool('transfer_task', { agent: 'helper', task: 'count', expected_output: 'numbers' }), answer, answer, answer], turns);
  const teamFallback = team.replace('model: local/worker', 'model: local/worker, fallback: {models: [local/worker-backup]}');
  tests.transferFallback = await replay(api, 'transferFallback', teamFallback, [tool('transfer_task', { agent: 'helper', task: 'count' }),
    [{ error: { message: 'worker unavailable' } }], answer, answer], single);
  tests.handoff = await replay(api, 'handoff', team, [tool('handoff', { agent: 'helper' }), answer, answer], turns);
  tests.think = await replay(api, 'think', config.replace('model: local/test', 'toolsets: [{type: think}], model: local/test'),
    [tool('think', { thought: 'a plan' }), answer, answer], turns);
  tests.truncatedTool = await replay(api, 'truncatedTool', config.replace('model: local/test', 'toolsets: [{type: think}], model: local/test'),
    [[piece({ content: 'Partial result' }), piece({ tool_calls: [{ id: 'cut', index: 0, type: 'function', function: { name: 'think', arguments: '{' } }] }, 'length'), usage], answer], turns);
  tests.abortRestart = await replay(api, 'abortRestart', config, [answer, answer], async ({ api, options, record, trace }) => {
    let abortNext = true;
    const s = await api.createSession(options, e => {
      record(e);
      if (abortNext && e.type === 'delta') { abortNext = false; s.abort(); }
    });
    try {
      const turn = s.send('do not retain');
      await turn.then(() => { throw new Error('abort did not reject'); }, e => {
        if (!/aborted/i.test(e.message)) throw e;
        trace.results.push('aborted');
      });
      await s.restart();
      trace.results.push(await s.send('try again'));
    } finally { await s.close(); }
  });
  tests.fallback = await replay(api, 'fallback', config.replace('model: local/test', 'model: local/test, fallback: {models: [local/backup]}'),
    [[{ error: { message: 'primary is down' } }], answer, answer], turns);
  tests.streamError = await replay(api, 'streamError', config, [[{ error: { message: 'fixture model error' } }], answer], async ({ api, options, record, trace }) => {
    const s = await api.createSession(options, record);
    try {
      await s.send('invalid').then(() => { throw new Error('expected model error'); }, e => trace.results.push(e.message));
      trace.results.push(await s.send('recover'));
    } finally { await s.close(); }
  });
  const withOptions = config.replace('provider: openai,', 'provider: openai, temperature: 0.7, max_tokens: 24, parallel_tool_calls: true,')
    .replace('agents:', 'models:\n  chosen: {provider: local, model: test, temperature: 0, top_p: 0.8, frequency_penalty: 0.1, presence_penalty: 0.2, parallel_tool_calls: false}\nagents:')
    .replace('model: local/test', 'toolsets: [{type: think}], model: chosen');
  tests.sampling = await replay(api, 'sampling', withOptions, [answer, answer], turns);
  const native = 'agents: {root: {model: openai/gpt-4o-mini, instruction: hi, toolsets: [{type: think}]}}';
  tests.firstParty = await replay(api, 'firstParty', native, [answer], async ({ api, options, record, trace }) => {
    options.env = { OPENAI_API_KEY: 'fixture' };
    trace.results.push(await api.chat({ ...options, messages: [{ role: 'user', content: 'hi' }] }, record));
  }, 'https://api.openai.com/v1/chat/completions');
  const math = config.replace('model: local/test', 'toolsets: [{type: calculator}], model: local/test');
  tests.calculator = await replay(api, 'calculator', math, [tool('calculate', { expression: '1/3 + 2^-3' }), answer, answer], turns);
  const todo = config.replace('model: local/test', 'toolsets: [{type: todo}], model: local/test');
  tests.todos = await replay(api, 'todos', todo, [tool('create_todos', { descriptions: ['one', 'two'] }), answer,
    tool('update_todos', { updates: [{ id: 'todo_1', status: 'completed' }] }), answer], turns);
  tests.todoRestart = await replay(api, 'todoRestart', todo, [tool('create_todo', { description: 'keep' }), answer,
    tool('list_todos', {}), answer, tool('list_todos', {}), answer], async ({ api, options, record, trace }) => {
    const s = await api.createSession(options, record);
    try {
      trace.results.push(await s.send('create'));
      await s.restart();
      trace.results.push(await s.send('list after restart'));
    } finally { await s.close(); }
    const isolated = await api.createSession(options, record);
    try { trace.results.push(await isolated.send('list in another session')); } finally { await isolated.close(); }
  });
  tests.catalog = await replay(api, 'catalog', 'agents: {root: {model: groq/test, instruction: hello}}', [answer], async ({ api, options, record, trace }) => {
    options.env = { GROQ_API_KEY: 'fixture' };
    trace.results.push(await api.chat({ ...options, messages: [{ role: 'user', content: 'hi' }] }, record));
  }, 'https://api.groq.com/openai/v1/chat/completions');
  const local = kinds => config.replace('model: local/test', `toolsets: [${kinds.map(type => `{type: ${type}}`).join(',')}], model: local/test`);
  async function single({api, options, trace, record}) {
    trace.results.push(await api.chat({...options,messages:[{role:'user',content:'go'}]}, record));
  }
  tests.random = await replay(api, 'random', local(['random']), [tool('random_int', {min:42, max:42}), answer], single);
  tests.clock = await replay(api, 'clock', local(['datetime']), [tool('get_datetime', {format:'MST', timezone:'UTC'}), answer], single);
  tests.memory = await replay(api, 'memory', local(['memory']), [tool('get_memories', {}), answer], single);
  tests.plan = await replay(api, 'plan', local(['plan']), [tool('list_plans', {}), answer], single);
  tests.rejectWrite = await replay(api, 'rejectWrite', local(['memory']), [tool('add_memory', {memory:'not stored'}), answer], async ({api, options, record, trace}) => {
    trace.results.push(await api.chat({...options,messages:[{role:'user',content:'go'}]}, e => {
      record(e); return e.type === 'tool_confirmation' ? {decision:'reject',reason:'tester declined'} : undefined;
    }));
  });
  tests.readonly = await replay(api, 'readonly', config.replace('model: local/test', 'readonly: true, toolsets: [{type: memory, tools: [add_memory,get_memories], instruction: "Scoped"}], model: local/test'), [answer], single);
  const apiTool = config.replace('model: local/test', 'toolsets: [{type: api, allow_private_ips: true, api_config: {name: greet, instruction: Return a greeting, method: GET, endpoint: "https://tool.invalid/greet"}}], model: local/test');
  function greet(req) {
    if (req.url !== 'https://tool.invalid/greet' || req.method !== 'GET') throw new Error('unexpected API request');
    return new Response('hi');
  }
  tests.apiTool = await replay(api, 'apiTool', apiTool, [tool('greet', {}), answer], single, '/v1/chat/completions', greet);
  const apiFallback = apiTool.replace('model: local/test', 'model: local/test, fallback: {models: [local/backup]}');
  tests.fallbackHost = await replay(api, 'fallbackHost', apiFallback, [[{ error: { message: 'primary unavailable' } }], tool('greet', {}), answer], single,
    '/v1/chat/completions', greet);
  const mcp = config.replace('model: local/test', 'toolsets: [{type: mcp, allow_private_ips: true, remote: {transport_type: streamable, url: "https://tool.invalid/mcp"}}], model: local/test');
  tests.mcp = await replay(api, 'mcp', mcp,[tool('echo',{text:'from MCP'}),answer], async ({api,options,record,trace}) => {
    const s=await api.createSession({...options, autoApprove:true}, record);
    try {trace.results.push(await s.send('echo'));} finally {await s.close();}
  }, '/v1/chat/completions', async req => {
    if (req.url !== 'https://tool.invalid/mcp') throw new Error('unexpected MCP endpoint');
    if (req.method === 'DELETE') return new Response(null,{status:204});
    const m=await req.json();
    if (m.method==='notifications/initialized') return new Response(null,{status:202});
    const val = m.method==='initialize' ? {protocolVersion:'2025-06-18',capabilities:{tools:{}},serverInfo:{name:'fixture',version:'1'}}
      : m.method==='tools/list' ? {tools:[{name:'echo',description:'Echo a word',inputSchema:{type:'object',properties:{text:{type:'string'}},required:['text'],additionalProperties:false}}]}
      : m.method==='tools/call' && m.params.name==='echo' ? {content:[{type:'text',text:m.params.arguments.text}]} : undefined;
    if (!val) throw new Error(`unexpected MCP method ${m.method}`);
    return Response.json({jsonrpc:'2.0',id:m.id,result:val},{headers:{'Mcp-Session-Id':'replay'}});
  });
  const question = config.replace('model: local/test', 'toolsets: [{type: user_prompt}], model: local/test');
  tests.ask = await replay(api, 'ask', question, [tool('user_prompt', { message: 'Which color?', title: 'Color', schema: { type: 'string' } }), answer],
    async ({ api, options, record, trace }) => {
      trace.results.push(await api.chat({ ...options, messages: [{ role: 'user', content: 'ask' }] }, async e => {
        record(e);
        if (e.type === 'elicitation') return { action: 'accept', content: { answer: 'blue' } };
      }));
    });
  tests.askDecline = await replay(api, 'askDecline', question, [tool('user_prompt', { message: 'Proceed?' }), answer],
    async ({ api, options, trace }) => {
      const s = await api.createSession(options); // No handler must decline, not hang or auto-accept.
      try { trace.results.push(await s.send('ask')); } finally { await s.close(); }
    });
  const fin = output => ({ type: 'response.completed', response: { id: 'resp', status: 'completed', output,
    usage: { input_tokens: 3, output_tokens: 2, total_tokens: 5 } } });
  const text = [{ type: 'response.output_text.delta', item_id: 'm', output_index: 0, delta: 'hi' }, fin([])];
  const fn = { type: 'function_call', id: 'item_1', name: 'think', call_id: 'call_1', arguments: '' };
  const args = '{"thought":"plan"}';
  const responseTool = [{ type: 'response.output_item.added', output_index: 0, item: fn },
    { type: 'response.function_call_arguments.delta', output_index: 0, item_id: fn.id, delta: args },
    { type: 'response.function_call_arguments.done', output_index: 0, item_id: fn.id, arguments: args },
    fin([{ ...fn, arguments: args, status: 'completed' }])];
  const switchArgs = '{"model":"careful"}';
  const change = { ...fn, name: 'change_model' };
  const switched = [{ type: 'response.output_item.added', output_index: 0, item: change },
    { type: 'response.function_call_arguments.done', output_index: 0, item_id: fn.id, arguments: switchArgs },
    fin([{ ...change, arguments: switchArgs, status: 'completed' }])];
  tests.responsesPicker = await replay(api, 'responsesPicker', picker.replace('provider: openai,', 'provider: openai, api_type: openai_responses,'),
    [switched, text, text], turns, '/v1/responses');
  const responses = config.replace('provider: openai,', 'provider: openai, api_type: openai_responses,')
    .replace('model: local/test', 'toolsets: [{type: think}], model: local/test');
  const combined = config.replace('model: local/test', 'toolsets: [{type: user_prompt}, {type: think}, {type: calculator}], model: local/test');
  tests.toolOrder = await replay(api, 'toolOrder', combined, [answer, answer], turns);
  tests.responses = await replay(api, 'responses', responses, [responseTool, text, text], turns, '/v1/responses');
  tests.autoResponses = await replay(api, 'autoResponses', 'agents: {root: {model: openai/o3-mini, instruction: hi}}', [text], async ({ api, options, record, trace }) => {
    options.env = { OPENAI_API_KEY: 'fixture' };
    trace.results.push(await api.chat({ ...options, messages: [{ role: 'user', content: 'hi' }] }, record));
  }, 'https://api.openai.com/v1/responses');
  return tests;
}

try { await fetch('/trace', { method: 'POST', body: JSON.stringify({ tests: await run() }) }); }
catch (e) { await fetch('/trace', { method: 'POST', body: JSON.stringify({ error: e.stack || String(e) }) }); }
