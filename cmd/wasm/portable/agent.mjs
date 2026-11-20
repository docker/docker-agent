import { localTimestamp } from './clock.mjs';
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

  // time.Local is initialized once by the Go wasm runtime.
  const timezoneOffset = new Date().getTimezoneOffset();

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
    let sessionID;
    function sessionStamp() {
      sessionID = crypto.randomUUID();
      return { id: sessionID, created_at: localTimestamp(new Date(), timezoneOffset) };
    }
    const stateless = history !== undefined;
    if (typeof opts?.yaml !== 'string') throw new Error('options.yaml must be a string');
    if ('safety' in opts || 'permissions' in opts) throw new Error('portable safety and permissions must be configured in YAML');
    validateDocuments(opts.documents);
    let host = createHost(opts, onEvent, stateless, timezoneOffset);
    let handle = call('open', { yaml: opts.yaml, agent: opts.agentName ?? '', env: { ...opts.env }, auto_approve: opts.autoApprove === true, history, stamp: sessionStamp() });
    try {
      const profiles = Object.values(handle.profiles);
      for (const { targets } of profiles) {
        for (const { transport, keyBase } of targets) {
          prepareModelTransport(transport, keyBase);
          if (transport.opencode && profiles.length > 1) throw new Error('OpenCode in a team requires per-delegation session IDs');
        }
      }
      host.configure(handle.profiles);
    } catch (e) { call('close', { session: handle.session }); throw e; }
    let active, batch, restarting = false, closing;

    function emit(event) {
      if (typeof onEvent !== 'function') return;
      try { Promise.resolve(onEvent(event)).catch(report); } catch (e) { report(e); }
    }
    function report(e) { console.warn('docker-agent: onEvent failed', e); }
    function abort() {
      active?.controller.abort();
      batch?.wake();
    }

    // For batches outside the scheduler's envelope, retain the original host-order delivery.
    async function serialBatch(next, signal) {
      const session = handle.session, pending = new Set(), finished = Promise.withResolvers();
      const owner = new AbortController();
      signal = AbortSignal.any([signal, owner.signal]);
      const settle = step => {
        if (step.result) { finished.resolve(step); return; }
        for (const event of step.events) emit(event);
        if (!step.waiting) finished.resolve({...step, events:[]});
      };
      function answer(reply) {
        const { id, action, content } = reply ?? {};
        if (typeof id !== 'string' || !id) throw new Error('respondToElicitation: id is required');
        if (signal.aborted || !pending.has(id)) throw new Error('no pending elicitation with that ID');
        const step = call('answerElicitation', {session, id, action, content: answerContent(content)});
        pending.delete(id);
        settle(step);
      }
      async function deliver(step) {
        for (const e of step.events) {
          signal.throwIfAborted();
          if (e.type === 'host_call') {
            emit({type:'tool_call', id:e.id, name:e.name, args:e.args});
            const result = await host.execute(e, signal);
            signal.throwIfAborted();
            settle(call('hostResult', {session, agent:e.agent, token:e.token, ...result}));
          } else if (e.type === 'approval') {
            const reply = await host.approval(e, signal);
            signal.throwIfAborted();
            await deliver(call('confirm', {session, now:performance.now(), agent:e.agent, token:e.token, ...reply}));
          } else if (e.type === 'elicitation') {
            const id = e.id;
            pending.add(id);
            if (typeof onEvent !== 'function') answer({id,action:'decline'});
            else if (!stateless) emit(e);
            else void (async () => {
              try {
                const reply = await onEvent(e);
                if (!signal.aborted) answer({id, action:typeof reply === 'string' ? reply : reply === true ? 'accept' : reply?.action, content:reply?.content});
              } catch { if (!signal.aborted && pending.has(id)) answer({id,action:'decline'}); }
            })().catch(finished.reject);
          } else emit(e);
        }
        if (!step.waiting) finished.resolve({...step, events:[]});
      }
      batch = {answer, wake:() => finished.reject(new Error('chat aborted'))};
      finished.promise.catch(() => {}); // abort can reject during host I/O
      try { await deliver(next); return await finished.promise; }
      finally { owner.abort(); batch = null; }
    }

    // Drive runnable calls before consuming a callback answer. Only a plain chat answer
    // can wake Go before its parked stderr writers; promises and session replies need JS.
    async function driveBatch(next, signal) {
      const owner = new AbortController();
      signal = AbortSignal.any([signal, owner.signal]);
      const session = handle.session, ready = [], external = [], visible = [], pending = new Set();
      let wake, blocked = false, live = true, justUnlocked = false;
      const notify = () => wake?.();
      function schedule(queue, job) {
        if (!live || signal.aborted) return;
        queue.push(job);
        notify();
      }
      function resume(value, job, interactive) {
        let then;
        try { then = value?.then; } catch { value = undefined; }
        if (typeof then !== 'function') { schedule(ready, () => job(value)); return; }
        if (interactive && stateless) blocked = true;
        Promise.resolve(value).then(result => schedule(external, () => {
          if (interactive && stateless) blocked = false;
          return job(result);
        }), error => schedule(external, () => {
          if (!interactive) throw error;
          blocked = false;
          return job(undefined);
        }));
      }
      function publish() {
        while (!blocked && visible.length) {
          signal.throwIfAborted();
          const event = visible.shift();
          if (event.type === 'approval') {
            resume(host.approval(event, signal), reply => {
              justUnlocked = true;
              return call('confirm', {now:performance.now(), session, agent:event.agent, token:event.token, ...reply});
            }, true);
          } else if (event.type === 'elicitation') {
            const id = event.id;
            pending.add(id);
            if (typeof onEvent !== 'function' || stateless) {
              let value;
              try { value = onEvent?.(event); } catch { /* Decline a failed callback. */ }
              resume(value, reply => {
                pending.delete(id);
                try {
                  const action = typeof reply === 'string' ? reply : reply === true ? 'accept' : reply?.action;
                  return call('answerElicitation', {session, id, action, content: answerContent(reply?.content)});
                } catch { return call('answerElicitation', {session, id, action:'decline'}); }
              }, true);
            } else emit(event);
          } else emit(event);
        }
      }
      batch = {wake:notify, answer(reply) {
        const { id, action, content } = reply ?? {};
        if (typeof id !== 'string' || !id) throw new Error('respondToElicitation: id is required');
        if (!pending.has(id) || signal.aborted) throw new Error('no pending elicitation with that ID');
        // Snapshot and validate now; the session reply becomes runnable only when Go is idle.
        const args = {session, id, action: typeof action === 'string' ? action : '', content: answerContent(content)};
        call('validateElicitation', args);
        pending.delete(id);
        schedule(external, () => call('answerElicitation', args));
      }};
      try {
        for (;;) {
          let atomic;
          for (const event of next.events) {
            signal.throwIfAborted();
            if (event.type === 'host_call') {
              visible.push({type:'tool_call', id:event.id, name:event.name, args:event.args});
              publish();
              const result = host.execute(event, signal);
              const done = reply => call('hostResult', {session, agent:event.agent, token:event.token, ...reply});
              if (event.network) resume(result, done, false);
              else atomic = done(result);
            } else visible.push(event);
          }
          publish();
          if (!next.waiting) return {...next, events:[]};
          if (atomic) { next = atomic; continue; }
          signal.throwIfAborted();
          // Unlock wakes one mutex waiter, not the entire line of approvals.
          const poll = (next.unlocked && justUnlocked) || (!ready.length && (next.unlocked || next.logged));
          justUnlocked = false;
          if (poll) next = call('pollBatch', { session });
          else if (ready.length) next = ready.shift()();
          else {
            if (!external.length) await new Promise(resolve => { wake = resolve; });
            signal.throwIfAborted();
            next = external.shift()();
          }
        }
      } finally { live = false; batch = null; owner.abort(); }
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
                date: addDate ? dynamicPrompt({ add_date: true }, {timezoneOffset})[0] : undefined,
              };
            }
            request = call('prepare', { session, ...roundContext });
          }
          let outcome;
          try {
            const { transport } = handle.profiles[request.agent].targets[request.target];
            const response = await modelFetch(transport, request, signal, sessionID);
            let calls = false;
            for await (const chunk of sse(response, request.responses)) {
              signal.throwIfAborted();
              if (chunk === chatEOF) {
                if (calls) throw new Error('tool stream ended before [DONE]');
                break;
              }
              const step = call('chunk', { session, chunk, now: performance.now() });
              if (step.request || step.retry) { outcome = step; break; }
              for (const event of step.events) {
                signal.throwIfAborted();
                if (!runError) emit(event);
              }
              calls = step.calls;
              if (step.terminal && !calls) break;
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
            next = await (next.serial ? serialBatch : driveBatch)(next, signal);
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
        if (active) throw new Error('embeddedchat: a run is already active');
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
        if (!host.respond(reply)) {
          if (!batch) throw new Error('no pending elicitation with that ID');
          batch.answer(reply);
        }
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
      if (!Array.isArray(messages)) throw new Error('chat: parsing messages: messages must be an array');
      const last = messages.at(-1);
      if (last?.role !== 'user') throw new Error('chat: the last message must be from the user');
      const parts = last.multi_content;
      if (parts != null && (!Array.isArray(parts) || parts.length)) {
        throw new Error('the final user prompt is text-only; attachments belong in prior history');
      }
      const s = session(opts, onEvent, messages.slice(0, -1));
      currentChat?.abort();
      currentChat = s;
      try { return await s.send(last.content ?? ''); } // Go decodes a null or missing content as empty text.
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

// Go ignores response content that cannot round-trip to map[string]any.
function answerContent(content) {
  if (!content || typeof content !== 'object') return undefined;
  try {
    const raw = JSON.stringify(content);
    return raw?.startsWith('{') ? raw : undefined;
  } catch { return undefined; }
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
  const query = new URLSearchParams(target.query).toString();
  if (query) url.search = query.replaceAll('%7E', '~').replaceAll('*', '%2A');
  target.endpoint = url.href;
  target.opencode = url.hostname === 'opencode.ai' || url.hostname.endsWith('.opencode.ai');
  // Browser-visible headers of the reference wasm SDK (versions pinned in go.mod).
  Object.assign(target.headers, { Accept: 'application/json', 'Content-Type': 'application/json',
    'X-Stainless-Arch': 'other:wasm', 'X-Stainless-OS': 'Other:js', 'X-Stainless-Lang': 'go',
    'X-Stainless-Runtime': 'go', 'X-Stainless-Runtime-Version': 'go1.27.4', 'X-Stainless-Package-Version': '3.61.0' });
  if (target.chatgpt) target.headers.session_id = crypto.randomUUID();
}

class TransportError extends Error {
  constructor(reason, endpoint, status) { super(reason); this.failure = { reason, endpoint, status }; }
}

// OpenCode hashes the conversation ID in its own UUIDv5 namespace.
async function openCodeID(sessionID) {
  const bytes = Uint8Array.of(0x6f, 0x0c, 0x2a, 0x1e, 0x8d, 0x4b, 0x4c, 0x7f,
    0x9a, 0x3e, 0x2b, 0x5d, 0x7e, 0x9f, 0x1c, 0x03, ...encoder.encode(sessionID));
  const digest = new Uint8Array(await crypto.subtle.digest('SHA-1', bytes), 0, 16);
  digest[6] = (digest[6] & 15) | 80;
  digest[8] = (digest[8] & 63) | 128;
  const hex = Array.from(digest, b => b.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0,8)}-${hex.slice(8,12)}-${hex.slice(12,16)}-${hex.slice(16,20)}-${hex.slice(20)}`;
}

async function modelFetch({ endpoint, headers, opencode }, { body, summary }, signal, sessionID) {
  const init = {
    method: 'POST', redirect: 'error', credentials: 'omit', signal,
    headers: { ...headers },
    body: JSON.stringify(body),
  };
  if (summary && headers.session_id) init.headers.session_id = crypto.randomUUID();
  if (opencode) init.headers['X-OpenCode-Session'] = await openCodeID(summary ? crypto.randomUUID() : sessionID);
  for (let attempt = 0; ; attempt++) {
    init.headers['X-Stainless-Retry-Count'] = String(attempt);
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

const chatEOF = Symbol('Chat Completions EOF');

async function* sse(response, responses) {
  if (!response.body || !/^text\/event-stream(?:;|$)/i.test(response.headers.get('Content-Type') ?? '')) {
    await response.body?.cancel();
    throw new Error('expected a text/event-stream model response');
  }
  const reader = response.body.getReader();
  const utf8 = new TextDecoder('utf-8', { fatal: true, ignoreBOM: !responses });
  let pending = '', data = [], total = 0, toolCompletion, thread = false, hasData = false;
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
        if (line.startsWith('data:')) {
          hasData = true;
          data.push(line.slice(5).replace(/^ /, ''));
        } else if (!responses && line === 'data') {
          // Go's transport requires a data: line, but the SDK also reads bare data fields in that frame.
          data.push('');
        } else if (!responses && (line === 'event' || line.startsWith('event:'))) {
          thread = line.slice(6).replace(/^ /, '').startsWith('thread.');
        } else if (!line) {
          const wrapped = thread, record = hasData ? data.join('\n') : undefined;
          thread = hasData = false;
          data = [];
          if (record === undefined) continue;
          if (record === '[DONE]') return;
          if (!responses) {
            if (wrapped) {
              // The SDK wraps thread.* data before decoding, except for top-level errors.
              let event;
              try { event = JSON.parse(record); } catch { throw new Error('invalid thread stream JSON'); }
              if (!event || !Object.hasOwn(event, 'error')) continue;
            }
            yield record;
            continue;
          }
          let chunk;
          try { chunk = JSON.parse(record); } catch { throw new Error('invalid Responses stream JSON'); }
          if (!chunk || typeof chunk !== 'object' || Array.isArray(chunk)) throw new Error('invalid model stream event');
          if (toolCompletion && (record === toolCompletion ||
              (chunk.type === 'response.output_text.annotation.added' && !Object.hasOwn(chunk, 'error')))) continue;
          // Go's SDK retains the original JSON of top-level stream errors.
          yield Object.hasOwn(chunk, 'error') ? record : chunk;
          // Go drains tool-call completions: a later stream error must veto dispatch.
          if (chunk.type === 'response.incomplete') return;
          if (['response.completed', 'response.done'].includes(chunk.type)) {
            if (!chunk.response.output?.some(i => i.type === 'function_call')) return;
            toolCompletion = record;
          }
        }
      }
      if (done) {
        if (pending || data.length) throw new Error(responses ? 'incomplete Responses event at EOF' : 'incomplete Chat Completions event at EOF');
        if (!responses) yield chatEOF;
        return;
      }
    }
  } finally {
    try { await reader.cancel(); } finally { reader.releaseLock(); }
  }
}
