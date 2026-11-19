// Each live session has its own native resources, tool bindings, and approvals.
export function createHost(opts, onEvent, stateless) {
  const handlers = new Map(), closers = [], grants = new Set();
  const questions = new Map();
  let local, confirmation;
  const autoApprove = opts.autoApprove === true;
  const env = Object.assign(Object.create(null), opts.env);
  const proxy = opts.toolProxy;
  if (proxy != null && proxy !== '') {
    const u = new URL(proxy);
    if (u.protocol !== 'https:' || !u.hostname || u.username || u.password || u.hash || u.searchParams.has('url')) throw new Error('egress proxy must use https without userinfo, fragment or url parameter');
  }
  function dispatch(event) {
    try { return onEvent?.(event); } catch { return undefined; }
  }
  function confirm(reply, reason) {
    const waiter = confirmation;
    if (!waiter) throw new Error('no tool confirmation is pending');
    const decision = typeof reply === 'boolean' ? reply ? 'approve' : 'reject' : typeof reply === 'string' ? reply : reply?.decision;
    if (!['reject','approve','approve_tool','approve_balanced','approve_autonomous'].includes(decision)) throw new Error(`unknown decision ${JSON.stringify(decision)}`);
    if (decision === 'approve_tool') {
      if (reply?.toolName !== waiter.name) throw new Error('approve_tool requires toolName matching pending tool');
      grants.add(waiter.name);
    }
    // Broader modes authorize this invocation. Do not infer an always-allow rule
    // for other tools from the current tool's safety classification.
    confirmation = null;
    waiter.resolve({ approved: decision !== 'reject', reason: String(reason ?? reply?.reason ?? '').trim() });
  }
  async function approval(event, signal) {
    if (autoApprove || grants.has(event.name)) return { approved: true };
    if (typeof onEvent !== 'function') return { approved: false };
    const { promise, resolve, reject } = Promise.withResolvers();
    const waiter = { resolve, name: event.name };
    confirmation = waiter;
    const abort = () => reject(new Error('chat aborted'));
    signal.addEventListener('abort', abort, { once: true });
    try {
      signal.throwIfAborted();
      const reply = dispatch({ ...event, type: 'tool_confirmation' });
      if (stateless) Promise.resolve(reply).then(v => {
        if (confirmation === waiter) confirm(v ?? false);
      }).catch(() => { if (confirmation === waiter) confirm(false); });
      else Promise.resolve(reply).catch(() => {});
      return await promise;
    } finally {
      signal.removeEventListener('abort', abort);
      if (confirmation === waiter) confirmation = null;
    }
  }
  async function elicit(event, signal) {
    if (typeof onEvent !== 'function') return { action: 'decline' };
    signal.throwIfAborted();
    const id = crypto.randomUUID();
    const { promise, resolve, reject } = Promise.withResolvers();
    questions.set(id, resolve);
    const abort = () => reject(new Error('chat aborted'));
    signal.addEventListener('abort', abort, { once: true });
    try {
      const reply = dispatch({ ...event, type: 'elicitation', id });
      if (stateless) Promise.resolve(reply).then(v => resolve(v && ['accept','decline','cancel'].includes(v.action) ? v : { action: 'decline' }), () => resolve({ action: 'decline' }));
      else Promise.resolve(reply).catch(() => {});
      return await promise;
    } finally { signal.removeEventListener('abort', abort); questions.delete(id); }
  }
  function respond(reply) {
    const resolve = questions.get(reply?.id);
    if (!resolve) return false;
    if (!['accept','decline','cancel'].includes(reply.action)) throw new Error('unknown elicitation action');
    questions.delete(reply.id); resolve(reply); return true;
  }
  async function prepare(specs, signal) {
    const sets = [], bindings = new Map(), resources = [];
    try {
    for (const [index, spec] of specs.entries()) {
      signal.throwIfAborted();
      if (spec.type === 'memory' || spec.type === 'plan') {
        local ??= (await import('./local-tools.mjs')).localTools();
      } else if (['mcp','fetch','api'].includes(spec.type)) {
        const { toolFetch, httpTools } = await import('./http-tools.mjs');
        const clean = { ...spec };
        for (const key of ['readonly', 'tools', 'instruction', 'model']) delete clean[key]; // enforced by Wasm registration
        const set = spec.type === 'mcp'
          ? await (await import('./mcp.mjs')).openMCP(clean, env, proxy, signal, { toolFetch, elicit, oauthRedirectURI:opts.oauthRedirectURI })
          : await httpTools(clean, env, proxy);
        if (set.close) resources.push(() => set.close());
        if (!Array.isArray(set.tools) || set.tools.length > 256) throw new Error('invalid host tool catalog');
        for (const tool of set.tools) {
          const name = tool.function?.name;
          if (bindings.has(name)) throw new Error(`duplicate tool name ${name}`);
          bindings.set(name, (args, signal) => set.call(name, args, signal));
        }
        sets.push({ index, tools: set.tools, instructions: set.instructions ?? '' });
      }
    }
    signal.throwIfAborted();
    for (const [name, fn] of bindings) handlers.set(name, fn);
    closers.push(...resources);
    return sets;
    } catch (error) {
      await Promise.allSettled(resources.map(fn => Promise.resolve().then(fn)));
      throw error;
    }
  }
  async function execute(internal, signal, emit) {
    const event = { type:'tool_call', id:internal.id, name:internal.name, args:internal.args };
    if (!internal.readOnly) {
      const { approved, reason } = await approval(event, signal);
      if (!approved) return { output: 'The user rejected the tool call.' + (reason ? ` Reason: ${reason}` : ''), is_error:true };
    }
    signal.throwIfAborted();
    emit(event);
    signal.throwIfAborted();
    try {
      const fn = handlers.get(event.name);
      let result;
      if (fn) result = await fn(event.args, signal);
      else if (['get_datetime','random_int'].includes(event.name)) result = (await import('./clock.mjs')).clockTool(event.name,event.args);
      else if (local) result = await local.call(event.name,event.args,signal);
      else throw new Error(`unavailable host tool ${event.name}`);
      if (typeof result.output !== 'string' || typeof result.is_error !== 'boolean') throw new Error('invalid host tool result');
      return result;
    } catch (e) {
      signal.throwIfAborted();
      return { output:e.message || String(e), is_error:true };
    }
  }
  return { prepare, execute, confirm, respond, restart: () => grants.clear(), async close() {
    grants.clear(); handlers.clear(); local = null;
    await Promise.allSettled(closers.splice(0).map(fn => Promise.resolve().then(fn)));
  } };
}
