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
    if ('safety' in opts || 'permissions' in opts) throw new Error('portable safety and permissions must be configured in YAML');
    validateDocuments(opts.documents);
    let host = createHost(opts, onEvent, stateless);
    let handle = call('open', { yaml: opts.yaml, agent: opts.agentName ?? '', env: { ...opts.env }, auto_approve: opts.autoApprove === true, history: history ?? [], stamp: sessionStamp() });
    try {
      for (const { targets } of Object.values(handle.profiles)) {
        for (const { transport, keyBase } of targets) prepareModelTransport(transport, keyBase);
      }
      host.configure(handle.profiles);
    } catch (e) { call('close', { session: handle.session }); throw e; }
    let active, batch, restarting = false, closing;
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
      // A terminal delta and finish must follow every other event in this batch.
      if (next.result) { batch.resolve(next); return; }
      for (const event of next.events) emit(event);
      if (next.error) batch.reject(new Error(next.error));
      else if (!next.waiting) batch.resolve({ request: next.request, events: [] });
    }
    async function elicit(events, signal) {
      const replies = new Promise((resolve, reject) => { batch = { resolve, reject }; });
      // A direct abort can reject this promise while a host operation is awaiting I/O.
      replies.catch(() => {});
      try {
        await deliver(events, signal);
        return await replies;
      } finally { pending.clear(); batch = null; }
    }
    async function deliver(events, signal) {
      for (const event of events) {
        signal.throwIfAborted();
        if (event.type === 'approval') {
          const answer = await host.approval(event, signal);
          signal.throwIfAborted();
          const next = call('confirm', {now:performance.now(),session:handle.session, agent:event.agent, token:event.token, ...answer});
          await deliver(next.events, signal);
          if (next.error) batch.reject(new Error(next.error));
          else if (next.result) batch.resolve({...next,events:[]});
          else if (!next.waiting) batch.resolve({request:next.request,events:[]});
        } else if (event.type === 'host_call') {
          const result = await host.execute(event, signal, emit);
          signal.throwIfAborted();
          settle(call('hostResult', {session:handle.session, agent:event.agent, token:event.token, ...result}));
        } else if (event.type === 'elicitation') {
          const id = event.id;
          pending.add(id);
          if (typeof onEvent !== 'function') answer({ id, action:'decline' });
          else if (stateless) answerFromCallback(event, id);
          else emit(event);
        } else emit(event);
      }
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
      let runError, roundContext;
      try {
        signal.throwIfAborted();
        let request = call('begin', { session, prompt, id: crypto.randomUUID(), now: performance.now() });
        if (request.event) emit(request.event);
        for (;;) {
          signal.throwIfAborted();
          if (request.nextIteration) request = call('nextIteration', { session, now: performance.now() });
          if (request.openTools !== undefined) {
            const agent = request.openTools;
            await host.open(agent, signal, sets => call('registerTools', {session, agent, sets}));
            signal.throwIfAborted();
          }
          if (request.dynamic) {
            const { addEnvironmentInfo, addDate, reuse } = request.dynamic;
            if (!reuse) {
              const { dynamicPrompt } = addEnvironmentInfo || addDate ? await import('./clock.mjs') : {};
              signal.throwIfAborted();
              roundContext = {
                environment: addEnvironmentInfo ? dynamicPrompt({ add_environment_info: true })[0] : undefined,
                date: addDate ? dynamicPrompt({ add_date: true })[0] : undefined,
              };
            }
            request = call('prepare', { session, ...roundContext });
          }
          let outcome;
          try {
            const { transport } = handle.profiles[request.agent].targets[request.target];
            const response = await modelFetch(transport, request, signal);
            for await (const chunk of sse(response, request.responses, request.summary)) {
              signal.throwIfAborted();
              const step = call('chunk', { session, chunk, now: performance.now() });
              if (step.request || step.retry) { outcome = step; break; }
              for (const event of step.events) {
                signal.throwIfAborted();
                if (!runError) emit(event);
              }
            }
          } catch (e) {
            signal.throwIfAborted();
            if (!(e instanceof TransportError)) throw e;
            outcome = call('failure', { session, now: performance.now(), ...e.failure });
          }
          signal.throwIfAborted();
          if (outcome) {
            if (outcome.event && !runError) {
              if (outcome.event.type === 'error') runError = new Error(outcome.event.message);
              emit(outcome.event);
            }
            if (outcome.delay) await wait(outcome.delay * (0.9 + 0.2 * Math.random()), signal);
            if (!outcome.retry) request = outcome.request;
            continue;
          }
          let next = call('end', { session, now: performance.now() });
          if (runError) {
            // Drain the continuation without projecting it; Rust rejects silent tool actions.
            if (next.result || next.waiting || next.error) throw runError;
            request = next.request;
            continue;
          }
          if (next.waiting) {
            next = await elicit(next.events, signal);
            signal.throwIfAborted();
          }
          for (const event of next.events) {
            signal.throwIfAborted();
            emit(event);
          }
          if (next.error) throw new Error(next.error);
          if (next.result) return next.result;
          signal.throwIfAborted();
          request = next.request;
        }
      } catch (e) {
        call('cancel', { session });
        if (signal.aborted) throw new Error('chat aborted');
        if (runError) throw runError;
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
        } finally { restarting = false; }
      },
      close() {
        return closing ??= stop().then(async () => {
          await host.close();
          call('close', { session: handle.session });
          handle = host = onEvent = null;
        });
      },
      async confirm(reply, reason) { if (closing) throw new Error('session is closed'); host.confirm(reply, reason); },
      async respondToElicitation(reply) {
        if (closing) throw new Error('session is closed');
        if (!host.respond(reply)) answer(reply);
      },
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
      const last = messages.at(-1);
      if (last?.role !== 'user') throw new Error('the last message must be from the user');
      const parts = last.multi_content;
      if (parts != null && (!Array.isArray(parts) || parts.length)) {
        throw new Error('the final user prompt is text-only; attachments belong in prior history');
      }
      const s = session(opts, onEvent, messages.slice(0, -1));
      currentChat?.abort();
      currentChat = s;
      try { return await s.send(last.content); }
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
  let url;
  try { url = new URL(endpoint); }
  catch { throw new Error('invalid model endpoint'); }
  const local = ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname);
  if (url.username || url.password || url.search || url.hash || (url.protocol !== 'https:' && !(url.protocol === 'http:' && local))) {
    throw new Error('model endpoint must be HTTPS (or HTTP loopback) without credentials, query or fragment');
  }
  return url;
}

