import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { before, test } from 'node:test';
import boot from '../node/boot.js';

let api;
before(async () => { api = await boot.bootDockerAgent(); });

const chat = delta => ({ choices: [{ index: 0, delta, finish_reason: delta.tool_calls ? 'tool_calls' : 'stop' }] });
const text = content => chat({ content });
const call = (name, args, id = name) => chat({ tool_calls: [{ id, index: 0, type: 'function', function: { name, arguments: JSON.stringify(args) } }] });
const completed = (...output) => ({ type: 'response.completed', response: { id: 'r', status: 'completed', output } });
const responseCall = (name, args) => completed({ id: `item_${name}`, type: 'function_call', call_id: name, name, arguments: JSON.stringify(args) });
const responseText = content => completed({ id: 'msg', type: 'message', role: 'assistant', content: [{ type: 'output_text', text: content }] });

// Each script listens on a different origin, with no fetch or Wasm ABI mocks.
async function endpoint(t, ...steps) {
  const requests = [];
  const server = createServer(async (req, res) => {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const step = steps[requests.length];
    requests.push({ path: req.url, method: req.method, auth: req.headers.authorization, body: JSON.parse(Buffer.concat(chunks)) });
    if (typeof step === 'number' || step === undefined) {
      res.writeHead(step ?? 400).end();
    } else if (typeof step === 'string') {
      res.writeHead(200, { 'Content-Type': 'text/plain' }).end(step);
    } else {
      res.writeHead(200, { 'Content-Type': 'text/event-stream' }).end(`data: ${JSON.stringify(step)}\n\n${step.choices ? 'data: [DONE]\n\n' : ''}`);
    }
  });
  await new Promise((resolve, reject) => server.once('error', reject).listen(0, '127.0.0.1', resolve));
  t.after(() => { server.closeAllConnections(); return new Promise(resolve => server.close(resolve)); });
  return { requests, url: `http://127.0.0.1:${server.address().port}` };
}

function checkRoute({ requests }, model, key, responses = false) {
  for (const req of requests) {
    assert.equal(req.body.model, model);
    assert.equal(req.path, responses ? '/responses' : '/chat/completions');
    assert.equal(req.method, 'POST');
    assert.equal(req.auth, key && `Bearer ${key}`);
    assert.equal(Array.isArray(req.body.input), responses);
    assert.equal(Array.isArray(req.body.messages), !responses);
  }
}

test('child retries only its first request; each frame resumes its own pinned target and tools', async t => {
  const transfer = call('transfer_task', { agent: 'helper', task: 'count' });
  const root = await endpoint(t, 401);
  const rootBackup = await endpoint(t, transfer, text('done'), transfer, text('again'));
  const helper = await endpoint(t, 403);
  const helperBackup = await endpoint(t, responseCall('user_prompt', { message: 'Which?' }), responseCall('record', {}), responseText('three'), responseText('four'));
  const service = await endpoint(t, 'recorded');
  const yaml = `providers:
  p: {base_url: ${root.url}, token_key: A}
  q: {base_url: ${rootBackup.url}, token_key: B}
  h: {base_url: ${helper.url}, token_key: C}
  b: {base_url: ${helperBackup.url}, api_type: openai_responses}
agents:
  root: {model: p/root, fallback: {models: [q/r-backup]}, sub_agents: [helper], instruction: private-parent-rule}
  helper: {model: h/worker, fallback: {models: [b/h-backup]}, toolsets: [{type: user_prompt},
    {type: api, allow_private_ips: true, api_config: {name: record, method: POST, endpoint: '${service.url}/record', headers: {Authorization: 'Bearer \${env.SERVICE}'}}}]}
permissions: {ask: [record]}`;
  const events = [];
  const s = await api.createSession({ yaml, env: { A: 'a', B: 'b', C: 'c', SERVICE:'service-key' } }, e => {
    events.push(e);
    if (e.type === 'elicitation') return s.respondToElicitation({ id: e.id, action: 'accept', content: { number: 3 } });
    if (e.type === 'tool_confirmation') return s.confirm({confirmationId:e.confirmationId, decision:'approve'});
  });
  t.after(() => s.close());
  assert.equal((await s.send('private-parent-question')).message.content, 'done');
  assert.equal((await s.send('second question')).message.content, 'again');
  assert.deepEqual([root, rootBackup, helper, helperBackup].map(x => x.requests.length), [1, 4, 1, 4]);
  checkRoute(root, 'root', 'a');
  checkRoute(rootBackup, 'r-backup', 'b');
  checkRoute(helper, 'worker', 'c');
  checkRoute(helperBackup, 'h-backup', undefined, true);
  assert.deepEqual(helperBackup.requests[0].body.tools.map(t => t.name), ['user_prompt', 'record']);
  assert.equal(helperBackup.requests[2].body.input.at(-1).output, 'recorded');
  assert.deepEqual(service.requests, [{path:'/record', method:'POST', auth:'Bearer service-key', body:{}}]);
  assert.deepEqual(events.filter(e => e.type === 'tool_confirmation').map(e => e.agent), ['helper']);
  assert.doesNotMatch(JSON.stringify(helperBackup.requests), /private-parent/);
  assert.deepEqual(JSON.parse(helperBackup.requests[1].body.input.at(-1).output), { action: 'accept', content: { number: 3 } });
  assert.deepEqual([1, 3].map(i => rootBackup.requests[i].body.messages.at(-1).content), ['three', 'four']);
  assert.deepEqual(rootBackup.requests[1].body.tools.map(t => t.function.name), ['transfer_task']);
  assert.deepEqual(events.filter(e => e.type === 'fallback').map(e => [e.from, e.to]), [['p/root', 'q/r-backup'], ['h/worker', 'b/h-backup']]);
  assert.equal(events.filter(e => e.type === 'elicitation').length, 1);
});

