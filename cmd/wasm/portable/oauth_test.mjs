import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { test } from 'node:test';
import { createMCPOAuth } from './oauth.mjs';

const serverURL = 'https://mcp.example/v1/mcp', oauthRedirectURI = 'https://app.example/callback';
const prmURL = 'https://mcp.example/.well-known/oauth-protected-resource/v1/mcp';
const rootPRM = 'https://mcp.example/.well-known/oauth-protected-resource';
const challengedURL = 'https://discovery.example/resource?tenant=example';
const issuer = 'https://as.example/tenant';
const asmURL = 'https://as.example/.well-known/oauth-authorization-server/tenant';
const tokenURL = 'https://tokens.example/exchange', regURL = issuer + '/register';
const authEndpoint = 'https://login.example/authorize';
const toolProxy = 'https://egress.example/proxy';
const challenge = String.raw`Bearer error_description="do not parse scope=bogus, or \"quotes\"", resource_metadata="${challengedURL}", scope="read write"`;

function callback(event) {
  const { searchParams } = new URL(event.url);
  const params = new URLSearchParams({ code: 'test & code', state: searchParams.get('state'), iss: issuer });
  return { action: 'accept', content: { callback_url: oauthRedirectURI + '?' + params } };
}

function fixture(options = {}) {
  const resource = { resource: serverURL, authorization_servers: [issuer], scopes_supported: ['read'] };
  const as = { issuer, authorization_endpoint: authEndpoint, token_endpoint: tokenURL, registration_endpoint: regURL,
    code_challenge_methods_supported: ['S256'], response_types_supported: ['code'], grant_types_supported: ['authorization_code', 'refresh_token'],
    authorization_response_iss_parameter_supported: true, token_endpoint_auth_methods_supported: ['none'] };
  const registration = { client_id: 'registered', token_endpoint_auth_method: 'none', redirect_uris: [oauthRedirectURI] };
  const token = { token_type: 'Bearer', access_token: 'temporary', expires_in: 60, refresh_token: 'private +%& token' };
  const requests = [], events = [];
  const routes = new Map([[challengedURL, resource], [prmURL, resource], [asmURL, as], [regURL, registration], [tokenURL, token]]);
  const controller = new AbortController(), signal = controller.signal;
  const oauth = createMCPOAuth({ serverURL, oauthRedirectURI, toolProxy, allowPrivateIPs: false,
    headers: { Authorization: 'server-only', 'X-Private': 'server-only' }, // never propagated
    toolFetch(url, init, policy, s) {
      assert.deepEqual(policy, { toolProxy, allowPrivateIPs: options.allowPrivateIPs ?? false });
      assert.ok(s instanceof AbortSignal);
      s.throwIfAborted();
      assert.equal(init.redirect, 'error');
      assert.equal(init.credentials, 'omit');
      assert.deepEqual(Object.keys(init.headers).sort(), init.body ? ['Accept', 'Content-Type'] : ['Accept']);
      requests.push({ url, init, signal: s });
      assert.ok(routes.has(url), `unexpected request to ${url}`);
      const value = routes.get(url);
      if (typeof value === 'function') return value(init, s);
      return value instanceof Response ? value : Response.json(value, { status: url === regURL ? 201 : 200 });
    },
    elicit(event, s) {
      assert.ok(s instanceof AbortSignal);
      s.throwIfAborted();
      events.push(event);
      return callback(event);
    }, ...options });
  return { oauth, controller, signal, resource, as, registration, token, events, requests, routes,
    run: (h = challenge) => oauth.authorize(h, signal) };
}

