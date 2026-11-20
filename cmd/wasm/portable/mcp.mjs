import { createMCPOAuth, validateOAuthConfig } from './oauth.mjs';
import { compareKeys, decode, escaped, goQuote } from './go-json.mjs';
import { mcpParameters } from './mcp-schema.mjs';
import { checkedHeaders } from './http-tools.mjs';
import { chunks, legacySSE, maxBytes, sseEvents } from './legacy-sse.mjs';

const versions = ['2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
const statelessVersion = '2026-07-28';
const clientInfo = { name: 'docker agent', version: '1.0.0' };
const anyMap = ['map[string]interface {}', []];
const maxTools = 256;
const callError = 'Error calling tool: failed to call tool: calling "tools/call": ';
const publicRpcMessages = new Set(['', 'Invalid params', 'Invalid request', 'Method not found', 'Internal error', 'Parse error']);
// Use status codes, never a reason phrase or diagnostic supplied by the peer.
const httpReasons = new Map([[400, 'Bad Request'], [404, 'Not Found'], [405, 'Method Not Allowed'],
  [408, 'Request Timeout'], [409, 'Conflict'], [413, 'Request Entity Too Large'], [415, 'Unsupported Media Type'],
  [422, 'Unprocessable Entity'], [429, 'Too Many Requests'], [500, 'Internal Server Error'], [501, 'Not Implemented'],
  [502, 'Bad Gateway'], [503, 'Service Unavailable'], [504, 'Gateway Timeout']]);

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
    if (size > maxBytes || budget.left < 0) throw new RangeError('MCP response too large');
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
    // Never propagate a diagnostic that might echo credentials.
    if (message.method === 'tools/call') {
      const diagnostic = reply.error.message ?? '';
      if (publicRpcMessages.has(diagnostic)) throw new Error(callError + diagnostic);
    }
    throw new Error(`MCP ${message.method} failed (JSON-RPC ${code})`);
  }
  if (!object(reply.result)) throw new Error('invalid MCP result');
  return reply.result;
}

function output(result) {
  if ((result.content != null && !Array.isArray(result.content)) || ('isError' in result && typeof result.isError !== 'boolean')) throw new Error('invalid MCP tool result');
  const parts = [];
  for (const block of result.content ?? []) {
    let part;
    if (block?.type === 'text') part = block.text;
    else if (block?.type === 'resource') part = block.resource?.text;
    else if (block?.type === 'resource_link' && typeof block.uri === 'string') {
      part = block.name ? `[${block.name.replaceAll(']', '\\]')}](${block.uri.replaceAll(')', '%29')})` : block.uri;
    }
    if (typeof part !== 'string') throw new Error('unsupported non-text MCP result content');
    if (!part) continue;
    if (parts.length && !/\p{White_Space}$/u.test(parts.at(-1)) && !/^\p{White_Space}/u.test(part)) parts.push('\n');
    parts.push(part.toWellFormed());
  }
  return { output: parts.join('') || 'no output', is_error: result.isError ?? false };
}

// SDK structs keep declaration order; arguments and elicitation content are Go maps.
function encodeRPC(value, map = false) {
  if (Array.isArray(value)) return '[' + Array.from(value, v => encodeRPC(v, map) ?? 'null').join(',') + ']';
  if (!object(value)) return Object.is(value, -0) ? '-0' : JSON.stringify(value);
  const keys = Object.keys(value), fields = [];
  if (map) keys.sort(compareKeys);
  for (const key of keys) {
    const v = encodeRPC(value[key], map || key === 'arguments' || key === 'content');
    if (v !== undefined) fields.push(JSON.stringify(key) + ':' + v);
  }
  return '{' + fields.join(',') + '}';
}

