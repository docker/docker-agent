// Node-side smoke test for cmd/wasm/web/docker-agent.wasm.
//
// Boots the wasm binary through node/boot.js, then asserts that
// parseConfig and listAgents handle two YAML shapes:
//   1. A two-agent config (validates the full version-upgrade pipeline).
//   2. An OpenRouter-style custom provider with a slash-laden model name
//      (validates the path our browser demo exercises).
//
// Run with:  node cmd/wasm/smoke_test.js
// The JS bridge tests live next door:  node --test cmd/wasm/node/bridge_test.js

"use strict";

const { bootDockerAgent } = require("./node/boot.js");

function assert(cond, msg) {
  if (!cond) {
    console.error("ASSERTION FAILED:", msg);
    process.exit(1);
  }
}

(async () => {
  const dockerAgent = await bootDockerAgent();

  // --- Case 1: two-agent config exercising the version migration path. ---
  const twoAgents = dockerAgent.parseConfig(`
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
    orStyle = dockerAgent.parseConfig(`
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
  const agents = dockerAgent.listAgents(`
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

  // --- Case 4: invalid YAML throws a real Error ---
  let threw = false;
  try {
    dockerAgent.parseConfig("agents: [");
  } catch (e) {
    threw = e instanceof Error;
  }
  assert(threw, "parseConfig should throw an Error on invalid YAML");

  // --- Case 5: abort() should be callable without error ---
  assert(typeof dockerAgent.abort === "function",
    "abort should be a function");
  dockerAgent.abort(); // should not throw
  assert(typeof dockerAgent.createSession === "function",
    "createSession should be a function");

  console.log("OK — all smoke tests pass (parseConfig, listAgents, abort, createSession).");
  process.exit(0);
})().catch((e) => {
  console.error("smoke test failed:", e);
  process.exit(1);
});
