// Run against a build with the small profile (task build-wasm-small).
"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { before, it } = require("node:test");
const { bootDockerAgent } = require("./boot.js");

// The optimized binary is about 34 MiB, versus 42 MiB without wasm-opt.
const sizeBudget = 37 * 1024 * 1024;

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
  const { size } = fs.statSync(path.join(__dirname, "..", "web", "docker-agent.wasm"));
  assert.ok(size <= sizeBudget, `docker-agent.wasm is ${(size / 2 ** 20).toFixed(2)} MiB, over the ${sizeBudget / 2 ** 20} MiB budget`);
});

it("creates an OpenAI session", async () => {
  const session = await dockerAgent.createSession({ yaml, env: { OPENAI_API_KEY: "k" } });
  await session.close();
});

it("creates an Anthropic session", async () => {
  const anthropicYaml = `
agents:
  root:
    model: anthropic/claude-sonnet-4-6
`;
  const session = await dockerAgent.createSession({ yaml: anthropicYaml, env: { ANTHROPIC_API_KEY: "k" } });
  await session.close();
});

it("accepts the current config version only", () => {
  assert.equal(dockerAgent.parseConfig(yaml).agents.length, 1);
  assert.throws(() => dockerAgent.parseConfig(`version: "2"${yaml}`), /unsupported config version/);
  assert.throws(() => dockerAgent.listAgents(`version: "2"${yaml}`), /unsupported config version/);
});
