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
    trace.requests.push({ method: req.method, path: new URL(req.url).pathname,
      authorization: req.headers.get('Authorization'), body: await req.json() });
    if (!Array.isArray(chunks)) {
      const init = { status: chunks.status, headers: chunks.headers };
      return 'raw' in chunks ? new Response(chunks.raw, init) : Response.json(chunks.body, init);
    }
    const isChat = expected.endsWith('/chat/completions');
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
  const httpConfig = ts => config.replace('model: local/test', `toolsets: [${JSON.stringify({ ...ts, allow_private_ips: true })}], model: local/test`);
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
  tests.mcp = await replay(api, 'mcp', mcp,[tool('named_echo',{text:'from MCP'}),answer], async ({api,options,record,trace}) => {
    const s=await api.createSession({...options, autoApprove:true}, record);
    try {trace.results.push(await s.send('echo'));} finally {await s.close();}
  }, '/v1/chat/completions', async req => {
    if (req.url !== 'https://tool.invalid/mcp') throw new Error('unexpected MCP endpoint');
    if (req.method === 'DELETE') return new Response(null,{status:204});
    if (req.headers.get('X-Agent') !== 'Ada') throw new Error('missing inherited MCP header');
    const m=await req.json();
    if (m.method==='notifications/initialized') return new Response(null,{status:202});
    const val = m.method==='initialize' ? {protocolVersion:'2025-06-18',capabilities:{tools:{}},serverInfo:{name:'fixture',version:'1'}}
      : m.method==='tools/list' ? {tools:[{name:'echo',description:'Echo a word',inputSchema:{type:'object',properties:{text:{type:'string'}},required:['text'],additionalProperties:false}}]}
      : m.method==='tools/call' && m.params.name==='echo' ? {content:[{type:'text',text:m.params.arguments.text}]} : undefined;
    if (!val) throw new Error(`unexpected MCP method ${m.method}`);
    return Response.json({jsonrpc:'2.0',id:m.id,result:val},{headers:{'Mcp-Session-Id':'replay'}});
  });
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
          if (trace.requests[1].authorization !== 'Bearer summary-only' || JSON.stringify(trace.requests).includes(secret)) throw new Error('bad summary credentials or unredacted history');
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
    ['transferred', routed, 'transfer_task', {lenient: '{"AGENT":"helper","Task":"count","extra":1}', noTask: '{"agent":"helper"}'}, [answer]],
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
    ...[['clockHistorical', Date.UTC(1900, 6, 1), ['Europe/Paris', 'Europe/Dublin', 'Antarctica/Troll', 'Asia/Jerusalem']],
      ['clockSeasonal', memStart, ['America/Vancouver', 'Canada/Mountain', 'Local', 'Asia/Kathmandu']],
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
  return tests;
}

try { await fetch('/trace', { method: 'POST', body: JSON.stringify({ tests: await run() }) }); }
catch (e) { await fetch('/trace', { method: 'POST', body: JSON.stringify({ error: e.stack || String(e) }) }); }
