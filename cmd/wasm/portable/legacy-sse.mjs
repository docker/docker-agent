export const maxBytes = 1 << 20;

export async function* chunks(body, signal, consume) {
  const reader = body.getReader();
  const cancel = () => { reader.cancel().catch(() => {}); };
  signal.addEventListener('abort', cancel, { once: true });
  try {
    for (;;) {
      signal.throwIfAborted();
      let chunk;
      try { chunk = await reader.read(); } catch { signal.throwIfAborted(); throw new Error('MCP response stream failed'); }
      signal.throwIfAborted();
      if (chunk.done) break;
      consume?.(chunk.value.byteLength);
      yield chunk.value;
    }
  } finally {
    signal.removeEventListener('abort', cancel);
    cancel();
    reader.releaseLock();
  }
}

// Bound an event, not the lifetime of a persistent stream. CR, LF and UTF-8
// characters may all straddle chunks.
export async function* sseEvents(source) {
  const decoder = new TextDecoder('utf-8', { fatal: true });
  let partial = '', afterCR = false, data = [], event = '', size = 0;
  for await (const bytes of source) {
    let start = 0;
    for (let i = 0; i < bytes.length; i++) {
      if (afterCR) {
        afterCR = false;
        if (bytes[i] === 10) { start = i + 1; continue; }
      }
      if (++size > maxBytes) throw new RangeError('MCP response too large');
      if (bytes[i] !== 13 && bytes[i] !== 10) continue;
      // Decode the delimiter too: an incomplete UTF-8 sequence cannot span lines.
      const line = (partial + decoder.decode(bytes.subarray(start, i + 1), { stream: true })).slice(0, -1);
      start = i + 1; partial = ''; afterCR = bytes[i] === 13;
      if (!line) {
        if (data.length) yield { event: event || 'message', data: data.join('\n'), size };
        data = []; event = ''; size = 0;
      } else if (line === 'data' || line.startsWith('data:')) data.push(line.slice(5).replace(/^ /, ''));
      else if (line.startsWith('event:')) event = line.slice(6).replace(/^ /, '');
    }
    if (start < bytes.length) partial += decoder.decode(bytes.subarray(start), { stream: true });
  }
  decoder.decode();
}

function sessionEndpoint(raw, base) {
  const invalid = 'invalid or cross-origin MCP SSE endpoint';
  if (!raw || /[\\\x00-\x20\x7f#]/.test(raw) || !URL.canParse(raw, base)) throw new Error(invalid);
  // Reject empty userinfo and ambiguous authorities before URL canonicalizes them.
  if (/^(?:[a-z][a-z\d+.-]*:|\/\/)/i.test(raw) && !/^(?:https?:)?\/\/[^/?#@]+(?:[/?]|$)/i.test(raw)) throw new Error(invalid);
  const url = new URL(raw, base);
  if (url.origin !== new URL(base).origin || url.username || url.password) throw new Error(invalid);
  return url.href;
}

export function legacySSE({ url, request, lifetime, parseMessage }) {
  const pending = new Map();
  let endpoint, reading;

  async function receive(events) {
    for await (const { event, data, size } of events) {
      lifetime.signal.throwIfAborted();
      if (event === 'endpoint') throw new Error('MCP SSE endpoint changed');
      if (event !== 'message') continue;
      const reply = parseMessage(data);
      if (!reply) continue;
      const waiter = pending.get(reply.id);
      if (!waiter) throw new Error('unexpected MCP SSE response ID');
      if ((waiter.budget.left -= size) < 0) throw new RangeError('MCP response too large');
      pending.delete(reply.id);
      waiter.resolve(reply);
    }
    throw new Error('MCP SSE stream ended');
  }

  async function connect(signal) {
    const abort = () => lifetime.abort(signal.reason);
    signal.addEventListener('abort', abort, { once: true });
    let res, events;
    try {
      signal.throwIfAborted();
      res = await request(url, 'GET', undefined, lifetime.signal);
      if (res.status !== 200) throw new Error(`MCP SSE connection failed: HTTP ${res.status}`);
      if (!res.body || res.headers.get('Content-Type')?.split(';', 1)[0].trim().toLowerCase() !== 'text/event-stream') {
        throw new Error('expected an MCP event-stream response');
      }
      events = sseEvents(chunks(res.body, lifetime.signal));
      const { value } = await events.next();
      signal.throwIfAborted();
      if (value?.event !== 'endpoint') throw new Error('expected MCP SSE endpoint event');
      endpoint = sessionEndpoint(value.data, url);
      reading = receive(events).catch(e => lifetime.abort(e));
    } catch (e) {
      if (events) await events.return();
      else res?.body?.cancel().catch(() => {});
      throw e;
    } finally { signal.removeEventListener('abort', abort); }
  }

  async function post(message, signal) {
    const res = await request(endpoint, 'POST', message, signal);
    res.body?.cancel().catch(() => {});
    if (res.status !== 202) throw new Error(`expected MCP SSE acknowledgement (HTTP 202), got HTTP ${res.status}`);
  }

  async function exchange(message, signal, budget) {
    signal.throwIfAborted();
    if (message.id === undefined) { await post(message, signal); return; }
    const { promise, resolve, reject } = Promise.withResolvers();
    const abort = () => reject(signal.reason);
    signal.addEventListener('abort', abort, { once: true });
    pending.set(message.id, { budget, resolve });
    try {
      // A reply may arrive on the GET before the POST has been acknowledged.
      const [, reply] = await Promise.all([post(message, signal), promise]);
      return reply;
    } catch (e) {
      // With an uncertain POST outcome, retire this stream rather than replaying.
      lifetime.abort(e);
      throw e;
    } finally {
      pending.delete(message.id);
      signal.removeEventListener('abort', abort);
    }
  }

  return { connect, exchange, closed: () => reading };
}