test('guarded discovery, public registration, URL elicitation, and one PKCE code exchange bound to the exact resource', async () => {
  const f = fixture();
  assert.equal(f.oauth.accessToken(), undefined);
  assert.equal(await f.run(), 'temporary');
  assert.equal(f.oauth.accessToken(), 'temporary');
  assert.deepEqual(f.requests.map(r => r.url), [challengedURL, asmURL, regURL, tokenURL]);
  const [reg, exchange] = f.requests.slice(-2).map(r => r.init);
  assert.equal(reg.method, 'POST');
  assert.equal(reg.headers['Content-Type'], 'application/json');
  assert.deepEqual(JSON.parse(reg.body), { client_name: 'docker-agent', redirect_uris: [oauthRedirectURI], grant_types: ['authorization_code', 'refresh_token'],
    response_types: ['code'], token_endpoint_auth_method: 'none', scope: 'read write' });
  assert.equal(exchange.headers['Content-Type'], 'application/x-www-form-urlencoded');
  assert.equal(exchange.method, 'POST');
  const { code_verifier, ...form } = Object.fromEntries(new URLSearchParams(exchange.body));
  assert.match(code_verifier, /^[a-zA-Z0-9_-]{43}$/);
  assert.deepEqual(form, { grant_type: 'authorization_code', client_id: 'registered', code: 'test & code', redirect_uri: oauthRedirectURI, resource: serverURL });
  const [event] = f.events, url = new URL(event.url);
  const { state, ...params } = Object.fromEntries(url.searchParams);
  assert.equal(url.origin + url.pathname, authEndpoint);
  assert.match(state, /^[a-zA-Z0-9_-]{43}$/);
  assert.notEqual(state, code_verifier);
  assert.deepEqual(params, { response_type: 'code', client_id: 'registered', redirect_uri: oauthRedirectURI, scope: 'read write', resource: serverURL,
    code_challenge_method: 'S256', code_challenge: createHash('sha256').update(code_verifier).digest('base64url') });
  assert.ok(event.id);
  assert.deepEqual(event, { id: event.id, type: 'elicitation', mode: 'url', url: url.href, message: `OAuth authorization required for ${serverURL}`,
    schema: null, server_elicitation_id: '', meta: { 'docker-agent/type': 'oauth_flow', 'docker-agent/server_url': serverURL,
      'docker-agent/authorize_url': url.href, 'docker-agent/state': state, auth_server: issuer, auth_server_metadata: f.as, resource_metadata: f.resource } });
  assert.ok(!JSON.stringify(event).includes(code_verifier));
  assert.equal(fixture().oauth.accessToken(), undefined);
  f.oauth.close();
  assert.equal(f.oauth.accessToken(), undefined);
  await assert.rejects(f.oauth.beforeRequest(f.signal), /closed/);
});

test('well-known path then root, OIDC discovery with issuer path, configured client and scope precedence', async () => {
  const f = fixture({ clientId: 'configured', scopes: ['custom'], allowPrivateIPs: true });
  const oidc = 'https://as.example/.well-known/openid-configuration/tenant';
  const appended = issuer + '/.well-known/openid-configuration';
  for (const url of [prmURL, asmURL, oidc]) f.routes.set(url, new Response(null, { status: 404 }));
  f.routes.set(rootPRM, f.resource);
  f.routes.set(appended, f.as);
  await f.run('Bearer scope="read", resource="not-a-metadata-url"');
  assert.deepEqual(f.requests.map(r => r.url), [prmURL, rootPRM, asmURL, oidc, appended, tokenURL]);
  const auth = new URL(f.events[0].url);
  assert.equal(auth.searchParams.get('scope'), 'custom');
  assert.equal(auth.searchParams.get('client_id'), 'configured');
  for (const supported of [[], ['read', 'read']]) {
    const f = fixture();
    f.resource.scopes_supported = supported;
    await f.run(null);
    assert.equal(new URL(f.events[0].url).searchParams.get('scope'), supported.length ? 'read' : null);
  }
});

