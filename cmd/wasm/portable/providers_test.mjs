// Run these same wire assertions against Go small and portable; no requests leave the process.
import assert from 'node:assert/strict';
import { before, test } from 'node:test';
import boot from '../node/boot.js';

const portable = Boolean(process.env.DOCKER_AGENT_TEST_PORTABLE);
let api;
before(async () => {
  // Go net/http selects its browser fetch transport once, at boot, based on argv0.
  const process = globalThis.process;
  if (!portable) globalThis.process = Object.create(process, { argv0: { value: 'browser' } });
  try { api = await boot.bootDockerAgent(); } finally { globalThis.process = process; }
});

const yaml = (provider, model, fields = '', agentFields = '') => `models:\n  chosen: {provider: ${provider}, model: ${model}${fields}}\nagents: {root: {model: chosen${agentFields}}}`;
const env = {
  GITHUB_TOKEN: 'github', GH_TOKEN: 'gh', AZURE_API_KEY: 'azure', CLOUDFLARE_API_TOKEN: 'cf',
  CLOUDFLARE_ACCOUNT_ID: 'acct-1', CLOUDFLARE_GATEWAY_ID: 'gw-2', OPENAI_API_KEY: 'openai',
  CHATGPT_OAUTH_TOKEN: 'codex', OPENAI_BASE_URL: 'https://ignored.invalid', OTHER: 'explicit',
};
function intercept(t) {
  const requests = [];
  t.mock.method(globalThis, 'fetch', async (input, init) => {
    const req = new Request(input, init);
    assert.equal(req.method, 'POST');
    if (portable) {
      assert.equal(req.redirect, 'error');
      assert.equal(req.credentials, 'omit');
    }
    requests.push({ url: req.url, headers: req.headers, body: await req.json() });
    const stream = new URL(req.url).pathname.endsWith('/responses')
      ? 'data: {"type":"response.output_text.delta","output_index":0,"item_id":"m","delta":"ok"}\n\ndata: {"type":"response.completed","response":{"status":"completed","output":[]}}\n\n'
      : 'data: {"id":"m","object":"chat.completion.chunk","model":"test","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n';
    return new Response(stream, { headers: { 'Content-Type': 'text/event-stream' } });
  });
  return requests;
}

const azure = 'https://resource.openai.azure.com/openai/deployments/d1';
test('provider endpoint, credential precedence, query and headers match Go', async t => {
  const requests = intercept(t);
  const github = 'https://api.githubcopilot.com';
  for (const [cfg, overrides, url, key, copilot] of [
    [yaml('github-copilot', 'gpt-4o'), {}, `${github}/chat/completions`, 'github', 'copilot-developer-cli'],
    [yaml('github-copilot', 'gpt-5'), { GITHUB_TOKEN: '' }, `${github}/responses`, 'gh', 'copilot-developer-cli'],
    [yaml('github-copilot', 'gpt-4.1', ', token_key: OTHER, provider_opts: {http_headers: {copilot-integration-id: example}}'), {}, `${github}/responses`, 'explicit', 'example'],
    [yaml('cloudflare-workers-ai', "'@cf/test'"), {}, 'https://api.cloudflare.com/client/v4/accounts/acct-1/ai/v1/chat/completions', 'cf'],
    [yaml('cloudflare-ai-gateway', 'openai/gpt-5'), {}, 'https://gateway.ai.cloudflare.com/v1/acct-1/gw-2/compat/chat/completions', 'cf'],
    [yaml('azure', 'gpt-5', ", base_url: '${BASE}/', provider_opts: {api_version: 'preview 1&flag'}"), { BASE: azure }, `${azure}/chat/completions?api-version=preview+1%26flag`, 'azure'],
    [yaml('azure', 'gpt-4o', `, base_url: ${azure}, token_key: OTHER`), {}, `${azure}/chat/completions`, 'explicit'],
    [yaml('openai', 'gpt-4o'), {}, 'https://api.openai.com/v1/chat/completions', 'openai'],
  ]) {
    const result = await api.chat({ yaml: cfg, env: { ...env, ...overrides }, messages: [{ role: 'user', content: 'hi' }] });
    assert.equal(result.message.content, 'ok');
    assert.equal(requests.length, 1);
    const r = requests.pop();
    assert.equal(r.url, url);
    assert.equal(r.headers.get('authorization'), `Bearer ${key}`);
    assert.equal(r.headers.get('copilot-integration-id'), copilot ?? null);
    for (const name of ['api-key', 'chatgpt-account-id', 'session_id', 'originator', 'OpenAI-Beta']) assert.equal(r.headers.get(name), null);
  }
});

