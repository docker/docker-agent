// page.js runs inside headless Chrome, loaded by smoke_test.js. It boots
// docker-agent.wasm the way web/index.html does and drives real sessions
// against the runner's fake OpenAI endpoint and egress proxy. Results are
// collected in window.__done, which the runner awaits over CDP.

"use strict";

const cfg = window.CFG; // {openai, proxy} origins, injected by the runner
const results = [];
const log = (...args) => console.log(...args);

function assert(cond, msg) {
  if (!cond) throw new Error("assert: " + msg);
}
function withTimeout(promise, ms, what) {
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(`timeout after ${ms}ms: ${what}`)), ms);
  });
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
}
async function test(name, fn) {
  const start = performance.now();
  try {
    await withTimeout(fn(), 60_000, name);
    results.push({ name, ok: true, ms: Math.round(performance.now() - start) });
    log(`PASS ${name}`);
  } catch (e) {
    results.push({ name, ok: false, error: String((e && e.stack) || e) });
    log(`FAIL ${name}: ${(e && e.message) || e}`);
  }
}
async function rejects(promise, re) {
  try {
    await promise;
  } catch (e) {
    assert(re.test(String(e && e.message ? e.message : e)), `expected ${re}, got: ${e && e.message ? e.message : e}`);
    return;
  }
  throw new Error(`expected rejection matching ${re}`);
}

function yaml(model, toolsets = "") {
  return `
providers:
  local:
    provider: openai
    base_url: ${cfg.openai}/v1
    token_key: LOCAL_KEY
agents:
  root:
    model: local/${model}
    instruction: test agent
${toolsets}`;
}
const env = { LOCAL_KEY: "test-key" };
const toolsYAML = yaml("tools", `
    toolsets:
      - type: todo
      - type: plan
      - type: memory
      - type: user_prompt
      - type: rag
        ref: handbook
rag:
  handbook:
    tool:
      name: search_handbook
      description: Search the handbook.
    docs: [handbook]
    strategies:
      - type: bm25
        chunking: {size: 200, overlap: 20}
    results: {limit: 2}
`);
const fetchYAML = yaml("fetch", `
    toolsets:
      - type: fetch
`);

async function boot() {
  const go = new Go();
  const { instance } = await WebAssembly.instantiateStreaming(fetch("/docker-agent.wasm"), go.importObject);
  go.run(instance); // main() blocks on select{}, never settles
  for (let i = 0; i < 1000 && !globalThis.dockerAgent; i++) await new Promise((r) => setTimeout(r, 10));
  assert(globalThis.dockerAgent, "dockerAgent global was never registered");
}

// answers tool_confirmation/elicitation asynchronously, like a UI would.
function collector(answer) {
  const events = [];
  const onEvent = (ev) => {
    events.push(ev);
    if (answer && (ev.type === "tool_confirmation" || ev.type === "elicitation")) setTimeout(() => answer(ev), 0);
  };
  return { events, onEvent, of: (type) => events.filter((e) => e.type === type) };
}

