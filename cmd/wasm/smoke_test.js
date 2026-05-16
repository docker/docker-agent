// Node-side smoke test for cmd/wasm/web/docker-agent.wasm.
//
// We mirror the polyfill setup from Go's stock wasm_exec_node.js but stop
// short of its auto-exit logic — instead we call dockerAgent.parseConfig
// after the runtime is up and assert two YAML shapes parse correctly:
//   1. A two-agent config (validates the full version-upgrade pipeline).
//   2. An OpenRouter-style custom provider with a slash-laden model name
//      (validates the path our browser demo exercises).
//
// Run with:  node cmd/wasm/smoke_test.js

"use strict";

globalThis.require = require;
globalThis.fs = require("node:fs");
globalThis.path = require("node:path");
globalThis.TextEncoder = require("node:util").TextEncoder;
globalThis.TextDecoder = require("node:util").TextDecoder;
globalThis.performance ??= require("node:perf_hooks").performance;
globalThis.crypto ??= require("node:crypto").webcrypto;

require("/opt/homebrew/Cellar/go/1.26.2/libexec/lib/wasm/wasm_exec.js");

function assert(cond, msg) {
  if (!cond) {
    console.error("ASSERTION FAILED:", msg);
    process.exit(1);
  }
}

(async () => {
  const wasm = fs.readFileSync(__dirname + "/web/docker-agent.wasm");
  const go = new Go();
  go.argv = ["docker-agent.wasm"];
  const { instance } = await WebAssembly.instantiate(wasm, go.importObject);
  // Don't await go.run — main() blocks on `select{}`. Fire and forget.
  go.run(instance);

  for (let i = 0; i < 100 && !globalThis.dockerAgent; i++) {
    await new Promise((r) => setTimeout(r, 10));
  }
  if (!globalThis.dockerAgent) {
    console.error("dockerAgent global was never registered");
    process.exit(1);
  }

  // --- Case 1: two-agent config exercising the version migration path. ---
  const twoAgents = globalThis.dockerAgent.parseConfig(`
version: "2"
agents:
  root:
    model: openai/gpt-4o-mini
    instruction: hi
  helper:
    model: anthropic/claude-3-5-sonnet-latest
    instruction: be helpful
models:
  custom:
    provider: openai
    model: gpt-4o-mini
`);
  assert(Array.isArray(twoAgents.agents) && twoAgents.agents.length === 2,
    "two-agent config should yield two agents");
  assert(
    twoAgents.agents.some((a) => a.name === "helper"),
    "two-agent config should include 'helper'",
  );
  assert(
    twoAgents.models.custom && twoAgents.models.custom.provider === "openai",
    "two-agent config: models.custom.provider should be 'openai'",
  );

  // --- Case 2: OpenRouter custom provider — exact YAML the browser demo
  //     ships in its default textarea, with a slash-laden model name. ---
  let orStyle;
  try {
    orStyle = globalThis.dockerAgent.parseConfig(`
providers:
  openrouter:
    provider: openai
    base_url: https://openrouter.ai/api/v1
    token_key: OPENROUTER_API_KEY
agents:
  root:
    model: openrouter/meta-llama/llama-3.3-70b-instruct:free
    instruction: be helpful
`);
  } catch (e) {
    console.error("OR-style YAML parseConfig threw:");
    console.error(" ", e && e.message ? e.message : e);
    process.exit(1);
  }
  assert(orStyle.agents.length === 1, "OR-style config should have one agent");
  assert(
    orStyle.agents[0].model === "openrouter/meta-llama/llama-3.3-70b-instruct:free",
    "OR-style model field should round-trip with both slashes intact",
  );

  // --- Case 3: listAgents API ---
  const agents = globalThis.dockerAgent.listAgents(`
providers:
  openrouter:
    provider: openai
    base_url: https://openrouter.ai/api/v1
    token_key: OPENROUTER_API_KEY
agents:
  root:
    model: openrouter/meta-llama/llama-3.3-70b-instruct:free
    description: The main agent
    instruction: be helpful
  helper:
    model: openrouter/google/gemma-3-27b-it:free
    description: A helper
    instruction: assist the user
`);
  assert(Array.isArray(agents) && agents.length === 2,
    "listAgents should return 2 agents");
  assert(agents[0].name === "root" && agents[0].description === "The main agent",
    "listAgents[0] should be root with description");
  assert(agents[1].name === "helper",
    "listAgents[1] should be helper");

  // --- Case 4: abort() should be callable without error ---
  assert(typeof globalThis.dockerAgent.abort === "function",
    "abort should be a function");
  globalThis.dockerAgent.abort(); // should not throw

  console.log("OK — all smoke tests pass (parseConfig, listAgents, abort).");
  process.exit(0);
})().catch((e) => {
  console.error("smoke test failed:", e);
  process.exit(1);
});