const jwtPart = value => Buffer.from(JSON.stringify(value)).toString('base64url');
test('native ChatGPT uses per-session Codex auth, not platform API-key auth', async t => {
  const requests = intercept(t);
  const token = `${jwtPart({ alg: 'RS256' })}.${jwtPart({ 'https://api.openai.com/auth': { chatgpt_account_id: 'acct_123' } })}.c2ln`;
  const sessions = [];
  t.after(() => Promise.all(sessions.map(s => s.close())));
  for (const [model, fields, access] of [['gpt-5', ', instruction: behave', token], ['test', '', 'opaque']]) {
    const cfg = yaml('chatgpt', model, ', temperature: 0.5, top_p: 0.9, max_tokens: 50', fields);
    sessions.push(await api.createSession({ yaml: cfg, env: { ...env, CHATGPT_OAUTH_TOKEN: access } }));
  }
  // Interleave two clients so reused or globally cached headers fail the test.
  for (const s of [sessions[0], sessions[1], sessions[0]]) assert.equal((await s.send('hi')).message.content, 'ok');
  assert.equal(requests.length, 3);
  const ids = [];
  for (const [i, { url, headers, body }] of requests.entries()) {
    assert.equal(url, 'https://chatgpt.com/backend-api/codex/responses');
    assert.equal(headers.get('authorization'), `Bearer ${i === 1 ? 'opaque' : token}`);
    assert.equal(headers.get('chatgpt-account-id'), i === 1 ? null : 'acct_123');
    assert.equal(headers.get('originator'), 'codex_cli_rs');
    assert.equal(headers.get('OpenAI-Beta'), 'responses=experimental');
    assert.equal(headers.get('copilot-integration-id'), null);
    ids.push(headers.get('session_id'));
    assert.match(ids[i], /^[0-9a-f-]{36}$/);
    assert.equal(body.store, false);
    assert.equal(body.instructions, i === 1 ? 'You are a helpful assistant.' : 'behave');
    assert.equal(body.input.some(m => m.role === 'system'), false);
    assert.equal(body.reasoning?.summary, i === 1 ? undefined : 'auto');
    for (const key of ['max_output_tokens', 'temperature', 'top_p']) assert.equal(key in body, false);
  }
  assert.notEqual(ids[0], ids[1]);
  assert.equal(ids[0], ids[2]);
});

test('portable rejects unbound credentials, unsafe paths and unsupported auth before fetch', { skip: !portable }, async t => {
  const requests = intercept(t);
  for (const [cfg, supplied] of [
    [yaml('github-copilot', 'test', ', token_key: ABSENT'), env],
    [yaml('github-copilot', 'test', ', base_url: https://other.invalid'), env],
    [yaml('chatgpt', 'test', ', base_url: https://api.openai.com/v1'), env],
    [yaml('chatgpt', 'test'), { OPENAI_API_KEY: 'wrong' }],
    [yaml('openai', 'test'), { CHATGPT_OAUTH_TOKEN: 'wrong' }],
    [yaml('azure', 'test'), env],
    [yaml('azure', 'test', ', base_url: https://resource.openai.azure.com.example.invalid'), env],
    [yaml('azure', 'test', `, base_url: '${azure}?api-version=bad'`), env],
    [yaml('azure', 'test', ', base_url: http://resource.openai.azure.com'), env],
    [yaml('cloudflare-workers-ai', 'test'), { ...env, CLOUDFLARE_ACCOUNT_ID: '../other' }],
    [yaml('cloudflare-ai-gateway', 'test'), { ...env, CLOUDFLARE_GATEWAY_ID: '%2f' }],
    [yaml('cloudflare-ai-gateway', 'test'), { ...env, CLOUDFLARE_GATEWAY_ID: '' }],
    [yaml('cloudflare-ai-gateway', 'test'), { CLOUDFLARE_API_TOKEN: 'cf' }],
    [yaml('github-copilot', 'test', ', provider_opts: {http_headers: {Authorization: wrong}}'), env],
    [yaml('anthropic', 'test'), env], [yaml('dmr', 'test'), env],
  ]) await assert.rejects(api.createSession({ yaml: cfg, env: supplied }), Error, cfg);
  assert.equal(requests.length, 0);
});

test('model_picker cannot reuse a handle for a different auth domain, key, header or query', { skip: !portable }, async t => {
  const fetch = intercept(t);
  for (const [first, next] of [
    ['provider: github-copilot', 'provider: chatgpt'],
    ['provider: github-copilot', 'provider: github-copilot, token_key: OTHER'],
    ['provider: github-copilot', 'provider: github-copilot, provider_opts: {http_headers: {copilot-integration-id: other}}'],
    [`provider: azure, base_url: ${azure}`, `provider: azure, base_url: ${azure}, provider_opts: {api_version: preview}`],
    [`provider: azure, base_url: ${azure}`, 'provider: azure, base_url: https://other.openai.azure.com'],
  ]) {
    const cfg = `models: {start: {model: gpt-5, ${first}}, next: {model: gpt-5, ${next}}}
agents: {root: {model: start, toolsets: [{type: model_picker, models: [next]}]}}`;
    await assert.rejects(api.createSession({ yaml: cfg, env: { ...env, OTHER: env.GITHUB_TOKEN } }), /model_picker/);
  }
  assert.equal(fetch.length, 0);
});

test('explicit token_key does not inherit an alternative credential', async () => {
  const cfg = yaml('github-copilot', 'test', ', token_key: GITHUB_TOKEN');
  await assert.rejects(api.createSession({ yaml: cfg, env: { GH_TOKEN: 'fallback' } }), /GITHUB_TOKEN/);
});