test('fallback uses approved dynamic tools once and a failed follow-up retains the host result', async t => {
  const write = responseCall('record', {});
  const primary = await endpoint(t, 429, 429);
  const backup = await endpoint(t, write, 503, responseText('recovered'), write, responseText('denied'));
  const service = await endpoint(t, 'receipt-1');
  const opts = { yaml: `providers:
  p: {base_url: ${primary.url}, token_key: MODEL}
  b: {base_url: ${backup.url}, api_type: openai_responses, token_key: BACKUP}
agents:
  root:
    model: p/main
    fallback: {models: [b/spare]}
    toolsets:
      - type: api
        allow_private_ips: true
        api_config: {name: record, method: POST, endpoint: '${service.url}/record', headers: {Authorization: 'Bearer \${env.SERVICE}'}}
`, env: { MODEL: 'primary-key', BACKUP: 'backup-key', SERVICE: 'service-key' } };
  const events = [];
  const s = await api.createSession(opts, e => {
    events.push(e);
    if (e.type === 'tool_confirmation') return s.confirm({decision: 'approve_tool', toolName: 'record'});
  });
  t.after(() => s.close());
  await assert.rejects(s.send('record'), /503/);
  assert.deepEqual(events.filter(e => e.type === 'tool_result').map(e => e.output), ['receipt-1']);
  assert.equal((await s.send('recover')).message.content, 'recovered');
  assert.equal(backup.requests[2].body.input.filter(m => m.type === 'function_call_output' && m.output === 'receipt-1').length, 1);
  assert.equal(backup.requests[0].body.tools[0].name, 'record');
  assert.equal(events.filter(e => e.type === 'tool_confirmation').length, 1);
  const other = await api.createSession(opts); // No callback: the first session's grant cannot authorize this call.
  t.after(() => other.close());
  await other.send('record');
  assert.match(backup.requests[4].body.input.at(-1).output, /rejected/);
  assert.deepEqual(service.requests, [{ path: '/record', method: 'POST', auth: 'Bearer service-key', body: {} }]);
  checkRoute(primary, 'main', 'primary-key');
  checkRoute(backup, 'spare', 'backup-key', true);
  assert.equal(primary.requests.length, 2);
  assert.equal(backup.requests.length, 5);
  assert.doesNotMatch(JSON.stringify(backup.requests.map(r => r.body)), /service-key/);
});

