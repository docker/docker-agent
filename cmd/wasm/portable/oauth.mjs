import { chunks, maxBytes } from './legacy-sse.mjs';

const maxGrantWaiters = 32;

// A grant and its pinned public client, issuer and endpoints belong to one MCP session.
export function createMCPOAuth({ serverURL, oauthRedirectURI, clientId, clientSecret, scopes,
  toolFetch, toolProxy, allowPrivateIPs = false, elicit }) {
  const policy = { toolProxy, allowPrivateIPs };
  let binding, token, pending, closed = false;

  async function request(url, signal, init = {}) {
    signal.throwIfAborted();
    try {
      return await toolFetch(url, {
        redirect: 'error', credentials: 'omit', ...init,
        headers: { Accept: 'application/json', ...init.headers },
      }, policy, signal);
    } catch {
      signal.throwIfAborted();
      throw new Error('MCP OAuth transport request failed');
    }
  }

  async function discover(urls, signal) {
    for (const url of new Set(urls)) {
      const res = await request(url, signal);
      if (!res.redirected && res.status === 404 && urls.length > 1) {
        res.body?.cancel().catch(() => {});
        continue;
      }
      return responseJSON(res, signal);
    }
    throw new Error('MCP OAuth metadata not found');
  }

  async function bind(challenge, signal) {
    validateOAuthConfig(serverURL, oauthRedirectURI, clientId, scopes);
    if (clientSecret) throw new Error('MCP OAuth requires a public client');
    if (typeof toolFetch !== 'function' || typeof elicit !== 'function') throw new Error('MCP OAuth requires guarded toolFetch and URL elicitation');
    const lookup = urls => discover(urls, signal);
    const resource = await resourceMetadata(serverURL, challenge.get('resource_metadata'), lookup);
    const issuer = resource.authorization_servers[0];
    const as = await authorizationMetadata(issuer, lookup);
    const authorizationURL = httpsURL(as.authorization_endpoint, 'authorization endpoint').href;
    const tokenURL = httpsURL(as.token_endpoint, 'token endpoint').href;
    const scope = scopeString(scopes) || scopeString(challenge.get('scope')?.split(' ')) || scopeString(resource.scopes_supported);
    let id = clientId;
    if (!id) {
      if (!as.registration_endpoint) throw new Error('MCP OAuth requires a clientId or dynamic client registration');
      const registerURL = httpsURL(as.registration_endpoint, 'registration endpoint');
      const grantTypes = ['authorization_code'];
      if (!as.grant_types_supported || supports(as.grant_types_supported, 'refresh_token')) grantTypes.push('refresh_token');
      const res = await request(registerURL.href, signal, { method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ client_name: 'docker-agent', redirect_uris: [oauthRedirectURI],
          grant_types: grantTypes, response_types: ['code'], token_endpoint_auth_method: 'none', ...(scope && { scope }) }),
      });
      const reg = await responseJSON(res, signal, 201);
      if (!validID(reg.client_id) || reg.client_secret !== undefined || reg.token_endpoint_auth_method !== 'none' ||
          !Array.isArray(reg.redirect_uris) || reg.redirect_uris.length !== 1 || reg.redirect_uris[0] !== oauthRedirectURI) throw new Error('invalid MCP OAuth public client registration');
      id = reg.client_id;
    }
    return { id, scope, issuer, authorizationURL, tokenURL, as, resource };
  }

  async function grant(params, signal) {
    const res = await request(binding.tokenURL, signal, { method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ ...params, client_id: binding.id, resource: serverURL }).toString(),
    });
    if (!res.redirected && res.status === 400 && params.grant_type === 'refresh_token') {
      if ((await readJSON(res, signal)).error === 'invalid_grant') return null;
      throw new Error('MCP OAuth refresh failed: HTTP 400');
    }
    const received = Date.now();
    const result = await responseJSON(res, signal);
    if ('error' in result || typeof result.access_token !== 'string' || !/^[A-Za-z0-9._~+/-]+=*$/.test(result.access_token) ||
        typeof result.token_type !== 'string' || result.token_type.toLowerCase() !== 'bearer') throw new Error('invalid MCP OAuth bearer token');
    if (result.refresh_token !== undefined && (typeof result.refresh_token !== 'string' || !/^[\x20-\x7e]+$/.test(result.refresh_token))) {
      throw new Error('invalid MCP OAuth refresh token');
    }
    if (result.expires_in !== undefined && (!Number.isSafeInteger(result.expires_in) || result.expires_in <= 0)) throw new Error('invalid MCP OAuth token lifetime');
    const lifetime = (result.expires_in ?? Infinity) * 1000;
    const expires = received + lifetime;
    if (Date.now() >= expires) throw new Error('MCP OAuth token already expired');
    return { value: result.access_token, refresh: result.refresh_token ?? params.refresh_token, expires,
      refreshAt: expires - Math.min(30000, lifetime / 2) };
  }

  async function exchange(signal) {
    const { authorizationURL, id, scope, issuer, as, resource } = binding;
    const authURL = new URL(authorizationURL);
    const verifier = randomValue(), state = randomValue();
    const challengeBytes = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(verifier));
    authURL.search = new URLSearchParams({ response_type: 'code', client_id: id, redirect_uri: oauthRedirectURI,
      state, code_challenge_method: 'S256', code_challenge: base64url(new Uint8Array(challengeBytes)), resource: serverURL, ...(scope && { scope }) });
    signal.throwIfAborted();
    const reply = await elicit({ type: 'elicitation', id: crypto.randomUUID(), mode: 'url', url: authURL.href,
      message: `OAuth authorization required for ${serverURL}`, server_elicitation_id: '', schema: null,
      meta: { 'docker-agent/type': 'oauth_flow', 'docker-agent/server_url': serverURL, 'docker-agent/authorize_url': authURL.href,
        'docker-agent/state': state, auth_server: issuer, auth_server_metadata: as, resource_metadata: resource },
    }, signal);
    signal.throwIfAborted();
    const code = callbackCode(reply, oauthRedirectURI, state, issuer);
    return grant({ grant_type: 'authorization_code', redirect_uri: oauthRedirectURI, code, code_verifier: verifier }, signal);
  }

  async function renew(challenge, signal) {
    signal.throwIfAborted();
    if (!binding) {
      const discovered = await bind(challenge, signal);
      signal.throwIfAborted();
      binding = discovered;
    }
    let next;
    if (token?.refresh) {
      next = await grant({ grant_type: 'refresh_token', refresh_token: token.refresh }, signal);
      signal.throwIfAborted();
      if (!next) token = undefined; // Explicit invalid_grant, not a transport or parsing failure.
    }
    if (!next) next = await exchange(signal);
    signal.throwIfAborted();
    token = next;
    return token.value;
  }

  async function acquire(challenge, signal) {
    signal.throwIfAborted();
    if (closed) throw new Error('MCP OAuth session is closed');
    if (!pending) {
      pending = { controller: new AbortController(), waiters: new Set() };
      const flight = pending;
      const notify = (method, result) => {
        pending = undefined;
        for (const waiter of flight.waiters) waiter[method](result);
      };
      Promise.resolve().then(() => renew(challenge, flight.controller.signal)).then(
        value => notify('resolve', value), error => notify('reject', error),
      );
    }
    const flight = pending;
    if (flight.controller.signal.aborted) throw new Error('MCP OAuth cancellation in progress');
    if (flight.waiters.size >= maxGrantWaiters) throw new Error('too many concurrent MCP OAuth requests');
    const waiter = Promise.withResolvers();
    flight.waiters.add(waiter);
    const waiting = AbortSignal.any([signal, flight.controller.signal]);
    const abort = () => {
      waiter.reject(waiting.reason);
      flight.waiters.delete(waiter);
      if (!flight.waiters.size) flight.controller.abort();
    };
    waiting.addEventListener('abort', abort, { once: true });
    try {
      const value = await waiter.promise;
      waiting.throwIfAborted();
      return value;
    } finally { waiting.removeEventListener('abort', abort); }
  }

  function accessToken() { return token && Date.now() < token.expires ? token.value : undefined; }
  return {
    accessToken,
    async beforeRequest(signal) {
      signal.throwIfAborted();
      if (closed) throw new Error('MCP OAuth session is closed');
      const value = accessToken();
      if (pending || (binding && (!value || (token.refresh && Date.now() >= token.refreshAt)))) return acquire(null, signal);
      return value;
    },
    async authorize(wwwAuthenticate, signal, rejectedToken) {
      signal.throwIfAborted();
      const challenge = bearerChallenge(wwwAuthenticate);
      if (rejectedToken && challenge.get('error') !== 'invalid_token') throw new Error('MCP authorization failed (HTTP 401); bearer was not rejected as invalid_token');
      // A slower 401 may arrive after a concurrent request already replaced this token.
      const current = accessToken();
      if (current && current !== rejectedToken) return current;
      if (token) token.value = undefined;
      return acquire(challenge, signal);
    },
    close() {
      closed = true;
      pending?.controller.abort();
      binding = token = undefined;
    },
  };
}

