// Run against a build with the small profile (task build-wasm-small).
"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const { before, it } = require("node:test");
const { bootDockerAgent, wasmPath } = require("./boot.js");

// The packed binary is 35.7 MiB. Linking a second provider SDK, or an engine
// that defeats the linker's method pruning, costs far more than the headroom.
const sizeBudget = 38 * 1024 * 1024;

const yaml = `
agents:
  root:
    model: openai/gpt-4o-mini
`;

let dockerAgent;
before(async () => {
  dockerAgent = await bootDockerAgent();
});

it("stays within the size budget", () => {
  const { size } = fs.statSync(wasmPath);
  assert.ok(size <= sizeBudget, `docker-agent.wasm is ${(size / 2 ** 20).toFixed(2)} MiB, over the ${sizeBudget / 2 ** 20} MiB budget`);
});

it("can abort a turn immediately, before it emits events", async () => {
  const events = [];
  const session = await dockerAgent.createSession({ yaml, env: { OPENAI_API_KEY: "k" } }, (ev) => events.push(ev));
  try {
    const turn = session.send("hello");
    session.abort();
    await assert.rejects(turn, /aborted/);
    assert.deepEqual(events, []);
  } finally {
    await session.close();
  }
});

it("accepts the current config version only", () => {
  assert.equal(dockerAgent.parseConfig(yaml).agents.length, 1);
  assert.throws(() => dockerAgent.parseConfig(`version: "2"${yaml}`), /unsupported config version/);
  assert.throws(() => dockerAgent.listAgents(`version: "2"${yaml}`), /unsupported config version/);
});

// Shared with the Rust port: new config versions, defaults and projection fields
// must not silently diverge between the browser implementations.
it("projects named agents, named models and custom provider shorthands", () => {
  const yaml = `
providers:
  local: { provider: openai, base_url: https://example.invalid/v1, token_key: LOCAL_KEY }
models:
  writer: { provider: local, model: demo }
agents:
  helper: { model: writer, description: First }
  root: { model: local/meta/demo, instruction: Hi }
`;
  const agents = [
    { name: "helper", model: "writer", description: "First", instruction: "" },
    { name: "root", model: "local/meta/demo", description: "", instruction: "Hi" },
  ];
  assert.deepEqual(dockerAgent.listAgents(yaml), agents);
  assert.deepEqual(dockerAgent.parseConfig(yaml), {
    version: "16",
    agents: agents.map(a => ({ ...a, sub_agents: [], handoffs: [], add_date: false, add_env_info: false })),
    models: {
      writer: { provider: "local", model: "demo", base_url: "" },
      "local/meta/demo": { provider: "local", model: "meta/demo", base_url: "" },
    },
  });
});

it("accepts validated host documents without enabling RAG", async () => {
  const options = { yaml, env: { OPENAI_API_KEY: "k" } };
  const session = await dockerAgent.createSession({ ...options, documents: { "notes.md": "hello" } });
  await session.close();
  for (const [documents, expected] of [
    ["notes.md", /must be an object/], [{ " ": "text" }, /paths must not be empty/],
    [{ "notes.md": 123 }, /must be a string/],
    [Object.fromEntries(Array.from({ length: 1001 }, (_, i) => [i, ""])), /limit of 1000/],
  ]) {
    await assert.rejects(dockerAgent.createSession({ ...options, documents }), expected);
  }
});
