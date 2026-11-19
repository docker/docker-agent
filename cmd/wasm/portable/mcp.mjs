import { createMCPOAuth, validateOAuthConfig } from './oauth.mjs';
import { checkedHeaders } from './http-tools.mjs';
import { chunks, legacySSE, maxBytes, sseEvents } from './legacy-sse.mjs';

const versions = ['2025-11-25', '2025-06-18', '2025-03-26'];
const maxTools = 256;

function object(v) { return v !== null && typeof v === 'object' && !Array.isArray(v); }

function options(value, keys, where) {
  if (!object(value)) throw new Error(`${where} must be an object`);
  for (const key of Object.keys(value)) {
    if (!keys.includes(key)) throw new Error(`unsupported ${where} option: ${key}`);
  }
}

function expand(text, env) {
  if (typeof text !== 'string') throw new Error('MCP URL and headers must be strings');
  return text.replace(/\$\{([^}]*)\}|\$\{/g, (_, expr) => {
    const key = /^\s*env\.([a-zA-Z_]\w*)\s*$/.exec(expr)?.[1];
    if (!key) throw new Error('only ${env.NAME} expansion is supported in MCP URLs and headers');
    if (!Object.hasOwn(env, key) || typeof env[key] !== 'string') throw new Error(`missing MCP env value: ${key}`);
    return env[key];
  });
}