async function responseJSON(res, signal, status = 200) {
  if (res.redirected || res.status !== status) {
    res.body?.cancel().catch(() => {});
    throw new Error(`MCP OAuth request failed: HTTP ${res.status}`);
  }
  return readJSON(res, signal);
}

export function validateOAuthConfig(serverURL, redirectURI, clientId, scopes) {
  httpsURL(serverURL, 'MCP server');
  httpsURL(redirectURI, 'OAuth redirect URI');
  if (clientId != null && !validID(clientId)) throw new Error('MCP OAuth requires a public client');
  scopeString(scopes);
}

async function resourceMetadata(serverURL, challengedURL, discover) {
  const endpoint = httpsURL(serverURL, 'MCP server');
  const path = endpoint.pathname.replace(/\/$/, '');
  const wellKnown = `${endpoint.origin}/.well-known/oauth-protected-resource`;
  const metadata = await discover(challengedURL === undefined ? [wellKnown + path, wellKnown]
    : [httpsURL(challengedURL, 'resource metadata URL', true).href]);
  if (metadata.resource !== serverURL) throw new Error('MCP OAuth resource identifier mismatch');
  if ((metadata.bearer_methods_supported !== undefined && !supports(metadata.bearer_methods_supported, 'header')) || metadata.dpop_bound_access_tokens_required) {
    throw new Error('MCP OAuth requires header bearer tokens');
  }
  if (!Array.isArray(metadata.authorization_servers) || !metadata.authorization_servers.length) throw new Error('missing MCP OAuth authorization server');
  return metadata;
}