async function main() {
  await test("boot wasm via instantiateStreaming", boot);
  const { dockerAgent } = globalThis;

  await test("streams a real fetch() SSE turn, runs two turns, closes", async () => {
    const c = collector();
    const session = await dockerAgent.createSession({ yaml: yaml("stream"), env }, c.onEvent);
    const first = await session.send("hello");
    assert(first.message.content === "Hello world", `content: ${JSON.stringify(first.message)}`);
    assert(c.of("delta").length >= 2, `deltas: ${c.of("delta").length}`);
    assert(c.of("finish").length === 1 && c.of("usage").length === 1, "finish + usage events");
    assert(first.usage && first.usage.input_tokens === 3 && first.usage.output_tokens === 2, JSON.stringify(first.usage));

    const second = await session.send("again");
    // system + user + assistant + user
    assert(/^Turn 2 saw 4 messages$/.test(second.message.content), `turn two: ${second.message.content}`);
    await session.close();
    await rejects(session.send("late"), /session is closed/);
  });

  await test("scripted tool calls: todo, plan, memory, confirm approve/reject, user_prompt accept/decline, bm25 rag", async () => {
    let session;
    const c = collector((ev) => {
      if (ev.type === "tool_confirmation") {
        if (ev.name === "delete_plan") session.confirm("reject", "nope");
        else session.confirm(ev.name === "add_memory" ? { decision: "approve" } : true);
      } else if (ev.type === "elicitation") {
        const first = c.of("elicitation").length === 1;
        session.respondToElicitation(first ? { id: ev.id, action: "accept", content: { answer: "yes" } } : { id: ev.id, action: "decline" });
      }
    });
    session = await dockerAgent.createSession({
      yaml: toolsYAML,
      env,
      documents: {
        "handbook/leave.md": "Everyone gets 25 days of vacation per year. Unused days expire in March.",
        "handbook/expenses.md": "File expense reports within 30 days with receipts attached.",
        "other/ignored.md": "vacation vacation vacation",
      },
    }, c.onEvent);
    const result = await session.send("do everything");
    assert(result.message.content === "done", `final: ${result.message.content}`);

    const byName = (name) => c.of("tool_result").filter((e) => e.name === name);
    assert(byName("create_todo")[0]?.output.includes("buy milk"), "create_todo ran: " + JSON.stringify(c.of("tool_result").map((e) => e.name)));
    assert(byName("write_plan")[0]?.output.includes("trip"), "write_plan ran after approval");
    assert(byName("add_memory")[0]?.output.includes("Memory added"), "add_memory ran after approval");
    assert(byName("get_memories")[0]?.output.includes("likes tea"), "get_memories reads the session store: " + byName("get_memories")[0]?.output);
    const del = byName("delete_plan")[0];
    assert(del && del.is_error && /rejected/.test(del.output) && /nope/.test(del.output), "delete_plan rejected: " + JSON.stringify(del));
    const prompts = byName("user_prompt");
    assert(prompts.length === 2, `user_prompt results: ${prompts.length}`);
    assert(/"accept"/.test(prompts[0].output) && /yes/.test(prompts[0].output), "first prompt accepted: " + prompts[0].output);
    assert(/"decline"/.test(prompts[1].output), "second prompt declined: " + prompts[1].output);
    const rag = byName("search_handbook")[0];
    assert(rag && !rag.is_error && /handbook\/leave\.md/.test(rag.output) && /25 days/.test(rag.output), "rag hit: " + JSON.stringify(rag));
    assert(!/other\/ignored/.test(rag.output), "rag only indexes selected docs");
    assert(c.of("tool_confirmation").length === 3, `confirmations: ${c.of("tool_confirmation").length}`);
    assert(c.of("elicitation").length === 2, `elicitations: ${c.of("elicitation").length}`);
    await session.close();
  });

  await test("abort from inside the onEvent callback cancels the in-flight fetch", async () => {
    const c = collector();
    const session = await dockerAgent.createSession({ yaml: yaml("slow"), env }, (ev) => {
      c.onEvent(ev);
      if (ev.type === "delta") session.abort();
    });
    await rejects(session.send("slow"), /aborted/);
    assert(c.of("delta").length >= 1, "saw the first chunk before aborting");
    assert(c.of("finish").length === 0, "no finish after abort");
    await rejects(session.send("hello"), /aborted/); // slow model again, abort from callback again
    await session.close();
  });

  await test("stateless chat() is cancelled by dockerAgent.abort()", async () => {
    const pending = dockerAgent.chat({ yaml: yaml("slow"), env, messages: [{ role: "user", content: "slow" }] }, (ev) => {
      if (ev.type === "delta") dockerAgent.abort();
    });
    await rejects(pending, /aborted/);
  });

  await test("fetch tool fails closed without toolProxy", async () => {
    const c = collector();
    const session = await dockerAgent.createSession({ yaml: fetchYAML, env, autoApprove: true }, c.onEvent);
    await session.send("https://target.invalid/hello");
    const r = c.of("tool_result")[0];
    assert(r && r.name === "fetch" && r.is_error, "fetch errored: " + JSON.stringify(r));
    assert(/SSRF-guarded HTTP is not supported in the browser/.test(r.output), r.output);
    await session.close();
  });

  await test("fetch tool goes through the HTTPS toolProxy and follows relayed redirects", async () => {
    const c = collector();
    const session = await dockerAgent.createSession({ yaml: fetchYAML, env, autoApprove: true, toolProxy: cfg.proxy + "/proxy" }, c.onEvent);
    await session.send("https://target.invalid/redirect");
    const r = c.of("tool_result")[0];
    assert(r && !r.is_error && /Status: 200/.test(r.output) && /hello from target/.test(r.output), "relayed redirect followed: " + JSON.stringify(r));

    await session.send("https://target.invalid/leaky-redirect");
    const leaky = c.of("tool_result")[1];
    assert(leaky && leaky.is_error && /request failed/.test(leaky.output), "real Location on a proxy redirect is refused: " + JSON.stringify(leaky));
    await session.close();
  });

  await test("browser fetch redirect:error semantics the proxy protocol relies on", async () => {
    const res = await fetch(cfg.proxy + "/302-no-location", { redirect: "error" });
    assert(res.status === 302, `302 without Location is delivered as a response, got ${res.status}`);
    assert(res.headers.get("x-docker-agent-location") === "https://target.invalid/hello", "relayed Location header is exposed through CORS");
    let threw = false;
    try {
      await fetch(cfg.proxy + "/302-location", { redirect: "error" });
    } catch (e) {
      threw = e instanceof TypeError;
    }
    assert(threw, "302 with a real Location is a network error under redirect: error");
  });

  return results;
}

window.__done = main();