test('explicit metadata failures, malformed or ambiguous challenges, and insecure URLs fail closed', async () => {
  for (const h of ['Basic realm="bearer"', 'Bearer realm="x", Basic resource_metadata="x"', 'Bearer resource_metadata="x',
    `Bearer resource_metadata="${prmURL}", RESOURCE_metadata="${challengedURL}"`, 'Bearer scope=read\n']) {
    const f = fixture();
    await assert.rejects(f.run(h), /challenge/);
    assert.deepEqual(f.requests, []);
  }
  for (const url of ['http://example.test', 'http://localhost', 'https://user@example.test/a', 'https://@example.test/',
    'https://example.test/#', '/relative', 'https:incomplete']) {
    const f = fixture({ allowPrivateIPs: true });
    await assert.rejects(f.run(`Bearer resource_metadata="${url}"`), /metadata URL/);
    assert.deepEqual(f.requests, []);
  }
  for (const [route, status] of [[challengedURL, 404], [prmURL, 403], [prmURL, 201], [asmURL, 503], [asmURL, 302]]) {
    const f = fixture();
    f.routes.set(route, new Response(null, { status }));
    await assert.rejects(f.run(route === prmURL ? '' : challenge), new RegExp(`HTTP ${status}`));
    assert.equal(f.requests.at(-1).url, route);
    assert.deepEqual(f.events, []);
  }
  for (const opts of [{ serverURL: 'http://localhost/mcp' }, { oauthRedirectURI: 'http://app.example/cb' },
    { oauthRedirectURI: oauthRedirectURI + '?q=1' }, { oauthRedirectURI: undefined }, { clientSecret: 'confidential' }]) {
    const f = fixture(opts);
    await assert.rejects(f.run(), /HTTPS|public client/);
    assert.deepEqual(f.requests, []);
  }
});

test('reject mismatched resource/issuer, unsafe metadata, unsupported flows or changed registration before opening browser', async () => {
  for (const [doc, patch, last] of [
    ['resource', { resource: serverURL + '2' }, challengedURL], ['resource', { authorization_servers: [] }, challengedURL],
    ['resource', { authorization_servers: ['http://issuer.example'] }, challengedURL], ['resource', { bearer_methods_supported: ['query'] }, challengedURL],
    ['as', { issuer: issuer + '/' }, asmURL], ['as', { authorization_endpoint: 'http://login.example' }, asmURL],
    ['as', { token_endpoint: 'http://localhost/token' }, asmURL], ['as', { token_endpoint: undefined }, asmURL],
    ['as', { response_types_supported: ['token'] }, asmURL], ['as', { code_challenge_methods_supported: ['plain'] }, asmURL],
    ['as', { code_challenge_methods_supported: undefined }, asmURL], ['as', { token_endpoint_auth_methods_supported: ['client_secret_basic'] }, asmURL],
    ['as', { authorization_response_iss_parameter_supported: false }, asmURL], ['as', { registration_endpoint: undefined }, asmURL],
    ['as', { registration_endpoint: 'https://user@reg.example' }, asmURL], ['as', { grant_types_supported: 'authorization_code' }, asmURL],
    ['registration', { client_secret: 'not-public' }, regURL], ['registration', { redirect_uris: [oauthRedirectURI + '/'] }, regURL],
    ['registration', { token_endpoint_auth_method: undefined }, regURL], ['registration', { client_id: '' }, regURL],
  ]) {
    const f = fixture();
    Object.assign(f[doc], patch);
    await assert.rejects(f.run(), /MCP OAuth|invalid |missing /);
    assert.equal(f.requests.at(-1).url, last);
    assert.deepEqual(f.events, []);
    assert.equal(f.oauth.accessToken(), undefined);
  }
});