async function authorizationMetadata(issuer, discover) {
  const { origin, pathname } = httpsURL(issuer, 'authorization server issuer');
  const path = pathname.replace(/\/$/, '');
  const as = await discover([
    `${origin}/.well-known/oauth-authorization-server${path}`,
    `${origin}/.well-known/openid-configuration${path}`,
    `${origin}${path}/.well-known/openid-configuration`,
  ]);
  if (as.issuer !== issuer) throw new Error('MCP OAuth metadata issuer mismatch');
  if (!supports(as.response_types_supported, 'code') || !supports(as.code_challenge_methods_supported, 'S256') ||
      (as.grant_types_supported !== undefined && !supports(as.grant_types_supported, 'authorization_code')) ||
      (as.response_modes_supported !== undefined && !supports(as.response_modes_supported, 'query')) || as.require_pushed_authorization_requests) {
    throw new Error('MCP OAuth requires authorization code with S256 PKCE and query response mode');
  }
  if (!supports(as.token_endpoint_auth_methods_supported, 'none')) throw new Error('MCP OAuth requires public client authentication method none');
  // One host callback serves dynamically discovered issuers; prevent AS mix-up (RFC 9207).
  if (as.authorization_response_iss_parameter_supported !== true) throw new Error('MCP OAuth requires authorization response issuer identification (RFC 9207)');
  return as;
}