async function serverReply({ method, params: p }, elicit, signal) {
  const invalid = message => ({ error: { code: -32602, message } });
  switch (method) {
    case 'ping': return { result: {} };
    case 'roots/list': return { result: { roots: [] } };
    case 'sampling/createMessage': return { error: { code: -31001, message: 'client does not support CreateMessage' } };
    case 'elicitation/create': break;
    default: return { error: { code: -32601, message: `method not found: ${goQuote(method)}` } };
  }
  if (typeof elicit !== 'function') return invalid('client does not support elicitation');
  if (!object(p) || typeof p.message !== 'string' || !['form','url'].includes(p.mode ?? 'form')) throw new Error('invalid MCP elicitation');
  if (p.mode === 'url') {
    if (p.requestedSchema != null) return invalid('requestedSchema must not be set for URL elicitation');
    if (typeof p.url !== 'string' || !/^https:\/\//.test(p.url) || !URL.canParse(p.url)) throw new Error('invalid MCP elicitation URL');
  } else if (p.url) return invalid('URL must not be set for form elicitation');
  const r = await elicit({ message: p.message, mode: p.mode ?? '', schema: p.requestedSchema ?? null,
    url: p.url ?? '', meta: p._meta ?? null, server_elicitation_id: p.elicitationId ?? '' }, signal);
  const result = { action: r.action };
  if (r.action === 'accept' && object(r.content)) {
    // Snapshot host objects through their JSON view (including toJSON), not their private fields.
    const json = JSON.stringify(r.content);
    if (json?.length > maxBytes) throw new Error('MCP request too large');
    const content = json && JSON.parse(json);
    if (object(content) && Object.keys(content).length) result.content = content;
  }
  return { result };
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
  const capabilities = !isLegacy && opts.elicit ? { elicitation: { form: {} } } : {};
  const meta = {
    'io.modelcontextprotocol/clientCapabilities': capabilities,
    'io.modelcontextprotocol/clientInfo': clientInfo,
    'io.modelcontextprotocol/protocolVersion': statelessVersion,
  };

  async function request(target, method, message, signal) {
    signal.throwIfAborted();
    const headers = requestHeaders();
    if (sessionID) headers.set('MCP-Session-Id', sessionID);
    if (!isLegacy && version) headers.set('MCP-Protocol-Version', version);
    if (message?.method === 'server/discover' || (version === statelessVersion && message?.method)) {
      headers.set('MCP-Protocol-Version', statelessVersion);
      headers.set('Mcp-Method', message.method);
      if (message.method === 'tools/call') headers.set('Mcp-Name', message.params.name);
      message = { ...message, params: { _meta: meta, ...message.params } };
    }
    if (isLegacy && method === 'POST') headers.delete('Accept');
    if (method === 'GET') {
      headers.set('Accept', 'text/event-stream');
      headers.delete('Content-Type');
    }
    const init = { method, headers, credentials: 'omit', redirect: 'error' };
    if (message) {
      init.body = encodeRPC({ jsonrpc: '2.0', ...message }).replace(/[\u2028\u2029]/g, escaped);
      if (init.body.length > maxBytes) throw new Error('MCP request too large');
    }
    // DELETE is best effort: never start a grant during teardown.
    const token = oauth && (message ? await oauth.beforeRequest(signal) : oauth.accessToken());
    if (token) headers.set('Authorization', `Bearer ${token}`);
    const res = await send(init);
    if (res.status !== 401 || !oauth || !message) return res;
    res.body?.cancel().catch(() => {});
    const replacement = await oauth.authorize(res.headers.get('WWW-Authenticate'), signal, token);
    const retry = new Headers(headers);
    retry.set('Authorization', `Bearer ${replacement}`);
    return send({ ...init, headers: retry }); // Same serialized RPC and ID, exactly once.

    async function send(init) {
      signal.throwIfAborted();
      let res;
      try { res = await toolFetch(target, init, policy, signal); }
      catch { signal.throwIfAborted(); throw new Error('MCP transport request failed'); }
      if (res.redirected) {
        res.body?.cancel().catch(() => {});
        throw new Error('unexpected MCP redirect');
      }
      return res;
    }
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

  async function discover(signal, budget) {
    // A legacy endpoint may reject or ignore the probe. Only initialize may bind a session ID.
    const msg = { id: nextID++, method: 'server/discover' };
    const res = await request(url, 'POST', msg, signal);
    try {
      if (!res.ok || res.headers.has('MCP-Session-Id')) return;
      for await (const reply of records(res, budget, signal)) {
        if (object(reply) && reply.jsonrpc === '2.0' && !('method' in reply)) return rpcResult(msg, reply);
        break;
      }
    } finally { res.body?.cancel().catch(() => {}); }
  }

  async function exchange(message, signal, budget) {
    signal.throwIfAborted();
    if (legacy) {
      const reply = await legacy.exchange(message, signal, budget);
      if (reply) return rpcResult(message, reply);
      return;
    }
    const res = await request(url, message ? 'POST' : 'DELETE', message, signal);
    try {
      if (res.status === 401 || res.status === 403) throw new Error(`MCP authorization failed (HTTP ${res.status}); OAuth was unavailable or rejected`);
      if (!message && [404, 405].includes(res.status)) return; // Gone, or the server owns expiry.
      if (sessionID && res.status === 404) {
        sessionID = undefined;
        lifetime.abort(new Error('MCP session expired'));
      }
      if (!res.ok) {
        const reason = httpReasons.get(res.status);
        if (message?.method === 'tools/call' && reason) {
          const sending = 'sending "tools/call": ';
          const rejected = [429, 502, 503, 504].includes(res.status) ? 'rejected by transport: ' : '';
          throw new Error(callError + rejected + sending + reason);
        }
        throw new Error(`MCP request failed: HTTP ${res.status}`);
      }
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
          const answer = await serverReply(reply, opts.elicit, signal);
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
      }).finally(() => oauth?.close());
      lifetime.abort(new Error('MCP session is closed'));
    }
    return closing;
  }

  async function call(name, rawArgs, parent) {
    parent?.throwIfAborted();
    lifetime.signal.throwIfAborted();
    try {
      if (!names.has(name)) throw new Error('unknown MCP tool');
      if (typeof rawArgs === 'string') {
        if (rawArgs.length > maxBytes) throw new Error('MCP arguments too large');
        try { decode(rawArgs, anyMap); }
        catch (e) { throw new Error('Error calling tool: failed to parse tool arguments: ' + e.message); }
      }
      const args = typeof rawArgs === 'string' ? parseJSON(rawArgs || '{}') : rawArgs;
      if (args !== null && !object(args)) throw new Error('MCP arguments must be an object');
      // Go omits top-level null optional arguments, not nested nulls.
      const params = { name: names.get(name), arguments: args === null ? null : Object.fromEntries(Object.entries(args).filter(([, v]) => v !== null)) };
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
      const budget = { left: 2 * maxBytes };
      let discovery;
      if (legacy) await legacy.connect(s);
      else if (!oauth) {
        // OAuth discovery/grants remain tied to initialize, never to a speculative probe.
        try { discovery = await bounded([s], 2500, probe => discover(probe, budget)); }
        catch (e) {
          s.throwIfAborted();
          if (e instanceof RangeError) throw e;
        }
      }
      const rpc = (method, params) => exchange({ id: nextID++, method, params }, s, budget);
      let hello;
      if (Array.isArray(discovery?.supportedVersions) && discovery.supportedVersions.includes(statelessVersion)
        && object(discovery.capabilities) && !discovery.capabilities.tools?.listChanged && !discovery.capabilities.prompts?.listChanged) {
        hello = discovery;
        version = statelessVersion;
      } else {
        hello = await rpc('initialize', { clientInfo, protocolVersion: versions[0], capabilities });
        if (!versions.includes(hello.protocolVersion)) throw new Error('unsupported MCP protocol version');
        version = hello.protocolVersion;
        if (typeof hello.serverInfo?.name !== 'string' || typeof hello.serverInfo.version !== 'string') throw new Error('invalid MCP server info');
      }
      const { instructions = '' } = hello;
      if (!object(hello.capabilities) || typeof instructions !== 'string') throw new Error('invalid MCP initialize result');
      if (version !== statelessVersion) await exchange({ method: 'notifications/initialized', params: {} }, s, budget);
      const tools = [], seen = new Set();
      let cursor;
      if (!Object.hasOwn(hello.capabilities, 'tools')) return { tools, instructions, call, close };
      if (!object(hello.capabilities.tools)) throw new Error('invalid MCP tools capability');
      do {
        const page = await rpc('tools/list', cursor ? { cursor } : {});
        if (!Array.isArray(page.tools) || tools.length + page.tools.length > maxTools) throw new Error('invalid or excessive MCP tool list');
        for (const t of page.tools) {
          if (typeof t?.name !== 'string' || !t.name) throw new Error('invalid MCP tool name');
          const name = prefix + t.name;
          if (!/^[\w.-]{1,128}$/.test(name) || names.has(name)) throw new Error('invalid or duplicate MCP tool name');
          const description = t.description ?? '';
          if (typeof description !== 'string') throw new Error('invalid MCP tool definition');
          if (t.execution?.taskSupport === 'required') throw new Error('MCP task-augmented execution is not supported');
          names.set(name, t.name);
          const { parameters, strict } = mcpParameters(t.inputSchema);
          tools.push({ ...(strict && { mcpStrict: true }), type: 'function', function: { name, description, parameters } });
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