function prepareModelTransport(target, keyBase) {
  const url = modelURL(target.endpoint);
  if (keyBase != null && url.origin !== modelURL(keyBase).origin) {
    throw new Error('cross-origin model endpoint requires an explicit model token_key');
  }
  if (target.azure_default_key && !(url.protocol === 'https:' && !url.port &&
      ['.openai.azure.com', '.services.ai.azure.com'].some(suffix => url.hostname.endsWith(suffix)))) {
    throw new Error('AZURE_API_KEY requires an Azure resource endpoint; use a dedicated token_key for other endpoints');
  }
  for (const [key, value] of Object.entries(target.query)) url.searchParams.append(key, value);
  target.endpoint = url.href;
  target.headers['Content-Type'] = 'application/json';
  if (target.chatgpt) target.headers.session_id = crypto.randomUUID();
}

class TransportError extends Error {
  constructor(reason, endpoint, status) { super(reason); this.failure = { reason, endpoint, status }; }
}

async function modelFetch({ endpoint, headers }, { body, summary }, signal) {
  const init = {
    method: 'POST', redirect: 'error', credentials: 'omit', signal,
    headers,
    body: JSON.stringify(body),
  };
  for (let attempt = 0; ; attempt++) {
    let response;
    try { response = await fetch(endpoint, init); }
    catch (e) {
      signal.throwIfAborted();
      if (attempt >= 2) {
        if (summary) throw new TransportError(`model request failed: ${e.message}`);
        const cause = e.cause === undefined ? '' : `: ${e.cause}`;
        throw new TransportError(`error receiving from stream: Post "${endpoint}": net/http: fetch() failed: ${e}${cause}`, endpoint);
      }
    }
    if (response?.ok) return response;
    // SDK retries precede classification by the runtime fallback executor.
    const retry = response?.headers.get('x-should-retry');
    if (attempt < 2 && (!response || retry === 'true' ||
        (retry !== 'false' && ([408, 409, 429].includes(response.status) || response.status >= 500)))) {
      await response?.body?.cancel().catch(() => {});
      await wait(sdkRetryDelay(response?.headers, attempt), signal);
      continue;
    }
    throw new TransportError(await errorDetail(response), endpoint, response.status);
  }
}

