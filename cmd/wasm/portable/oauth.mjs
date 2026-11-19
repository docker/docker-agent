// One helper per MCP endpoint per session. Attach accessToken() only to that exact
// endpoint. On 401, authorize(header, signal) replaces the token; retry only once.
// Abort outstanding work and drop this helper on close. Nothing goes into storage.
export function createMCPOAuth({ serverURL, oauthRedirectURI, clientId, clientSecret, scopes,
  toolFetch, toolProxy, allowPrivateIPs = false, elicit }) {
  let token, registration, busy = false;
  const policy = { toolProxy, allowPrivateIPs };

  async function exchange(wwwAuthenticate, signal) {
    signal.throwIfAborted();
    httpsURL(oauthRedirectURI, 'OAuth redirect URI');
    if (clientSecret || (clientId != null && !validID(clientId))) throw new Error('MCP OAuth requires a public client');
    if (typeof toolFetch !== 'function' || typeof elicit !== 'function') throw new Error('MCP OAuth requires guarded toolFetch and URL elicitation');

    async function request(url, init = {}, status = 200, allowMissing = false) {
      signal.throwIfAborted();
      const res = await toolFetch(url, {
        redirect: 'error', credentials: 'omit', ...init,
        headers: { Accept: 'application/json', ...init.headers },
      }, policy, signal);
      if (res.redirected || res.status !== status) {
        await res.body?.cancel();
        if (!res.redirected && allowMissing && res.status === 404) return;
        throw new Error(`MCP OAuth request failed: HTTP ${res.status}`);
      }
      return readJSON(res, signal);
    }
    async function discover(urls) {
      for (const url of new Set(urls)) {
        const doc = await request(url, {}, 200, urls.length > 1);
        if (doc) return doc;
      }
      throw new Error('MCP OAuth metadata not found');
    }

    const challenge = bearerChallenge(wwwAuthenticate);
    const resource = await resourceMetadata(serverURL, challenge.get('resource_metadata'), discover);
    const issuer = resource.authorization_servers[0];
    const as = await authorizationMetadata(issuer, discover);
    const authURL = httpsURL(as.authorization_endpoint, 'authorization endpoint');
    const tokenURL = httpsURL(as.token_endpoint, 'token endpoint');

    const scope = scopeString(scopes) || scopeString(challenge.get('scope')?.split(' ')) || scopeString(resource.scopes_supported);
    let id = clientId;
    if (!id) {
      if (!as.registration_endpoint) throw new Error('MCP OAuth requires a clientId or dynamic client registration');
      const registerURL = httpsURL(as.registration_endpoint, 'registration endpoint');
      if (registration?.issuer !== issuer || registration.scope !== scope) {
        const reg = await request(registerURL.href, { method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ client_name: 'docker-agent', redirect_uris: [oauthRedirectURI],
            grant_types: ['authorization_code'], response_types: ['code'], token_endpoint_auth_method: 'none', ...(scope && { scope }) }),
        }, 201);
        if (!validID(reg.client_id) || reg.client_secret !== undefined || reg.token_endpoint_auth_method !== 'none' ||
            !Array.isArray(reg.redirect_uris) || reg.redirect_uris.length !== 1 || reg.redirect_uris[0] !== oauthRedirectURI) throw new Error('invalid MCP OAuth public client registration');
        registration = { issuer, scope, id: reg.client_id };
      }
      id = registration.id;
    }

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
    const result = await request(tokenURL.href, { method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ grant_type: 'authorization_code', client_id: id, redirect_uri: oauthRedirectURI,
        code, code_verifier: verifier, resource: serverURL }).toString(),
    });
    signal.throwIfAborted();
    if (typeof result.access_token !== 'string' || !/^[A-Za-z0-9._~+/-]+=*$/.test(result.access_token) ||
        typeof result.token_type !== 'string' || result.token_type.toLowerCase() !== 'bearer') throw new Error('invalid MCP OAuth bearer token');
    if (result.expires_in !== undefined && (!Number.isSafeInteger(result.expires_in) || result.expires_in <= 0)) throw new Error('invalid MCP OAuth token lifetime');
    return { value: result.access_token, expires: Date.now() + (result.expires_in ?? Infinity) * 1000 };
  }

  async function authorize(wwwAuthenticate, signal) {
    if (busy) throw new Error('MCP OAuth authorization is already in progress');
    busy = true;
    token = undefined;
    try {
      const result = await exchange(wwwAuthenticate, signal);
      signal.throwIfAborted();
      token = result;
      return token.value;
    } finally { busy = false; }
  }
  return {
    authorize,
    accessToken() {
      if (token && Date.now() >= token.expires) token = undefined;
      return token?.value;
    },
    invalidate() { token = undefined; },
  };
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
    await res.body?.cancel();
    throw new Error('expected MCP OAuth JSON response');
  }
  const reader = res.body.getReader();
  const decoder = new TextDecoder('utf-8', { fatal: true });
  let text = '', bytes = 0;
  try {
    for (;;) {
      signal.throwIfAborted();
      const { value, done } = await reader.read();
      if (done) break;
      bytes += value.byteLength;
      if (bytes > 1024 * 1024) throw new Error('MCP OAuth response too large');
      text += decoder.decode(value, { stream: true });
    }
    let doc;
    try { doc = JSON.parse(text + decoder.decode()); }
    catch { throw new Error('invalid MCP OAuth JSON object'); } // Don't echo token responses.
    if (!doc || typeof doc !== 'object' || Array.isArray(doc)) throw new Error('invalid MCP OAuth JSON object');
    return doc;
  } finally {
    try { await reader.cancel(); } finally { reader.releaseLock(); }
  }
}