test('capped tool rounds recheck cooldown after approval; a failed continuation never repeats a write', async t => {
  let now = 0;
  t.mock.method(performance, 'now', () => now);
  const primary = await endpoint(t, 401, 503, text('recovered'));
  const backup = await endpoint(t, responseCall('record', {}), 400);
  const service = await endpoint(t, 'one receipt');
  const yaml = `providers:
  p: {base_url: ${primary.url}, token_key: PRIMARY}
  b: {base_url: ${backup.url}, api_type: openai_responses, token_key: BACKUP}
agents:
  root:
    model: p/main
    fallback: {models: [b/spare], retries: -1, cooldown: 5s}
    max_iterations: 3
    session_compaction: false
    toolsets: [{type: api, allow_private_ips: true, api_config: {name: record, method: POST, endpoint: '${service.url}/record'}}]`;
  const events = [];
  const s = await api.createSession({yaml, env:{PRIMARY:'primary-key', BACKUP:'backup-key'}}, e => {
    events.push(e);
    if (e.type === 'tool_confirmation') {
      now = 5001;
      return s.confirm({confirmationId:e.confirmationId, decision:'approve'});
    }
  });
  t.after(() => s.close());
  await assert.rejects(s.send('write'), /all models failed/);
  assert.deepEqual(events.filter(e => e.type === 'tool_result').map(e => e.output), ['one receipt']);
  assert.equal(events.filter(e => e.type === 'tool_confirmation').length, 1);
  assert.equal(events.filter(e => e.type === 'finish').length, 0);
  assert.equal((await s.send('recover')).message.content, 'recovered');
  assert.deepEqual(service.requests, [{path:'/record', method:'POST', auth:undefined, body:{}}]);
  assert.equal(primary.requests[1].body.messages.at(-1).content, 'one receipt');
  assert.equal(backup.requests[1].body.input.at(-1).output, 'one receipt');
  assert.equal(primary.requests[2].body.messages.filter(m => m.role === 'tool' && m.content === 'one receipt').length, 1);
  assert.deepEqual([primary.requests.length, backup.requests.length], [3, 2]);
  checkRoute(primary, 'main', 'primary-key');
  checkRoute(backup, 'spare', 'backup-key', true);
});

test('abort on an approved write result keeps its receipt and never dispatches the continuation', async t => {
  const primary = await endpoint(t, 401);
  const backup = await endpoint(t, responseCall('record', {}), responseText('recovered'));
  const service = await endpoint(t, 'kept receipt');
  const yaml = `providers:
  p: {base_url: ${primary.url}, token_key: PRIMARY}
  b: {base_url: ${backup.url}, api_type: openai_responses, token_key: BACKUP}
agents:
  root:
    model: p/main
    fallback: {models: [b/spare], retries: -1}
    hooks: {before_llm_call: {type: builtin, command: max_iterations, args: ["2"]}}
    session_compaction: false
    toolsets: [{type: api, allow_private_ips: true, api_config: {name: record, method: POST, endpoint: '${service.url}/record'}}]`;
  const s = await api.createSession({yaml, env:{PRIMARY:'primary-key', BACKUP:'backup-key'}}, e => {
    if (e.type === 'tool_confirmation') return s.confirm({confirmationId:e.confirmationId, decision:'approve'});
    if (e.type === 'tool_result') s.abort();
  });
  t.after(() => s.close());
  await assert.rejects(s.send('write'), /chat aborted/);
  assert.equal(backup.requests.length, 1);
  assert.equal((await s.send('recover')).message.content, 'recovered');
  assert.equal(backup.requests[1].body.input.filter(m => m.type === 'function_call_output' && m.output === 'kept receipt').length, 1);
  assert.equal(service.requests.length, 1);
});

test('a picker change belongs to the selected agent, not the delegated or resumed frame', async t => {
  const parent = await endpoint(t, call('change_model', { model: 'p/precise' }), call('transfer_task', { agent: 'helper', task: 'compute' }), text('done'));
  const child = await endpoint(t, 403, text('backup'));
  const yaml = `providers:
  p: {base_url: ${parent.url}, token_key: ROOT}
  h: {base_url: ${child.url}, token_key: CHILD}
agents:
  root: {model: p/start, sub_agents: [helper], toolsets: [{type: model_picker, models: [p/precise]}]}
  helper: {model: h/start, fallback: {models: [h/alternate]}}`;
  const s = await api.createSession({ yaml, env: { ROOT: 'root-key', CHILD: 'child-key' } });
  t.after(() => s.close());
  assert.equal((await s.send('go')).message.content, 'done');
  assert.deepEqual(parent.requests.map(r => [r.body.model, r.auth]), [['start', 'Bearer root-key'], ['precise', 'Bearer root-key'], ['precise', 'Bearer root-key']]);
  assert.deepEqual(child.requests.map(r => [r.body.model, r.auth, r.body.tools]), [['start', 'Bearer child-key', undefined], ['alternate', 'Bearer child-key', undefined]]);
});

