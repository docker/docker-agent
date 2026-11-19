import { createHost } from './host.mjs';
// The module owns config, conversation history, tool execution and turn decisions.
// Only browser I/O and the asynchronous lifetime of a session live here.
const encoder = new TextEncoder();
const decoder = new TextDecoder();
const maxResponse = 16 * 1024 * 1024;
const maxDocumentBytes = 16 * 1024 * 1024;

export async function loadDockerAgent(source = new URL('./docker-agent.wasm', import.meta.url)) {
  const module = source instanceof WebAssembly.Module ? source
    : source instanceof URL ? await WebAssembly.compileStreaming(fetch(source))
    : await WebAssembly.compile(source);
  const { exports: wasm } = await WebAssembly.instantiate(module, {});

  function call(op, args) {
    const bytes = encoder.encode(JSON.stringify({ ...args, op }));
    if (bytes.length > 32 * 1024 * 1024) throw new Error('portable request too large');
    const ptr = wasm.input(bytes.length);
    new Uint8Array(wasm.memory.buffer, ptr, bytes.length).set(bytes);
    const length = wasm.dispatch();
    const reply = JSON.parse(decoder.decode(new Uint8Array(wasm.memory.buffer, wasm.output(), length)));
    if (Object.hasOwn(reply, 'error')) throw new Error(reply.error);
    return reply.ok;
  }

  function session(opts, onEvent, history) {
    const stateless = history !== undefined;
    if (typeof opts?.yaml !== 'string') throw new Error('options.yaml must be a string');
    validateDocuments(opts.documents);
    const host = createHost(opts, onEvent, stateless);
    let handle = call('open', { yaml: opts.yaml, agent: opts.agentName ?? '', env: { ...opts.env }, history: history ?? [], stamp: sessionStamp() });
    try { modelURL(handle.endpoint); }
    catch (e) { call('close', { session: handle.session }); throw e; }
    let active, batch, restarting = false, closing, ready;
    async function prepare(signal) {
      const sets = await host.prepare(handle.toolsets, signal);
      signal.throwIfAborted();
      if (sets.length) call('registerTools', {session:handle.session, sets});
    }
    const pending = new Set();

    function emit(event) {
      if (typeof onEvent !== 'function') return;
      try { Promise.resolve(onEvent(event)).catch(report); } catch (e) { report(e); }
    }
    function report(e) { console.warn('docker-agent: onEvent failed', e); }
    function abort() {
      active?.controller.abort();
      pending.clear();
      batch?.reject(new Error('chat aborted'));
    }

    function answer(reply) {
      if (closing) throw new Error('session is closed');
      const { id, action, content } = reply ?? {};
      if (typeof id !== 'string' || !id) throw new Error('respondToElicitation: id is required');
      if (!pending.has(id)) throw new Error('no pending elicitation with that ID');
      const next = call('answerElicitation', { session: handle.session, id, action, content });
      pending.delete(id);
      settle(next);
    }

    function settle(next) {
      for (const event of next.events) emit(event);
      if (next.error) batch.reject(new Error(next.error));
      else if (!next.waiting) batch.resolve(next.request);
    }
    async function elicit(events, signal) {
      const replies = new Promise((resolve, reject) => { batch = { resolve, reject }; });
      // A direct abort can reject this promise while a host operation is awaiting I/O.
      replies.catch(() => {});
      try {
        for (const event of events) {
          signal.throwIfAborted();
          if (event.type === 'host_call') {
            const result = await host.execute(event, signal, emit);
            signal.throwIfAborted();
            settle(call('hostResult', {session:handle.session, id:event.id, name:event.name, ...result}));
          } else if (event.type === 'elicitation') {
            const id = event.id;
            pending.add(id);
            if (typeof onEvent !== 'function') answer({ id, action:'decline' });
            else if (stateless) answerFromCallback(event, id);
            else emit(event);
          } else emit(event);
        }
        return await replies;
      } finally { pending.clear(); batch = null; }
    }
    async function answerFromCallback(event, id) {
      try {
        const reply = await onEvent(event);
        if (!pending.has(id)) return;
        const action = typeof reply === 'string' ? reply : reply === true ? 'accept' : reply?.action;
        answer({ id, action, content: reply?.content });
      } catch {
        if (pending.has(id)) answer({ id, action: 'decline' });
      }
    }
    async function stop() {
      abort();
      await active?.done.catch(() => {});
    }
    async function run(prompt, signal) {
      const session = handle.session;
      try {
        signal.throwIfAborted();
        if (!ready) { ready = prepare(signal).catch(e => { ready = undefined; throw e; }); }
        await ready;
        signal.throwIfAborted();
        let body = call('begin', { session, prompt, id: crypto.randomUUID() });
        for (;;) {
          const response = await fetch(handle.endpoint, {
            method: 'POST', redirect: 'error', credentials: 'omit', signal,
            headers: { 'Content-Type': 'application/json', ...(handle.token && { Authorization: `Bearer ${handle.token}` }) },
            body: JSON.stringify(body),
          });
          if (!response.ok) {
            await response.body?.cancel();
            throw new Error(`model request failed: HTTP ${response.status}`);
          }
          for await (const chunk of sse(response, handle.responses)) {
            signal.throwIfAborted();
            for (const event of call('chunk', { session, chunk })) {
              signal.throwIfAborted();
              emit(event);
            }
          }
          signal.throwIfAborted();
          const next = call('end', { session });
          if (next.waiting) {
            body = await elicit(next.events, signal);
            signal.throwIfAborted();
            continue;
          }
          for (const event of next.events) emit(event);
          if (next.error) throw new Error(next.error);
          if (next.result) return next.result;
          signal.throwIfAborted();
          body = next.request;
        }
      } catch (e) {
        call('cancel', { session });
        if (signal.aborted) throw new Error('chat aborted');
        emit({ type: 'error', message: e.message });
        throw e;
      }
    }
    return {
      async send(prompt) {
        if (closing) throw new Error('session is closed');
        if (restarting) throw new Error('session is restarting');
        if (active) throw new Error('a run is already active');
        if (typeof prompt !== 'string') throw new Error('send: expected a prompt string');
        const controller = new AbortController();
        // Reserve synchronously; send(); abort() must not start a request.
        const done = Promise.resolve().then(() => run(prompt, controller.signal)).finally(() => { active = null; });
        active = { controller, done };
        return done;
      },
      abort,
      async restart() {
        if (closing) throw new Error('session is closed');
        if (restarting) throw new Error('session is restarting');
        restarting = true;
        try {
          await stop();
          if (closing) throw new Error('session is closed');
          call('restart', { session: handle.session, stamp: sessionStamp() });
          host.restart();
        } finally { restarting = false; }
      },
      close() {
        return closing ??= stop().then(async () => {
          await host.close();
          call('close', { session: handle.session });
          handle = onEvent = null;
        });
      },
      async confirm(reply, reason) { if (closing) throw new Error('session is closed'); host.confirm(reply, reason); },
      async respondToElicitation(reply) { if (!host.respond(reply)) answer(reply); },
    };
  }

  let currentChat;
  return {
    parseConfig: yaml => call('parse', { yaml }),
    listAgents: yaml => call('list', { yaml }),
    createSession: async (opts, onEvent) => session(opts, onEvent),
    async chat(opts, onEvent) {
      const messages = opts?.messages;
      if (!Array.isArray(messages)) throw new Error('messages must be an array');
      if (messages.at(-1)?.role !== 'user') throw new Error('the last message must be from the user');
      const s = session(opts, onEvent, messages.slice(0, -1));
      currentChat?.abort();
      currentChat = s;
      try { return await s.send(messages.at(-1).content); }
      finally {
        if (currentChat === s) currentChat = null;
        await s.close();
      }
    },
    abort() { currentChat?.abort(); },
  };
}

