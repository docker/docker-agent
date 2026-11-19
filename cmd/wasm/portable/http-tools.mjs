import { convertHTML } from './http-html.mjs';

// Browser fetch has no dial hook. Only the operator's proxy can enforce the IP
// policy after DNS resolution. See pkg/httpclient/egress.go for its protocol.
const relayHeader = 'X-Docker-Agent-Location';
const redirects = new Set([301, 302, 303, 307, 308]);
const maxBody = 1 << 20;
const encoder = new TextEncoder();
const requestMethods = new Set(['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS']);
const userAgent = 'Cagent/dev (js; wasm)';

function fields(value, allowed, where) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(`${where} must be an object`);
  for (const key of Object.keys(value)) {
    if (!allowed.includes(key)) throw new Error(`unsupported ${where} option: ${key}`);
  }
}

function webURL(raw) {
  if (raw instanceof URL) raw = raw.href;
  // URL alone accepts e.g. https:///missing-host and repairs backslashes.
  if (typeof raw !== 'string' || !/^https?:\/\/[^/?#\\]/i.test(raw) || /[\\\x00-\x1f\x7f]/.test(raw)) {
    throw new Error('expected an absolute HTTP or HTTPS URL');
  }
  let url;
  try { url = new URL(raw); } catch { throw new Error('invalid HTTP(S) URL'); }
  if (url.username || url.password || /^https?:\/\/[^/?#]*@/i.test(raw)) {
    throw new Error('URL must not contain userinfo');
  }
  return url;
}

export function proxyURL(raw) {
  if (raw === undefined || raw === null || raw === '') return null;
  const url = webURL(raw);
  if (url.protocol !== 'https:' || url.href.includes('#') || url.searchParams.has('url') || /%(?![0-9a-f]{2})/i.test(url.href)) {
    throw new Error('toolProxy must use https without a fragment or reserved url parameter');
  }
  return url;
}

const forbiddenHeaders = new Set([
  'accept-charset', 'accept-encoding', 'connection', 'content-length', 'cookie', 'cookie2', 'date', 'dnt', 'expect',
  'forwarded', 'host', 'keep-alive', 'origin', 'permissions-policy', 'referer', 'set-cookie', 'te', 'trailer',
  'transfer-encoding', 'upgrade', 'via', 'x-http-method', 'x-http-method-override', 'x-method-override',
]);

export function checkedHeaders(init) {
  let headers;
  try { headers = new Headers(init); } catch { throw new Error('invalid HTTP header'); }
  // Browsers own these headers; Node fetch and fetch mocks need not police them.
  for (const key of headers.keys()) {
    if (forbiddenHeaders.has(key) || /^(proxy-|sec-|access-control-request-|x-forwarded-)/.test(key)) {
      throw new Error(`unsupported request header: ${key}`);
    }
  }
  return headers;
}

function policyOf(options) {
  fields(options, ['toolProxy', 'allowPrivateIPs'], 'egress');
  // Inherited properties can never opt an operator out of SSRF protection.
  const { toolProxy, allowPrivateIPs = false } = { __proto__: null, ...options };
  if (typeof allowPrivateIPs !== 'boolean') throw new Error('allow_private_ips must be a boolean');
  const proxy = proxyURL(toolProxy);
  return { proxy: allowPrivateIPs ? null : proxy, allowPrivateIPs };
}

export async function toolFetch(url, init = {}, policy = {}, signal) {
  return request(url, init, policyOf(policy), signal);
}

async function request(raw, init, { proxy, allowPrivateIPs, checkDomain }, signal) {
  if (!proxy && !allowPrivateIPs) {
    throw new Error('SSRF-guarded HTTP is not supported in the browser: configure a trusted HTTPS egress proxy (toolProxy) or opt in with allow_private_ips: true');
  }
  fields(init, ['method', 'body', 'headers', 'signal', 'credentials', 'redirect'], 'request');
  if (init.credentials !== undefined && init.credentials !== 'omit') throw new Error('requests must omit credentials');
  if (init.redirect !== undefined && init.redirect !== 'error') throw new Error('automatic redirects are not supported');
  if (init.signal) signal = signal ? AbortSignal.any([signal, init.signal]) : init.signal;
  let { method = 'GET', body } = init;
  let headers = checkedHeaders(init.headers), target = webURL(raw);
  if (typeof method === 'string') method = method.toUpperCase();
  if (!requestMethods.has(method)) throw new Error('unsupported request method');
  if (body != null && (method === 'GET' || method === 'HEAD' || !(typeof body === 'string' || body instanceof Uint8Array || body instanceof ArrayBuffer))) {
    throw new Error('expected a replayable request body on a non-GET/HEAD method');
  }
  for (let hop = 1; ; hop++) {
    signal?.throwIfAborted();
    target.hash = '';
    checkDomain?.(target);
    let destination = target;
    if (proxy) {
      destination = new URL(proxy);
      destination.searchParams.set('url', target.href);
    }
    const res = await fetch(destination.href, {
      method, body, headers, signal, credentials: 'omit', redirect: 'error', mode: 'cors',
      cache: 'no-store', referrerPolicy: 'no-referrer',
    });
    if (res.redirected || res.type === 'opaqueredirect' || res.type === 'opaque') {
      await res.body?.cancel();
      throw new Error('unexpected redirected or opaque HTTP response');
    }
    const redirect = redirects.has(res.status);
    const location = proxy && redirect && res.headers.get(relayHeader);
    if ((redirect && res.headers.has('Location')) || location) {
      await res.body?.cancel();
      if (res.headers.has('Location') || init.redirect === 'error') throw new Error('request failed: unexpected HTTP redirect');
      if (hop >= 10) throw new Error('stopped after 10 redirects');
      let next;
      try { next = webURL(new URL(location, target)); } catch { throw new Error('invalid HTTP(S) redirect target'); }
      if (next.origin !== target.origin) headers = new Headers(); // Never restore credentials, even on A → B → A.
      if (res.status !== 307 && res.status !== 308) {
        if (method !== 'HEAD') method = 'GET';
        body = undefined;
        headers.delete('Content-Type');
      }
      target = next;
      continue;
    }
    if (!proxy) return res;
    // Preserve streaming (including SSE), without exposing proxy metadata to callers.
    const responseHeaders = new Headers(res.headers);
    responseHeaders.delete(relayHeader);
    const response = new Response(res.body, { status: res.status, statusText: res.statusText, headers: responseHeaders });
    return Object.defineProperties(response, { url: { value: target.href }, redirected: { value: hop > 1 } });
  }
}

// Match Go small: substitute only top-level identifiers, never evaluate code or
// expand a reference inside an unrecognized expression (including quoted braces).
function expand(text, env, args = {}) {
  let out = '', from = 0, pos;
  while ((pos = text.indexOf('${', from)) !== -1) {
    let end = pos + 2, depth = 1, quote;
    for (; end < text.length && depth; end++) {
      const c = text[end];
      if (quote) {
        if (c === '\\') end++;
        else if (c === quote) quote = undefined;
      } else if ('"\'`'.includes(c)) quote = c;
      else if (c === '{') depth++;
      else if (c === '}') depth--;
    }
    if (depth) break;
    const ref = /^[\t\n\f\r ]*(env\.)?([a-zA-Z_]\w*)[\t\n\f\r ]*$/.exec(text.slice(pos + 2, end - 1));
    let value = text.slice(pos, end);
    if (ref?.[1]) value = Object.hasOwn(env, ref[2]) ? env[ref[2]] : '';
    else if (ref && Object.hasOwn(args, ref[2])) value = args[ref[2]] ?? '';
    out += text.slice(from, pos) + value;
    from = end;
  }
  return out + text.slice(from);
}

function toolHeaders(config = {}) {
  if (!config || typeof config !== 'object' || Array.isArray(config) || Object.values(config).some(v => typeof v !== 'string')) {
    throw new Error('headers must be a map of strings');
  }
  checkedHeaders(config);
  const entries = Object.entries(config);
  return (env, accept) => {
    const headers = new Headers({ 'User-Agent': userAgent, 'X-Docker-Agent-Version': 'dev' });
    if (accept) headers.set('Accept', accept);
    for (const [key, value] of entries) {
      try { headers.set(key, expand(value, env)); } catch { throw new Error(`invalid value for header: ${key}`); }
    }
    return headers;
  };
}

function deadline(seconds, parent) {
  const timeout = AbortSignal.timeout(seconds * 1000);
  return parent ? AbortSignal.any([timeout, parent]) : timeout;
}

async function readBytes(response, limit) {
  if (!response.body) return new Uint8Array();
  const reader = response.body.getReader(), chunks = [];
  let size = 0, reads = 0;
  try {
    while (size < limit) {
      const { done, value } = await reader.read();
      if (done) break;
      if (++reads > 65536) throw new Error('response chunk count limit reached');
      if (!value.length) continue;
      const chunk = value.subarray(0, limit - size);
      chunks.push(chunk);
      size += chunk.length;
    }
  } finally {
    try { await reader.cancel(); } finally { reader.releaseLock(); }
  }
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.length; }
  return bytes;
}

function normalizedHost(host) {
  const mapped = /^\[::ffff:([\da-f]+):([\da-f]+)\]$/.exec(host);
  if (!mapped) return host.replace(/\.$/, '');
  const high = parseInt(mapped[1], 16), low = parseInt(mapped[2], 16);
  return [high >> 8, high & 255, low >> 8, low & 255].join('.');
}

function domainPatterns(domains = []) {
  if (!Array.isArray(domains)) throw new Error('expected a list of domain patterns');
  return domains.map(pattern => {
    if (typeof pattern !== 'string') throw new Error('invalid domain pattern');
    const p = pattern.trim().toLowerCase(), strict = p.startsWith('.') || p.startsWith('*.');
    let host = p.replace(/^(?:\*\.|\.)/, '');
    if (!host || /[\s/?#@%*\\]/.test(host)) throw new Error('invalid or unsupported domain pattern (CIDRs are not supported)');
    if (host.includes(':') && !host.startsWith('[')) host = `[${host}]`;
    const url = webURL(`http://${host}`);
    if (url.port || host.endsWith('..')) throw new Error('invalid domain pattern');
    const name = normalizedHost(url.hostname);
    return candidate => (!strict && candidate === name) || candidate.endsWith('.' + name);
  });
}

// temoto/robotstxt ranks a wildcard by its *compiled* Go regexp length,
// and searches anywhere in the decoded path (literal rules are prefixes).
function robotsRule(value, allow) {
  if (!/^[/*]/.test(value)) value = '/' + value;
  let n = value.length;
  while (value[n - 1] === '*') n--;
  value = value.slice(0, n);
  const wildcard = /[*$]/.test(value);
  const end = value.endsWith('$');
  const pattern = end ? value.slice(0, -1) : value;
  if (pattern.includes('$')) throw new Error('unsupported interior $ in robots.txt rule');
  const length = encoder.encode(value).length;
  if (!wildcard) return { weight: length, allow, cost: 0, matches: path => value === '/' || path.startsWith(value) };
  const parts = pattern.split('*').filter(Boolean);
  const anchoredLast = end && !pattern.endsWith('*');
  const weight = length + (value.match(/[\\.+*?()|[\]{}^]/g) ?? []).length;
  function matches(path) {
    if (path.includes('\n')) throw new Error('unsupported newline in robots.txt wildcard match');
    let from = 0;
    for (const [i, part] of parts.entries()) {
      if (anchoredLast && i === parts.length - 1) return path.length - part.length >= from && path.endsWith(part);
      const pos = path.indexOf(part, from);
      if (pos < 0) return false;
      from = pos + part.length;
    }
    return true;
  }
  return { weight, allow, cost: value.length, matches };
}

function robotsRules(text) {
  const agentName = userAgent.toLowerCase();
  let specificity = -1, inRules = false, best = -1, rules = [], hasGroup = false;
  // A missing value at EOF has no token; before a newline Go consumes the EOL token.
  const lines = text.split(/[\r\n]/);
  for (const [i, raw] of lines.entries()) {
    const line = raw.replace(/^[ \t\v]+/, '');
    if (!line || line.startsWith('#')) continue;
    const m = /^([\w-]+):(.*)$/s.exec(line);
    if (!m) throw new Error('unsupported robots.txt directive');
    // A # inside a token is literal; at the start of a token it starts a comment.
    let [value, extra] = m[2].replace(/^[ \t\v]+/, '').split(/[ \t\v]+/, 2);
    if (value.startsWith('#')) value = '';
    else if (extra && !extra.startsWith('#')) throw new Error('unsupported robots.txt directive');
    if (!value && i === lines.length - 1) break;
    value ||= '\n';
    const key = m[1].toLowerCase();
    if (key === 'user-agent' || key === 'useragent') {
      if (inRules) specificity = -1;
      // In Go the wildcard beats a one-letter prefix, but no longer prefix.
      if (value === '*') specificity = Math.max(specificity, 1);
      else if (agentName.startsWith(value.toLowerCase())) specificity = Math.max(specificity, value.length === 1 ? 0 : value.length);
      hasGroup = true;
      inRules = false;
    } else if (['allow', 'disallow', 'crawl-delay', 'crawldelay'].includes(key)) {
      if (!hasGroup) throw new Error('robots.txt rule before user-agent');
      if (specificity > best) { best = specificity; rules = []; }
      inRules = true;
      if (key === 'allow' || key === 'disallow') {
        if (specificity >= 0 && specificity === best) rules.push(robotsRule(value, key === 'allow'));
      } else if (!/^\d+(?:\.\d+)?$/.test(value) || !Number.isFinite(Number(value))) throw new Error('unsupported robots.txt crawl-delay');
    }
  }
  return path => {
    let longest = 0, allow = true, work = 0;
    for (const rule of rules) {
      if (rule.weight <= longest) continue; // Go keeps the first match on a tie.
      work += rule.cost * path.length;
      if (work > (8 << 20)) throw new Error('robots.txt wildcard match limit reached');
      if (rule.matches(path)) { longest = rule.weight; allow = rule.allow; }
    }
    return allow;
  };
}

async function loadRobots(url, headers, policy, signal) {
  const res = await request(new URL('/robots.txt', url), { headers }, policy, signal);
  if (res.status !== 200) {
    await res.body?.cancel();
    if (res.status === 404) return () => true;
    throw new Error(`robots.txt check failed: unexpected status ${res.status}`);
  }
  const limit = 64 * 1024, bytes = await readBytes(res, limit + 1);
  if (bytes.length > limit) throw new Error('robots.txt too large');
  return robotsRules(new TextDecoder('utf-8', { fatal: true }).decode(bytes));
}

const accepts = {
  text: 'text/plain;q=1.0, text/markdown;q=0.9, text/html;q=0.8, */*;q=0.1',
  markdown: 'text/markdown;q=1.0, text/plain;q=0.9, text/html;q=0.7, */*;q=0.1',
  html: 'text/html;q=1.0, text/plain;q=0.8, */*;q=0.1',
};
const fetchParameters = {
  type: 'object', additionalProperties: false, required: ['urls', 'format', 'timeout'],
  properties: {
    urls: { type: 'array', description: 'URLs to fetch', items: { type: 'string' }, minItems: 1 },
    format: { type: 'string', description: 'Output format', enum: ['text', 'markdown', 'html'] },
    timeout: { type: ['integer', 'null'], description: 'Request timeout in seconds (default: 30)', minimum: 1, maximum: 300 },
  },
};

function fetchTool(ts, env, policy, seconds) {
  const allow = domainPatterns(ts.allowed_domains), block = domainPatterns(ts.blocked_domains);
  if (allow.length && block.length) throw new Error('allowed_domains and blocked_domains are mutually exclusive');
  if ('escape_html' in ts && typeof ts.escape_html !== 'boolean') throw new Error('escape_html must be a boolean');
  const escape = ts.escape_html;
  const makeHeaders = toolHeaders(ts.headers);
  policy.checkDomain = url => {
    const host = normalizedHost(url.hostname);
    if (block.some(matches => matches(host))) throw new Error(`URL host "${host}" is blocked by blocked_domains`);
    if (allow.length && !allow.some(matches => matches(host))) throw new Error(`URL host "${host}" is not in allowed_domains`);
  };
  let instructions = '## Fetch Tool\n\nFetch HTTP/HTTPS URLs; respects robots.txt.';
  if (allow.length) instructions += `\n\nThis tool is restricted to these domains (and any subdomain): ${ts.allowed_domains.join(', ')}. Other hosts are rejected without a network call.`;
  if (block.length) instructions += `\n\nThis tool must not fetch these domains (or any subdomain): ${ts.blocked_domains.join(', ')}. They are rejected without a network call.`;
  return {
    name: 'fetch', description: 'Fetch HTTP/HTTPS URLs; return response bodies and metadata.',
    parameters: structuredClone(fetchParameters), instructions,
    async run(args, signal) {
      fields(args, ['urls', 'format', 'timeout'], 'fetch argument');
      const { urls, format, timeout } = args;
      if (!Array.isArray(urls) || urls.length === 0 || urls.some(url => typeof url !== 'string')) throw new Error('at least one URL is required');
      if (urls.length > 8) throw new Error('portable fetch supports at most 8 URLs per call');
      if (format != null && (typeof format !== 'string' || !Object.hasOwn(accepts, format))) throw new Error('unsupported fetch format');
      if (timeout != null && (!Number.isInteger(timeout) || timeout < 1 || timeout > 300)) throw new Error('fetch timeout must be 1–300 seconds');
      const timeoutSeconds = timeout ?? seconds;
      const robots = new Map(), results = [];
      const robotsHeaders = makeHeaders(env);
      const headers = makeHeaders(env, accepts[format] ?? 'text/plain;q=1.0, */*;q=0.1');
      for (const raw of urls) {
        signal?.throwIfAborted();
        const r = { url: raw, statusCode: 0, status: '', contentType: undefined, contentLength: 0 };
        try {
          if (raw.length > 16384) throw new Error('portable fetch URL too long');
          const url = webURL(raw);
          if (!robots.has(url.origin)) robots.set(url.origin, await loadRobots(url, robotsHeaders, policy, deadline(timeoutSeconds, signal)));
          // WHATWG URL removes dot segments. Check Go's original path as well
          // as the path the browser will actually send. Neither includes query.
          const original = decodeURIComponent(/^https?:\/\/[^/?#]+([^?#]*)/i.exec(raw)[1]);
          const path = decodeURIComponent(url.pathname), allows = robots.get(url.origin);
          if (!allows(original) || (path !== original && !allows(path))) throw new Error('URL blocked by robots.txt');
          const res = await request(url, { headers }, policy, deadline(timeoutSeconds, signal));
          r.statusCode = res.status;
          r.status = `${res.status} ${res.statusText}`.trimEnd();
          const type = res.headers.get('Content-Type');
          if (type) r.contentType = type;
          const bytes = await readBytes(res, maxBody);
          let body = new TextDecoder('utf-8', { ignoreBOM: true }).decode(bytes);
          if (type?.includes('text/html') && (format === 'text' || format === 'markdown')) {
            body = convertHTML(body, format);
            const length = encoder.encode(body).length;
            if (length > maxBody) throw new Error('converted HTML exceeds portable size limit');
            r.contentLength = length;
          } else r.contentLength = bytes.length;
          if (body) r.body = body;
        } catch (err) {
          signal?.throwIfAborted();
          r.error = err.message;
        }
        results.push(r);
      }
      if (results.length === 1) {
        const [r] = results;
        if (r.error) throw new Error(`Error fetching ${r.url}: ${r.error}`);
        return `Successfully fetched ${r.url} (Status: ${r.statusCode}, Length: ${r.contentLength} bytes):\n\n${r.body ?? ''}`;
      }
      const json = JSON.stringify(results);
      return escape ? json.replace(/[<>&]/g, c => `\\u${c.charCodeAt(0).toString(16).padStart(4, '0')}`) : json;
    },
  };
}

// Go sorts valid UTF-8 map keys by bytes, not UTF-16 code units.
function compareKeys(a, b) {
  for (let i = 0; i < a.length && i < b.length;) {
    const x = a.codePointAt(i), y = b.codePointAt(i);
    if (x !== y) return x - y;
    i += x > 0xffff ? 2 : 1;
  }
  return a.length - b.length;
}

// Keep the API schema subset small and explicit rather than silently dropping
// constraints while translating optional properties for OpenAI strict mode.
function apiParameters(args, required, method) {
  let nodes = 0;
  function schema(prop, depth) {
    if (++nodes > 512 || depth > 16) throw new Error('portable API schema size or depth limit reached');
    const type = prop?.type;
    if (method === 'GET' && depth === 1 && type !== 'string') throw new Error('GET arguments must be strings');
    const keys = ['type', 'description'];
    if (type === 'array') keys.push('items');
    else if (type === 'object') keys.push('properties', 'required', 'additionalProperties');
    else keys.push('enum');
    fields(prop, keys, 'api_config argument schema');
    if ('description' in prop && typeof prop.description !== 'string') throw new Error('invalid argument description');
    if (type === 'array') return { ...prop, items: schema(prop.items, depth + 1) };
    if (type === 'object') {
      const { properties, required = [] } = prop;
      if (!properties || typeof properties !== 'object' || Array.isArray(properties) || ('additionalProperties' in prop && prop.additionalProperties !== false)) {
        throw new Error('portable API objects require named properties');
      }
      if (!Array.isArray(required) || required.length > 512 || required.some(k => typeof k !== 'string' || !Object.hasOwn(properties, k))) throw new Error('invalid api_config.required');
      const names = Object.keys(properties), strict = Object.create(null);
      if (names.length > 512) throw new Error('portable API schema size limit reached');
      if (names.some(name => !name.isWellFormed())) throw new Error('invalid Unicode API argument name');
      names.sort(compareKeys);
      for (const name of names) {
        const child = schema(properties[name], depth + 1);
        if (required.includes(name)) strict[name] = child;
        else if (child.enum || ['array', 'object'].includes(child.type)) strict[name] = { anyOf: [child, { type: 'null' }] };
        else strict[name] = { ...child, type: [child.type, 'null'] };
      }
      return { ...prop, properties: strict, required: names, additionalProperties: false };
    }
    if (!['string', 'integer', 'number', 'boolean'].includes(type)) throw new Error('unsupported portable API argument type');
    if ('enum' in prop && (!Array.isArray(prop.enum) || !prop.enum.length || prop.enum.length > 512 || prop.enum.some(v => !scalarMatches(type, v)))) throw new Error('invalid argument enum');
    return structuredClone(prop);
  }
  return schema({ type: 'object', properties: args, required }, 0);
}

// Only POST encodes the validated values. A JSON.stringify key-list would
// filter nested keys too; encode each object's own keys in Go's sorted order.
function apiArguments(values, args, required, post) {
  const parts = [];
  let nodes = 0, size = 0, inputChars = 0;
  function write(s) {
    if ((size += s.length) > maxBody) throw new Error('API argument size limit reached');
    parts.push(s);
  }
  function visit(value, prop, key, optional = false) {
    if (++nodes > 16384) throw new Error('API argument size limit reached');
    if (value === null && optional) { if (post) write('null'); return; }
    if (prop.type === 'object') {
      if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error(`invalid argument: ${key}`);
      const required = prop.required ?? [];
      for (const name of required) if (!Object.hasOwn(value, name) || value[name] == null) throw new Error(`missing required argument: ${name}`);
      const names = Object.keys(value);
      if (names.length > 512) throw new Error('API argument size limit reached');
      if (post) { write('{'); names.sort(compareKeys); }
      for (const [i, name] of names.entries()) {
        if (!Object.hasOwn(prop.properties, name)) throw new Error(`unsupported API argument option: ${name}`);
        if (post) write((i ? ',' : '') + JSON.stringify(name) + ':');
        visit(value[name], prop.properties[name], name, !required.includes(name));
      }
      if (post) write('}');
    } else if (prop.type === 'array') {
      if (!Array.isArray(value)) throw new Error(`invalid array argument: ${key}`);
      if (post) write('[');
      for (const [i, item] of value.entries()) {
        if (post && i) write(',');
        visit(item, prop.items, key);
      }
      if (post) write(']');
    } else {
      if (!scalarMatches(prop.type, value) || (prop.enum && !prop.enum.includes(value))) throw new Error(`invalid argument: ${key}`);
      if (typeof value === 'string' && (inputChars += value.length) > maxBody) throw new Error('API argument size limit reached');
      if (post) {
        const scalar = typeof value === 'string' ? value.toWellFormed() : value;
        write(Object.is(scalar, -0) ? '-0' : JSON.stringify(scalar));
      }
    }
  }
  visit(values, { type: 'object', properties: args, required }, 'API arguments');
  if (post) {
    const json = parts.join('').replace(/[<>&\u2028\u2029]/g, c => `\\u${c.charCodeAt(0).toString(16).padStart(4, '0')}`);
    if (encoder.encode(json).length > maxBody) throw new Error('API request body too large');
    return json;
  }
}

function scalarMatches(type, value) {
  return type === 'integer' ? Number.isSafeInteger(value) : type === 'number' ? Number.isFinite(value) : typeof value === type;
}

function apiTool(config, env, policy, seconds) {
  fields(config, ['name', 'method', 'endpoint', 'instruction', 'headers', 'args', 'required'], 'api_config');
  const { name, method, endpoint, instruction = '', args = {}, required = [] } = config;
  if (typeof name !== 'string' || !/^[\w-]{1,64}$/.test(name)) throw new Error('invalid api_config.name');
  if (!requestMethods.has(method)) throw new Error('unsupported api_config.method');
  if (typeof instruction !== 'string') throw new Error('invalid api_config.instruction');
  if (typeof endpoint !== 'string' || !/^https?:\/\//i.test(endpoint)) throw new Error('expected absolute HTTP(S) api_config.endpoint');
  const authority = endpoint.slice(endpoint.indexOf('://') + 3).split(/[/?#]/, 1)[0];
  if (authority.includes('${')) throw new Error('api_config.endpoint must have a fixed origin');
  const origin = webURL(endpoint).origin;
  const parameters = apiParameters(args, required, method), makeHeaders = toolHeaders(config.headers);
  return {
    name, description: instruction, instructions: instruction, parameters,
    async run(values, signal) {
      const body = apiArguments(values, args, required, method === 'POST');
      let url = expand(endpoint, env);
      const headers = makeHeaders(env);
      // Like Go, other verbs have neither argument interpolation nor a JSON body.
      if (method === 'GET') url = expand(url, env, values);
      else if (method === 'POST') headers.set('Content-Type', 'application/json');
      const target = webURL(url);
      if (target.origin !== origin) throw new Error('endpoint expansion must not change origin');
      const res = await request(target, { method, headers, body }, policy, deadline(seconds, signal));
      const limit = 30000, bytes = await readBytes(res, limit + 1);
      const text = new TextDecoder('utf-8', { ignoreBOM: true }).decode(bytes.subarray(0, limit))
        + (bytes.length > limit ? '\n\n[Output truncated: exceeded 30,000 character limit]' : '');
      if (res.status >= 400) throw new Error(`API request failed with status ${res.status} ${res.statusText}: ${text}`);
      return text;
    },
  };
}

// Pass only handler-specific fields here: the session owns approvals, tool
// filtering and dispatch. Unknown options must not silently weaken policy.
export function httpTools(toolset, env = {}, toolProxy) {
  const common = ['type', 'allow_private_ips', 'timeout'];
  let options;
  switch (toolset?.type) {
    case 'api': options = ['api_config']; break;
    case 'fetch': options = ['headers', 'allowed_domains', 'blocked_domains', 'escape_html']; break;
    default: throw new Error('expected a fetch or api toolset');
  }
  fields(toolset, [...common, ...options], 'toolset');
  const ts = structuredClone(toolset), seconds = ts.timeout ?? 30;
  if (!Number.isInteger(seconds) || seconds < 1 || seconds * 1000 > 0x7fffffff) throw new Error('invalid toolset.timeout');
  const policy = policyOf({ toolProxy, allowPrivateIPs: Object.hasOwn(ts, 'allow_private_ips') && ts.allow_private_ips });
  const sessionEnv = { ...env };
  if (Object.values(sessionEnv).some(v => typeof v !== 'string')) throw new Error('session env values must be strings');
  const tool = ts.type === 'api' ? apiTool(ts.api_config, sessionEnv, policy, seconds) : fetchTool(ts, sessionEnv, policy, seconds);
  const { name, description, parameters } = tool;
  return {
    tools: [{ type: 'function', function: { name, description, parameters } }], instructions: tool.instructions,
    async call(called, rawArgs, signal) {
      try {
        signal?.throwIfAborted();
        if (called !== name) throw new Error('unknown HTTP tool');
        if (ts.type === 'api' && typeof rawArgs === 'string' && rawArgs.length > maxBody) throw new Error('API argument size limit reached');
        const args = typeof rawArgs === 'string' ? JSON.parse(rawArgs || '{}') : rawArgs;
        return { output: await tool.run(args ?? {}, signal), is_error: false };
      } catch (e) { return { output: e.message ?? 'request failed', is_error: true }; }
    },
  };
}