test('reject callback substitution, duplicate params, mixed responses, missing/incorrect issuer or state, and cancellation before exchange', async () => {
  const replace = (text, replacement) => reply => ({ ...reply, content: { callback_url: reply.content.callback_url.replace(text, replacement) } });
  for (const alter of [
    replace(oauthRedirectURI, oauthRedirectURI + '/'), replace(oauthRedirectURI, 'https://other.example/callback'),
    replace(oauthRedirectURI, 'https://app.example/./callback'), replace(oauthRedirectURI, 'https://APP.example/callback'),
    replace('state=', 'bad-state='), replace('state=', 'state=x'), replace('state=', 'state=extra&state='),
    replace('iss=', 'other='), replace('iss=', 'iss=mismatch'), replace('iss=', 'iss=other&iss='), replace('code=', 'code=x&code='),
    replace('code=', 'error=declined&code='), replace('code=', 'access_token=other&code='), replace('code=', '#code='),
    () => ({ action: 'cancel' }), () => ({ action: 'decline' }), () => ({ action: 'accept', content: { access_token: 'no' } }),
    r => ({ action: 'accept', content: Object.fromEntries(new URL(r.content.callback_url).searchParams) }),
  ]) {
    const f = fixture({ elicit: event => alter(callback(event)) });
    await assert.rejects(f.run(), /callback|not accepted/);
    assert.equal(f.requests.at(-1).url, regURL);
    assert.equal(f.oauth.accessToken(), undefined);
  }
});

test('never uses a redirected discovery response, even when a transport reports 200', async () => {
  const f = fixture();
  const response = Response.json(f.resource);
  Object.defineProperty(response, 'redirected', { value: true });
  f.routes.set(challengedURL, response);
  await assert.rejects(f.run(), /MCP OAuth/);
  assert.deepEqual(f.requests.map(r => r.url), [challengedURL]);
});

test('limits untrusted response bodies, redacts errors, and never accepts malformed bearer tokens', async () => {
  for (const response of [new Response('sensitive-secret', { status: 400 }), Response.json('sensitive-secret'),
    new Response('sensitive-secret', { headers: { 'Content-Type': 'application/json' } }), Response.json({ big: 'x'.repeat(1024 * 1024) }),
    Response.json({ access_token: 'sensitive-secret\nnot-header-safe', token_type: 'bearer' }),
    Response.json({ access_token: 'sensitive-secret', token_type: 'DPoP' }), Response.json({ access_token: 'sensitive-secret', token_type: 'bearer', expires_in: -1 }),
  ]) {
    const f = fixture();
    f.routes.set(tokenURL, response);
    await assert.rejects(f.run(), e => !e.message.includes('sensitive-secret') && /MCP OAuth/.test(e.message));
    assert.equal(f.oauth.accessToken(), undefined);
  }
});

test('aborted waiters release a shared authorization; a late callback cannot install a token', async () => {
  const prompt = Promise.withResolvers(), reply = Promise.withResolvers();
  const f = fixture({ elicit: (event, signal) => { prompt.resolve({ event, signal }); return reply.promise; } });
  const first = f.run(), second = f.run();
  const { event, signal } = await prompt.promise;
  f.controller.abort();
  await Promise.all([first, second].map(p => assert.rejects(p, /abort/i)));
  assert.equal(signal.aborted, true);
  reply.resolve(callback(event));
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(f.requests.at(-1).url, regURL);
  assert.equal(f.oauth.accessToken(), undefined);
});

test('refresh uses the pinned resource, token endpoint and registered client; only explicit invalid_grant reopens PKCE', async t => {
  let now = 0;
  t.mock.method(Date, 'now', () => now);
  const f = fixture();
  await f.run();
  // Reauthorization must not rediscover or switch to different endpoints.
  f.as.token_endpoint = 'https://other.example/token';
  f.resource.authorization_servers = ['https://other.example'];
  now = 30001;
  f.routes.set(tokenURL, init => new URLSearchParams(init.body).get('grant_type') === 'refresh_token'
    ? Response.json({ error: 'invalid_grant' }, { status: 400 }) : Response.json({ token_type: 'Bearer', access_token: 'new' }));
  assert.equal(await f.oauth.beforeRequest(f.signal), 'new');
  assert.equal(f.events.length, 2);
  assert.notEqual(f.events[0].meta['docker-agent/state'], f.events[1].meta['docker-agent/state']);
  assert.deepEqual(f.requests.slice(4).map(r => r.url), [tokenURL, tokenURL]);
  const [refresh, exchange] = f.requests.slice(-2).map(r => new URLSearchParams(r.init.body));
  assert.deepEqual(Object.fromEntries(refresh), { client_id: 'registered', grant_type: 'refresh_token', resource: serverURL, refresh_token: f.token.refresh_token });
  assert.equal(exchange.get('grant_type'), 'authorization_code');
  assert.equal(exchange.has('refresh_token'), false);
  assert.ok(!JSON.stringify(f.events).includes(f.token.refresh_token));
});