function sdkRetryDelay(headers, attempt) {
  for (const [key, unit] of [['Retry-After-Ms', 1], ['Retry-After', 1000]]) {
    const raw = headers?.get(key);
    if (!raw) continue;
    const n = Number(raw);
    if (Number.isFinite(n) && n >= 0) return Math.min(n * unit, 8000);
    if (unit === 1000 && Number.isNaN(n)) {
      const date = Date.parse(raw);
      if (Number.isFinite(date)) return Math.max(0, Math.min(date - Date.now(), 8000));
    }
  }
  return (500 << attempt) * (1 - Math.random() / 4);
}

// Bound diagnostics before copying them into Wasm, including proxy error pages.
async function errorDetail(response) {
  if (!response.body) return '';
  const reader = response.body.getReader();
  const utf8 = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true });
  let text = '', bytes = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) return text + utf8.decode();
      if ((bytes += value.byteLength) > 8192) break;
      text += utf8.decode(value, { stream: true });
    }
  } catch { /* Never project a partial diagnostic. */ }
  finally { await reader.cancel().catch(() => {}); reader.releaseLock(); }
}

function wait(delay, signal) {
  signal.throwIfAborted();
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      signal.removeEventListener('abort', abort);
      resolve();
    }, delay);
    function abort() { clearTimeout(timer); reject(signal.reason); }
    signal.addEventListener('abort', abort, { once: true });
  });
}

async function* sse(response, responses, summary) {
  if (!response.body || !/^text\/event-stream(?:;|$)/i.test(response.headers.get('Content-Type') ?? '')) {
    await response.body?.cancel();
    throw new Error('expected a text/event-stream model response');
  }
  const reader = response.body.getReader();
  const utf8 = new TextDecoder('utf-8', { fatal: true });
  let pending = '', data = [], total = 0, toolCompletion;
  try {
    for (;;) {
      const { done, value } = await reader.read().catch(e => { throw new TransportError(e.message); });
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
          if (!chunk || typeof chunk !== 'object' || Array.isArray(chunk)) throw new Error('invalid model stream event');
          if (toolCompletion && (record === toolCompletion ||
              (chunk.type === 'response.output_text.annotation.added' && !Object.hasOwn(chunk, 'error')))) continue;
          // Go's SDK retains the original JSON of top-level stream errors.
          yield (responses ? Object.hasOwn(chunk, 'error') : chunk.error != null) ? record : chunk;
          // Go's Chat adapter treats usage-only chunks as terminal, even without an explicit stop.
          if (summary && !responses && chunk.usage && (!chunk.choices?.length || chunk.choices[0].finish_reason)) return;
          // Go drains tool-call completions: a later stream error must veto dispatch.
          if (responses && chunk.type === 'response.incomplete') return;
          if (responses && ['response.completed', 'response.done'].includes(chunk.type)) {
            if (!chunk.response.output?.some(i => i.type === 'function_call')) return;
            toolCompletion = record;
          }
        }
      }
      if (done && responses) {
        if (pending || data.length) throw new Error('incomplete Responses event at EOF');
        return;
      }
      if (done) throw new TransportError('model stream ended before [DONE]');
    }
  } finally {
    try { await reader.cancel(); } finally { reader.releaseLock(); }
  }
}