// Only the host observes the real callback. It must verify its source/origin and
// return {action:'accept', content:{callback_url: <full observed URL>}}, not a
// reconstructed URL, bare {code,state}, or client-supplied tokens.
function callbackCode(reply, redirect, state, issuer) {
  if (reply?.action !== 'accept') throw new Error('MCP OAuth authorization was not accepted');
  const raw = reply.content?.callback_url;
  if (typeof raw !== 'string' || raw.length > 65536 || !raw.startsWith(redirect + '?') || /[#\s\\\x00-\x1f\x7f]/.test(raw)) {
    throw new Error('MCP OAuth callback URL must match the exact redirect URI');
  }
  const params = new URL(raw).searchParams;
  for (const [name, expected] of [['state', state], ['iss', issuer]]) {
    if (params.getAll(name).length !== 1 || params.get(name) !== expected) throw new Error(`MCP OAuth callback ${name} mismatch`);
  }
  if (params.has('error') || params.has('access_token') || params.has('id_token') || params.getAll('code').length !== 1 || !params.get('code')) {
    throw new Error('MCP OAuth callback did not return one authorization code');
  }
  return params.get('code');
}

function httpsURL(raw, label, allowQuery = false) {
  // Don't let WHATWG repair whitespace, backslashes, missing slashes or a fragment.
  const authority = typeof raw === 'string' && /^https:\/\/([^/?#]+)/i.exec(raw)?.[1];
  if (!authority || authority.includes('@') || /[\s\\#\x00-\x1f\x7f]/.test(raw) || (!allowQuery && raw.includes('?')) || !URL.canParse(raw)) {
    throw new Error(`invalid ${label}: expected an absolute HTTPS URL without userinfo or fragment${allowQuery ? '' : ' or query'}`);
  }
  return new URL(raw);
}

function supports(list, value) { return Array.isArray(list) && list.includes(value); }
function validID(id) { return typeof id === 'string' && /^[\x21-\x7e]+$/.test(id); }

function scopeString(values) {
  if (values === undefined) return '';
  if (!Array.isArray(values) || !values.every(s => typeof s === 'string' && /^[\x21\x23-\x5b\x5d-\x7e]+$/.test(s))) throw new Error('invalid MCP OAuth scope list');
  return [...new Set(values)].join(' ');
}

function randomValue() { return base64url(crypto.getRandomValues(new Uint8Array(32))); }
function base64url(bytes) { return btoa(String.fromCharCode(...bytes)).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', ''); }

// Parse only a single Bearer challenge. Ambiguous/mixed challenges must not
// let another scheme's parameters select where to fetch metadata.
function bearerChallenge(header) {
  const params = new Map();
  if (!header) return params;
  if (typeof header !== 'string' || header.length > 16384 || /[\x00-\x08\x0a-\x1f\x7f]/.test(header) || !/^Bearer(?:[ \t]|$)/i.test(header)) throw new Error('unsupported MCP OAuth challenge');
  let rest = header.slice(6).trim();
  while (rest) {
    const m = /^([!#$%&'*+.^_`|~0-9A-Za-z-]+)[ \t]*=[ \t]*(?:"((?:[^"\\]|\\.)*)"|([!#$%&'*+.^_`|~0-9A-Za-z-]+))[ \t]*(,|$)/.exec(rest);
    if (!m) throw new Error('invalid or mixed MCP OAuth challenge');
    const key = m[1].toLowerCase();
    if (params.has(key)) throw new Error('duplicate MCP OAuth challenge parameter');
    params.set(key, m[2]?.replace(/\\(.)/g, '$1') ?? m[3]);
    rest = rest.slice(m[0].length).trimStart();
  }
  return params;
}

async function readJSON(res, signal) {
  if (!/^application\/json(?:\s*;|$)/i.test(res.headers.get('Content-Type') ?? '') || !res.body) {
    res.body?.cancel().catch(() => {});
    throw new Error('expected MCP OAuth JSON response');
  }
  const decoder = new TextDecoder('utf-8', { fatal: true });
  let text = '', bytes = 0;
  for await (const chunk of chunks(res.body, signal)) {
    bytes += chunk.byteLength;
    if (bytes > maxBytes) throw new Error('MCP OAuth response too large');
    text += decoder.decode(chunk, { stream: true });
  }
  let doc;
  try { doc = JSON.parse(text + decoder.decode()); }
  catch { throw new Error('invalid MCP OAuth JSON object'); } // Don't echo token responses.
  if (!doc || typeof doc !== 'object' || Array.isArray(doc)) throw new Error('invalid MCP OAuth JSON object');
  return doc;
}