// Like Go small without RAG, validate host documents but do not index or send them.
function validateDocuments(documents) {
  if (documents == null) return;
  if (typeof documents !== 'object') throw new Error('options.documents: must be an object of path to content');
  const paths = Object.keys(documents);
  if (paths.length > 1000) throw new Error(`options.documents: ${paths.length} documents exceed the limit of 1000`);
  let size = 0;
  for (const path of paths) {
    if (!path.trim()) throw new Error('options.documents: document paths must not be empty');
    const text = documents[path];
    if (typeof text !== 'string') throw new Error(`options.documents: document ${JSON.stringify(path)} must be a string`);
    // A code unit takes at least one UTF-8 byte. Avoid encoding oversized inputs.
    if (size + text.length > maxDocumentBytes || (size += encoder.encode(text).length) > maxDocumentBytes) {
      throw new Error(`options.documents: documents exceed the limit of ${maxDocumentBytes} bytes in total`);
    }
  }
}

// Match time.Local.Format(time.RFC3339) in the Go browser runtime.
function sessionStamp() {
  const now = new Date();
  const minutes = -now.getTimezoneOffset();
  const local = new Date(now.getTime() + minutes * 60_000).toISOString().slice(0, 19);
  const pad = n => String(n).padStart(2, '0');
  const zone = minutes === 0 ? 'Z' : `${minutes > 0 ? '+' : '-'}${pad(Math.floor(Math.abs(minutes) / 60))}:${pad(Math.abs(minutes) % 60)}`;
  return { id: crypto.randomUUID(), created_at: local + zone };
}

function modelURL(endpoint) {
  const url = new URL(endpoint);
  const local = ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname);
  if (url.username || url.password || url.search || url.hash || (url.protocol !== 'https:' && !(url.protocol === 'http:' && local))) {
    throw new Error('model endpoint must be HTTPS (or HTTP loopback) without credentials, query or fragment');
  }
  return url;
}

async function* sse(response, responses) {
  if (!response.body || !/^text\/event-stream(?:;|$)/i.test(response.headers.get('Content-Type') ?? '')) {
    await response.body?.cancel();
    throw new Error('expected a text/event-stream model response');
  }
  const reader = response.body.getReader();
  const utf8 = new TextDecoder('utf-8', { fatal: true });
  let pending = '', data = [], total = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      total += value?.byteLength ?? 0;
      if (total > maxResponse) throw new Error('model response too large');
      pending += utf8.decode(value, { stream: !done });
      let i;
      while ((i = pending.indexOf('\n')) >= 0) {
        const line = pending.slice(0, i).replace(/\r$/, '');
        pending = pending.slice(i + 1);
        if (line.startsWith('data:')) data.push(line.slice(5).replace(/^ /, ''));
        else if (!line && data.length) {
          const record = data.join('\n');
          data = [];
          if (record === '[DONE]') return;
          const chunk = JSON.parse(record);
          yield chunk;
          // Responses has a terminal JSON event, not a Chat Completions [DONE] sentinel.
          if (responses && ['response.completed', 'response.done', 'response.incomplete'].includes(chunk.type)) return;
        }
      }
      if (done) throw new Error('model stream ended before [DONE]');
    }
  } finally {
    try { await reader.cancel(); } finally { reader.releaseLock(); }
  }
}