test('no refresh or new lifetime is inferred from a failed or malformed token response', async t => {
  let now;
  t.mock.method(Date, 'now', () => now);
  for (const bad of [Response.json({ error: 'invalid_grant' }, { status: 502 }), new Response('invalid_grant', { status: 400 }),
    Response.json({ error: 'invalid_client', error_description: 'private-value invalid_grant' }, { status: 400 }),
    Response.json({ access_token: 'private-value', token_type: 'Bearer', refresh_token: null }),
    Response.json({ access_token: 'private-value', token_type: 'Bearer', expires_in: 0 }),
    Response.json({ error: 'invalid_grant', access_token: 'private-value', token_type: 'Bearer' }),
  ]) {
    now = 0;
    const f = fixture();
    await f.run();
    now = 60000;
    assert.equal(f.oauth.accessToken(), undefined);
    f.routes.set(tokenURL, bad);
    await assert.rejects(f.oauth.beforeRequest(f.signal), e => !e.message.includes('private-value') && /MCP OAuth/.test(e.message));
    assert.equal(f.oauth.accessToken(), undefined);
    assert.equal(f.events.length, 1);
    f.routes.set(tokenURL, { access_token: 'recovered', token_type: 'bearer' });
    assert.equal(await f.oauth.beforeRequest(f.signal), 'recovered');
    assert.equal(new URLSearchParams(f.requests.at(-1).init.body).get('refresh_token'), f.token.refresh_token);
    now += 86400000;
    assert.equal(await f.oauth.beforeRequest(f.signal), 'recovered');
    assert.equal(f.requests.length, 6, 'omitted expires_in must not inherit the previous access token expiry');
  }
});

test('cancellation releases a stalled OAuth response reader and bounds simultaneous waiters', async t => {
  t.mock.method(Date, 'now', () => 0);
  const f = fixture();
  await f.run();
  const reading = Promise.withResolvers(), cancelled = Promise.withResolvers();
  f.routes.set(tokenURL, new Response(new ReadableStream({
    pull() { reading.resolve(); }, cancel() { cancelled.resolve(); },
  }, { highWaterMark: 0 }), { headers: { 'Content-Type': 'application/json' } }));
  const controller = new AbortController();
  const first = f.oauth.authorize('Bearer error="invalid_token"', controller.signal, 'temporary');
  await reading.promise;
  const others = Array.from({ length: 31 }, () => f.oauth.beforeRequest(controller.signal));
  await assert.rejects(f.oauth.beforeRequest(f.signal), /too many concurrent/);
  assert.equal(f.requests.length, 5);
  controller.abort();
  await Promise.all([first, ...others].map(p => assert.rejects(p, /abort/i)));
  await cancelled.promise;
  assert.equal(f.events.length, 1);
  assert.equal(f.oauth.accessToken(), undefined);
});

test('an access token that expires while its response streams is never installed', async t => {
  let now = 0;
  t.mock.method(Date, 'now', () => now);
  const f = fixture();
  f.routes.set(tokenURL, new Response(new ReadableStream({ pull(controller) {
    now = 1000;
    controller.enqueue(new TextEncoder().encode(JSON.stringify({ ...f.token, expires_in: 1 })));
    controller.close();
  } }, { highWaterMark: 0 }), { headers: { 'Content-Type': 'application/json' } }));
  await assert.rejects(f.run(), /already expired/);
  assert.equal(f.oauth.accessToken(), undefined);
});
