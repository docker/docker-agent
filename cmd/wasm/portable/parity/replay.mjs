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
function tool(name, args) { return rawTool(name, JSON.stringify(args)); }
function rawTool(name, text, id = 'call_1') {
  return [piece({ role: 'assistant', tool_calls: [{ id, index: 0, type: 'function', function: { name, arguments: text.slice(0, 4) } }] }),
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
    const expected = Array.isArray(endpoint) ? endpoint[trace.requests.length] : endpoint;
    const match = expected && req.url === new URL(expected, location.origin).href;
    if (service && !match) return service(req);
    if (!match || req.method !== 'POST' || !replies[trace.requests.length]) {
      unexpected = `unexpected ${req.method} ${req.url} (call ${trace.requests.length + 1})`;
      throw new Error(unexpected);
    }
    const chunks = replies[trace.requests.length];
    const url = new URL(req.url), headers = Object.fromEntries(req.headers);
    for (const [key, version] of [['session_id', '4'], ['x-opencode-session', '5']]) {
      if (!(key in headers)) continue;
      if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[45][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(headers[key]) || headers[key][14] !== version) throw new Error(`invalid ${key}`);
      headers[key] = correlation(headers[key]);
    }
    // The two pages use different ephemeral loopback ports. Credential values here are scripted fixtures.
    trace.requests.push({ method: req.method, origin: url.origin === location.origin ? 'self' : url.origin, path: url.pathname + url.search,
      headers, body: await req.json() });
    if (!Array.isArray(chunks)) {
      const init = { status: chunks.status, headers: chunks.headers };
      return 'raw' in chunks ? new Response(chunks.raw, init) : Response.json(chunks.body, init);
    }
    const isChat = url.pathname.endsWith('/chat/completions');
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
  const limited = config.replace('model: local/test', 'model: local/test, num_history_items: 2, toolsets: [{type: think}]');
  tests.historyLimit = await replay(api, 'historyLimit', limited, [tool('think', { thought: 'private assistant work' }), answer, answer], turns);
  const historyLimits = 'num_history_items: 5, max_tool_result_tokens: 4, max_old_tool_call_tokens: 4';
  const historyFallback = limited.replace('num_history_items: 2',
    `${historyLimits}, session_compaction: false, fallback: {models: [local/backup], retries: -1, cooldown: 1h}`);
  for (const [name, failure] of [['historyFallbackTransient', 'service unavailable'], ['historyFallbackPinned', 'invalid model']]) {
    tests[name] = await replay(api, name, historyFallback,
      [tool('think', {thought: 'first long thought'}), [{error: {message: failure}}],
        rawTool('think', '{"thought":"second long thought"}', 'call_2'), answer, answer], turns);
  }
  tests.historyFallbackSeed = await replay(api, 'historyFallbackSeed', historyFallback,
    [[{error: {message: 'invalid model'}}], answer], async ({api, options, record, trace}) => {
      const messages = [
        {role: 'user', name: 'caller', content: 'previous'},
        {role: 'assistant', tool_calls: [{id: 'old', type: 'function', function: {name: 'think', arguments: '{}'}}]},
        {role: 'tool', tool_call_id: 'old', content: '🌍'.repeat(5)},
        {role: 'user', content: 'again'},
      ];
      trace.results.push(await api.chat({...options, messages}, record));
    });
  const override = config.replace('agents:', 'models: {cheap: {provider: local, model: fast, max_tokens: 37}}\nagents:')
    .replace('model: local/test', 'model: local/test, toolsets: [{type: think, model: cheap}, {type: calculator}]');
  tests.toolModel = await replay(api, 'toolModel', override, [tool('think', { thought: 'plan' }), tool('calculate', { expression: '2+2' }), answer, answer], turns);
  const compact = config.replace('agents:', 'models:\n  small: {provider: local, model: test, context_window: 2048, compaction_threshold: 0.5}\nagents:')
    .replace('model: local/test', 'model: small, session_compaction: true, redact_secrets: false, toolsets: [{type: think}]');
  const highUsage = { choices: [], usage: { prompt_tokens: 1000, completion_tokens: 25, total_tokens: 1025 } };
  const secret = 'dckr_pat_' + 'A'.repeat(27);
  for (const [name, setting, expected] of [
    ['compact', 'redact_secrets: false, ', secret],
    ['compactRedaction', '', '[REDACTED]'],
    ['compactExplicitRedaction', 'redact_secrets: false, hooks: {before_llm_call: {type: builtin, command: redact_secrets}}, ', '[REDACTED]'],
  ]) {
    const yaml = compact.replace('redact_secrets: false, ', setting);
    tests[name] = await replay(api, name, yaml, [tool('think', { thought: 'remember' }),
      [piece({ content: 'first answer' }, 'stop'), highUsage], [piece({ content: 'key facts' }, 'stop'), usage], answer],
      async ({ api, options, record, trace }) => {
        const s = await api.createSession(options, record);
        try {
          const prefix = 'old '.repeat(500);
          trace.results.push(await s.send(prefix + secret));
          trace.results.push(await s.send('continue'));
          const summary = trace.requests[2].body;
          if (!summary.messages[0].content.includes('summarize conversations') ||
              !summary.messages.some(m => m.role === 'user' && m.content?.startsWith(prefix + expected)) ||
              (expected !== secret && JSON.stringify(summary).includes(secret))) {
            throw new Error('summary request did not honor outgoing redaction policy');
          }
        } finally { await s.close(); }
      });
  }
  const chain = (await (await fetch('/fallback-compaction.yaml')).text()).replace('$MODEL_URL', `${location.origin}/v1`);
  const disabled = chain.replace('session_compaction: true', 'session_compaction: false');
  const failover = [[{ error: { message: 'primary unavailable' } }], [piece({ content: 'first answer' }, 'stop'), highUsage]];
  tests.compactionDisabledFallback = await replay(api, 'compactionDisabledFallback', disabled, [...failover, answer], turns);
  const compactModels = (name, yaml, endpoint, replies, failed) => replay(api, name, yaml, replies,
    async ({ api, options, record, trace }) => {
      const s = await api.createSession(options, record);
      try {
        const old = 'old '.repeat(500);
        trace.results.push(await s.send(old + secret));
        const before = trace.events.length;
        const next = s.send('continue');
        if (failed) {
          const message = await next.then(() => { throw new Error('expected summary rejection'); }, e => e.message);
          trace.results.push(message);
          if (trace.events.length !== before + 1 || trace.events[before].type !== 'error' || trace.events[before].message !== message) {
            throw new Error('expected only the summary error on the rejected send');
          }
        } else {
          trace.results.push(await next);
        }
        // Even after a summary stream error, Go drains a silent spare reply before rejecting.
        if (trace.requests.length !== 4) throw new Error(`expected 4 requests after the second send; got ${trace.requests.length}`);
        trace.results.push(await s.send('afterwards'));
        const body = trace.requests.at(-1).body;
        const history = body.messages ?? body.input;
        const prefix = failed ? old + '[REDACTED]' : 'Session Summary: key facts';
        if (history[0].content !== prefix || history.at(-2).content !== 'second answer') {
          throw new Error('continuation lost the prefix or the spare reply');
        }
        if (JSON.stringify(trace.requests).includes(secret)) throw new Error('unredacted fallback history');
      } finally { await s.close(); }
    }, endpoint);
  for (const failed of [false, true]) {
    const name = failed ? 'fallbackCompactionError' : 'fallbackCompaction';
    const summary = failed ? [{ error: { message: 'summary unavailable' } }] : [piece({ content: 'key facts' }, 'stop'), usage];
    tests[name] = await compactModels(name, chain, '/v1/chat/completions',
      [...failover, summary, [piece({ content: 'second answer' }, 'stop'), usage], answer], failed);
  }
  const toolAtBoundary = [...tool('think', { thought: 'plan' }).slice(0, -1),
    { choices: [], usage: { prompt_tokens: 1022, completion_tokens: 2, total_tokens: 1024 } }];
  tests.compactAfterTools = await replay(api, 'compactAfterTools', compact, [toolAtBoundary,
    [piece({ content: 'finished the plan' }, 'stop'), usage], answer, answer], turns);
  const limits = config.replace('model: local/test','model: local/test, max_iterations: 1, max_tool_result_tokens: 4, toolsets: [{type: think}]');
  tests.sessionLimits = await replay(api,'sessionLimits',limits,[tool('think',{thought:'0123456789abcdefghijklmnopqrstuvwxyz'}),answer],turns);
  const cappedFallback = config.replace('agents:', 'models: {backup: {provider: local, model: backup, context_window: 4096}}\nagents:')
    .replace('model: local/test', 'model: local/test, max_iterations: 1, session_compaction: false, fallback: {models: [backup]}, toolsets: [{type: think}]');
  tests.fallbackIterationLimit = await replay(api, 'fallbackIterationLimit', cappedFallback,
    [[{ error: { message: 'primary unavailable' } }], tool('think', { thought: 'plan' }), answer], turns);
  const twoRounds = cappedFallback.replace('max_iterations: 1', 'max_iterations: 2').replace('models: [backup]', 'models: [backup], retries: -1');
  for (const pinned of [false, true]) {
    const name = pinned ? 'cappedPinnedToolRounds' : 'cappedTransientToolRounds';
    const fail = [{error: {message: pinned ? 'invalid model' : 'service unavailable'}}];
    tests[name] = await replay(api, name, twoRounds, [fail, tool('think', {thought:'first'}),
      rawTool('think', '{"thought":"second"}', 'second_call'), answer], turns);
  }
  tests.cappedContinuationFailover = await replay(api, 'cappedContinuationFailover', twoRounds,
    [tool('think', {thought:'one'}), [{error:{message:'invalid model'}}], answer, answer], turns);
  tests.cappedContinuationError = await replay(api, 'cappedContinuationError', twoRounds,
    [tool('think', {thought:'committed'}), [{error:{message:'invalid model'}}], [{error:{message:'backup unavailable'}}], answer],
    rejectsThenRecovers);
  const hookedFallback = twoRounds.replace('max_iterations: 2', 'hooks: {before_llm_call: {type: builtin, command: max_iterations, args: ["2"]}}');
  async function rejectsThenRecovers({api, options, record, trace}) {
    const s = await api.createSession(options, record);
    try {
      await s.send('first').then(() => { throw new Error('expected rejected send'); }, e => trace.results.push(e.message));
      trace.results.push(await s.send('again'));
    } finally { await s.close(); }
  }
  tests.fallbackIterationHook = await replay(api, 'fallbackIterationHook', hookedFallback,
    [[{error:{message:'service unavailable'}}], tool('think', {thought:'first'}),
      rawTool('think', '{"thought":"second"}', 'second_call'), answer], rejectsThenRecovers);
  tests.fallbackIterationHookAndCap = await replay(api, 'fallbackIterationHookAndCap',
    hookedFallback.replace('session_compaction: false', 'session_compaction: false, max_iterations: 2'),
    [[{error:{message:'invalid model'}}], tool('think', {thought:'first'}),
      rawTool('think', '{"thought":"second"}', 'second_call'), answer], turns);
  tests.plain = await replay(api, 'plain', config, [answer, answer], turns);
  const team = config.replace('model: local/test', 'model: local/test, sub_agents: [helper], handoffs: [helper]') + '  helper: {model: local/worker, description: Helps, instruction: Be precise}\n';
  tests.transfer = await replay(api, 'transfer', team, [tool('transfer_task', { agent: 'helper', task: 'count', expected_output: 'numbers' }), answer, answer, answer], turns);
  const teamFallback = team.replace('model: local/worker', 'model: local/worker, fallback: {models: [local/worker-backup]}');
  tests.transferFallback = await replay(api, 'transferFallback', teamFallback, [tool('transfer_task', { agent: 'helper', task: 'count' }),
    [{ error: { message: 'worker unavailable' } }], answer, answer], single);
  const pipeline = config.replace('model: local/test', 'model: local/test, force_handoff: helper') + '  helper: {model: local/worker, instruction: Summarize}\n';
  tests.forceHandoff = await replay(api, 'forceHandoff', pipeline, [answer, answer, answer], turns);
  tests.handoff = await replay(api, 'handoff', team, [tool('handoff', { agent: 'helper' }), answer, answer], turns);
  // Go fixes the cap when a session or delegated task opens; hooks follow the acting agent.
  const ponder = (text, id = 'call_1') => rawTool('think', JSON.stringify({thought: text}), id);
  const limitedTeam = (root, helper) => config.replace('model: local/test', `model: local/test, ${root} toolsets: [{type: think}], sub_agents: [helper], handoffs: [helper]`)
    + `  helper: {model: local/worker, ${helper} toolsets: [{type: think}], description: Helps, instruction: Be precise}\n`;
  const limitedPipeline = (root, helper) => config.replace('model: local/test', `model: local/test, ${root} toolsets: [{type: think}], force_handoff: helper`)
    + `  helper: {model: local/worker, ${helper} toolsets: [{type: think}], instruction: Summarize}\n`;
  const limitedTrio = (root, first, second) => config.replace('model: local/test', `model: local/test, ${root} toolsets: [{type: think}]`)
    + `  first: {model: local/first, ${first} toolsets: [{type: think}], description: First, instruction: Be first}\n`
    + `  second: {model: local/second, ${second} toolsets: [{type: think}], description: Second, instruction: Be second}\n`;
  const callLimit = n => `hooks: {before_llm_call: {type: builtin, command: max_iterations, args: ["${n}"]}},`;
  const delegate = (agent = 'helper') => tool('transfer_task', {agent, task: 'count'});
  const pass = (agent = 'helper') => tool('handoff', {agent});
  const handoffOnly = yaml => yaml.replace('sub_agents: [helper], ', '');
  for (const [name, yaml, replies, drive = turns] of [
    ['teamCapTransfer', limitedTeam('max_iterations: 2,', ''), [delegate(), answer, ponder('a'), answer]],
    ['teamCapDelegate', limitedTeam('', 'max_iterations: 1,'), [delegate(), ponder('a'), answer, answer]],
    ['teamCapHandoff', limitedTeam('max_iterations: 2,', 'max_iterations: 5,'), [pass(), ponder('a'), ponder('b'), ponder('c', 'call_2')]],
    ['teamCapHandoffTargetIgnored', limitedTeam('', 'max_iterations: 1,'), [pass(), ponder('a'), answer, ponder('b'), answer]],
    ['teamCapAtHandoff', limitedTeam('max_iterations: 1,', ''), [pass(), ponder('a')]],
    ['teamCapAtReturn', limitedTeam('max_iterations: 1,', ''), [delegate(), answer, answer]],
    ['teamCapAtDenial', limitedTeam('max_iterations: 1,', ''), [delegate('ghost'), answer]],
    ['teamCapBounce', handoffOnly(limitedTeam('max_iterations: 3,', 'handoffs: [root],')), [pass(), pass('root'), ponder('a'), answer]],
    ['teamCapForced', limitedPipeline('max_iterations: 1,', ''), [answer, answer]],
    ['teamCapForcedTool', limitedPipeline('max_iterations: 2,', ''), [answer, ponder('a'), answer]],
    ['teamCapBeforeForced', limitedPipeline('max_iterations: 1,', ''), [ponder('a'), answer]],
    ['teamCapForcedTargetIgnored', limitedPipeline('', 'max_iterations: 1,'), [answer, ponder('a'), answer, answer]],
    ['teamCapNested', limitedTrio('max_iterations: 3, sub_agents: [first],', 'max_iterations: 2, sub_agents: [second],', 'max_iterations: 1,'),
      [delegate('first'), delegate('second'), ponder('deep'), ponder('middle'), answer, answer]],
    ['teamCapDelegateHandoff', limitedTrio('sub_agents: [first],', 'max_iterations: 2, handoffs: [second],', 'max_iterations: 7,'),
      [delegate('first'), pass('second'), ponder('a'), answer, answer]],
    ['teamCapDelegateForced', limitedTrio('sub_agents: [first],', 'max_iterations: 1, force_handoff: second,', ''),
      [delegate('first'), answer, answer, answer]],
    ['transferHandoffReturn', limitedTrio('sub_agents: [first],', 'handoffs: [second],', ''),
      [delegate('first'), pass('second'), answer, answer, answer]],
    ['teamCapChain', limitedTeam('max_iterations: 2,', '').replace('model: local/worker,', 'model: local/worker, fallback: {models: [local/worker-backup]},'),
      [delegate(), [{error: {message: 'worker unavailable'}}], answer, ponder('a'), answer]],
    ['teamCapAndHook', limitedTeam(`max_iterations: 2, ${callLimit(2)}`, '').replace('handoffs: [helper]', 'handoffs: []'),
      [delegate(), answer, ponder('a'), answer]],
    ['teamHookTransfer', limitedTeam(callLimit(2), ''), [delegate(), answer, ponder('a'), answer], rejectsThenRecovers],
    ['teamHookAtReturn', limitedTeam(callLimit(1), '').replace('handoffs: [helper]', 'handoffs: []'), [delegate(), answer, answer], rejectsThenRecovers],
    ['teamHookHandoffSource', limitedTeam(callLimit(2), ''), [pass(), ponder('a'), answer, answer]],
    ['teamHookHandoffTarget', handoffOnly(limitedTeam('', callLimit(1))), [pass(), answer], rejectsThenRecovers],
    ['teamHookBounce', handoffOnly(limitedTeam(callLimit(3), 'handoffs: [root],')), [pass(), pass('root'), ponder('a'), answer], rejectsThenRecovers],
    ['teamHookForced', limitedPipeline('', callLimit(1)), [answer, answer], rejectsThenRecovers],
  ]) tests[name] = await replay(api, name, yaml, replies, drive);
  // Tool budgets and the loop guard stay with the session; num_history_items follows the acting agent.
  const lengthy = n => `long thought number ${n} ${'x'.repeat(40)}`;
  const budgets = 'max_tool_result_tokens: 4, max_old_tool_call_tokens: 4, num_history_items: 3,';
  const passAs = id => rawTool('handoff', '{"agent":"helper"}', id);
  const afterHandoff = [ponder(lengthy(2), 'c'), ponder(lengthy(3), 'd'), answer, ponder(lengthy(4), 'e'), answer];
  for (const [name, yaml, replies, drive = turns] of [
    ['teamLoopGuard', limitedTeam('max_consecutive_tool_calls: 2,', ''), [pass(), ponder('same'), ponder('same'), answer], rejectsThenRecovers],
    ['teamLoopGuardTargetIgnored', limitedTeam('', 'max_consecutive_tool_calls: 2,'), [pass(), ponder('same'), ponder('same'), ponder('same'), answer, answer]],
    ['teamHistoryCaller', limitedTeam('max_tool_result_tokens: 4,', ''), [ponder(lengthy(1)), delegate(), ponder(lengthy(2)), answer, answer, answer]],
    ['teamHistoryDelegate', limitedTeam('', budgets), [delegate(), ponder(lengthy(1)), ponder(lengthy(2), 'b'), ponder(lengthy(3), 'c'), answer, answer, answer]],
    ['teamHistoryDelegatedAnswer', limitedTeam('max_tool_result_tokens: 1,', ''),
      [delegate(), [piece({role: 'assistant', content: 'a long delegated answer that exceeds the cap'}, 'stop'), usage], answer, answer]],
    ['teamHistoryHandoff', limitedTeam(budgets, 'num_history_items: 4, max_tool_result_tokens: 100,'), [ponder(lengthy(1), 'a'), passAs('b'), ...afterHandoff]],
    ['teamHistoryHandoffTarget', limitedTeam('', budgets), [ponder(lengthy(1), 'a'), passAs('b'), ...afterHandoff]],
    ['teamHistoryReusedIds', limitedTeam(budgets, ''), [ponder(lengthy(1)), pass(), ...afterHandoff]],
    ['teamHistoryChain', limitedTeam(budgets, '').replace('model: local/worker,', 'model: local/worker, fallback: {models: [local/worker-backup]},'),
      [ponder(lengthy(1), 'a'), passAs('b'), answer, [{error: {message: 'invalid model'}}], ponder(lengthy(2), 'c'), answer]],
  ]) tests[name] = await replay(api, name, yaml, replies, drive);
  tests.teamCapForcedRestart = await replay(api, 'teamCapForcedRestart', limitedPipeline('max_iterations: 1,', ''), [answer, answer, answer],
    async ({api, options, record, trace}) => {
      const s = await api.createSession(options, record);
      try {
        trace.results.push(await s.send('first'));
        await s.restart();
        for (const prompt of ['fresh', 'again']) trace.results.push(await s.send(prompt));
      } finally { await s.close(); }
    });
  tests.think = await replay(api, 'think', config.replace('model: local/test', 'toolsets: [{type: think}], model: local/test'),
    [tool('think', { thought: 'a plan' }), answer, answer], turns);
  tests.loopGuard = await replay(api,'loopGuard', config.replace('model: local/test','max_consecutive_tool_calls: 2, toolsets: [{type: think}], model: local/test'),
    [tool('think',{thought:'repeat'}),tool('think',{thought:'repeat'}),answer],async ({api,options,record,trace})=>{
      const s=await api.createSession(options,record);
      try {await s.send('loop').then(()=>{throw new Error('expected termination');},e=>trace.results.push(e.message));trace.results.push(await s.send('recover'));}finally{await s.close();}
    });
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
  tests.streamError = await replay(api, 'streamError', config, [[{ error: { message: 'scripted model error' } }], answer], async ({ api, options, record, trace }) => {
    const s = await api.createSession(options, record);
    try {
      await s.send('invalid').then(() => { throw new Error('expected model error'); }, e => trace.results.push(e.message));
      trace.results.push(await s.send('recover'));
    } finally { await s.close(); }
  });
  const withOptions = config.replace('provider: openai,', 'provider: openai, provider_opts: {service_tier: priority}, temperature: 0.7, max_tokens: 24, parallel_tool_calls: true,')
    .replace('agents:', 'models:\n  chosen: {provider: local, model: test, provider_opts: {service_tier: flex}, temperature: 0, top_p: 0.8, frequency_penalty: 0.1, presence_penalty: 0.2, parallel_tool_calls: false}\nagents:')
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
  // A refusal is a tool result: Go redacts its reason and caps the stored copy.
  const refuse = async ({api, options, record, trace}) => {
    trace.results.push(await api.chat({...options,messages:[{role:'user',content:'go'}]}, e => {
      record(e); return e.type === 'tool_confirmation' ? {decision:'reject',reason:`no ${secret}`} : undefined;
    }));
  };
  tests.rejectSecret = await replay(api, 'rejectSecret', local(['memory']), [tool('add_memory', {memory:'not stored'}), answer], refuse);
  tests.rejectCapped = await replay(api, 'rejectCapped', local(['memory']).replace('model: local/test', 'max_tool_result_tokens: 4, model: local/test'),
    [tool('add_memory', {memory:'not stored'}), answer], refuse);
  tests.deniedCapped = await replay(api, 'deniedCapped',
    local(['think']).replace('model: local/test', 'max_tool_result_tokens: 4, model: local/test') + 'permissions: {deny: ["think:thought=blocked"]}\n',
    [tool('think', {thought:'blocked'}), answer], single);
  tests.readonly = await replay(api, 'readonly', config.replace('model: local/test', 'readonly: true, toolsets: [{type: memory, tools: [add_memory,get_memories], instruction: "Scoped"}], model: local/test'), [answer], single);
  const apiTool = config.replace('model: local/test', 'toolsets: [{type: api, allow_private_ips: true, api_config: {name: greet, instruction: Return a greeting, method: GET, endpoint: "https://tool.invalid/greet"}}], model: local/test');
  function greet(req) {
    if (req.url !== 'https://tool.invalid/greet' || req.method !== 'GET') throw new Error('unexpected API request');
    return new Response('hi');
  }
  tests.apiTool = await replay(api, 'apiTool', apiTool, [tool('greet', {}), answer], single, '/v1/chat/completions', greet);
  for (const [name, body] of [['blankToolResult', ''], ['whitespaceToolResult', '  \n ']]) {
    tests[name] = await replay(api, name, apiTool, [tool('greet', {}), answer], single, '/v1/chat/completions', () => new Response(body));
  }
  const apiFallback = apiTool.replace('model: local/test', 'model: local/test, fallback: {models: [local/backup]}');
  tests.fallbackHost = await replay(api, 'fallbackHost', apiFallback, [[{ error: { message: 'primary unavailable' } }], tool('greet', {}), answer], single,
    '/v1/chat/completions', greet);
  const fixture = 'https://tool.invalid';
  const httpConfig = ts => config.replace('model: local/test', `toolsets: [${JSON.stringify({ allow_private_ips: true, ...ts })}], model: local/test`);
  async function approved({ api, options, trace, record }) {
    trace.results.push(await api.chat({ ...options, messages: [{ role: 'user', content: 'go' }], autoApprove: true }, record));
  }
  const pages = new Map([
    ['/article', '<!doctype html><html><head><title>Ignore</title></head><body><h2>News</h2><p>Hé <b>there</b> &amp; <em>friends</em>!<br>Read <a href="/next">next</a>.</p></body></html>'],
    ['/ok.php', '<ul><li>One</li><li>Two</li></ul>'],
    ['/hidden', '<script src="/never.js">throw 0</script><style>@import "/never.css";</style><img src="/never.png" alt="diagram">'],
  ]);
  for (const format of ['text', 'markdown']) {
    const name = `fetch_${format}`;
    const urls = ['/article', '/nested/no.php', '/ok.php', '/hidden'].map(path => fixture + path);
    tests[name] = await replay(api, name, httpConfig({ type: 'fetch' }), [tool('fetch', { urls, format }), answer], approved, '/v1/chat/completions', req => {
      const path = new URL(req.url).pathname;
      if (path === '/robots.txt') return new Response('User-agent: *\nDisallow: /\nUser-agent: Cagent\nDisallow: *.php$\nAllow: /ok.php$');
      if (!pages.has(path)) throw new Error('unexpected fetch fixture request');
      return new Response(pages.get(path), { statusText: 'OK', headers: { 'Content-Type': 'text/html; charset=utf-8' } });
    });
  }
  // Include the service's view in the trace, even when the tool only returns an error.
  async function httpReplay(name, yaml, call, respond, drive = approved) {
    const calls = [];
    tests[name] = await replay(api, name, yaml, [call, answer], drive, '/v1/chat/completions', async req => {
      calls.push({ method: req.method, url: req.url, headers: Object.fromEntries(req.headers), body: await req.text() });
      return respond(req);
    });
    tests[name].http = calls;
  }
  const page = fixture + '/page', directFetch = httpConfig({ type: 'fetch' });
  const robotsPage = (pageResponse, robots = () => new Response(null, { status: 404 })) =>
    req => new URL(req.url).pathname === '/robots.txt' ? robots() : pageResponse(req);
  const htmlPage = robotsPage(() => new Response('<ol><li> </li><li><a href="/x"></a></li><li><img src="/i" alt="a  b"></li></ol>',
    { headers: { 'Content-Type': 'text/html;charset=latin1' } }));
  for (const [name, args, yaml = directFetch, service = htmlPage] of [
    ['fetch_defaultFormat', { urls: [page] }],
    ['fetch_caseFormat', { urls: [page], format: 'MARKDOWN' }],
    ['fetch_unknownFormat', { urls: [page], format: 'constructor', timeout: -1 }],
    ['fetch_zeroToolTimeout', { urls: [page], timeout: 0 }, httpConfig({ type: 'fetch', timeout: 0 })],
    ['fetch_negativeToolTimeout', { urls: [page] }, httpConfig({ type: 'fetch', timeout: -1 })],
    ['fetch_html', { urls: [page, page], format: 'html' }],
    ['fetch_emptyListItems', { urls: [page], format: 'markdown' }],
    ['fetch_nullURL', { urls: [null] }],
    ['fetch_invalidURLs', { urls: ['', 'path', '//bad.invalid', 'urn:test', 'ftp://bad.invalid/', 'https:///x', ' https://bad.invalid', page + '#bad%'] }],
    ['fetch_invalidAuthority', { urls: ['https://tool.invalid:bad/p', 'https://bad host.invalid/', '/bad%'] }],
    ['fetch_noProxy', { urls: [page + '?q=abc', fixture, 'HTTPS://TOOL.INVALID/a b#x y'] }, httpConfig({ type: 'fetch', allow_private_ips: false })],
    ['fetch_escapedPaths', { urls: [page + '%2Fb (*) 🌍', page + ' ', fixture + ':444/%2f?q=é'] }],
    ['fetch_allowDomains', { urls: [page, 'https://OTHER.invalid./no'] }, httpConfig({ type: 'fetch', allowed_domains: ['tool.invalid'] })],
    ['fetch_blockDomains', { urls: [page, 'https://OTHER.invalid./no'] }, httpConfig({ type: 'fetch', blocked_domains: ['other.invalid'] })],
    ['fetch_configHeaders', { urls: [page] }, httpConfig({ type: 'fetch', headers: { 'X-Name': '${env.WHO}', Accept: 'example/test' } })],
    ['fetch_binaryMulti', { urls: [page, page] }, directFetch, robotsPage(() => new Response(Uint8Array.of(0xef, 0xbf, 0xbd, 0xff, 0xe2, 0x82)))],
  ]) await httpReplay(name, yaml, tool('fetch', args), service);
  for (const [name, respond] of [
    ['empty204', () => new Response(null, { status: 204 })],
    ['movedNoLocation', () => new Response('moved', { status: 301 })],
    ['custom400', () => new Response('<bad>&\u2028\u2029', { status: 400, statusText: 'custom' })],
    ['empty404', () => new Response(null, { status: 404 })],
    ['busy429', () => new Response('busy', { status: 429 })],
    ['error500', () => new Response('error', { status: 500 })],
    ['unknown599', () => new Response(null, { status: 599, statusText: 'custom' })],
    ['badLength', () => new Response('body', { headers: { 'Content-Length': '-1' } })],
    ['closedEarly', () => new Response(new ReadableStream({ start(c) { c.error(new TypeError('connection closed')); } }))],
    ['offline', () => { throw new TypeError('Failed to fetch'); }],
  ]) {
    await httpReplay(`fetch_${name}`, directFetch, tool('fetch', { urls: [page, page] }), robotsPage(respond));
    await httpReplay(`api_${name}`, apiTool, tool('greet', {}), respond);
  }
  for (const [name, robots] of [
    ['robots403', () => new Response(null, { status: 403 })], ['robots500', () => new Response(null, { status: 500 })],
    ['robotsDisallow', () => new Response('User-agent: *\nDisallow: /')],
    ['robotsReadError', () => new Response(new ReadableStream({ start(c) { c.error(new Error('disconnected')); } }))],
  ]) await httpReplay(`fetch_${name}`, directFetch, tool('fetch', { urls: [page, page] }), robotsPage(() => new Response('ok'), robots));
  await httpReplay('api_responseByteLimit', apiTool, tool('greet', {}), () => new Response('é'.repeat(15001)));
  await httpReplay('api_noProxy', apiTool.replace('allow_private_ips: true', 'allow_private_ips: false'), tool('greet', {}), greet);
  const proxied = args => approved({ ...args, options: { ...args.options, toolProxy: 'https://egress.invalid/relay' } });
  const sameOriginRedirect = req => {
    const path = new URL(new URL(req.url).searchParams.get('url')).pathname;
    if (path === '/robots.txt') return new Response(null, { status: 302, headers: { 'X-Docker-Agent-Location': '/rules' } });
    if (path === '/rules') return new Response('User-agent: *\nDisallow: /private');
    if (path === '/page') return new Response(null, { status: 307, headers: { 'X-Docker-Agent-Location': '/done#omitted' } });
    return new Response('ok');
  };
  await httpReplay('fetch_relayRedirect', httpConfig({ type: 'fetch', allow_private_ips: false }),
    tool('fetch', { urls: [page, fixture + '/private'] }), sameOriginRedirect, proxied);
  await httpReplay('api_relayRedirect', httpConfig({ type: 'api', allow_private_ips: false,
    api_config: { name: 'greet', method: 'POST', endpoint: page, headers: { 'X-Name': '${env.WHO}' } } }),
    tool('greet', { message: 'hello' }), sameOriginRedirect, proxied);
  const row = { type: 'object', properties: { label: { type: 'string' }, text: { type: 'string' } }, required: ['text'] };
  const postArgs = { rows: { type: 'array', items: row } };
  for (const method of ['POST', 'PUT', 'PATCH', 'DELETE', 'HEAD', 'OPTIONS']) {
    const name = `api_${method}`;
    const ts = { type: 'api', api_config: { name: 'submit', instruction: 'Submit', method, endpoint: fixture + '/submit', args: postArgs } };
    const values = { rows: [{ text: '{"keep":"<literal>&"}', label: null }, { text: '${env.WHO}' }] };
    tests[name] = await replay(api, name, httpConfig(ts), [tool('submit', values), answer], approved, '/v1/chat/completions', async req => {
      if (req.url !== fixture + '/submit' || req.method !== method) throw new Error('unexpected API fixture request');
      const body = await req.text();
      if (method === 'HEAD') return new Response(null, { status: 418, statusText: "I'm a teapot" });
      return new Response(`${method} ${req.headers.get('Content-Type') ?? '(no content type)'}: ${body}`);
    });
  }
  const workerAPI = teamFallback.replace('model: local/worker,', 'use_toolsets: [greeting], model: local/worker,')
    + 'toolsets: {greeting: {type: api, allow_private_ips: true, api_config: {name: greet, instruction: Return a greeting, method: GET, endpoint: "https://tool.invalid/greet"}}}';
  tests.transferHostFallback = await replay(api, 'transferHostFallback', workerAPI, [tool('transfer_task', {agent:'helper', task:'greet'}),
    [{error:{message:'worker unavailable'}}], tool('greet', {}), answer, answer], single, '/v1/chat/completions', greet);
  tests.teamCapHostTool = await replay(api, 'teamCapHostTool', workerAPI.replace('fallback: {models: [local/worker-backup]}', 'max_iterations: 1'),
    [tool('transfer_task', {agent:'helper', task:'greet'}), tool('greet', {}), answer], single, '/v1/chat/completions', greet);
  const mcp = config.replace('agents:', `mcps:
  remote: {name: default, allow_private_ips: true, remote: {transport_type: streamable, url: 'https://tool.invalid/mcp', headers: {X-Agent: '\${env.WHO}'}}}
toolsets:
  echo: {type: mcp, ref: remote, name: named, instruction: 'Use the named MCP.', tools: [named_echo]}
agents:`).replace('model: local/test', 'toolsets: [{type: think}], use_toolsets: [echo], model: local/test');
  const echoService = output => async req => {
    if (req.url !== 'https://tool.invalid/mcp') throw new Error('unexpected MCP endpoint');
    if (req.method === 'DELETE') return new Response(null,{status:204});
    if (req.headers.get('X-Agent') !== 'Ada') throw new Error('missing inherited MCP header');
    const m=await req.json();
    if (m.method==='server/discover') return new Response(null,{status:404});
    if (m.method==='notifications/initialized') return new Response(null,{status:202});
    const val = m.method==='initialize' ? {protocolVersion:'2025-06-18',capabilities:{tools:{}},serverInfo:{name:'fixture',version:'1'}}
      : m.method==='tools/list' ? {tools:[{name:'echo',description:'Echo a word',inputSchema:{type:'object',properties:{text:{type:'string'}},required:['text'],additionalProperties:false}}]}
      : m.method==='tools/call' && m.params.name==='echo' ? {content:[{type:'text',text:output ?? m.params.arguments.text}]} : undefined;
    if (!val) throw new Error(`unexpected MCP method ${m.method}`);
    return Response.json({jsonrpc:'2.0',id:m.id,result:val},{headers:{'Mcp-Session-Id':'replay'}});
  };
  tests.mcp = await replay(api, 'mcp', mcp,[tool('named_echo',{text:'from MCP'}),answer], async ({api,options,record,trace}) => {
    const s=await api.createSession({...options, autoApprove:true}, record);
    try {trace.results.push(await s.send('echo'));} finally {await s.close();}
  }, '/v1/chat/completions', echoService());
  const redacted = 'dckr_pat_' + 'A'.repeat(27);
  tests.hostRedaction = await replay(api,'hostRedaction',apiTool,[tool('greet',{}),answer],single,'/v1/chat/completions', req => {
    if(req.url!=='https://tool.invalid/greet') throw new Error('unexpected URL');
    return new Response(redacted);
  });
  tests.dynamicContext = await replay(api,'dynamicContext',config.replace('model: local/test','add_date: true, add_environment_info: true, model: local/test'),[answer,answer],turns);
  // Advance the wall clock after each request, leaving retry/cooldown timers monotonic.
  async function datedReplay(name, yaml, replies, drive = turns, endpoints, timestamp) {
    tests[name] = await replay(api, name, yaml, replies, async args => {
      const Clock = globalThis.Date;
      const start = timestamp ?? (Clock.UTC(2030, 4, 2, 12) + new Clock().getTimezoneOffset() * 60_000);
      globalThis.Date = class extends Clock {
        constructor(...a) { super(...(a.length ? a : [start + args.trace.requests.length * 86_400_000])); }
      };
      try { await drive(args); } finally { globalThis.Date = Clock; }
    }, endpoints);
  }
  const datedFallback = historyFallback.replace('num_history_items: 5', 'add_date: true, add_environment_info: true, num_history_items: 5');
  await datedReplay('dateDuringFailover', datedFallback,
    [[{error:{message:'invalid model'}}], tool('think', {thought:'once'}), answer, answer]);
  const datedTeam = team.replace('sub_agents: [helper]', 'add_environment_info: true, sub_agents: [helper]')
    .replace('model: local/worker', 'model: local/worker, add_date: true');
  await datedReplay('contextAcrossHandoff', datedTeam, [tool('handoff', {agent:'helper'}), answer, answer]);
  await datedReplay('contextAcrossTransfer', datedTeam, [tool('transfer_task', {agent:'helper', task:'count'}), answer, answer, answer]);
  const forcedContext = pipeline.replace('force_handoff: helper', 'add_date: true, force_handoff: helper')
    .replace('model: local/worker', 'model: local/worker, add_environment_info: true');
  await datedReplay('contextAcrossForcedHandoff', forcedContext, [answer, answer, answer]);
  const deferred = config.replace('model: local/test', 'toolsets: [{type: think, defer: true}], model: local/test');
  tests.discovery = await replay(api, 'discovery', deferred, [tool('search_tool',{query:'think'}),tool('add_tool',{name:'think'}),tool('think',{thought:'plan'}),answer],single);
  const compactTodo = config.replace('model: local/test','toolsets: [{type: todo, toon: create_todo}], model: local/test');
  tests.toon = await replay(api, 'toon', compactTodo, [tool('create_todo',{description:'one'}),answer],single);
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
  const responseCall = (item, args) => [{ type: 'response.output_item.added', output_index: 0, item },
    { type: 'response.function_call_arguments.delta', output_index: 0, item_id: item.id, delta: args },
    { type: 'response.function_call_arguments.done', output_index: 0, item_id: item.id, arguments: args },
    fin([{ ...item, arguments: args, status: 'completed' }])];
  const responseTool = responseCall(fn, args);
  const withSpareKey = drive => async args => {
    args.options.env.SPARE_KEY = 'spare-only';
    await drive(args);
  };
  const responsesHelper = (root, helper) => limitedTeam(root, helper).replace('model: local/worker', 'model: spare/worker')
    .replace('providers:', `providers:\n  spare: {provider: openai, api_type: openai_responses, base_url: ${location.origin}/v1, token_key: SPARE_KEY}`);
  tests.teamCapResponsesDelegate = await replay(api, 'teamCapResponsesDelegate', responsesHelper('max_iterations: 2,', 'max_iterations: 1,'),
    [delegate(), responseTool, ponder('a'), answer], withSpareKey(turns),
    ['/v1/chat/completions', '/v1/responses', '/v1/chat/completions', '/v1/chat/completions']);
  tests.teamCapResponsesHandoff = await replay(api, 'teamCapResponsesHandoff', responsesHelper('max_iterations: 2,', ''),
    [pass(), responseTool, text], withSpareKey(turns), ['/v1/chat/completions', '/v1/responses', '/v1/responses']);
  const responseBackup = twoRounds.replace('providers:', `providers:\n  spare: {provider: openai, api_type: openai_responses, base_url: ${location.origin}/v1, token_key: SPARE_KEY}`)
    .replace('backup: {provider: local', 'backup: {provider: spare');
  tests.cappedCrossApi = await replay(api, 'cappedCrossApi', responseBackup,
    [[{error:{message:'service unavailable'}}], responseTool, answer, answer], withSpareKey(turns),
    ['/v1/chat/completions', '/v1/responses', '/v1/chat/completions', '/v1/chat/completions']);
  const responseError = message => [{ type: 'response.failed', response: { id: 'id', status: 'failed', error: { code: 'invalid', message } } }];
  const chat = 'openai_chatcompletions', resp = 'openai_responses';
  const historyRoutes = (main, spare, cap = '') => `providers:
  local: {provider: openai, api_type: ${main}, base_url: ${location.origin}/v1, token_key: LOCAL_KEY}
  spare: {provider: openai, api_type: ${spare}, base_url: ${location.origin}/backup, token_key: SPARE_KEY, max_tokens: 71}
agents: {root: {model: local/test, instruction: 'Hello \${env.WHO}', ${historyLimits}, ${cap}
  session_compaction: false, fallback: {models: [spare/backup], retries: -1, cooldown: 1h}, toolsets: [{type: think}]}}`;
  const datedHistoryCap = historyRoutes(chat, resp, 'max_iterations: 2, add_date: true,');
  await datedReplay('datedCappedMixedHistory', datedHistoryCap,
    [[{error:{message:'service unavailable'}}], responseTool, answer, answer], withSpareKey(turns),
    ['/v1/chat/completions', '/backup/responses', '/v1/chat/completions', '/v1/chat/completions']);
  const dates = tests.datedCappedMixedHistory.requests.map(({body}) =>
    /Today's date: (\d{4}-\d{2}-\d{2})/.exec(JSON.stringify(body.messages ?? body.input))?.[1]);
  if (dates.join() !== '2030-05-02,2030-05-02,2030-05-04,2030-05-05') throw new Error(`unexpected round dates: ${dates}`);
  const pathFor = protocol => protocol === chat ? 'chat/completions' : 'responses';
  function thought(protocol, id) {
    const args = JSON.stringify({ thought: `long thought ${id}` });
    return protocol === chat ? rawTool('think', args, id) : responseCall({...fn, call_id: id}, args);
  }
  const iterationHook = 'hooks: {before_llm_call: {type: builtin, command: max_iterations, args: ["3"]}},';
  for (const [name, main, spare, cap, pinned, drive] of [
    ['historyResponsesFallback', resp, resp, '', true, turns],
    ['historyChatToResponsesHook', chat, resp, iterationHook, false, rejectsThenRecovers],
    ['historyResponsesToChatCap', resp, chat, 'max_iterations: 3,', true, turns],
    ['historyChatToResponsesCapAndHook', chat, resp, `max_iterations: 3, ${iterationHook}`, true, turns],
  ]) {
    const active = pinned ? spare : main;
    const mainPath = `/v1/${pathFor(main)}`, sparePath = `/backup/${pathFor(spare)}`;
    const activePath = pinned ? sparePath : mainPath;
    const failure = pinned ? 'invalid model' : 'service unavailable';
    const finish = active === chat ? answer : text;
    const replies = [thought(main, 'first'), main === chat ? [{error:{message:failure}}] : responseError(failure),
      thought(spare, 'second'), cap ? thought(active, 'third') : finish, finish];
    tests[name] = await replay(api, name, historyRoutes(main, spare, cap), replies, withSpareKey(drive),
      [mainPath, mainPath, sparePath, activePath, activePath]);
  }
  const seedRoutes = historyRoutes(chat, resp).replace('num_history_items: 5', 'num_history_items: 7');
  tests.historySeedCrossApi = await replay(api, 'historySeedCrossApi', seedRoutes,
    [[{error:{message:'invalid model'}}], text], withSpareKey(async ({api, options, record, trace}) => {
      const call = id => ({id, type: 'function', function: {name: 'think', arguments: '{}'}});
      const messages = [{role: 'user', content: 'one'}, {role: 'assistant', tool_calls: [call('trim')]},
        {role: 'tool', tool_call_id: 'trim', content: 'drop with call'}, {role: 'user', content: 'two'},
        {role: 'assistant', content: 'checking', tool_calls: [call('a'), call('b')]},
        {role: 'tool', tool_call_id: 'a', content: '🌍'.repeat(5)}, {role: 'tool', tool_call_id: 'b', content: 'four'},
        {role: 'user', content: 'again'}];
      trace.results.push(await api.chat({...options, messages}, record));
    }), ['/v1/chat/completions', '/backup/responses']);
  const switchArgs = '{"model":"careful"}';
  const change = { ...fn, name: 'change_model' };
  const switched = [{ type: 'response.output_item.added', output_index: 0, item: change },
    { type: 'response.function_call_arguments.done', output_index: 0, item_id: fn.id, arguments: switchArgs },
    fin([{ ...change, arguments: switchArgs, status: 'completed' }])];
  tests.responsesPicker = await replay(api, 'responsesPicker', picker.replace('provider: openai,', 'provider: openai, api_type: openai_responses,'),
    [switched, text, text], turns, '/v1/responses');
  const responses = config.replace('provider: openai,', 'provider: openai, api_type: openai_responses, provider_opts: {service_tier: priority},')
    .replace('model: local/test', 'toolsets: [{type: think}], model: local/test');
  const combined = config.replace('model: local/test', 'toolsets: [{type: user_prompt}, {type: think}, {type: calculator}], model: local/test');
  tests.toolOrder = await replay(api, 'toolOrder', combined, [answer, answer], turns);
  tests.responses = await replay(api, 'responses', responses, [responseTool, text, text], turns, '/v1/responses');
  tests.historyResponses = await replay(api, 'historyResponses', responses.replace('model: local/test', `model: local/test, ${historyLimits}`),
    [thought(resp, 'first'), thought(resp, 'second'), text, text], turns, '/v1/responses');
  const compactResponses = compact.replace('provider: openai,', 'provider: openai, api_type: openai_responses,');
  const busy = structuredClone(text);
  busy.at(-1).response.usage = { input_tokens: 1000, output_tokens: 25, total_tokens: 1025 };
  tests.compactResponses = await replay(api, 'compactResponses', compactResponses, [busy, text, text], turns, '/v1/responses');
  const responseText = delta => [{ ...text[0], delta }, fin([])];
  for (const failed of [false, true]) {
    const name = failed ? 'fallbackCompactionErrorResponses' : 'fallbackCompactionResponses';
    const summary = failed ? responseError('summary unavailable') : responseText('key facts');
    tests[name] = await compactModels(name, chain.replace('openai_chatcompletions', 'openai_responses'), '/v1/responses',
      [responseError('primary unavailable'), busy, summary, responseText('second answer'), text], failed);
  }
  // Distinct windows, formats and bearers catch accidental reuse of the conversation route.
  for (const [name, smallSummary, failed] of [
    ['dedicatedSmallerSummary', true, false], ['dedicatedLargerSummary', false, false], ['dedicatedSummaryFailure', true, true],
  ]) {
    const mainApi = smallSummary ? 'openai_chatcompletions' : 'openai_responses';
    const summaryApi = smallSummary ? 'openai_responses' : 'openai_chatcompletions';
    const mainPath = smallSummary ? '/v1/chat/completions' : '/v1/responses';
    const summaryPath = smallSummary ? '/summary/responses' : '/summary/chat/completions';
    const yaml = `providers:
  local: {provider: openai, api_type: ${mainApi}, base_url: ${location.origin}/v1, token_key: LOCAL_KEY${failed ? ', compaction_model: summarizer' : ''}}
  brief: {provider: openai, api_type: ${summaryApi}, base_url: ${location.origin}/summary, token_key: SUMMARY_KEY}
models:
  ordinary: {provider: local, model: main, temperature: 0.7, context_window: ${smallSummary ? 8192 : 2048}, max_tokens: 6000${!smallSummary ? ', compaction_model: summarizer' : ''}}
  summarizer: {provider: brief, model: short, temperature: 0.25, context_window: ${smallSummary ? 2048 : 4096}, max_tokens: 17}
agents: {root: {model: ordinary, ${smallSummary && !failed ? 'compaction_model: summarizer, ' : ''}session_compaction: true, compaction_threshold: 0.5,
  structured_output: {name: result, schema: {type: object, properties: {}}}}}`;
    const summary = failed ? responseError('summary unavailable') : smallSummary ? responseText('key facts') : [piece({content:'key facts'}, 'stop'),usage];
    const ordinary = smallSummary ? answer : text;
    tests[name] = await replay(api, name, yaml, [smallSummary ? [piece({content:'first'},'stop'),highUsage] : busy, summary, ordinary, ordinary],
      async ({api, options, record, trace}) => {
        options.env.SUMMARY_KEY = 'summary-only';
        const s = await api.createSession(options, record);
        try {
          trace.results.push(await s.send('old '.repeat(800) + secret));
          const next = s.send('continue');
          if (failed) await next.then(() => {throw new Error('expected summary failure');}, e => trace.results.push(e.message));
          else trace.results.push(await next);
          trace.results.push(await s.send('afterwards'));
          if (trace.requests[1].headers.authorization !== 'Bearer summary-only' || JSON.stringify(trace.requests).includes(secret)) throw new Error('bad summary credentials or unredacted history');
        } finally { await s.close(); }
      }, [mainPath, summaryPath, mainPath, mainPath]);
  }
  const summaryChain = chain.replace('providers:', `providers:\n  brief: {provider: openai, api_type: openai_responses, base_url: ${location.origin}/summary, token_key: LOCAL_KEY}`)
    .replace('models:', 'models:\n  brief: {provider: brief, model: summary, context_window: 2048, temperature: 0.25}')
    .replace('session_compaction: true', 'session_compaction: true\n    compaction_model: brief');
  for (const failed of [false, true]) {
    const name = failed ? 'dedicatedPinnedFailure' : 'dedicatedPinned';
    const main = '/v1/chat/completions';
    tests[name] = await compactModels(name, summaryChain, [main, main, '/summary/responses', main, main],
      [...failover, failed ? responseError('summary unavailable') : responseText('key facts'),
        [piece({content:'second answer'}, 'stop'), usage], answer], failed);
  }
  const tooLong = [{error: {code: 'context_length_exceeded', message: 'over budget'}}];
  const busyChat = [piece({content: 'first answer'}, 'stop'), highUsage];
  const mainPath = '/v1/chat/completions', summaryPath = '/summary/responses';
  tests.summaryOverflowChat = await replay(api, 'summaryOverflowChat', compact,
    [busyChat, [{...piece({content:'discard'}), usage:usage.usage}, ...tooLong], answer, answer], turns);
  const transient = responseError('service unavailable');
  tests.summaryOverflowResponsesRetries = await replay(api, 'summaryOverflowResponsesRetries', compactResponses,
    [busy, transient, responseError('Exceeds the context window'), transient, transient, text, text], turns, '/v1/responses');

  const sends = prompts => async ({api, options, record, trace}) => {
    const s = await api.createSession(options, record);
    try {
      for (const prompt of prompts) trace.results.push(await s.send(prompt).catch(e => e.message));
    } finally { await s.close(); }
  };
  tests.summaryOverflowPinned = await replay(api, 'summaryOverflowPinned', chain.replace(chat, resp),
    [responseError('primary unavailable'), busy, responseText('key facts'), busy, responseError('prompt too long'), text, text],
    sends(['first', 'again', 'last']), '/v1/responses');
  tests.summaryOverflowLimit = await replay(api, 'summaryOverflowLimit', chain,
    [answer, busyChat, tooLong, tooLong, tooLong, tooLong, answer, answer],
    sends(['a'.repeat(500), 'b'.repeat(160), 'last', 'afterwards']));
  tests.summaryOverflowNothingFits = await replay(api, 'summaryOverflowNothingFits', chain,
    [...failover, tooLong, [piece({content:'second answer'}, 'stop'),usage], answer],
    sends(['old '.repeat(500), 'again', 'afterwards']));
  const dedicatedSummary = summaryChain.replace(/^    fallback:.*$/m, '');
  tests.summaryOverflowThenMedia = await replay(api, 'summaryOverflowThenMedia', dedicatedSummary,
    [busyChat, ...Array(3).fill({status:500, body:{error:{code:'context_length_exceeded'}}}),
      responseError('image exceeds the token limit'), answer, answer], sends(['first', 'again', 'afterwards']),
    [mainPath, summaryPath, summaryPath, summaryPath, summaryPath, mainPath, mainPath]);
  tests.summaryOverflowHiddenCode = await replay(api, 'summaryOverflowHiddenCode', chain.replace(/^    fallback:.*$/m, '    compaction_model: spare'),
    [busyChat, tooLong, {status:400, body:{error:{code:'context_length_exceeded', message:'over budget'}}}, answer, answer],
    sends(['first', 'again', 'afterwards']));
  tests.summaryNothingFitsInitially = await replay(api, 'summaryNothingFitsInitially', chain,
    [busyChat, answer], sends(['old '.repeat(2000), 'again']));
  const schema = { type: 'object', properties: { colors: { type: 'array', items: { type: 'string' } } },
    required: ['colors'], additionalProperties: false };
  const jsonAnswer = '{"colors":["blue"]}';
  const withSchema = (yaml, options, model = 'local/test') => yaml.replace(`model: ${model}`,
    `model: ${model}, structured_output: ${JSON.stringify({ name: 'palette', schema, ...options })}`);
  const chatJSON = [piece({ role: 'assistant', content: jsonAnswer }, 'stop'), usage];
  tests.structuredChat = await replay(api, 'structuredChat', withSchema(config, {}), [chatJSON, chatJSON], turns);
  const responsesJSON = [{ ...text[0], delta: jsonAnswer }, fin([])];
  tests.structuredResponses = await replay(api, 'structuredResponses',
    withSchema(responses, { description: 'Chosen colors', strict: true, mode: 'native' }),
    [responseTool, responsesJSON, responsesJSON], turns, '/v1/responses');
  const claude = 'local/anthropic/CLAUDE-sonnet-4';
  const escapedSchema = { ...schema, $defs: { 'z<&>': { enum: ['🌍\u2028\u2029<> & " \\', -7, 9007199254740991, null, true] } } };
  const claudeOutput = yaml => withSchema(yaml, { schema: escapedSchema, strict: true, description: 'Palette <&>' })
    .replace('local/test', claude);
  tests.claudeStructuredChat = await replay(api, 'claudeStructuredChat', claudeOutput(local(['think'])),
    [tool('think', {thought: 'plan'}), chatJSON, chatJSON], turns);
  tests.claudeStructuredResponses = await replay(api, 'claudeStructuredResponses', claudeOutput(responses),
    [responseTool, responsesJSON, responsesJSON], turns, '/v1/responses');
  tests.claudeCopilot = await replay(api, 'claudeCopilot', withSchema(local(['think']), {}).replace('local/test', 'github-copilot/claude-test'),
    [chatJSON], async args => { args.options.env.GITHUB_TOKEN = 'fixture'; await single(args); }, 'https://api.githubcopilot.com/chat/completions');
  const noInstruction = withSchema(config, {}).replace('local/test', 'local/claude-test').replace(/, instruction: '[^']*'/, '');
  tests.claudeStructuredHistory = await replay(api, 'claudeStructuredHistory', noInstruction, [chatJSON], async ({api, options, record, trace}) => {
    const messages = [{role:'system', content:'first system'}, {role:'system', content:'second system'}, {role:'user', content:'go'}];
    trace.results.push(await api.chat({...options, messages}, record));
  });
  const claudeFallback = withSchema(config, {}).replace('model: local/test',
    'model: local/openai/claude-test, fallback: {models: [local/global.anthropic.claude-test]}');
  tests.claudeStructuredFallback = await replay(api, 'claudeStructuredFallback', claudeFallback,
    [[{error:{message:'invalid model'}}], chatJSON, chatJSON], turns);
  const claudePicker = claudeOutput(picker).replace(claude, 'primary')
    .replace('models: {careful:', 'models: {primary: {provider: local, model: claude-start, context_window: 2048}, careful:')
    .replace('model: precise', 'model: claude-picked, context_window: 2048')
    .replace('models: [careful, local/fast]', 'models: [careful]');
  tests.claudeStructuredPicker = await replay(api, 'claudeStructuredPicker', claudePicker,
    [tool('change_model', {model:'careful'}), [chatJSON[0], {...usage, usage:{prompt_tokens:1900, completion_tokens:2, total_tokens:1902}}],
      [piece({content:'key facts'}, 'stop'), usage], tool('revert_model', {}), chatJSON], turns);
  tests.claudeStructuredToolOverride = await replay(api, 'claudeStructuredToolOverride', claudeOutput(override).replace('model: fast', 'model: claude-override'),
    [tool('think', {thought:'plan'}), chatJSON, chatJSON], turns);
  const claudeCompact = chain.replace('model: main, context_window', 'model: claude-main, context_window');
  tests.claudeStructuredCompaction = await replay(api, 'claudeStructuredCompaction', claudeCompact,
    [[piece({content:'first'}, 'stop'), highUsage], [piece({content:'key facts'}, 'stop'), usage], chatJSON], turns);
  const output = '__structured_output__';
  const final = tool(output, JSON.parse(jsonAnswer));
  const palette = withSchema(local(['think']), { mode: 'tool', description: 'Chosen colors' });
  const toolMode = {mode: 'tool'};
  const childOutput = yaml => withSchema(yaml, {...toolMode, description: 'Worker palette'}, 'local/worker');
  const boundedChild = childOutput(limitedTeam('', 'max_iterations: 2, session_compaction: false,'));
  const bounced = withSchema(handoffOnly(limitedTeam('', 'handoffs: [root],')), toolMode);
  const secretReply = [piece({content: secret}, 'stop'), usage];
  const secretTask = tool('transfer_task', {agent: 'helper', task: secret});
  for (const [name, yaml, replies, drive = turns] of [
    ['teamOutputDelegate', boundedChild, [delegate(), answer, final, [piece({content: '  '}, 'stop'), usage], answer]],
    ['teamOutputDelegateCap', boundedChild, [delegate(), answer, answer, answer, answer]],
    ['teamOutputCaller', withSchema(team, toolMode), [answer, delegate(), answer, final, final]],
    ['teamOutputHandoffReset', bounced, [answer, pass(), pass('root'), answer, answer, final, final]],
    ['teamOutputHandoffFailure', childOutput(handoffOnly(team)), [pass(), answer, answer, answer, final], rejectsThenRecovers],
    ['teamOutputForced', childOutput(withSchema(pipeline, toolMode)), [answer, final, answer, final, final]],
    ['teamOutputForcedCap', withSchema(limitedPipeline('max_iterations: 3, session_compaction: false,', ''), toolMode), [answer, final, ponder('cap'), answer]],
    ['teamRedactionHandoff', handoffOnly(limitedTeam('redact_secrets: false,', 'handoffs: [root],')),
      [ponder(secret), pass(), ponder(secret, 'redacted'), pass('root'), answer],
      async ({api, options, record, trace}) => trace.results.push(await api.chat({...options, messages: [{role: 'user', content: secret}]}, record))],
    ['teamRedactionTransferIn', limitedTeam('redact_secrets: false,', '').replace(', handoffs: [helper]', ''),
      [secretTask, ponder(secret), secretReply, answer], sends([secret])],
    ['teamRedactionTransferOut', limitedTeam('', 'redact_secrets: false,').replace(', handoffs: [helper]', ''),
      [secretTask, ponder(secret), secretReply, answer], sends([secret])],
  ]) tests[name] = await replay(api, name, yaml, replies, drive);
  const workerResponses = withSchema(responsesHelper('', 'max_iterations: 2, session_compaction: false,'), toolMode, 'spare/worker');
  tests.teamOutputDelegateResponses = await replay(api, 'teamOutputDelegateResponses', workerResponses,
    [delegate(), text, responseCall({...fn, name: output}, jsonAnswer), answer], withSpareKey(single),
    ['/v1/chat/completions', '/v1/responses', '/v1/responses', '/v1/chat/completions']);
  // The ordinary tool is first on the wire; Go intercepts and rejects the output call before dispatch.
  const mixed = [['think', '{"thought":"plan"}'], [output, jsonAnswer]].map(([name, args], index) =>
    ({ index, id: `mixed_${index}`, type: 'function', function: { name, arguments: args } }));
  const mixedTransfer = [{...mixed[0], function: {name: 'transfer_task', arguments: '{"agent":"helper","task":"count"}'}}, mixed[1]];
  tests.teamOutputMixedTransfer = await replay(api, 'teamOutputMixedTransfer', withSchema(team, toolMode),
    [[piece({tool_calls: mixedTransfer}, 'tool_calls'), usage], answer, final], single);
  tests.outputTool = await replay(api, 'outputTool', palette, [answer, rawTool(output, '{"colors":'), tool(output, {}),
    [piece({ tool_calls: mixed }, 'tool_calls'), usage], final, final], turns);
  const toolFailure = async ({ api, options, record, trace }) => {
    const s = await api.createSession(options, record);
    try {
      await s.send('first').then(() => { throw new Error('accepted plain text'); }, e => trace.results.push(e.message));
      trace.results.push(await s.send('again'));
    } finally { await s.close(); }
  };
  tests.outputFailure = await replay(api, 'outputFailure', palette, [answer, answer, answer, final], toolFailure);
  tests.claudeOutputFailure = await replay(api, 'claudeOutputFailure', palette.replace('local/test', 'local/claude-sonnet-5'), [answer, final], toolFailure);
  tests.outputDiscovery = await replay(api, 'outputDiscovery', withSchema(deferred, {mode:'tool'}),
    [tool('add_tool', {name:'think'}), tool('think', {thought:'discovered'}), final], single);
  tests.outputFallback = await replay(api, 'outputFallback', palette.replace('model: local/test', 'model: local/test, fallback: {models: [local/claude-sonnet-5]}'),
    [[{error:{message:'primary unavailable'}}], answer, final], toolFailure);
  tests.outputRepetition = await replay(api, 'outputRepetition', palette.replace('model: local/test', 'model: local/test, max_consecutive_tool_calls: 2'),
    [tool(output, {}), tool(output, {}), final], toolFailure);
  const outputFn = { ...fn, name: output };
  const raw = '{ "colors" : [ "blue" ] }';
  const deliver = args => [{ type: 'response.output_item.added', output_index: 0, item: outputFn },
    { type: 'response.function_call_arguments.delta', output_index: 0, item_id: fn.id, delta: args },
    fin([{ ...outputFn, arguments: args, status: 'completed' }])];
  const wireCall = (protocol, name, args, id) => protocol === chat ? rawTool(name, args, id)
    : responseCall({...fn, name, call_id: id}, args);
  for (const [protocol, suffix, plain] of [[chat, 'Chat', answer], [resp, 'Responses', text]]) {
    const call = (name, args, id) => wireCall(protocol, name, args, id);
    const finish = call(output, jsonAnswer, 'final');
    const replayOutput = async (name, base, fields, replies, drive = turns) => {
      name += suffix;
      const yaml = withSchema(base, {mode: 'tool'}).replace('provider: openai,', `provider: openai, api_type: ${protocol},`)
        .replace('model: local/test,', `model: local/test, session_compaction: false, ${fields},`);
      tests[name] = await replay(api, name, yaml, replies, drive, `/v1/${pathFor(protocol)}`);
    };
    await replayOutput('outputReminderCap', config, 'max_iterations: 2', [plain, plain, plain, finish]);
    await replayOutput('outputLimitedHistory', local(['think']), `${historyLimits}, redact_secrets: false`, [plain, thought(protocol, 'older'),
      call(output, '{}', 'invalid'), call(output, '{"colors":["ultramarine"]}', 'final'), plain, call(output, jsonAnswer, 'again')]);
    const denied = override + 'permissions: {deny: ["think:thought=blocked"]}\n';
    const transient = protocol === chat ? [{error:{message:'service unavailable'}}] : responseError('service unavailable');
    // Reusing the trimmed call ID drops the denied result, not the model override.
    await replayOutput('outputToolModel', denied, 'max_iterations: 3, num_history_items: 4', [thought(protocol, 'work'), transient, finish,
      call('think', '{"thought":"blocked"}', 'work'), plain, call(output, jsonAnswer, 'again')]);
    const claudeOverride = override.replace('model: fast', 'model: claude-sonnet-5, capabilities: {image: false}');
    await replayOutput('outputClaudeOverride', claudeOverride, 'max_iterations: 2',
      [thought(protocol, 'plan'), plain, finish], toolFailure);
  }
  const rowsSchema = { type: 'object', properties: { rows: { type: 'array', items: { $ref: '#/$defs/Row' } },
    featured: { $ref: '#/$defs/Row' } }, required: ['rows'], $defs: { Row: { $ref: '#/$defs/a~1~0b' },
    'a/~b': { type: 'object', properties: { value: { type: 'string' }, rank: { type: 'integer' } },
      required: ['value'], additionalProperties: true } } };
  tests.outputRefs = await replay(api, 'outputRefs', withSchema(responses, { mode: 'tool', schema: rowsSchema }),
    [deliver('{"rows":[{}]}'), deliver('{"rows":[{"value":"ok","extra":true}]}')], single, '/v1/responses');
  const choiceSchema = (mode, choice) => ({ type: 'object', properties: { values: { type: 'array', items: { $ref: '#/$defs/Choice' } },
    mode }, required: ['values'], $defs: { Choice: choice } });
  const choices = choiceSchema({ anyOf: [{ type: 'boolean' }, { type: 'string', enum: ['auto'] }] },
    { anyOf: [{ type: 'string', enum: ['skip'] }, { type: 'number' }] });
  tests.outputAnyOf = await replay(api, 'outputAnyOf', withSchema(responses, { mode: 'tool', schema: choices }),
    [deliver('{"values":["other"]}'), deliver('{"values":[],"mode":{"z":false,"a":[null,1e6]}}'),
      deliver('{ "values": [ "skip", -2.50e+1 ], "mode": false }')], single, '/v1/responses');
  const exclusiveChoices = choiceSchema({ oneOf: [{ type: 'boolean' }, { type: 'null' }] },
    { oneOf: [{ type: 'string', enum: ['red', 'both'] }, { type: 'number' }, { type: 'string', enum: ['both', 'blue'] }] });
  tests.outputOneOf = await replay(api, 'outputOneOf', withSchema(responses, { mode: 'tool', schema: exclusiveChoices }),
    [deliver('{"values":[true]}'), deliver('{"values":["both"]}'),
      deliver('{ "values": [ "red", -2.50e+1, "blue" ], "mode": null }')], single, '/v1/responses');
  tests.outputResponses = await replay(api, 'outputResponses', withSchema(responses, { mode: 'tool',
    schema: { ...schema, properties: { ...schema.properties, note: { type: 'string', enum: ['extra'] } }, additionalProperties: true } }),
    [responseTool, deliver(raw), deliver(raw)], turns, '/v1/responses');
  for (const [protocol, suffix, plain, spare] of [[chat, 'Chat', answer, resp], [resp, 'Responses', text, chat]]) {
    const fail = message => protocol === chat ? [{error: {message}}] : responseError(message);
    const invalid = fail('invalid model'), transient = fail('service unavailable');
    const empty = protocol === chat ? [piece({content: ''}, 'stop'), usage] : [fin([])];
    const pick = wireCall(protocol, 'change_model', switchArgs, 'pick');
    const revert = wireCall(protocol, 'revert_model', '{}', 'revert');
    const selectedChain = (base, models, retries) => base.replace('provider: openai,', `provider: openai, api_type: ${protocol},`)
      .replace('model: local/test,', `model: local/test, session_compaction: false, fallback: {models: [${models}], retries: ${retries}, cooldown: 1h},`);
    const path = `/v1/${pathFor(protocol)}`;
    const combined = picker.replace('{type: session_context}', '{type: think, model: local/fast}')
      .replace('model: local/test,', `model: local/test, ${historyLimits}, max_iterations: 3,`);
    for (const [prefix, base, retries, replies, drive] of [
      ['pickerRetryRevert', picker, 1, [pick, transient, transient, empty, revert, plain], turns],
      ['pickerPinnedRevert', withSchema(picker, {}), -1, [pick, invalid, invalid, revert, empty, plain], turns],
      ['pickOnFallback', withSchema(picker, {mode: 'tool'}), -1, [protocol === chat ? invalid : transient, pick, empty,
        wireCall(protocol, output, jsonAnswer, 'final'), wireCall(protocol, output, jsonAnswer, 'again')], turns],
      ['pickerToolModelExhausted', combined, -1, [pick, thought(protocol, 'work'), invalid, invalid,
        protocol === chat ? invalid : fail('prompt too long'), plain], rejectsThenRecovers],
    ]) {
      const name = prefix + suffix;
      tests[name] = await replay(api, name, selectedChain(base, 'local/backup, local/last', retries), replies, drive, path);
    }
    const http503 = {status: 503, headers: {'Retry-After-Ms': '1'}, body: {error: {message: 'service unavailable'}}};
    const name = 'toolModelCrossApi' + suffix;
    const mixed = selectedChain(override, 'spare/backup', -1).replace('providers:', `providers:\n  spare: {provider: openai, api_type: ${spare}, base_url: ${location.origin}/backup, token_key: SPARE_KEY}`);
    tests[name] = await replay(api, name, mixed, [thought(protocol, 'work'), http503, http503, http503,
      wireCall(spare, 'calculate', '{"expression":"2+2"}', 'calc'), empty, plain], withSpareKey(turns),
      [path, path, path, path, `/backup/${pathFor(spare)}`, path, path]);
  }
  const mediaConfig = config.replace('agents:', `models:
  capable: {provider: local, model: test, capabilities: {image: true, pdf: true, audio: true}}
agents:`)
    .replace('model: local/test', 'model: capable');
  const doc = (name, mime_type, source) => ({ type: 'document', document: { name, mime_type, source } });
  const note = doc('Note.txt', 'text/plain', { inline_text: `body </DOCUMENT-NOTE-TXT-TEXT-PLAIN x> end ${secret}` });
  const image = doc('tiny.gif', 'image/gif', { inline_data: 'R0lGODlhAQABAAAAACwAAAAAAQABAAA=' });
  const pdf = doc('page.pdf', 'application/pdf', { inline_data: btoa('%PDF-1.7\nmock') });
  const wav = doc('note.wav', 'audio/wav', { inline_data: btoa('RIFFmockWAVEdata') });
  const remote = { type: 'image_url', image_url: { url: 'https://example.invalid/a.png', detail: 'low' } };
  const part = text => ({ type: 'text', text });
  async function mediaHistory({ api, options, record, trace }) {
    const messages = [
      { role: 'system', content: 'fallback', multi_content: [part('rules'), part(' ')] },
      { role: 'user', multi_content: [note, image, remote, pdf, wav, part('describe')] },
      { role: 'assistant', content: 'alternate', multi_content: [part('checking')], tool_calls: ['a', 'b'].map(id =>
        ({ type: 'function', id, function: { name: 'lookup', arguments: '{}' } })) },
      { role: 'tool', tool_call_id: 'a', content: 'fallback', multi_content: [part('result'), image, note] },
      { role: 'tool', tool_call_id: 'b', content: 'caption', multi_content: [remote] },
      { role: 'user', content: 'continue' },
    ];
    trace.results.push(await api.chat({ ...options, messages }, record));
  }
  tests.media = await replay(api, 'media', mediaConfig, [answer], mediaHistory);
  tests.mediaResponses = await replay(api, 'mediaResponses', mediaConfig.replace('provider: openai,', 'provider: openai, api_type: openai_responses,'), [text], mediaHistory, '/v1/responses');
  // Actual failover, not the text fallback used for unsupported modalities below.
  const withMediaBackup = mediaConfig.replace('  capable:', '  backup: {provider: local, model: backup, capabilities: {image: true, pdf: true, audio: true}}\n  capable:')
    .replace('model: capable', 'model: capable, fallback: {models: [backup]}');
  tests.mediaFailoverChat = await replay(api, 'mediaFailoverChat', withMediaBackup,
    [[{ error: { message: 'vision primary unavailable' } }], answer], mediaHistory);
  tests.mediaFailoverResponses = await replay(api, 'mediaFailoverResponses', withMediaBackup.replace('provider: openai,', 'provider: openai, api_type: openai_responses,'),
    [[{ type:'response.failed', response:{id:'failed',status:'failed',error:{code:'invalid_request',message:'vision primary unavailable'}} }], text], mediaHistory, '/v1/responses');
  const chatPath = '/v1/chat/completions', responsesPath = '/v1/responses';
  async function routedMedia(args) {
    args.options.env.SPARE_KEY = 'route-token';
    await mediaHistory(args);
    if (JSON.stringify(args.trace.requests).includes(secret)) throw new Error('unredacted routed attachment');
  }
  for (const [name, first, second] of [
    ['mediaChatToResponses', chatPath, responsesPath], ['mediaResponsesToChat', responsesPath, chatPath],
    ['mediaCapabilityChange', chatPath, chatPath], ['mediaRestoredOnBackup', chatPath, responsesPath],
  ]) {
    const protocol = path => path === chatPath ? 'openai_chatcompletions' : 'openai_responses';
    let yaml = withMediaBackup.replace('providers:', `providers:\n  spare: { provider: openai, base_url: ${location.origin}/v1, token_key: SPARE_KEY, api_type: ${protocol(second)} }`)
      .replace('  local: { provider: openai,', `  local: { provider: openai, api_type: ${protocol(first)},`)
      .replace('backup: {provider: local', 'backup: {provider: spare')
      .replace('model: backup, capabilities: {image: true, pdf: true, audio: true}', 'model: backup, capabilities: {image: false, pdf: true}')
      .replace('model: capable, fallback:', 'model: capable, toolsets: [{type: think}], fallback:');
    if (name === 'mediaRestoredOnBackup') {
      yaml = yaml.replace('model: test, capabilities: {image: true, pdf: true, audio: true}', 'model: test, capabilities: {image: false}')
        .replace('model: backup, capabilities: {image: false, pdf: true}', 'model: backup, capabilities: {image: true, pdf: true}');
    }
    const failure = first === chatPath ? [{ error: { message: 'vision primary unavailable' } }] : responseError('vision primary unavailable');
    tests[name] = await replay(api, name, yaml,
      [failure, second === chatPath ? tool('think', {thought:'plan'}) : responseTool, second === chatPath ? answer : text],
      routedMedia, [first, second, second]);
  }
  const mediaTeam = mediaConfig.replace('providers:', `providers:\n  worker: {provider: openai, base_url: ${location.origin}/v1, token_key: SPARE_KEY, api_type: openai_responses}`)
    .replace('model: capable,', 'model: capable, handoffs: [helper], sub_agents: [helper],') +
    '  helper: {model: worker/worker, description: Helps, instruction: Be precise}\n';
  for (const transfer of [false, true]) {
    const name = transfer ? 'mediaTransfer' : 'mediaHandoff';
    const call = transfer ? tool('transfer_task', {agent:'helper', task:'answer'}) : tool('handoff', {agent:'helper'});
    const replies = transfer ? [call, text, answer] : [call, text];
    tests[name] = await replay(api, name, mediaTeam, replies,
      routedMedia,
      transfer ? [chatPath, responsesPath, chatPath] : [chatPath, responsesPath]);
  }
  const forcedMedia = mediaTeam.replace('handoffs: [helper], sub_agents: [helper]', 'force_handoff: helper');
  tests.mediaForcedHandoff = await replay(api, 'mediaForcedHandoff', forcedMedia, [answer, text],
    routedMedia, [chatPath, responsesPath]);
  tests.mediaNoCaps = await replay(api, 'mediaNoCaps', config, [answer], mediaHistory);
  async function mediaFallback({ api, options, record, trace }) {
    const messages = [
      { role: 'user', content: 'image fallback', multi_content: [image] },
      { role: 'assistant', content: 'caption', multi_content: [image] },
      { role: 'user', content: 'PDF fallback', multi_content: [pdf] },
      { role: 'assistant', multi_content: [image] },
      { role: 'system', multi_content: [image] },
      { role: 'assistant', multi_content: [part('text part only')] },
      { role: 'user', multi_content: [wav] },
      { role: 'assistant', content: 'text field', multi_content: [part('text part')] },
      { role: 'user', content: 'continue' },
    ];
    trace.results.push(await api.chat({ ...options, messages }, record));
  }
  tests.mediaFallback = await replay(api, 'mediaFallback', config, [answer], mediaFallback);
  tests.mediaFallbackResponses = await replay(api, 'mediaFallbackResponses', mediaConfig.replace('provider: openai,', 'provider: openai, api_type: openai_responses,'), [text], mediaFallback, '/v1/responses');
  tests.autoResponses = await replay(api, 'autoResponses', 'agents: {root: {model: openai/o3-mini, instruction: hi}}', [text], async ({ api, options, record, trace }) => {
    options.env = { OPENAI_API_KEY: 'fixture' };
    trace.results.push(await api.chat({ ...options, messages: [{ role: 'user', content: 'hi' }] }, record));
  }, 'https://api.openai.com/v1/responses');
  // A turn without a visible reply: Go warns, keeps no history for it and answers with the last kept reply.
  const says = (delta, reason = 'stop') => [piece({role: 'assistant', ...delta}, reason), usage];
  const silent = says({content: ''});
  const thinks = local(['think']);
  const partialCall = [piece({role: 'assistant', tool_calls: [{id: 'cut', index: 0, type: 'function', function: {name: 'think', arguments: '{"thou'}}]}, 'length'), usage];
  const filteredCall = [piece({role: 'assistant', tool_calls: [{id: 'cut', index: 0, type: 'function', function: {name: 'think', arguments: '{"thought":"a"}'}}]}, 'content_filter'), usage];
  const narratedCall = [piece({role: 'assistant', content: 'Let me think. '}), ...tool('think', {thought: 'a'})];
  const mixedChunk = [piece({role: 'assistant', content: 'Let me think. ', reasoning_content: 'hmm',
    tool_calls: [{id: 'mixed', index: 0, type: 'function', function: {name: 'think', arguments: '{"thought":"a"}'}}]}, 'tool_calls'), usage];
  for (const [name, yaml, replies] of [
    ['emptyReply', config, [silent, answer]],
    ['emptyReplyKeepsEarlier', config, [answer, silent]],
    ['emptyReplyNoUsage', config, [[piece({role: 'assistant', content: ''}, 'stop')], answer]],
    ['whitespaceReply', config, [says({content: ' \n '}), answer]],
    ['reasoningOnlyReply', config, [says({reasoning_content: 'pondering', content: '  '}), answer]],
    ['paddedReply', config, [says({content: '  padded reply \n'}), says({content: '\n second  '})]],
    ['emptyReplyAfterTools', thinks, [tool('think', {thought: 'a'}), silent, answer]],
    ['emptyReplyAfterNarratedTools', thinks, [narratedCall, silent, answer]],
    ['emptyReplyNextSend', thinks, [tool('think', {thought: 'a'}), answer, silent]],
    ['truncatedEmptyReply', config, [says({content: ''}, 'length'), answer]],
    ['truncatedCallReply', thinks, [partialCall, answer]],
    ['refusedReply', config, [says({content: 'partial'}, 'content_filter'), says({content: ''}, 'refusal')]],
    ['refusedCallReply', thinks, [filteredCall, answer]],
    ['mixedTerminalChunk', thinks, [mixedChunk, answer, answer]],
    ['emptyReplyPicker', picker, [tool('change_model', {model: 'careful'}), silent, answer]],
    ['emptyReplyToolModel', override, [tool('think', {thought: 'plan'}), silent, answer]],
    ['emptyReplyFallback', config.replace('model: local/test', 'model: local/test, fallback: {models: [local/backup]}'),
      [[{error: {message: 'invalid model'}}], silent, answer]],
    ['delegateBlankReply', limitedTeam('', ''), [delegate(), says({content: '  '}), answer, answer]],
    ['delegateBlankAfterTools', limitedTeam('', ''), [delegate(), ponder('a'), silent, silent, answer]],
    ['callerBlankAfterDelegate', limitedTeam('', ''), [delegate(), says({content: 'child says'}), silent, answer]],
    ['handoffBlankReply', limitedTeam('', ''), [pass(), silent, answer]],
    ['forcedBlankReply', pipeline, [silent, answer, answer]],
    ['outputBlankReply', withSchema(config, {mode: 'tool'}), [silent, tool(output, JSON.parse(jsonAnswer)), answer, tool(output, JSON.parse(jsonAnswer))]],
  ]) tests[name] = await replay(api, name, yaml, replies, turns);
  tests.emptyReplySeed = await replay(api, 'emptyReplySeed', config, [silent], async ({api, options, record, trace}) => {
    const messages = [{role: 'user', content: 'a'}, {role: 'assistant', content: ' earlier answer '}, {role: 'user', content: 'go'}];
    trace.results.push(await api.chat({...options, messages}, record));
  });
  const incomplete = reason => [{type: 'response.incomplete', response: {id: 'resp', status: 'incomplete', incomplete_details: {reason},
    output: [], usage: {input_tokens: 3, output_tokens: 2, total_tokens: 5}}}];
  const plainResponses = config.replace('provider: openai,', 'provider: openai, api_type: openai_responses,');
  const http = (status, body = {error: {message: `boom ${status}`}}, headers = {'Retry-After-Ms': '1'}) => ({status, body, headers});
  const fixedBase = 'https://provider.invalid/v1'; // Raw SDK errors quote the URL; page origins differ per runtime.
  const fixed = yaml => yaml.replace(`${location.origin}/v1`, fixedBase);
  const httpChain = config.replace('model: local/test', 'model: local/test, fallback: {models: [local/backup], retries: -1}');
  for (const [name, yaml, replies, drive, path] of [
    ['httpSdkConflict', config, [http(409, undefined, {'Retry-After': '0.001'}), answer], single],
    ['httpSdkRetryAfterDate', plainResponses, [http(408, undefined, {'Retry-After': 'Sun, 06 Nov 1994 08:49:37 GMT'}), http(429), text], single, '/v1/responses'],
    ['httpRetryHeaderTrue', config, [http(422, undefined, {'x-should-retry': 'true', 'Retry-After-Ms': '1'}), answer], single],
    ['httpRetryHeaderFalse', httpChain, [http(503, undefined, {'x-should-retry': 'false'}), answer, answer], turns],
    ['httpRuntimeRetry', config.replace('model: local/test', 'model: local/test, fallback: {retries: -1}'), [http(503), http(503), http(503), answer], single],
    ['httpRateLimitPinned', httpChain, [http(429), http(429), http(429), answer, answer], turns],
    ['httpChainExhausted', httpChain, [http(401), http(403), answer], rejectsThenRecovers],
    ['httpHandoffError', team, [pass(), http(400), answer], rejectsThenRecovers],
    ['httpResponseScalar', plainResponses, [...Array(3).fill(http(500, 'invalid')), text], rejectsThenRecovers, '/v1/responses'],
    ['httpHiddenCode', config, [http(400, {error: {message: 'over budget', code: 'context_length_exceeded'}}), answer], rejectsThenRecovers],
    ['httpWireOverflow', config.replace('model: local/test', 'model: local/test, session_compaction: false'), [http(413), answer], rejectsThenRecovers],
    ['httpProxyEnvelope', fixed(config), [http(400, {error: 'string'}), answer], rejectsThenRecovers, `${fixedBase}/chat/completions`],
    ['httpEmptyObject', fixed(plainResponses), [http(404, {}), text], rejectsThenRecovers, `${fixedBase}/responses`],
    ['httpEmptyBody', config, [{status: 400, raw: null}, answer], rejectsThenRecovers],
    ['httpHtmlBody', plainResponses, [{status: 400, raw: '<html>bad gateway</html>'}, text], rejectsThenRecovers, '/v1/responses'],
    ['httpCappedContinuation', twoRounds, [ponder('once'), http(503), http(503), http(503), answer, answer], turns],
    ['httpAfterTool', local(['think']), [ponder('once'), http(409), answer], single],
    ['httpDelegateRetry', responsesHelper('', ''), [delegate(), http(429), text, answer], withSpareKey(single),
      ['/v1/chat/completions', '/v1/responses', '/v1/responses', '/v1/chat/completions']],
    ['httpHandoffRetry', team, [pass(), http(529), answer, answer], turns],
  ]) tests[name] = await replay(api, name, yaml, replies, drive, path);
  for (const [name, replies] of [
    ['responsesEmptyReply', [[fin([])], text]],
    ['responsesWhitespaceReply', [[{...text[0], delta: '  '}, fin([])], text]],
    ['responsesTruncatedReply', [incomplete('max_output_tokens'), text]],
    ['responsesRefusedReply', [incomplete('content_filter'), text]],
    ['responsesPaddedReply', [[{...text[0], delta: ' hi there \n'}, fin([])], text]],
  ]) tests[name] = await replay(api, name, plainResponses, replies, turns, '/v1/responses');
  const [callAdded, argsDelta, argsDone, callFinished] = responseTool;
  const doneItem = item => ({type: 'response.output_item.done', item});
  const messageItem = value => ({type: 'message', role: 'assistant', id: 'm', content: [{type: 'output_text', text: value}]});
  const reasonDelta = {type: 'response.reasoning_summary_text.delta', item_id: 'r', delta: 'summary'};
  const reasoningItem = {type: 'reasoning', id: 'r', summary: [{type: 'summary_text', text: 'snapshot'}]};
  for (const [name, first] of [
    ['responsesReasoningDeltas', [reasonDelta, {type: 'response.reasoning_text.delta', item_id: 'r', delta: 'thinking'},
      {type: 'response.reasoning_text.done', item_id: 'r', text: 'thinking snapshot'}, text[0], fin([reasoningItem])]],
    ['responsesReasoningSnapshotOnly', [fin([reasoningItem])]],
    ['responsesTextSnapshots', [text[0], {type: 'response.output_text.done', item_id: 'm', text: 'revised'},
      doneItem(messageItem('hi suffix')), fin([messageItem('different')])]],
    ['responsesMetadata', [{type: 'response.future.metadata'}, doneItem({type: 'web_search_call', id: 'ws'}),
      {type: 'response.output_text.delta', code: 'not text'}, {type: 'response.output_text.delta'},
      {type: 'response.content_part.delta', part: {type: 'output_text', text: 'part'}}, text[0], fin([])]],
    ['responsesInProgressMessage', [doneItem({...messageItem('hi'), status: 'in_progress'}), fin([])]],
    ['responsesTextEOF', [text[0]]], ['responsesReasoningEOF', [reasonDelta]], ['responsesEmptyEOF', []],
  ]) tests[name] = await replay(api, name, plainResponses, [first, text], turns, '/v1/responses');
  for (const [name, usage] of [
    ['responsesMissingTotal', {input_tokens: 9, output_tokens: 2}], ['responsesZeroTotal', {input_tokens: 3, total_tokens: 0}],
    ['responsesOnlyInput', {input_tokens: 5, total_tokens: 5}], ['responsesOnlyOutput', {output_tokens: 3, total_tokens: 3}],
    ['responsesOnlyTotal', {total_tokens: 5}], ['responsesNullCounts', {input_tokens: null, total_tokens: 1}],
    ['responsesNegativeTotal', {total_tokens: -1}],
  ]) tests[name] = await replay(api, name, plainResponses,
    [[text[0], {type: 'response.completed', response: {output: [], usage}}], text], turns, '/v1/responses');
  const parallel = responseCall({...fn, id: 'item_2', call_id: 'call_2'}, '{"thought":"second"}');
  for (const [name, first] of [
    ['responsesPendingArgs', [{...argsDelta, delta: 'stale'}, argsDone, callAdded, callFinished]],
    ['responsesReplacedPendingArgs', [{...argsDone, arguments: '{}'}, argsDone, callAdded, callFinished]],
    ['responsesItemDoneFallback', [callAdded, doneItem({...fn, arguments: args}), callFinished]],
    ['responsesDeltaAfterItemDone', [callAdded, argsDelta, doneItem({...fn, arguments: args}), {...argsDelta, delta: ' '}, fin([fn])]],
    ['responsesParallelText', [text[0], callAdded, {...parallel[0], output_index: 1}, text[0], {...parallel[1], output_index: 1},
      argsDelta, text[0], fin([fn, parallel[0].item])]],
    ['responsesToolTrailer', [...responseTool, {type: 'response.output_text.annotation.added'}, callFinished]],
  ]) tests[name] = await replay(api, name, responses, [first, text, text], turns, '/v1/responses');
  tests.responsesUnannouncedFinalCall = await replay(api, 'responsesUnannouncedFinalCall', responses, [[callFinished], text], turns, '/v1/responses');
  for (const [name, first] of [
    ['responsesLateToolFailure', [...responseTool, ...responseError('late failure')]],
    ['responsesNullError', [{...callAdded, error: null}, argsDelta, callFinished]],
  ]) tests[name] = await replay(api, name, responses, [first, text], rejectsThenRecovers, '/v1/responses');
  const errorFallback = plainResponses.replace('model: local/test', 'model: local/test, fallback: {models: [local/spare], retries: -1}');
  for (const [name, first] of [
    ['responsesUntypedFailure', [{type: 'response.failed', response: {id: 'r', error: {code: 'invalid', message: 'busy', status: 503}}}]],
    ['responsesEnvelopeStatus', [{error: {message: 'invalid 401', status: 503}}]],
  ]) tests[name] = await replay(api, name, errorFallback, [first, text, text], turns, '/v1/responses');
  // A hallucinated tool gets an error the model can correct; the send goes on.
  const twoTools = local(['think', 'calculator']);
  const shell = args => tool('shell', args);
  const handoffTeam = config.replace('model: local/test', 'model: local/test, toolsets: [{type: think}], handoffs: [helper]')
    + '  helper: {model: local/worker, description: Helps, instruction: Be precise}\n';
  for (const [name, yaml, replies, drive = turns] of [
    ['unavailableTool', twoTools, [shell({cmd: 'x'}), answer, answer]],
    ['unavailableToolRepeated', twoTools.replace('model: local/test', 'max_consecutive_tool_calls: 2, model: local/test'),
      [shell({cmd: 'x'}), shell({cmd: 'x'}), answer], rejectsThenRecovers],
    ['unavailableToolSecret', twoTools, [shell({token: secret}), answer, answer]],
    ['unavailableToolCapped', twoTools.replace('model: local/test', 'max_tool_result_tokens: 4, model: local/test'), [shell({}), answer, answer]],
    ['deferredToolGuess', deferred, [[piece({role: 'assistant', tool_calls: [
      {id: 'add', index: 0, type: 'function', function: {name: 'add_tool', arguments: '{"name":"think"}'}},
      {id: 'early', index: 1, type: 'function', function: {name: 'think', arguments: '{"thought":"early"}'}}]}, 'tool_calls'), usage],
      tool('think', {thought: 'now'}), answer, answer]],
    ['handoffToolHallucination', handoffTeam, [pass(), tool('think', {thought: 'not mine'}), answer, answer]],
    ['handoffHallucinatedHandoff', handoffTeam, [pass(), pass('root'), answer, answer]],
  ]) tests[name] = await replay(api, name, yaml, replies, drive);
  tests.unavailableToolResponses = await replay(api, 'unavailableToolResponses',
    plainResponses.replace('model: local/test', 'toolsets: [{type: think}], model: local/test'),
    [responseCall({...fn, name: 'shell'}, '{"cmd":"x"}'), text, text], turns, '/v1/responses');
  // Go finishes a turn without tool calls however its stream stopped.
  const open = content => piece({role: 'assistant', content});
  for (const [name, replies] of [
    ['streamWithoutFinish', [[open('cut off'), usage], answer]],
    ['streamWithoutFinishOrUsage', [[open('cut off')], answer]],
    ['blankStreamWithoutFinish', [[open(''), usage], answer]],
    ['blankStreamWithoutFinishOrUsage', [[open('')], answer]],
    ['whitespaceStreamWithoutFinishOrUsage', [[open('  ')], answer]],
    ['toolFinishWithoutCalls', [says({content: 'odd'}, 'tool_calls'), says({content: ''}, 'tool_calls')]],
    ['unknownFinishReason', [says({content: 'odd'}, 'eos'), says({content: ''}, 'eos')]],
  ]) tests[name] = await replay(api, name, config, replies, turns);
  const cachedUsage = {choices: [], usage: {prompt_tokens: 100, completion_tokens: 7, total_tokens: 107,
    prompt_tokens_details: {cached_tokens: 60}, completion_tokens_details: {reasoning_tokens: 3}}};
  tests.cachedPromptUsage = await replay(api, 'cachedPromptUsage', thinks,
    [[...tool('think', {thought: 'a'}).slice(0, -1), cachedUsage], [piece({role: 'assistant', content: 'done'}, 'stop'), cachedUsage], answer], turns);
  tests.cachedPromptUsageResponses = await replay(api, 'cachedPromptUsageResponses', plainResponses,
    [[text[0], {type: 'response.completed', response: {id: 'resp', status: 'completed', output: [],
      usage: {input_tokens: 100, output_tokens: 7, total_tokens: 107, input_tokens_details: {cached_tokens: 60}}}}], text], turns, '/v1/responses');
  // Arguments decode like encoding/json; what cannot be decoded goes back to the model.
  const answering = (decision, elicitation = {action: 'accept', content: {answer: 'blue'}}) => async ({api, options, record, trace}) => {
    trace.results.push(await api.chat({...options, messages: [{role: 'user', content: 'go'}]}, e => {
      record(e);
      return e.type === 'tool_confirmation' ? decision : e.type === 'elicitation' ? elicitation : undefined;
    }));
  };
  const approve = answering({decision: 'approve'});
  const routed = config.replace('model: local/test', 'model: local/test, sub_agents: [helper], handoffs: [helper]')
    + '  helper: {model: local/worker, description: Helps, instruction: Be precise}\n';
  const greeter = apiTool.replace('endpoint: "https://tool.invalid/greet"', 'endpoint: "https://tool.invalid/greet", args: {who: {type: string}}');
  const typedAPI = method => httpConfig({type: 'api', api_config: {name: 'greet', method,
    endpoint: fixture + '/${who}?extra=${extra}', headers: {'X-Note': '${env.WHO}'},
    args: {who: {type: 'string', enum: ['friend']}, flag: {type: 'boolean'}, count: {type: 'number', enum: [1, 2.5, 'other', false]}},
    required: ['flag', 'missing']}});
  const echoAPI = async req => new Response(JSON.stringify([req.method, req.url, req.headers.get('X-Note'), req.headers.get('Content-Type'), await req.text()]));
  const apiCases = {empty: '', missing: '{}', null: 'null', array: '[1]', scalar: 'false', malformed: '{"who":oops}',
    caseExtra: '{"Who":"upper","extra":"used","unused":"ignored"}', outsideEnum: '{"who":"stranger"}',
    duplicate: '{"who":"first","who":"last"}', duplicateNull: '{"who":"first","who":null}', duplicateBad: '{"who":3,"who":"last"}',
    bool: '{"flag":true}', number: '{"count":1}', unknownNumber: '{"unused":1}', object: '{"who":{}}', unknownArray: '{"unused":[]}',
    surrogate: '{"who":"\\ud800"}', escaped: '{"who":"a%2Fb (*) 🌍","extra":"a b/+&?"}'};
  for (const [name, yaml, toolName, cases, extra = [], drive = approve, service] of [
    ['think', thinks, 'think', {
      badValue: '{"thought": oops', trailingComma: '{"thought":"x",}', singleQuotes: "{'thought':'x'}", openSecret: `{"thought":"${secret}`,
      twoObjects: '{"thought":"a"}{"thought":"b"}', blank: '   ', null: 'null', array: '["x"]', string: '"x"', wrongType: '{"thought":3}',
      wrongSecret: `{"thought":["${secret}"]}`, lenient: '{"THOUGHT":"x","more":true,"thought":"y"}', laterType: '{"thought":"ok","thought":3}',
      badEscape: '{"thought":"\\x"}', badUnicode: '{"thought":"\\u12G4"}', rawNewline: '{"thought":"a\nb"}', control: '{"thought":"\u0001"}',
      badLiteral: '{"thought":tru}', badNumber: '{"thought":1.}', leadingZero: '{"thought":01}', missingColon: '{"thought" "x"}',
      bareKey: '{thought:"x"}', trailingText: '{"thought":"x"} trailing', latin1: '{"thought": é}', surrogate: '{"thought":"\\ud83d"}',
    }],
    ['thinkAsked', thinks + 'permissions: {ask: [think]}\n', 'think', {approved: '{"thought": oops'}],
    ['thinkRefused', thinks + 'permissions: {ask: [think]}\n', 'think', {refused: '{"thought": oops'}, [], answering({decision: 'reject', reason: 'no'})],
    ['calculate', local(['calculator']), 'calculate', {bad: '{"expression": 1+', wrongType: '{"expression":3}', array: '[1]', lenient: '{"EXPRESSION":"1+1","x":1}'}],
    ['userPrompt', local(['user_prompt']), 'user_prompt', {
      bad: '{"message": "x', missing: '{}', null: 'null', array: '["q"]', wrongType: '{"message":3}', titleType: '{"message":"Q?","title":3}',
      schemaType: '{"message":"Q?","schema":"oops"}', lenient: '{"Message":"Q?","more":1,"title":"T"}',
      mergedSchema: '{"message":"Q?","schema":{"type":"object"},"schema":{"properties":{"answer":{"type":"string"}}}}',
      clearedSchema: '{"message":"Q?","schema":{"type":"object"},"schema":null}',
    }],
    ['todo', local(['todo']), 'create_todo', {bad: '{"description": "x', wrongType: '{"description":3}', array: '[1]'}],
    ['todos', local(['todo']), 'create_todos', {bad: '{"descriptions": ["a"', repaired: '{"descriptions": "a"}'}],
    ['listTodos', local(['todo']), 'list_todos', {ignored: '{"x": oops'}],
    ['transfer', routed, 'transfer_task', {bad: '{"agent": "helper', missing: '{}', wrongType: '{"agent":3,"task":"x"}', taskType: '{"agent":"helper","task":["a"]}', array: '["helper"]'}],
    ['transferred', routed, 'transfer_task', {lenient: '{"AGENT":"helper","Task":"count","extra":1}', noTask: '{"agent":"helper"}',
      folded: '{"agent":"helper","ta\\u017f\\u212a":"count"}'}, [answer]],
    ['handoff', routed, 'handoff', {bad: '{"agent": "helper', missing: '{}', wrongType: '{"agent":3}', array: '["helper"]'}],
    ['handedOff', routed, 'handoff', {lenient: '{"Agent":"helper","extra":1}'}],
    ['changeModel', picker, 'change_model', {bad: '{"model": "careful', wrongType: '{"model":3}', array: '["careful"]'}],
    ['revertModel', picker, 'revert_model', {ignored: '{"x": oops'}],
    ['searchTool', deferred, 'search_tool', {bad: '{"query": "x', wrongType: '{"query":3}', array: '[1]'}],
    ['addTool', deferred, 'add_tool', {bad: '{"name": "think', wrongType: '{"name":3}'}],
    ['listSessions', local(['session_context']), 'list_sessions', {bad: '{"limit": 1', wrongType: '{"limit":"x"}', blank: '  '}],
    ['readSession', local(['session_context']), 'read_session', {bad: '{"session_id": "x', wrongType: '{"session_id":3}'}],
    ['addMemory', local(['memory']), 'add_memory', {bad: '{"memory": "x', wrongType: '{"memory":3}', array: '[1]'}],
    ['getMemories', local(['memory']), 'get_memories', {bad: '{"x": oops'}],
    ['writePlan', local(['plan']), 'write_plan', {bad: '{"name": "x', wrongType: '{"name":3,"content":"c"}', array: '[1]'}],
    ['listPlans', local(['plan']), 'list_plans', {ignored: '{"x": oops'}],
    ['datetime', local(['datetime']), 'get_datetime', {bad: '{"format": "x', wrongType: '{"format":3}', array: '[1]'}],
    ['random', local(['random']), 'random_int', {bad: '{"min": 1', wrongType: '{"min":"a","max":2}', array: '[1]'}],
    ['api', greeter, 'greet', {bad: '{"who": "x'}, [], approve, greet],
    ['apiGet', typedAPI('GET'), 'greet', {...apiCases, badPercent: '{"who":"10%"}', numericKey: '{"x.2":false}',
      escapedKey: '{"a/b~c":3}', firstError: '{"z":true,"a":2}'}, [], approve, echoAPI],
    ['apiPost', typedAPI('POST'), 'greet', {...apiCases,
      redactedNumbers: `{"who":"${secret}","n":[1.0,9007199254740993,1e20,1e21,1e-6,1e-7,-0]}`,
      numbers: '{"int":9007199254740993,"exp":1e21,"tiny":1e-7,"decimal":1e-6,"one":1.0,"zero":-0.0}',
      nested: '{"2":2,"10":{"b":true,"a":[{"<>&":"\\u2028\\u2029"},null]},"__proto__":"x"}',
      surrogateKeys: '{"\\udfff":1,"�":2,"😀":3}', overflow: '{"foo":[{"a/b":1e400}]}', duplicateOverflow: '{"foo":1e400,"foo":1}'}, [], approve, echoAPI],
    ['apiGetContainers', httpConfig({type: 'api', api_config: {name: 'greet', method: 'GET', endpoint: fixture + '/${rows}',
      args: postArgs}}), 'greet', {stringValue: '{"rows":"still a string"}'}, [], approve, echoAPI],
    ['apiIgnored', typedAPI('PUT'), 'greet', {malformed: '{oops'}, [], approve, echoAPI],
    ['fetch', local(['fetch']), 'fetch', {bad: '{"urls": ["https://x', missing: '{}', null: 'null', array: '[1]', emptyList: '{"urls":[]}',
      wrongType: '{"urls":3}', elementType: '{"urls":["https://tool.invalid/a",3]}', formatType: '{"urls":["https://tool.invalid/a"],"format":3}',
      timeoutType: '{"urls":["https://tool.invalid/a"],"timeout":"x"}', timeoutFraction: '{"URLS":["https://tool.invalid/a"],"timeout":1.5}'}],
  ]) {
    for (const [label, args] of Object.entries(cases)) {
      const id = `args_${name}_${label}`;
      tests[id] = await replay(api, id, yaml, [rawTool(toolName, args), ...extra, answer], drive, '/v1/chat/completions', service);
    }
  }
  const todoLifecycle = [tool('create_todos', {descriptions: ['', ' \t', 'x,&\n']}),
    tool('update_todos', {updates: [{id: 'todo_3', status: 'in-progress'}, {id: 'absent', status: 'completed'},
      {id: 'todo_1', status: 'completed'}, {id: 'todo_1', status: 'other'}]}),
    tool('update_todos', {updates: [{id: 'absent', status: 'completed'}]}), tool('list_todos', {}), answer];
  const todoTeam = routed.replaceAll('model: local/', 'toolsets: [{type: todo, shared: true}], model: local/');
  const makeTodo = description => tool('create_todo', {description});
  const listTodos = tool('list_todos', {});
  const thinkText = thought => tool('think', {thought});
  for (const [name, yaml, replies, drive = single] of [
    ['todoLifecycle', todo, todoLifecycle], ['todoLifecycleToon', todo.replace('type: todo', 'type: todo, toon: .*'), todoLifecycle],
    ['todoSharedTransfer', todoTeam, [makeTodo('root'), delegate(), listTodos, makeTodo('helper'), answer, listTodos, answer]],
    ['todoSharedHandoff', todoTeam, [makeTodo('root'), pass(), makeTodo('helper'), listTodos, answer]],
    ['thinkAcrossTurns', thinks, [thinkText(''), thinkText(' one '), answer, thinkText('next\n'), answer], turns],
    ['thinkAfterFailedSend', thinks, [thinkText('first'), [{error: {message: 'provider failed'}}], thinkText('second'), answer], rejectsThenRecovers],
    ['discoveryRepeated', deferred, [tool('search_tool', {query: '  ThK '}), tool('add_tool', {name: 'THINK'}),
      tool('add_tool', {name: 'think'}), tool('add_tool', {name: 'think'}), tool('search_tool', {query: ''}), answer]],
    ['randomBounds', local(['random']), [...['{}', '{"min":-1,"max":-2}', '{"min":9007199254740993,"max":9007199254740993}',
      '{"min":-9223372036854775808,"max":-9223372036854775808}', '{"min":9223372036854775808,"max":1}'].map(s => rawTool('random_int', s)), answer]],
    ['calculatorGrammar', math, [...['-2^2 + 2^-3^2', '2e-3 + 1./3', '-17%-3', '1\u00a0+2', 'sqrt(9)', 'pi', '1/0'].map(expression => tool('calculate', {expression})), answer]],
    ['calculatorLimits', math, [...['9'.repeat(8193), '('.repeat(64) + '1' + ')'.repeat(64), '9'.repeat(1400), '1e-1024', '1e1024', '-1/3e24'].map(expression => tool('calculate', {expression})), answer]],
    ['sessionReferences', local(['session_context']), [...[' -01 ', '-0009', 'bell\x07\u00a0', 'x\u0301\u200b', '-9223372036854775808', '\u0085 '].map(session_id => tool('read_session', {session_id})), answer]],
  ]) tests[name] = await replay(api, name, yaml, replies, drive);

  for (const [name, firstShared, secondShared] of [['todoPrivateSources', false, false], ['todoSharedSources', true, true], ['todoMixedSources', true, false]]) {
    const yaml = todo.replace('{type: todo}', `{type: todo, shared: ${firstShared}, tools: [create_todo]}, {type: todo, shared: ${secondShared}, tools: [list_todos, create_todos], defer: [list_todos]}`);
    tests[name] = await replay(api, name, yaml, [makeTodo('first'), tool('add_tool', {name: 'list_todos'}), listTodos,
      tool('create_todos', {descriptions: ['second']}), listTodos, answer], single);
  }

  const planRef = {name: 'release'};
  const writePlan = tool('write_plan', {...planRef, title: '<ship>', author: 'A', content: ' ', status: 'draft'});
  const memStart = Date.UTC(2026, 6, 1, 12);
  const memoryID = String(BigInt(memStart + 86_400_000) * 1_000_000n);
  const remember = tool('add_memory', {memory: 'Σ i, CAT dog<>', category: 'K'});
  for (const [name, kind, replies, start = memStart] of [
    ['planRevisions', 'plan', [writePlan, tool('write_plan', {name: '0-first_', content: 'next'}),
      tool('set_plan_status', {...planRef, status: 'ready', last_known_revision: 1}), tool('write_plan', {...planRef, content: 'new', last_known_revision: 2}),
      tool('read_plan', planRef), tool('list_plans', {}), tool('get_plan_status', planRef)]],
    ['planFailures', 'plan', [writePlan, tool('delete_plan', {...planRef, last_known_revision: 0}), tool('delete_plan', planRef), tool('delete_plan', planRef),
      tool('set_plan_status', {...planRef, status: '!'}), tool('write_plan', {name: 'a/b', content: ''}), tool('write_plan', {name: 'A\n', content: '!'})]],
    ['memorySearch', 'memory', [remember, remember, tool('add_memory', {memory: ''}), tool('search_memories', {query: 'at\u0085DO', category: 'k'}),
      tool('search_memories', {query: 'missing'}), tool('get_memories', {})]],
    ['memoryMutations', 'memory', [remember, tool('update_memory', {id: memoryID, memory: 'changed'}), tool('get_memories', {}),
      tool('delete_memory', {id: memoryID}), tool('delete_memory', {id: memoryID}), tool('update_memory', {id: memoryID}), tool('get_memories', {})]],
    ['memoryUnixNanoWrap', 'memory', [remember, tool('get_memories', {})], Date.UTC(2400, 10, 3)],
    ...[['clockSeasonal', memStart, ['America/Vancouver', 'Canada/Mountain', 'Local', 'Asia/Kathmandu']],
      ['clockFuture', Date.UTC(2050, 2, 25), ['America/Vancouver', 'Asia/Hebron', 'Europe/Paris', 'Europe/Dublin', 'Pacific/Chatham']],
    ].map(([name, start, zones]) => [name, 'datetime', zones.map(timezone => tool('get_datetime', {format: 'Mon _2 15:04:05 MST Z07:00:00', timezone})), start]),
  ]) await datedReplay(name, local([kind]), [...replies, answer], approve, undefined, start);

  for (const [name, action, content] of [
    ['promptAcceptContent', 'accept', {text: '<&>\u2028\ud800', n: [1e20, 1e-6], nested: {'2': 2, '10': 10}}],
    ['promptAcceptArray', 'accept', ['not an object']], ['promptDeclineContent', 'decline', {why: '<no>'}], ['promptCancelContent', 'cancel', {why: '&'}],
  ]) tests[name] = await replay(api, name, question, [tool('user_prompt', {message: 'Ask', schema: {type: 'object', required: ['missing']}}), answer],
    answering({decision: 'approve'}, {action, content}));

  await datedReplay('builtinRestartIsolation', local(['todo', 'think', 'memory', 'plan']),
    [makeTodo('first'), thinkText('initial'), remember, writePlan, answer, makeTodo('second'), thinkText('fresh'),
      tool('get_memories', {}), tool('list_plans', {}), answer, listTodos, thinkText('isolated'), tool('get_memories', {}), tool('list_plans', {}), answer],
    async ({api, options, record, trace}) => {
      const session = await api.createSession({...options, autoApprove: true}, record);
      try {
        trace.results.push(await session.send('first'));
        await session.restart();
        trace.results.push(await session.send('restart'));
      } finally { await session.close(); }
      const other = await api.createSession(options, record);
      try { trace.results.push(await other.send('isolated')); } finally { await other.close(); }
    });

  for (const [name, min, max] of [['randomFullWidth', -(1n << 63n), (1n << 63n) - 1n], ['randomNegative', -7n, -2n]]) {
    // A random 19-digit draw can pass a credit-card checksum; compare the draw itself here.
    const yaml = local(['random']).replace('model: local/test', 'redact_secrets: false, model: local/test');
    tests[name] = await replay(api, name, yaml, [rawTool('random_int', `{"min":${min},"max":${max}}`), answer], async args => {
      await single(args);
      const result = args.trace.events.find(e => e.type === 'tool_result');
      const stored = args.trace.requests[1].body.messages.at(-1);
      if (!/^-?\d+$/.test(result.output) || BigInt(result.output) < min || BigInt(result.output) > max || stored.content !== result.output) {
        throw new Error('random draw out of range or lost in history');
      }
      result.output = stored.content = 'random draw';
    });
  }

  await datedReplay('sessionArchiveOrder', local(['session_context']), [answer, answer, tool('list_sessions', {limit: 0}), tool('list_sessions', {limit: 1}),
    tool('list_sessions', {limit: 101}), tool('read_session', {session_id: '-2'}), tool('read_session', {session_id: '-3'}), answer],
    async ({api, options, record, trace}) => {
      const s = await api.createSession(options, record);
      try {
        for (const prompt of ['one', 'two', 'three']) {
          if (prompt !== 'one') await s.restart();
          trace.results.push(await s.send(prompt));
        }
      } finally { await s.close(); }
      // Rename only IDs returned by list_sessions, including their copies in history and transcripts.
      const list = trace.events.find(e => e.type === 'tool_result' && e.name === 'list_sessions');
      const ids = JSON.parse(list.output).map(row => row.id);
      if (ids.length !== 2 || new Set(ids).size !== 2 || ids.some(id => !/^[\da-f-]{36}$/.test(id))) throw new Error('expected two different session IDs');
      for (const key of ['requests', 'events']) trace[key] = JSON.parse(ids.reduce((s, id, i) => s.replaceAll(id, `session:${i}`), JSON.stringify(trace[key])));
    });
  // Go small leaves approval to the host: a config cannot waive it.
  for (const [name, yaml] of [['runtime', local(['memory']) + 'runtime: {safety: autonomous}\n'],
    ['agent', config.replace('model: local/test', 'safety: autonomous, toolsets: [{type: memory}], model: local/test')]]) {
    tests[`autonomousYaml_${name}`] = await replay(api, `autonomousYaml_${name}`, yaml,
      [tool('add_memory', {memory: 'not stored'}), answer], answering({decision: 'reject', reason: 'tester declined'}));
  }
  const terminalText = piece({content: 'last'}, 'stop');
  const chatRecord = chunk => `data: ${JSON.stringify(chunk)}\n\n`;
  const chatRaw = raw => ({raw, headers: {'Content-Type': 'text/event-stream'}});
  const chatFailure = {error: {message: 'invalid model'}};
  for (const [name, stream] of [
    ['Choices', [{choices: [{index: 1, delta: {content: 'first'}}, {index: 0, delta: {content: 'ignored'}}]}, terminalText]],
    ['Prelude', [{}, {choices: null}, {choices: [{}]}, piece({role: '', content: null}), piece({content: ''}),
      piece({content: 'one'}), piece({role: 'assistant'}), terminalText]],
    ['Parts', chatRaw('data: {"choices":[{"delta":{"content":[{ "type": "text", "text": "part" }]}}]}\n\n')],
    ['Reasoning', [piece({reasoning_content: {ignored: true}, reasoning: 'plan', content: 'one'}),
      piece({reasoning_content: 'two', reasoning: {ignored: true}}), terminalText]],
    ['BlankReasoning', [piece({reasoning: '  ', content: 'visible'}, 'stop')]],
    ['Legacy', [piece({function_call: {name: 'think', arguments: args}}, 'function_call')]],
    ['Refusal', [piece({refusal: 'no'}, 'content_filter')]],
    ['UsageInline', [{...piece({content: 'one'}), usage: {prompt_tokens: 8}}, {...terminalText, usage: {completion_tokens: 2}}, usage]],
    ['UsageNoChoices', [terminalText, {usage: {prompt_tokens: 4}}, usage]],
    ['UsageHeldStop', [terminalText, piece({content: 'late'}), usage]],
    ['NullCounters', [terminalText, {usage: {prompt_tokens: 3, completion_tokens: null, prompt_tokens_details: {cached_tokens: null}}}]],
    ['UsageNotStop', [{...piece({reasoning: 'plan'}), usage: {}}]],
    ['UsageToolTrailers', [piece({content: 'text'}, 'tool_calls'), usage, {...usage, usage: {prompt_tokens: 9}}]],
    ['ErrorAfterUsage', [terminalText, usage, chatFailure]],
    ['SseLines', chatRaw(': ping\r\n\r\nevent: chat\r\ndata: {"choices":\r\ndata: [{"delta":{"content":"ok"}}]}\r\n\r\n')],
    ['Bom', chatRaw('\uFEFF' + chatRecord(tool('think', {thought: 'ignored'})[0]) + chatRecord(terminalText))],
    ['ThreadEvent', chatRaw('event: thread.run\n' + chatRecord(tool('think', {thought: 'ignored'})[0]) + chatRecord(terminalText))],
    ['EarlyDone', chatRaw(chatRecord(piece({content: 'enough'})) + 'data: [DONE]\n\n' + chatRecord(chatFailure))],
  ]) tests[`chat${name}`] = await replay(api, `chat${name}`, thinks, [stream, answer], turns);
  for (const [name, stream] of [
    ['Null', [{error: null}]], ['Between', [piece({content: 'started'}), chatFailure, terminalText]],
    ['HeldStop', [terminalText, chatFailure]], ['NonJson', chatRaw(chatRecord(piece({content: 'started'})) + 'data: not JSON\n\n')],
  ]) tests[`chatError${name}`] = await replay(api, `chatError${name}`, thinks, [stream, answer], rejectsThenRecovers);

  const fragment = (id, index, name, bytes) => ({id, index, type: 'function', function: {name, arguments: bytes}});
  const fragments = (...calls) => piece({tool_calls: calls});
  const callEnd = piece({}, 'tool_calls');
  for (const [name, stream] of [
    ['Prefix', rawTool('thi', '{}')],
    ['SplitName', [fragments(fragment('a', 0, 'th', args.slice(0, 3))), fragments(fragment(null, 0, 'ink', args.slice(3))), callEnd]],
    ['RepeatName', [fragments(fragment('a', 0, 'think', args.slice(0, 3))), fragments(fragment(null, 0, 'think', args.slice(3))), callEnd]],
    ['ArgsFirst', [fragments(fragment('a', undefined, '', args)), fragments(fragment(null, undefined, 'think', '')), callEnd]],
    ['NoName', [fragments(fragment('a', 0, '', '{}')), callEnd]],
    ['Parallel', [fragments(fragment('b', 1, 'think', args.slice(0, 5)), fragment('a', 0, 'think', args.slice(0, 3))),
      fragments(fragment(null, 0, undefined, args.slice(3)), fragment(null, 1, undefined, args.slice(5))), callEnd]],
    ['Narration', [piece({content: 'before'}), fragments(fragment('a', 0, 'think', args)), callEnd, piece({content: 'after'}), usage]],
  ]) tests[`chatCall${name}`] = await replay(api, `chatCall${name}`, thinks, [stream, answer, answer], turns);
  tests.chatBareDataBeforeDone = await replay(api, 'chatBareDataBeforeDone', thinks,
    [chatRaw(tool('think', {thought: 'not executed'}).map(chatRecord).join('') + 'data\ndata: [DONE]\n\n'), answer], rejectsThenRecovers);
  for (const reason of ['stop', 'length', 'tool_calls', 'function_call', 'content_filter', null, '', 'custom']) {
    const name = `chatFinish_${reason === '' ? 'empty' : reason ?? 'missing'}`;
    tests[name] = await replay(api, name, config, [[piece({}, reason)], [piece({content: 'visible'}, reason)]], turns);
  }
  for (const model of ['gpt-4o', 'gpt-4.1', 'gpt-5', 'o3-mini', 'unknown']) {
    for (const tools of [false, true]) {
      const name = `chatRequest_${model}_${tools ? 'tools' : 'plain'}`;
      const yaml = (tools ? withSchema(thinks, {mode: 'native'}) : config)
        .replace('provider: openai,', 'provider: openai, api_type: openai_chatcompletions,')
        .replace('agents:', `models: {m: {provider: local, model: ${model}, max_tokens: 123, temperature: 0, top_p: 0.7, presence_penalty: 0.1, frequency_penalty: -0.2, parallel_tool_calls: false}}\nagents:`)
        .replace('model: local/test', 'model: m');
      tests[name] = await replay(api, name, yaml, tools ? [tool('think', {thought: 'plan'}), chatJSON, chatJSON] : [answer, answer], turns);
    }
  }
  const encryptedItem = {...reasoningItem, encrypted_content: 'opaque fixture', summary: [{type: 'summary_text', text: reasonDelta.delta}]};
  const reasoningCall = [reasonDelta, doneItem(encryptedItem), ...responseTool.slice(0, -1), fin([encryptedItem, ...callFinished.response.output])];
  const requestScripts = responses => responses ? [reasoningCall, text, text] : [[piece({reasoning: 'thinking'}), ...ponder('plan')], answer, answer];
  const providerCatalog = await (await fetch('/providers.json')).json();
  const aliases = {fireworks: 'fireworks-ai', together: 'togetherai', moonshot: 'moonshotai', 'opencode-zen': 'opencode'};
  for (const p of providerCatalog.filter(p => p.api_type === 'openai')) {
    for (const provider of [p.name, ...Object.keys(aliases).filter(k => aliases[k] === p.name)]) {
      const name = `requestProvider_${provider}`, model = p.default_model ?? 'gpt-4o';
      const responses = provider === 'chatgpt' || (p.auto_responses && model.startsWith('gpt-5'));
      const azure = provider === 'azure';
      const base = (p.base_url ?? 'https://fixture.openai.azure.com/openai/deployments/test')
        .replace('${CLOUDFLARE_ACCOUNT_ID}', 'acct').replace('${CLOUDFLARE_GATEWAY_ID}', 'gw');
      const yaml = JSON.stringify({agents: {root: {model: azure ? 'm' : `${provider}/${model}`, instruction: 'behave', toolsets: [{type: 'think'}]}},
        models: azure ? {m: {provider, model, base_url: base, provider_opts: {api_version: 'v ~*&'}}} : {}});
      tests[name] = await replay(api, name, yaml, requestScripts(responses), async args => {
        for (const key of p.token_env_vars ?? []) args.options.env[key] = `fixture-${key}`;
        Object.assign(args.options.env, {CLOUDFLARE_ACCOUNT_ID: 'acct', CLOUDFLARE_GATEWAY_ID: 'gw'});
        await turns(args);
      }, base + (responses ? '/responses' : '/chat/completions') + (azure ? '?api-version=v+~%2A%26' : ''));
    }
  }
  const requestBase = 'https://model.invalid/v1';
  for (const responses of [false, true]) {
    const api_type = responses ? 'openai_responses' : 'openai_chatcompletions';
    const endpoint = requestBase + (responses ? '/responses' : '/chat/completions');
    for (const [label, model, options, defaults, plain] of [
      ['medium', 'o1', {}], ['expanded', '${env.MODEL}', {}], ['minimal', 'o4-mini', {thinking_budget: 'minimal'}],
      ['low', 'gpt-5-mini', {thinking_budget: ' LOW '}], ['mediumExplicit', 'gpt-5.1', {thinking_budget: 'medium'}],
      ['xhigh', 'gpt-5.2', {thinking_budget: 'xhigh'}], ['max', 'gpt-5.4', {thinking_budget: 'max'}],
      ['maxPlain', 'gpt-5.4', {thinking_budget: 'max'}, {}, true], ['off', 'o3-mini', {thinking_budget: 0, max_tokens: -1}],
      ['none', 'gpt-5.6-sol', {thinking_budget: 'none', max_tokens: 0}], ['uncapped', 'o1', {thinking_budget: 'none'}, {}, true],
      ['sol', 'gpt-6-sol-2026-10-12', {thinking_budget: 'none', max_tokens: 12}], ['astra', 'gpt-6-astra', {thinking_budget: 0, max_tokens: 12}],
      ['chatLatest', 'gpt-5-chat-latest', {thinking_budget: 'high'}], ['legacy', 'gpt-4.1', {thinking_budget: 1024}],
      ['compatible', 'qwen3', {thinking_budget: 'adaptive/high', max_tokens: 9007199254740991, temperature: -2, top_p: 1.1}],
      ['inherited', 'gpt-5.2', {max_tokens: 0, temperature: 0, provider_opts: {service_tier: 'flex'}},
        {thinking_budget: 'high', max_tokens: 123, temperature: -1, top_p: 0, parallel_tool_calls: false, presence_penalty: -1, frequency_penalty: 2,
          provider_opts: {service_tier: 'auto', replay_reasoning: false}}],
      ['override', 'o1', {thinking_budget: 'low', provider_opts: {api_type, replay_reasoning: true}},
        {api_type: responses ? 'openai_chatcompletions' : 'openai_responses', provider_opts: {replay_reasoning: false}, thinking_budget: 0}],
      ['proxyState', 'o3', {provider_opts: {preserve_reasoning: true}, track_usage: !responses}],
      ['underlyingCopilot', 'gpt-5', {token_key: 'LOCAL_KEY'}, {provider: 'github-copilot', provider_opts: {http_headers: {'copilot-integration-id': 'test'}}}],
    ]) {
      const name = `requestOptions_${responses ? 'responses' : 'chat'}_${label}`;
      const yaml = JSON.stringify({providers: {p: {provider: 'openai', base_url: requestBase, token_key: 'LOCAL_KEY', api_type, ...defaults}},
        models: {m: {provider: 'p', model, ...options}}, agents: {root: {model: 'm', instruction: 'behave', toolsets: plain ? [] : [{type: 'think'}]}}});
      tests[name] = await replay(api, name, yaml, requestScripts(responses).slice(plain ? 1 : 0), async args => {
        args.options.env.MODEL = 'o3-mini'; await turns(args);
      }, endpoint);
    }
  }
  const inherited = {api_type: 'openai_chatcompletions', base_url: requestBase, token_key: 'LOCAL_KEY', thinking_budget: 'high', max_tokens: 97};
  const chosen = {provider: 'p', model: 'o3-mini', thinking_budget: 0, max_tokens: 2};
  tests.requestNamedPrecedence = await replay(api, 'requestNamedPrecedence', JSON.stringify({providers: {p: inherited},
    models: {'p/gpt-5': chosen}, agents: {root: {model: 'p/gpt-5', toolsets: [{type: 'think'}]}}}),
    requestScripts(false), turns, requestBase + '/chat/completions');
  for (const [name, agent, first] of [
    ['requestPickerPrecedence', {toolsets: [{type: 'model_picker', models: ['next']}, {type: 'think'}]}, tool('change_model', {model: 'next'})],
    ['requestFallbackPrecedence', {fallback: {models: ['next'], retries: -1}, toolsets: [{type: 'think'}]}, http(401)],
  ]) {
    const yaml = JSON.stringify({providers: {p: inherited}, models: {next: chosen}, agents: {root: {model: 'p/gpt-5', ...agent}}});
    tests[name] = await replay(api, name, yaml, [first, ponder('plan'), answer, answer], turns, requestBase + '/chat/completions');
  }
  for (const [base, path] of [["path!'[]", 'path%21%27%5B%5D'], ["path!'[]/", "path!'[]"]]) {
    const name = base.endsWith('/') ? 'requestPathRaw' : 'requestPathEscaped';
    tests[name] = await replay(api, name, config.replace(`${location.origin}/v1`, JSON.stringify(`${requestBase}/${base}`)),
      [answer, answer], turns, `${requestBase}/${path}/chat/completions`);
  }
  tests.requestDeferredOptOut = await replay(api, 'requestDeferredOptOut',
    `models: {m: {provider: openai, model: gpt-5.4, thinking_budget: high, provider_opts: {supports_deferred_tools: false}}}\nagents: {root: {model: m, toolsets: [{type: think, defer: true}]}}`,
    [responseCall({...fn, name: 'add_tool'}, '{"name":"think"}'), reasoningCall, text, text], async args => {
      args.options.env.OPENAI_API_KEY = 'fixture'; await turns(args);
    }, 'https://api.openai.com/v1/responses');
  const routedReasoning = JSON.stringify({providers: {p: {base_url: requestBase, token_key: 'LOCAL_KEY'}},
    models: {next: {provider: 'p', model: 'o4-mini', thinking_budget: 'high'}},
    agents: {root: {model: 'p/gpt-5', fallback: {models: ['next'], retries: -1}, toolsets: [{type: 'think'}]}}});
  tests.requestFallbackReasoning = await replay(api, 'requestFallbackReasoning', routedReasoning,
    [http(401), reasoningCall, text, text], turns, requestBase + '/responses');
  for (const provider of ['openai_chatcompletions', 'openai_responses']) {
    const name = `requestProtocol_${provider}`;
    const yaml = JSON.stringify({models: {m: {provider, model: 'o3-mini', base_url: requestBase + '//', token_key: 'LOCAL_KEY'}},
      agents: {root: {model: 'm', toolsets: [{type: 'think'}]}}});
    tests[name] = await replay(api, name, yaml, requestScripts(false), turns, requestBase + '//chat/completions');
  }
  // Go advertises a denied tool, with its instructions, and refuses the call at dispatch.
  const stocked = local(['think', 'todo', 'memory']);
  const denying = list => `permissions: {deny: [${list}]}\n`;
  const batch = (...calls) => [piece({role: 'assistant', tool_calls: calls.map(([name, args], index) =>
    ({id: `call_${index}`, index, type: 'function', function: {name, arguments: args}}))}, 'tool_calls'), usage];
  const workers = routed.replace('instruction: Be precise', 'instruction: Be precise, toolsets: [{type: think}]');
  const narrowed = entry => config.replace('model: local/test', `toolsets: [${entry}, {type: think}], model: local/test`);
  for (const [name, yaml, replies] of [
    ['denyName', stocked + denying('think'), [tool('think', {thought: 'x'})]],
    ['denyNameCase', stocked + denying('THINK'), [tool('think', {thought: 'x'})]],
    ['denyGlob', stocked + denying('"create_*", "thin?"'), [batch(['create_todo', '{"description":"d"}'], ['think', '{"thought":"x"}'])]],
    ['denyBeforeDecoding', stocked + denying('think'), [rawTool('think', '{"thought": oops')]],
    ['denyAllStillNamesUnknown', stocked + denying('"*"'), [batch(['think', '{"thought":"x"}'], ['nope', '{}'])]],
    ['denyBesideAllowed', stocked + denying('think'), [batch(['think', '{"thought":"x"}'], ['create_todo', '{"description":"d"}'])]],
    ['denySecretArguments', stocked + denying('think'), [tool('think', {thought: 'ghp_' + 'a'.repeat(36)})]],
    ['denyHosted', stocked + denying('add_memory'), [tool('add_memory', {memory: 'm'})]],
    ['denyFetch', local(['fetch']) + denying('fetch'), [tool('fetch', {urls: ['https://tool.invalid/a']})]],
    ['denyDeferred', deferred + denying('think'), [tool('search_tool', {query: 'think'}), tool('add_tool', {name: 'think'}), tool('think', {thought: 'plan'})]],
    ['denySearch', deferred + denying('search_tool'), [tool('search_tool', {query: 'think'}), tool('add_tool', {name: 'think'}), tool('think', {thought: 'plan'})]],
    ['denyActivation', deferred + denying('add_tool'), [tool('search_tool', {query: 'think'}), tool('add_tool', {name: 'think'})]],
    ['denyTransfer', workers + denying('transfer_task'), [tool('transfer_task', {agent: 'helper', task: 'count'})]],
    ['denyHandoff', workers + denying('handoff'), [tool('handoff', {agent: 'helper'})]],
    ['denyInsideTransfer', workers + denying('think'), [tool('transfer_task', {agent: 'helper', task: 'count'}), tool('think', {thought: 'x'}), answer]],
    ['denyAfterHandoff', workers + denying('think'), [tool('handoff', {agent: 'helper'}), tool('think', {thought: 'x'})]],
    ['denyQuestion', local(['user_prompt', 'think']) + denying('user_prompt'), [tool('user_prompt', {message: 'Which?'})]],
    ['denyModelChange', picker + denying('change_model'), [tool('change_model', {model: 'local/careful'})]],
    ['denyFiltered', narrowed('{type: memory, tools: [get_memories]}') + denying('"*"'), [tool('get_memories', {})]],
    // A filtered toolset keeps its instructions, even with nothing left to call.
    ['filteredInstructions', narrowed('{type: memory, tools: [get_memories]}'), [tool('add_memory', {memory: 'm'})]],
    ['readonlyInstructions', narrowed('{type: memory, readonly: true}'), [tool('add_memory', {memory: 'm'})]],
    ['emptiedInstructions', narrowed('{type: memory, tools: [nope]}'), [tool('think', {thought: 'x'})]],
    ['filteredCustomInstructions', narrowed('{type: memory, tools: [get_memories], instruction: "Mine. {ORIGINAL_INSTRUCTIONS} End."}'), [tool('get_memories', {})]],
  ]) tests[name] = await replay(api, name, yaml, [...replies, answer], approve);
  // Go drops a whitespace-only message from each request and refuses a request left empty.
  const sending = (...prompts) => async ({api, options, record, trace}) => {
    const s = await api.createSession(options, record);
    try { for (const prompt of prompts) trace.results.push(await s.send(prompt).catch(e => `error: ${e.message}`)); } finally { await s.close(); }
  };
  const unprompted = config.replace(", instruction: 'Hello ${env.WHO}'", '');
  for (const [name, yaml, replies, prompts, endpoint] of [
    ['blankPrompt', config, [answer, answer], ['', 'two']],
    ['blankFollowUp', config, [answer, answer], ['one', ' \u00a0\u3000\n']],
    ['blankPromptWithTool', thinks, [tool('think', {thought: 'x'}), answer], [' ']],
    ['blankPromptResponses', plainResponses, [text], [''], '/v1/responses'],
    ['blankInstruction', config.replace("'Hello ${env.WHO}'", "'  '"), [answer], ['hi']],
    ['emptyRequest', unprompted, [answer], ['', 'two']],
  ]) tests[name] = await replay(api, name, yaml, replies, sending(...prompts), endpoint);
  for (const [name, prompt] of [['blankChatPrompt', {role: 'user', content: ''}], ['absentChatPrompt', {role: 'user'}], ['nullChatPrompt', {role: 'user', content: null}]]) {
    tests[name] = await replay(api, name, config, [answer], async ({api, options, record, trace}) => {
      trace.results.push(await api.chat({...options, messages: [{role: 'user', content: 'a'}, {role: 'assistant', content: 'ok'}, prompt]}, record));
    });
  }
  // One synthetic candidate per pinned scanner rule, in catalog order. x{N} repeats a character.
  const fixtureJWT = [btoa('{"alg":"HS256"}'), btoa('{"sub":"fixture"}').replace(/=+$/, ''), 'a'.repeat(15)].join('.');
  const catalogSamples = [
    'AKIA' + 'RZPUZDIKQEXAMPLE', 'AWSKEYa{40}', 'ghp_A{30}0uCPlr', 'gho_A{30}0uCPlr', 'ghs_A{30}0uCPlr',
    'ghs_' + fixtureJWT, 'ghr_A{30}0uCPlr', 'github_pat_a{22}_a{53}0RaYBX', 'glpat-a{20}', 'hf_a{34}',
    '-----BEGIN PRIVATE KEY-----\n' + 'a'.repeat(5) + '\n-----END PRIVATE KEY-----', 'shpss_a{32}', 'xoxa-a{10}',
    'SK_TEST_a{10}', 'HEROKU="{uuid}"', 'https://hooks.slack.com/services/a{44}', 'SKa{32}',
    'AGE-SECRET-KEY-1Q{52}8H00W3', 'FACEBOOK="a{32}"', 'TWITTER="a{35}"', 'ADOBE="a{32}"', 'p8e-a{32}',
    'ALIBABA="a{30}"', 'ASANA="1{16}"', 'ASANA="a{32}"', 'ATLASSIAN="a{24}"', 'BITBUCKET="a{32}"', 'BITBUCKET="a{64}"',
    'BEAMER="B_a{44}"', 'CLOJARS_a{60}', 'CONTENTFUL="a{43}"', 'dapia{32}', 'DISCORD="a{64}"', 'DISCORD="1{18}"',
    'DISCORD="a{32}"', 'dp.pt.a{43}', 'DROPBOX="a{15}"', 'DROPBOX="SL.a{135}"', 'DROPBOX="a{11}A{10}a{43}"',
    '"duffel_test_a{43}"', 'dt0c01.a{24}.a{64}', 'EZAKa{54}', 'FASTLY="a{32}"', 'FINICITY="a{20}"', 'FINICITY="a{32}"',
    'FLWPUBK_TEST-a{32}-X', 'FLWSECK_TESTa{12}', 'fio-u-a{64}', '"live_a{40}"', 'eyJrIjoia{72}', 'a{14}.ATLASV1.a{60}',
    'HUBSPOT="{uuid}"', 'INTERCOM="a{60}"', 'INTERCOM="{uuid}"', 'IONIC="ION_a{42}"', fixtureJWT, 'lin_api_a{40}',
    'LINEAR="a{32}"', 'LOB="LIVE_a{35}"', 'LOB="TEST_PUB_a{31}"', 'MAILCHIMP="a{32}-US20"', 'MAILGUN="KEY-a{32}"',
    'MAILGUN="a{32}-a{8}-a{8}"', 'PK.a{60}.a{22}', 'MESSAGEBIRD="a{25}"', 'MESSAGEBIRD="{uuid}"', 'NRAK-A{27}',
    'NEWRELIC="a{64}"', 'NRJS-a{19}', 'npm_a{36}', 'pscale_pw_a{43}', 'pscale_tkn_a{43}', 'packagist_uut_a{68}',
    'PMAK-a{24}-a{34}', 'pul-a{40}', 'rubygems_a{48}', 'SG.a{66}', 'xkeysib-a{64}-a{16}', 'shippo_live_a{40}',
    'LINKEDIN="a{16}"', 'LINKEDIN="a{14}"', 'TWITCH="a{30}"', 'TYPEFORM=TFP_a{59}',
    '.dockerconfigjson: ' + btoa(JSON.stringify({auths: {fixture: {auth: btoa('a:')}}})), 'dckr_pat_a{27}',
    'dckr_oat_a{27}', 'sk-a{20}T3BlbkFJa{20}', 'sk-ant-api11-a{93}AA', 'AIzaa{35}', 'GOCSPX-a{28}', 'doo_v1_a{64}',
    'whsec_a{32}', 'AKCpa{69}', 'sntrys_eyJa{40}', 'RK_TEST_a{10}', 'ntn_a{46}', 'glptt-a{40}', 'hvs.a{90}',
    'xoxe-a{40}', 'r8_a{37}', 'ATATT3xFfGF0a{180}', 'MTa{22}.b{6}.c{27}', 'https://discord.com/api/webhooks/1/a{16}',
    '1{8}:AAa{33}', 'FlyV1 fm2_a{40}', 'gsk_a{52}', 'pplx-a{48}', 'xai-a{80}', 'co_a{40}', 'bkua_a{40}',
    'CCIPRJ_a_a{32}', 'cloudinary://' + '1:a@a', 'mongodb://' + 'a:a@', 'postgres://' + 'a:a@',
    'DefaultEndpointsProtocol=https;AccountName=fixture;AccountKey=' + btoa('a'.repeat(64)), 'SK.a{60}.a{22}',
    'hvb.a{90}', 'hvr.a{90}', 'nfp_a{40}', '1/1{14}:a{32}', 'v1.0-a{32}-a{146}', 'ops_eyJa{250}', 'sk-or-v1-a{64}',
    'sqa_a{40}', 'pckey_a_a{24}', 'sb_secret_a{40}', 'tskey-auth-a{10}-a{20}', 'tskey-api-a{10}-a{20}', 'vci_a{20}',
    'rzp_test_a{14}', 'access-sandbox-{uuid}', 'phx_a{43}', 'rnd_a{30}', 'hcaic_a{58}', 'akab-a{16}-a{16}',
    'aio_a{28}', 'A3-A{6}-A{11}-A{5}-A{5}-A{5}', 'glcbt-a_a{20}', 'gldt-a{20}', 'glffct-a{20}', 'glft-a{20}',
    'glagent-a{50}', 'gloas-a{64}', 'GR1348941a{20}', 'glrt-a{20}', 'glsoat-a{20}', 'glimt-a{25}', 'glpat-a{27}.a{9}',
    'ABSK' + btoa('BedrockAPIKey-'.padEnd(96, 'a')), 'bedrock-api-key-YmVkcm9jay5hbWF6b25hd3MuY29ta{20}',
    'HRKU-AAa{58}', 'a{3}1Q~a{31}', 'sha256~a{43}', 'pscale_oauth_a{32}', 'glc_a{32}', 'glsa_a{32}_a{8}', 'NRII-a{32}',
    'sntryu_a{64}', 'xapp-1-A{8}-1{8}-a{32}',
    'https://a.webhook.office.com/webhookb2/{uuid}@{uuid}/IncomingWebhook/a{32}/{uuid}', 'cmVmda{59}', 'ico-a{32}',
    'pnu_a{36}', 'rdme_a{70}', 'a{6}_a{29}_mmk', 'AQVNa{35}', 'sgp_a{40}', 'dnkey-a{26}-a{52}', 'tk-us-a{48}',
    'DATADOG=a{40}', 'SNYK={uuid}', 'LAUNCHDARKLY=a{40}', 'CLOUDFLARE=a{40}', 'CLOUDFLARE=a{37}', 'CONFLUENT=a{16}',
    'CONFLUENT=a{64}', 'LOOKER=a{20}', 'LOOKER=a{24}', 'MATTERMOST=a{26}', 'SUMO=a{64}', 'TRAVIS=a{22}',
    'ZENDESK=a{40}', 'CODECOV=a{32}', 'ALGOLIA=a{32}', 'AIRTABLE=a{17}', 'SENDBIRD=a{40}', 'OKTA=00a{40}',
    'TRELLO=a{32}', 'MERAKI=a{40}', 'pk_1{7}_A{32}', 'alcht_a{30}', 'NGROK=2a{26}_1a{20}', 'DEEPSEEK=SK-a{32}',
    'ASSEMBLYAI=a{32}', 'DEEPGRAM=a{40}', 'LOGZ={uuid}', 'ETHERSCAN=a{34}', 'MORALIS=a{64}', 'figd_a{22}',
    'CFPAT-a{43}', 'dp.st.a{43}', 'pat-na1-{uuid}', 'sdk-{uuid}', 'access_token$production$a$a{25}',
    'mysql://' + 'a:a@', 'redis://' + ':a@', 'amqp://' + 'a:a@', 'HTTP://a{8}:a{8}@GEMS.CONTRIBSYS.COM',
    'pat.a{22}.a{24}.a{20}', 'api_org_a{34}', 'sm_pat_a{16}', 'sc_a{5}.a{4}.acc-a{10}.a{30}', 'pcsk_a{5}_a{63}',
    'lsv2_pt_a{32}_a{10}', 'nvapi-a{64}', 'NF-a{32}', 'https://hooks.zapier.com/hooks/catch/a{16}',
    'https://a.tines.com/webhook/a{32}/a{32}', 'pub-c-{uuid}', 'sub-c-{uuid}', 'rootly_a{64}', 'VF.a{24}.a{16}',
    'ddp_a{36}', 'BBFF-a{30}', 'CCIPAT_a{22}_a{40}', 'endr+a{16}', 'ATBBa{20}', 'pi_a{24}_secret_a{25}',
    'thog-key-a{16}', 'ramp_id_a{40}', 's-s4t2ud-a{64}', 'FLWSECK-a{32}-X',
    'https://hooks.slack.com/workflows/TA/AA/1{17}/a{20}', 'slk_a{64}', 'dtn_a{64}', 'bl_a{32}', 'sq0atp-a{22}',
    'API-A{26}', '4242 '.repeat(3) + '4242', 'DE89' + ' 3704 0044 0532 0130 00', '123-45-' + '6789',
    'secret-live-a{40}',
  ].map(s => s.replaceAll('{uuid}', 'a{8}-a{4}-a{4}-a{4}-a{12}').replace(/(.)\{(\d+)\}/g, (_, ch, n) => ch.repeat(Number(n))));
  tests.redactCatalog = await replay(api, 'redactCatalog', config, [answer], async ({api, options, record, trace}) => {
    // Keep keyword prefilters and positional assertions local to each sample.
    const messages = catalogSamples.flatMap(content => [{role: 'user', content}, {role: 'assistant', content: 'ok'}]);
    trace.results.push(await api.chat({...options, messages: [...messages, {role: 'user', content: 'done'}]}, record));
    const scrubbed = trace.requests[0].body.messages.filter(m => m.role === 'user').slice(0, -1);
    if (scrubbed.length !== catalogSamples.length || scrubbed.some(m => !m.content.includes('[REDACTED]'))) throw new Error('unmasked catalog sample');
  });
  const leg = {type: 'builtin', command: 'redact_secrets'};
  const escapedArgs = `{"thought":"${secret.replace('pat', 'p\\u0061t')}","extra":[3,true,null,{"${secret}":"${secret}"}]}`;
  for (const [name, setting] of [
    ['redactAllLegs', {}], ['redactOff', {redact_secrets: false}],
    ...['before_llm_call', 'tool_input_transform', 'tool_response_transform'].map(event => [
      'redactLeg_' + event, {redact_secrets: false, hooks: {[event]: event === 'before_llm_call' ? leg : [{matcher: 'think|other', hooks: [leg]}]}},
    ]),
  ]) {
    const yaml = thinks.replace("instruction: 'Hello ${env.WHO}'", Object.entries({instruction: secret, ...setting}).map(([k, v]) => `${k}: ${JSON.stringify(v)}`).join(', '));
    tests[name] = await replay(api, name, yaml, [rawTool('think', escapedArgs), secretReply, answer], sends([secret, 'again']));
  }
  const promptExpressions = ['${f("}", "${env.WHO}")} ${env.WHO}', '${g(`{`)} ${ env.WHO }',
    '${\u00a0env.WHO} ${\u000benv.WHO} ${\tenv.WHO}', '$${env.WHO} ${env.MISSING} ${unbound} ${env.WHO'];
  tests.promptExpressions = await replay(api, 'promptExpressions', config.replace("'Hello ${env.WHO}'", () => JSON.stringify(promptExpressions)), [answer, answer], turns);
  tests.guardCanonicalArguments = await replay(api, 'guardCanonicalArguments', thinks.replace('model: local/test', 'max_consecutive_tool_calls: 2, model: local/test'),
    [rawTool('think', '{"thought":"x","extras":[1.0,9007199254740993,"\\ud800"]}'),
      rawTool('think', '{"extras":[1,9007199254740992,"�"],"thought":"x"}', 'other'), answer], rejectsThenRecovers);
  for (const [name, yaml] of [['redactDocumentLabels', config], ['rawDocumentLabels', config.replace('model: local/test', 'redact_secrets: false, model: local/test')]]) {
    tests[name] = await replay(api, name, yaml, [answer], async ({api, options, record, trace}) => {
      const documents = [doc(secret + '.txt', 'text/plain', {inline_text: '</document-redacted-txt-text-plain> A note.'}),
        doc('file', 'text/' + secret, {inline_text: 'Hello ' + secret})];
      trace.results.push(await api.chat({...options, messages: [{role: 'user', multi_content: documents}, {role: 'user', content: 'describe'}]}, record));
    });
  }
  const edgeSamples = [secret.slice(0, -1), secret.slice(0, -1) + '!', secret.replace('pat', 'pet'),
    secret.slice(0, 10) + '\n' + secret.slice(10), 'https://fixture.invalid/' + secret, btoa(secret),
    secret + secret, secret + ' ' + secret, 'glpat-' + 'a'.repeat(50) + '.aa1234567', 'ghp_' + 'a'.repeat(36)];
  tests.redactEdges = await replay(api, 'redactEdges', thinks, [thinkText(edgeSamples.join('\n')), secretReply, answer], sends([edgeSamples.join('\n'), 'again']));
  for (const [name, tokens, thought] of [['toolCapRounding', 4, 'x'.repeat(9)], ['toolCapUtf8', 13, '🌍'.repeat(16)]]) {
    const yaml = thinks.replace('model: local/test', `max_tool_result_tokens: ${tokens}, max_old_tool_call_tokens: ${tokens}, model: local/test`);
    tests[name] = await replay(api, name, yaml, [thinkText(thought), answer, answer], turns);
  }
  const limitFirst = mcp.replace('model: local/test', 'hooks: {tool_response_transform: {matcher: named_echo, hooks: {type: builtin, command: limit_large_tool_results}}}, model: local/test');
  for (const [name, yaml, payload] of [
    ['largeResultUtf8', mcp, 'a' + '🌍'.repeat(12800)],
    ['largeResultBackstop', limitFirst, 'x'.repeat(51190) + ' 123' + '456789'],
  ]) tests[name] = await replay(api, name, yaml, [tool('named_echo', {text: 'fixture'}), answer, answer], async ctx => {
    await turns({...ctx, options: {...ctx.options, autoApprove: true}});
  }, '/v1/chat/completions', echoService(payload));
  // Calls the API refuses carry Go's wording.
  tests.refusedCalls = await replay(api, 'refusedCalls', config, [answer, answer], async ({api, options, record, trace}) => {
    const refused = promise => promise.then(() => 'accepted', e => e.message);
    trace.results.push(await refused(api.chat({...options, messages: 'hello'}, record)));
    trace.results.push(await refused(api.chat({...options, messages: []}, record)));
    trace.results.push(await refused(api.chat({...options, messages: [{role: 'user', content: 'hi'}, {role: 'assistant', content: 'yes'}]}, record)));
    const s = await api.createSession(options, record);
    try {
      const first = s.send('one');
      trace.results.push(await refused(s.send('two')), await first, await s.send('three'));
    } finally { await s.close(); }
  });
  // Argument conditions see what Go's map[string]any holds: nothing unless the arguments are an object.
  const declining = answering({decision: 'reject', reason: 'no'});
  const noBad = {deny: ['think:thought=bad*']}, safeIds = {allow: ['delete_memory:id=safe*']};
  for (const [name, rules, toolName, args] of [
    ['conditionMalformed', noBad, 'think', '{"thought": bad'],
    ['conditionNull', noBad, 'think', 'null'],
    ['conditionEmpty', noBad, 'think', ''],
    ['conditionArray', noBad, 'think', '["bad"]'],
    ['conditionLastDuplicate', noBad, 'think', '{"thought":"ok","thought":"bad idea"}'],
    ['conditionFirstDuplicate', noBad, 'think', '{"thought":"bad idea","thought":"ok"}'],
    ['conditionEscapedKey', noBad, 'think', '{"t\\u0068ought":"BAD idea"}'],
    ['conditionUpperKey', {deny: ['think:Thought=bad*']}, 'think', '{"Thought":"bad"}'],
    ['conditionNumber', {deny: ['think:thought=1e+06']}, 'think', '{"thought":1000000}'],
    ['conditionNothing', {deny: ['think:thought=<nil>']}, 'think', '{"thought":null}'],
    ['conditionObject', {deny: ['think:thought=map*']}, 'think', '{"thought":{"b":1,"a":"x"}}'],
    ['conditionList', {deny: ['create_todos:descriptions=*b*']}, 'create_todos', '{"descriptions":["a","b"]}'],
    ['conditionListMiss', {deny: ['create_todos:descriptions=z*']}, 'create_todos', '{"descriptions":["a","b"]}'],
    ['allowCondition', safeIds, 'delete_memory', '{"id":"safe1"}'],
    ['allowConditionMiss', safeIds, 'delete_memory', '{"id":"other"}'],
    ['allowConditionMalformed', safeIds, 'delete_memory', '{"id":"safe1"'],
    ['allowConditionCase', safeIds, 'delete_memory', '{"ID":"safe1"}'],
    ['allowConditionDuplicate', safeIds, 'delete_memory', '{"id":"other","id":"safe1"}'],
  ]) tests[name] = await replay(api, name, stocked + `permissions: ${JSON.stringify(rules)}\n`, [rawTool(toolName, args), answer], declining);
  const thinkCall = ['think', '{"thought":"plan"}'], badThink = ['think', '{"thought": oops'], absent = ['absent', '{}'];
  const forget = ['delete_memory', '{"id":"unused"}'], badForget = ['delete_memory', '[1]'];
  const memories = ['get_memories', '{}'], askUser = ['user_prompt', '{"message":"Choose"}'], greeting = ['greet', '{}'];
  const localBatch = local(['think', 'memory', 'todo', 'user_prompt']);
  const apiBatch = apiTool.replace('toolsets: [', 'toolsets: [{type: think}, ');
  for (const [name, yaml, replies, drive = approve, service] of [
    ['batchLocalHost', localBatch, [batch(memories, thinkCall, memories), answer]],
    ['batchWaitingWriter', localBatch, [batch(absent, forget, badThink, thinkCall), answer]],
    ['batchLoggedHandlers', localBatch, [batch(['update_memory', '{"id":"unknown"}'], badThink, thinkCall), answer]],
    ['batchLogQueue', localBatch, [batch(badThink, absent, forget, ['update_todos', '{"updates":[{"id":"unknown","status":"completed"}]}']), answer]],
    ['batchDenied', localBatch + denying('list_todos'), [batch(forget, ['list_todos', '{}'], thinkCall), answer]],
    ['batchRejected', localBatch, [batch(forget, thinkCall, forget), answer], answering({decision:'reject', reason:'no'})],
    ['batchGranted', localBatch, [batch(forget, askUser, badForget, forget), answer], answering({decision:'approve_tool', toolName:'delete_memory'})],
    ['batchAutoApprove', localBatch, [batch(absent, forget, memories), answer], approved],
    ['batchElicitation', localBatch, [batch(askUser, badForget, askUser, forget), answer]],
    ['batchRedaction', localBatch, [batch(absent, ['think', JSON.stringify({thought:secret})], badForget), answer]],
    ['batchHookLimit', localBatch.replace('model: local/test', `${callLimit(1)} model: local/test`), [batch(badThink, memories, thinkCall), answer], rejectsThenRecovers],
    ['batchDiscovery', deferred, [batch(thinkCall, ['add_tool', '{"name":"think"}'], absent), tool('think',{}), answer]],
    ['batchModelPicker', picker, [batch(['change_model', '{"model":"off-list"}'], ['revert_model', '{}']), answer]],
    ['batchOutput', palette, [batch(badThink, [output, jsonAnswer], thinkCall), final]],
    ['batchAfterHandoff', workers, [pass(), batch(absent, thinkCall), answer]],
    ['batchInTransfer', workers, [delegate(), batch(badThink, thinkCall), answer, answer]],
    ['batchApiThink', apiBatch, [batch(greeting, thinkCall), answer], single, greet],
    ['batchApiPair', apiBatch, [batch(greeting, greeting), answer], single, greet],
    ['batchFetchWrite', httpConfig({type:'fetch'}).replace('toolsets: [', 'toolsets: [{type: memory}, '),
      [batch(['fetch', '{"urls":["https://tool.invalid/a"]}'], forget), answer], approved,
      req => new Response(req.url.endsWith('/robots.txt') ? 'User-agent: *\nAllow: /' : 'page')],
  ]) tests[name] = await replay(api, name, yaml, replies, drive, '/v1/chat/completions', service);
  const remoteSpec = {type: 'mcp', name: 'notes', remote: {url: fixture + '/mcp', transport_type: 'streamable'}};
  const catalogPage = [{name: 'lookup', annotations: {title: 'Lookup', readOnlyHint: true}},
    {name: 'read', description: 'Find a note', annotations: {destructiveHint: false}, inputSchema: {
      $defs: {Tag: {enum: ['blue', 3]}}, additionalProperties: false, required: ['unknown', 'rows'],
      properties: {tag: {$ref: '#/$defs/Tag'}, rows: {type: ['null', 'array'], items: row}, mode: {enum: ['red', false]}}}}];
  const mcpTool = 'notes_read', echoArgs = '{"pick":null,"rows":[{"tag":null}],"extra":"yes"}';
  const form = {message: 'Color?', requestedSchema: {type: 'object', properties: {color: {type: 'string'}}}, _meta: {title: 'Choose'}};
  for (const [name, c] of [
    ['mcpCatalog', {pages: true}], ['mcpCatalogResponses', {responses: true, pages: true}],
    ['mcpFilter', {filter: [mcpTool], pages: true}], ['mcpDiscover', {modern: true}], ['mcpNovember2024', {version: '2024-11-05'}],
    ['mcpTextParts', {result: {content: [part('A\ufeff'), part('B'), part('\u0085C'), {type: 'resource_link', name: 'note]', uri: 'note:a)b'},
      {type: 'resource', resource: {uri: 'note:one', text: ' footnote\udfff'}}], isError: true}}],
    ['mcpStructured', {result: {content: [], structuredContent: {color: 'blue'}}}], ['mcpEmptyResult', {result: {}}],
    ['mcpStructuredArray', {result: {content: [part('Useful text')], structuredContent: [1, 2]}}],
    ['mcpRpcInvalidParams', {error: {code: -32602, message: 'Invalid params', data: {ignored: 'do not display'}}}],
    ['mcpRpcEmptyMessage', {error: {code: -32602, message: '', data: {ignored: true}}}], ['mcpUnavailable', {status: 503}],
    ['mcpProgress', {push: 'notifications/progress', params: {progress: 1, progressToken: 'p'}}],
    ['mcpPing', {push: 'ping'}], ['mcpSampling', {push: 'sampling/createMessage'}], ['mcpRoots', {push: 'roots/list'}],
    ['mcpForm', {push: 'elicitation/create', params: form, reply: {action: 'accept', content: {color: 'blue'}}}],
    ['mcpDecline', {push: 'elicitation/create', params: {...form, mode: 'form'}, reply: 'decline'}],
    ['mcpCancel', {push: 'elicitation/create', params: form, reply: 'cancel'}],
    ['mcpUrl', {push: 'elicitation/create', params: {message: 'Visit', mode: 'url', url: fixture + '/verify', elicitationId: 'visit'}, reply: true}],
    ...Object.entries({numbers: '{"10":1.0,"2":{"z":-0,"a":"<>&\\u2028"},"u":1e21}', null: 'null', array: '[1]', scalar: 'false', bad: '{"x":', overflow: '{"x":[1e400],"x":0}',
      surrogate: '{"\\udfff": 1,"�":2,"v":"\\udfff"}'}).map(([label, args]) => [`args_mcp_${label}`, {args}]),
  ]) {
    let yaml = httpConfig({...remoteSpec, tools: c.filter});
    if (c.responses) yaml = yaml.replace('provider: openai,', 'provider: openai, api_type: openai_responses,');
    const args = c.args ?? echoArgs;
    let trace, acknowledge;
    const replies = c.responses ? [responseCall({...fn, name: mcpTool}, args), text] : [rawTool(mcpTool, args), answer];
    tests[name] = await replay(api, name, yaml, replies, async ctx => {
      trace = ctx.trace;
      trace.mcp = [];
      trace.results.push(await api.chat({...ctx.options, messages: [{role: 'user', content: 'go'}]}, event => {
        ctx.record(event);
        return event.type === 'tool_confirmation' ? {decision: 'approve'} : event.type === 'elicitation' ? c.reply : undefined;
      }));
    }, c.responses ? responsesPath : chatPath, async req => {
      if (req.url !== remoteSpec.remote.url || req.method !== 'POST') throw new Error('unexpected MCP request');
      const body = await req.text(), msg = JSON.parse(body);
      trace.mcp.push({round: trace.requests.length, method: req.method, url: req.url, headers: Object.fromEntries(req.headers), body});
      const response = result => Response.json({jsonrpc: '2.0', id: msg.id, result});
      switch (msg.method) {
        case 'server/discover': return response({supportedVersions: c.modern ? ['2026-07-28'] : [], capabilities: {tools: {}}, instructions: 'Modern notes.'});
        case 'initialize': return response({protocolVersion: c.version ?? '2025-11-25', serverInfo: {name: 'notes', version: '1'}, capabilities: {tools: {}}, instructions: 'Use the notes.'});
        case 'notifications/initialized': return new Response(null, {status: 202});
        case 'tools/list': return response(c.pages && msg.params.cursor ? {tools: [{name: 'last', inputSchema: {type: 'object', additionalProperties: {type: 'string'}}}]}
          : {tools: catalogPage, ...(c.pages && {nextCursor: 'next'})});
        case undefined: acknowledge(); return new Response(null, {status: 202});
        case 'tools/call': break;
        default: throw new Error(`unexpected MCP method ${msg.method}`);
      }
      if (c.status) return new Response('discarded diagnostic', {status: c.status});
      if (c.error) return Response.json({jsonrpc: '2.0', id: msg.id, error: c.error});
      const result = c.result ?? {content: [part('ok')]};
      if (!c.push) return response(result);
      const notice = c.push.startsWith('notifications/');
      const frame = fields => new TextEncoder().encode(`data: ${JSON.stringify({jsonrpc: '2.0', ...fields})}\n\n`);
      return new Response(new ReadableStream({async start(controller) {
        controller.enqueue(frame({method: c.push, params: c.params ?? {}, ...(!notice && {id: 'question'})}));
        if (!notice) await new Promise(ok => { acknowledge = ok; });
        controller.enqueue(frame({id: msg.id, result}));
        controller.close();
      }}), {headers: {'Content-Type': 'text/event-stream'}});
    });
  }
  return tests;
}

try { await fetch('/trace', { method: 'POST', body: JSON.stringify({ tests: await run() }) }); }
catch (e) { await fetch('/trace', { method: 'POST', body: JSON.stringify({ error: e.stack || String(e) }) }); }