function connection(toolset, env, toolProxy) {
  options(toolset, ['type', 'remote', 'name', 'allow_private_ips'], 'MCP toolset');
  if (toolset.type !== 'mcp') throw new Error('expected a remote MCP toolset');
  const { remote, name = '', allow_private_ips: allowPrivateIPs = false } = toolset;
  options(remote, ['url', 'transport_type', 'headers', 'standalone_sse', 'oauth'], 'MCP remote');
  if (Object.hasOwn(remote, 'oauth')) options(remote.oauth, ['clientId','scopes','callbackRedirectURL'], 'MCP OAuth');
  if (!['sse', 'streamable', 'streamable-http'].includes(remote.transport_type ?? 'streamable') || (remote.standalone_sse ?? false) !== false) {
    throw new Error('unsupported MCP transport or standalone SSE');
  }
  if (typeof name !== 'string' || typeof allowPrivateIPs !== 'boolean') throw new Error('invalid MCP name or allow_private_ips');
  if (!toolProxy && !allowPrivateIPs) throw new Error('MCP requires a trusted toolProxy or allow_private_ips: true');
  const url = expand(remote.url, env);
  const authority = /^https?:\/\/([^/?#]+)/i.exec(url)?.[1];
  if (!authority || authority.includes('@') || /[\\\x00-\x20\x7f#]/.test(url) || !URL.canParse(url)) {
    throw new Error('MCP requires an absolute HTTP(S) URL without credentials or fragment');
  }
  if (!object(remote.headers ?? {})) throw new Error('MCP remote.headers must be a map');
  const templates = Object.entries(remote.headers ?? {});
  function requestHeaders() {
    const headers = new Headers({ Accept: 'application/json, text/event-stream', 'Content-Type': 'application/json' });
    for (const [key, template] of templates) {
      if (/^mcp-|^last-event-id$/i.test(key) || headers.has(key)) throw new Error('duplicate or reserved MCP header');
      const value = expand(template, env);
      if (/[\r\n\0]/.test(value)) throw new Error('invalid MCP header value');
      try { headers.set(key, value); } catch { throw new Error('invalid MCP header'); }
    }
    return headers;
  }
  return { url, requestHeaders, policy: { toolProxy, allowPrivateIPs }, prefix: name ? `${name}_` : '' };
}

async function bounded(signals, ms, run) {
  const timer = new AbortController();
  const signal = AbortSignal.any([...signals.filter(Boolean), timer.signal]);
  signal.throwIfAborted();
  const id = setTimeout(() => timer.abort(new DOMException('MCP request timed out', 'TimeoutError')), ms);
  try {
    const result = await run(signal);
    signal.throwIfAborted();
    return result;
  } finally { clearTimeout(id); }
}

function parseJSON(text) {
  try { return JSON.parse(text); } catch { throw new Error('invalid MCP JSON'); }
}

async function* records(response, budget, signal) {
  const type = response.headers.get('Content-Type')?.split(';', 1)[0].trim().toLowerCase();
  const sse = type === 'text/event-stream';
  if (!response.body || (!sse && type !== 'application/json')) throw new Error('expected an MCP JSON or event-stream response');
  let size = 0;
  const source = chunks(response.body, signal, n => {
    size += n;
    budget.left -= n;
    if (size > maxBytes || budget.left < 0) throw new Error('MCP response too large');
  });
  if (sse) {
    for await (const { event, data } of sseEvents(source)) {
      if (event === 'message' && data) yield parseJSON(data);
    }
  } else {
    const decoder = new TextDecoder('utf-8', { fatal: true });
    let text = '';
    for await (const chunk of source) text += decoder.decode(chunk, { stream: true });
    yield parseJSON(text + decoder.decode());
  }
}

function rpcResult(message, reply) {
  if (reply.id !== message.id || ('error' in reply) === ('result' in reply)) throw new Error('invalid or mismatched MCP response');
  if ('error' in reply) {
    const code = reply.error?.code;
    if (!Number.isInteger(code)) throw new Error('invalid MCP server error');
    if (code === -32042) throw new Error('MCP out-of-band URL elicitation is not supported');
    throw new Error(`MCP ${message.method} failed (JSON-RPC ${code})`);
  }
  if (!object(reply.result)) throw new Error('invalid MCP result');
  return reply.result;
}

function output(result) {
  if (!Array.isArray(result.content) || ('isError' in result && typeof result.isError !== 'boolean')) throw new Error('invalid MCP tool result');
  const parts = [];
  for (const block of result.content) {
    let part;
    if (block?.type === 'text') part = block.text;
    else if (block?.type === 'resource') part = block.resource?.text;
    else if (block?.type === 'resource_link' && typeof block.uri === 'string') {
      part = block.name ? `[${block.name.replaceAll(']', '\\]')}](${block.uri.replaceAll(')', '%29')})` : block.uri;
    }
    if (typeof part !== 'string') throw new Error('unsupported non-text MCP result content');
    if (!part) continue;
    if (parts.length && !/\s$/.test(parts.at(-1)) && !/^\s/.test(part)) parts.push('\n');
    parts.push(part);
  }
  if (result.structuredContent !== undefined && !object(result.structuredContent)) throw new Error('invalid MCP structuredContent');
  return { output: parts.join('') || JSON.stringify(result.structuredContent) || 'no output', is_error: result.isError ?? false };
}

export function prepareMCP(toolset, env, toolProxy, opts = {}) {
  options(opts, ['toolFetch', 'elicit', 'oauthRedirectURI'], 'MCP host');
  const { toolFetch, oauthRedirectURI } = opts;
  if (typeof toolFetch !== 'function') throw new Error('MCP requires an injected guarded toolFetch');
  if (toolset.remote?.oauth != null && !oauthRedirectURI) throw new Error('MCP OAuth requires an explicit host oauthRedirectURI');
  if (oauthRedirectURI && (typeof opts.elicit !== 'function' || (toolset.remote.oauth?.callbackRedirectURL && toolset.remote.oauth.callbackRedirectURL !== oauthRedirectURI))) throw new Error('MCP OAuth requires a matching host redirect URI and elicitation handler');
  const config = connection(toolset, env ?? {}, toolProxy);
  const isLegacy = toolset.remote.transport_type === 'sse';
  const oauthConfig = toolset.remote.oauth;
  if (isLegacy && oauthConfig) throw new Error('MCP OAuth is not supported with legacy SSE');
  const headers = checkedHeaders(config.requestHeaders());
  if (oauthConfig && headers.has('Authorization')) throw new Error('MCP OAuth conflicts with an explicit Authorization header');
  const useOAuth = !isLegacy && oauthRedirectURI && !headers.has('Authorization');
  if (useOAuth) validateOAuthConfig(config.url, oauthRedirectURI, oauthConfig?.clientId, oauthConfig?.scopes);
  return signal => {
    const oauth = useOAuth ? createMCPOAuth({
      serverURL: config.url, oauthRedirectURI, ...config.policy, toolFetch, elicit: opts.elicit,
      clientId: oauthConfig?.clientId, scopes: oauthConfig?.scopes,
    }) : null;
    return connect(config, opts, isLegacy, oauth, signal);
  };
}

export async function openMCP(toolset, env, toolProxy, signal, opts) {
  return prepareMCP(toolset, env, toolProxy, opts)(signal);
}

async function connect({ url, requestHeaders, policy, prefix }, opts, isLegacy, oauth, signal) {
  const { toolFetch } = opts;
  const lifetime = new AbortController(), names = new Map();
  let sessionID, version, closing, nextID = 1;

  async function request(target, method, message, signal) {
    signal.throwIfAborted();
    const headers = requestHeaders();
    const token = oauth?.accessToken();
    if (token) headers.set('Authorization', `Bearer ${token}`);
    if (sessionID) headers.set('MCP-Session-Id', sessionID);
    if (!isLegacy && version) headers.set('MCP-Protocol-Version', version);
    if (method === 'GET') {
      headers.set('Accept', 'text/event-stream');
      headers.delete('Content-Type');
    }
    const init = { method, headers, credentials: 'omit', redirect: 'error' };
    if (message) {
      init.body = JSON.stringify({ jsonrpc: '2.0', ...message });
      if (init.body.length > maxBytes) throw new Error('MCP request too large');
    }
    // A transport error may embed the URL (including query credentials).
    let res;
    try { res = await toolFetch(target, init, policy, signal); }
    catch { signal.throwIfAborted(); throw new Error('MCP transport request failed'); }
    if (res.redirected) {
      res.body?.cancel().catch(() => {});
      throw new Error('unexpected MCP redirect');
    }
    return res;
  }

  function validateMessage(reply) {
    if (!object(reply) || reply.jsonrpc !== '2.0') throw new Error('invalid MCP JSON-RPC message');
    if ('method' in reply) {
      if (typeof reply.method !== 'string' || 'result' in reply || 'error' in reply) throw new Error('invalid MCP server message');
      if ('params' in reply && !object(reply.params)) throw new Error('invalid MCP server parameters');
      if (reply.method === 'notifications/tools/list_changed' && !('id' in reply)) {
        lifetime.abort(new Error('MCP tool catalog changed; reopen the session'));
        lifetime.signal.throwIfAborted();
      }
    }
    return reply;
  }

  const legacy = isLegacy && legacySSE({ url, request, lifetime, parseMessage(data) {
    const reply = validateMessage(parseJSON(data));
    if (!('method' in reply)) return reply;
    if ('id' in reply) throw new Error('server requests are not supported with MCP legacy SSE');
  } });

  async function exchange(message, signal, budget, retriedAuth = false) {
    signal.throwIfAborted();
    if (legacy) {
      const reply = await legacy.exchange(message, signal, budget);
      if (reply) return rpcResult(message, reply);
      return;
    }
    const res = await request(url, message ? 'POST' : 'DELETE', message, signal);
    try {
      if (res.status === 401 && oauth && message && !retriedAuth) {
        // Retry only an explicit authentication rejection, never a transport failure.
        const challenge = res.headers.get('WWW-Authenticate');
        await res.body?.cancel();
        await oauth.authorize(challenge, signal);
        return exchange(message, signal, budget, true);
      }
      if (res.status === 401 || res.status === 403) throw new Error(`MCP authorization failed (HTTP ${res.status}); OAuth was unavailable or rejected`);
      if (!message && [404, 405].includes(res.status)) return; // Gone, or the server owns expiry.
      if (sessionID && res.status === 404) {
        sessionID = undefined;
        lifetime.abort(new Error('MCP session expired'));
      }
      if (!res.ok) throw new Error(`MCP request failed: HTTP ${res.status}`);
      const id = res.headers.get('MCP-Session-Id');
      if (message?.method === 'initialize' && id !== null) {
        if (!/^[!-~]{1,1024}$/.test(id)) throw new Error('invalid MCP session ID');
        sessionID = id;
      } else if (id !== null && id !== sessionID) throw new Error('unexpected MCP session ID');
      signal.throwIfAborted();
      if (!message) return;
      if (!message.method || message.id === undefined) {
        if (res.status !== 202) throw new Error('expected MCP acknowledgement (HTTP 202)');
        return;
      }
      let requests = 0;
      for await (const reply of records(res, budget, signal)) {
        signal.throwIfAborted();
        validateMessage(reply);
        if ('method' in reply) {
          if (!('id' in reply)) continue;
          if (++requests > 16 || !(typeof reply.id === 'string' || Number.isSafeInteger(reply.id))) throw new Error('invalid or excessive MCP server requests');
          let answer = reply.method === 'ping' ? { result: {} } : { error: { code: -32601, message: 'Method not supported' } };
          if (reply.method === 'elicitation/create' && typeof opts.elicit === 'function') {
            const p = reply.params;
            if (!object(p) || typeof p.message !== 'string' || !['form','url'].includes(p.mode ?? 'form')) throw new Error('invalid MCP elicitation');
            if (p.mode === 'url' && (typeof p.url !== 'string' || !/^https:\/\//.test(p.url) || !URL.canParse(p.url))) throw new Error('invalid MCP elicitation URL');
            const r = await opts.elicit({ message:p.message, mode:p.mode ?? 'form', schema:p.requestedSchema ?? null, url:p.url ?? '', meta:p._meta ?? null, server_elicitation_id:p.elicitationId ?? '' },signal);
            answer = { result: {action:r.action, ...(r.action === 'accept' && r.content ? {content:r.content} : {})} };
          }
          await exchange({ id: reply.id, ...answer }, signal, budget);
          continue;
        }
        return rpcResult(message, reply);
      }
      throw new Error('MCP stream ended without a matching response');
    } finally { res.body?.cancel().catch(() => {}); }
  }

  function close() {
    if (!closing) {
      closing = Promise.resolve().then(() => {
        if (legacy) return legacy.closed();
        if (sessionID) return bounded([], 2500, s => exchange(null, s));
      });
      lifetime.abort(new Error('MCP session is closed'));
    }
    return closing;
  }

  async function call(name, rawArgs, parent) {
    parent?.throwIfAborted();
    lifetime.signal.throwIfAborted();
    try {
      if (!names.has(name)) throw new Error('unknown MCP tool');
      if (typeof rawArgs === 'string' && rawArgs.length > maxBytes) throw new Error('MCP arguments too large');
      const args = typeof rawArgs === 'string' ? parseJSON(rawArgs || '{}') : rawArgs;
      if (!object(args)) throw new Error('MCP arguments must be an object');
      // Go omits top-level null optional arguments, not nested nulls.
      const params = { name: names.get(name), arguments: Object.fromEntries(Object.entries(args).filter(([, v]) => v !== null)) };
      const result = await bounded([parent, lifetime.signal], 30000, s => exchange({ id: nextID++, method: 'tools/call', params }, s, { left: maxBytes }));
      return output(result);
    } catch (e) {
      parent?.throwIfAborted();
      lifetime.signal.throwIfAborted();
      return { output: e.message, is_error: true };
    }
  }

  try {
    return await bounded([signal, lifetime.signal], 30000, async s => {
      if (legacy) await legacy.connect(s);
      const budget = { left: 2 * maxBytes };
      const rpc = (method, params) => exchange({ id: nextID++, method, params }, s, budget);
      const hello = await rpc('initialize', { protocolVersion: versions[0], clientInfo: { name: 'docker agent', version: '1.0.0' }, capabilities: !isLegacy && opts.elicit ? { elicitation: {form:{},url:{}} } : {} });
      if (!versions.includes(hello.protocolVersion) && !(isLegacy && hello.protocolVersion === '2024-11-05')) throw new Error('unsupported MCP protocol version');
      version = hello.protocolVersion;
      const { capabilities, instructions = '', serverInfo } = hello;
      if (!object(capabilities) || typeof serverInfo?.name !== 'string' || typeof serverInfo.version !== 'string' || typeof instructions !== 'string') {
        throw new Error('invalid MCP initialize result');
      }
      await exchange({ method: 'notifications/initialized' }, s, budget);
      const tools = [], seen = new Set();
      let cursor;
      if (!Object.hasOwn(capabilities, 'tools')) return { tools, instructions, call, close };
      if (!object(capabilities.tools)) throw new Error('invalid MCP tools capability');
      do {
        const page = await rpc('tools/list', cursor ? { cursor } : {});
        if (!Array.isArray(page.tools) || tools.length + page.tools.length > maxTools) throw new Error('invalid or excessive MCP tool list');
        for (const t of page.tools) {
          if (typeof t?.name !== 'string' || !t.name) throw new Error('invalid MCP tool name');
          const name = prefix + t.name;
          if (!/^[\w.-]{1,128}$/.test(name) || names.has(name)) throw new Error('invalid or duplicate MCP tool name');
          const description = t.description ?? '';
          if (typeof description !== 'string' || !object(t.inputSchema) || t.inputSchema.type !== 'object') throw new Error('invalid MCP tool definition');
          if (t.execution?.taskSupport === 'required') throw new Error('MCP task-augmented execution is not supported');
          names.set(name, t.name);
          tools.push({ type: 'function', function: { name, description, parameters: t.inputSchema } });
        }
        cursor = page.nextCursor;
        if (cursor != null && (typeof cursor !== 'string' || seen.has(cursor) || seen.size >= 32)) throw new Error('invalid or excessive MCP pagination');
        if (cursor) seen.add(cursor);
      } while (cursor);
      return { tools, instructions, call, close };
    });
  } catch (e) {
    await close().catch(() => {});
    throw e;
  }
}