test('unsafe combinations on any reachable agent fail at open, before model or MCP discovery', async t => {
  const trap = await endpoint(t);
  const base = `providers: {p: {base_url: '${trap.url}', token_key: KEY}}
agents:
  root: {model: p/one, sub_agents: [helper]}
  helper: {model: p/two, handoffs: [leaf]}
  leaf: `;
  const configs = [
    [base + `{model: p/three, toolsets: [{type: mcp, remote: {url: '${trap.url}/mcp'}}]}`, /requires a trusted toolProxy/],
    [base + `{model: p/three, toolsets: [{type: mcp, allow_private_ips: true, remote: {url: '${trap.url}/mcp', headers: {Authorization: 'Bearer \${env.MISSING}'}}}]}`, /missing MCP env value/],
    [base + `{model: p/three, toolsets: [{type: mcp, allow_private_ips: true, remote: {url: '${trap.url}/mcp', oauth: {}}}]}`, /oauthRedirectURI/],
    [base + `{model: p/three, toolsets: [{type: session_context}]}`, /session_context/],
    [base + `{model: p/three, toolsets: [{type: model_picker, models: [p/pick]}], fallback: {models: [p/backup]}}`, /model_picker with fallback/],
    [base + `{model: p/three, fallback: {models: [bad]}}\nmodels: {bad: {provider: p, model: wrong, base_url: 'https://elsewhere.invalid'}}`, /cross-origin/],
    [base + `{model: bad}\nmodels: {bad: {provider: p, model: wrong, base_url: 'https://elsewhere.invalid'}}`, /cross-origin/],
  ];
  for (const [yaml, expected] of configs) await assert.rejects(api.createSession({ yaml, env: { KEY: 'private' }, autoApprove: true }), expected);
  assert.deepEqual(trap.requests, []);
});

test('exhausting a child chain closes the parent transfer without erasing or replaying earlier tools', async t => {
  const parent = await endpoint(t, call('think', { thought: 'one' }), call('transfer_task', { agent: 'helper', task: 'count' }), text('recovered'));
  const helper = await endpoint(t, 403, 403);
  const yaml = `providers: {p: {base_url: ${parent.url}}, h: {base_url: ${helper.url}}}
agents:
  root: {model: p/root, sub_agents: [helper], toolsets: [{type: think}]}
  helper: {model: h/one, fallback: {models: [h/two]}}`;
  const events = [];
  const s = await api.createSession({ yaml }, e => events.push(e));
  t.after(() => s.close());
  await assert.rejects(s.send('start'), /all models failed/);
  assert.equal(parent.requests.length, 2);
  assert.deepEqual(helper.requests.map(r => r.body.model), ['one', 'two']);
  await s.send('recover');
  const history = parent.requests[2].body.messages;
  assert.deepEqual(history.filter(m => m.role === 'tool').map(m => m.content), ['Thoughts:\none', 'Delegated task failed']);
  assert.doesNotMatch(JSON.stringify(history), /403|all models failed/);
  assert.equal(events.filter(e => e.type === 'tool_call' && e.name === 'think').length, 1);
  assert.equal(events.filter(e => e.type === 'error').length, 1);
});

