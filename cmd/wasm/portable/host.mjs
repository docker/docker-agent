// Each live session has its own native resources, tool bindings, and outstanding questions.
export function createHost(opts, onEvent, stateless) {
  const handlers = new Map(), closers = [];
  const questions = new Map();
  let local, confirmation, delivering;
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
    if (!waiter || waiter.signal.aborted) throw new Error('no tool confirmation is pending');
    // Legacy synchronous callbacks are already bound to the event being delivered.
    if (reply?.confirmationId !== waiter.token && !(reply?.confirmationId === undefined && delivering === waiter)) {
      throw new Error('confirmationId must match the pending tool confirmation');
    }
    decide(waiter, reply, reason);
  }
  function decide(waiter, reply, reason) {
    const decision = typeof reply === 'boolean' ? reply ? 'approve' : 'reject' : typeof reply === 'string' ? reply : reply?.decision;
    if (!['reject','approve','approve_tool','approve_balanced','approve_autonomous'].includes(decision)) throw new Error(`unknown decision ${JSON.stringify(decision)}`);
    if (decision === 'approve_tool' && reply?.toolName !== waiter.name) throw new Error('approve_tool requires toolName matching pending tool');
    // Broader modes authorize this invocation. Do not infer an always-allow rule
    // for other tools from the current tool's safety classification.
    const result = { approved: decision !== 'reject', reason: String(reason ?? reply?.reason ?? '').trim() };
    if (decision === 'approve_tool') result.remember = true;
    confirmation = null;
    waiter.resolve(result);
  }
  async function approval({ name, args, id, token }, signal) {
    signal.throwIfAborted();
    if (typeof onEvent !== 'function') return { approved:false, reason:'' };
    const { promise, resolve, reject } = Promise.withResolvers();
    const waiter = { resolve, name, token, signal };
    confirmation = waiter;
    const abort = () => reject(new Error('chat aborted'));
    signal.addEventListener('abort', abort, { once: true });
    try {
      signal.throwIfAborted();
      delivering = waiter;
      let reply;
      try { reply = dispatch({ type:'tool_confirmation', id, name, args, ...(!stateless && {confirmationId:token}) }); }
      finally { delivering = null; }
      if (stateless) Promise.resolve(reply).then(v => {
        if (confirmation === waiter && !signal.aborted) decide(waiter, v ?? false);
      }).catch(() => { if (confirmation === waiter && !signal.aborted) decide(waiter, false); });
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
  async function prepare(specs, signal, register) {
    const sets = [], bindings = new Map(), resources = [];
    try {
      for (const [index, spec] of specs.entries()) {
        signal.throwIfAborted();
        if (spec.type === 'memory' || spec.type === 'plan') {
          local ??= (await import('./local-tools.mjs')).localTools();
          bindings.set(index, (...args) => local.call(...args));
        } else if (spec.type === 'datetime' || spec.type === 'random') {
          bindings.set(index, async (name, args) => (await import('./clock.mjs')).clockTool(name, args));
        } else if (['mcp','fetch','api'].includes(spec.type)) {
          const { toolFetch, httpTools } = await import('./http-tools.mjs');
          const clean = { ...spec };
          for (const key of ['readonly', 'tools', 'instruction']) delete clean[key];
          const set = spec.type === 'mcp'
            ? await (await import('./mcp.mjs')).openMCP(clean, env, proxy, signal, { toolFetch, elicit, oauthRedirectURI:opts.oauthRedirectURI })
            : await httpTools(clean, env, proxy);
          if (set.close) resources.push(() => set.close());
          if (!Array.isArray(set.tools) || set.tools.length > 256) throw new Error('invalid host tool catalog');
          bindings.set(index, (name, args, signal) => set.call(name, args, signal));
          sets.push({ index, tools: set.tools, instructions: set.instructions ?? '' });
        }
      }
      signal.throwIfAborted();
      if (sets.length) register(sets);
      for (const [index, fn] of bindings) handlers.set(index, fn);
      closers.push(...resources);
    } catch (error) {
      await Promise.allSettled(resources.map(fn => Promise.resolve().then(fn)));
      throw error;
    }
  }
  async function execute(internal, signal, emit) {
    const event = { type:'tool_call', id:internal.id, name:internal.name, args:internal.args };
    signal.throwIfAborted();
    emit(event);
    signal.throwIfAborted();
    try {
      const fn = handlers.get(internal.source);
      if (!fn) throw new Error(`unavailable host tool ${event.name}`);
      const result = await fn(internal.name, internal.args, signal);
      if (typeof result.output !== 'string' || typeof result.is_error !== 'boolean') throw new Error('invalid host tool result');
      return result;
    } catch (e) {
      signal.throwIfAborted();
      return { output:e.message || String(e), is_error:true };
    }
  }
  return { prepare, approval, execute, confirm, respond, async close() {
    handlers.clear(); local = null;
    await Promise.allSettled(closers.splice(0).map(fn => Promise.resolve().then(fn)));
  } };
}