test('child retries bind the child prompt, schema, redaction and transport; return restores the parent', async t => {
  const secret = 'dckr_pat_' + 'X'.repeat(27);
  const steps = [call('transfer_task', { agent: 'helper', task: secret }), 401,
    responseCall('think', { thought: secret }), responseText(secret), text('done')];
  const requests = [];
  t.mock.method(globalThis, 'fetch', (url, init) => {
    const step = steps[requests.length];
    requests.push({ url, headers: new Headers(init.headers), body: JSON.parse(init.body) });
    if (step === 401) return new Response(null, { status: 401 });
    return new Response(`data: ${JSON.stringify(step)}\n\n${step.choices ? 'data: [DONE]\n\n' : ''}`, { headers: { 'Content-Type': 'text/event-stream' } });
  });
  const yaml = `models:
  azure: {provider: azure, model: parent, base_url: https://resource.openai.azure.com, provider_opts: {api_version: preview}}
toolsets: {scratch: {type: think}}
agents:
  root: {model: azure, sub_agents: [helper], add_environment_info: true}
  helper:
    model: github-copilot/gpt-4o
    fallback: {models: [chatgpt/gpt-5]}
    hooks: {turn_start: {type: builtin, command: add_date}}
    use_toolsets: [scratch]
    structured_output: {name: child_result, schema: {type: object, properties: {}}}`;
  const env = { AZURE_API_KEY: 'azure-private', GITHUB_TOKEN: 'copilot-private', CHATGPT_OAUTH_TOKEN: 'codex-private' };
  const events = [];
  const s = await api.createSession({ yaml, env }, e => events.push(e));
  t.after(() => s.close());
  assert.equal((await s.send(secret)).message.content, 'done');
  assert.equal(requests.length, 5);
  for (const [i, { url, headers, body }] of requests.entries()) {
    const parent = i === 0 || i === 4, codex = i === 2 || i === 3;
    assert.equal(url, parent ? 'https://resource.openai.azure.com/chat/completions?api-version=preview' :
      codex ? 'https://chatgpt.com/backend-api/codex/responses' : 'https://api.githubcopilot.com/chat/completions');
    assert.equal(headers.get('Authorization'), 'Bearer ' + (parent ? env.AZURE_API_KEY : codex ? env.CHATGPT_OAUTH_TOKEN : env.GITHUB_TOKEN));
    assert.equal(headers.has('Copilot-Integration-Id'), i === 1);
    assert.equal(headers.has('session_id'), codex);
    assert.equal(headers.has('originator'), codex);
    const prompt = JSON.stringify(body);
    assert.equal(prompt.includes(secret), false);
    assert.equal(prompt.includes('[REDACTED]'), true);
    assert.equal(prompt.includes('Operating System: js'), parent);
    assert.equal(prompt.includes("Today's date:"), !parent);
    assert.equal(prompt.includes('child_result'), !parent);
    assert.doesNotMatch(prompt, /azure-private|copilot-private|codex-private/);
  }
  assert.equal(requests[2].headers.get('session_id'), requests[3].headers.get('session_id'));
  assert.equal(requests[2].body.text.format.name, 'child_result');
  assert.equal(requests[3].body.input.at(-1).output, 'Thoughts:\n[REDACTED]');
  assert.equal(requests[4].body.messages.at(-1).content, '[REDACTED]');
  assert.deepEqual(events.filter(e => e.type === 'tool_result').map(e => e.output), ['Thoughts:\n[REDACTED]', '[REDACTED]']);
});

test('rejects unsupported policy and media combinations before tool discovery or model I/O', async t => {
  t.mock.method(globalThis, 'fetch', () => assert.fail('unexpected fetch'));
  const yaml = `providers: {p: {}}\nmodels: {small: {provider: p, model: small, context_window: 4096}}\nagents:\n  root: {model: p/main, handoffs: [helper]}\n  helper: {model: p/child}`;
  for (const [cfg, reason] of [
    [yaml.replace('p/main,', 'p/main, readonly: true,'), /readonly/],
    [yaml.replace('p/child}', 'p/child, redact_secrets: false}'), /foreground handoffs with redaction disabled require one-shot chat/],
    [yaml.replace('handoffs:', 'sub_agents:').replace('p/child}', 'p/child, hooks: {before_llm_call: {type: builtin, command: max_iterations, args: ["1"]}}}'), /inside transfer_task/],
    [yaml.replace('p/child}', 'small}'), /compaction/],
    [yaml.replace('p/child}', 'p/child, toolsets: [{type: model_picker, models: [image]}]}').replace('small: {', 'image: {provider: p, model: other, capabilities: {image: true}}, small: {'), /media capabilities/],
    [yaml.replace('handoffs: [helper]', 'fallback: {models: [small]}'), /compaction/],
    [yaml.replace('p/child}', 'p/child, use_toolsets: [web]}') + '\ntoolsets: {web: {type: api, api_config: {name: test, method: TRACE, endpoint: https://tool.invalid}}}', /api_config.method/],
  ]) await assert.rejects(api.createSession({ yaml: cfg }), reason);
  await assert.rejects(api.chat({ yaml, messages: [{ role: 'user', multi_content: [{ type: 'file_id', id: 'untrusted' }] }, { role: 'user', content: 'go' }] }), /invalid history/);
});
