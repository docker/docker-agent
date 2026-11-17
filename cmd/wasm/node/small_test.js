// Run against a build with the small profile (task build-wasm-small).
"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { before, it } = require("node:test");
const { bootDockerAgent } = require("./boot.js");

// The packed binary is 35.7 MiB. Linking a second provider SDK, or an engine
// that defeats the linker's method pruning, costs far more than the headroom.
const sizeBudget = 38 * 1024 * 1024;

const agent = (lines) => `
agents:
  root:
    model: openai/gpt-4o-mini
${lines}`;

let dockerAgent;
before(async () => {
  dockerAgent = await bootDockerAgent();
});

it("stays within the size budget", () => {
  const { size } = fs.statSync(path.join(__dirname, "..", "web", "docker-agent.wasm"));
  assert.ok(size <= sizeBudget, `docker-agent.wasm is ${(size / 2 ** 20).toFixed(2)} MiB, over the ${sizeBudget / 2 ** 20} MiB budget`);
});

it("accepts the small example", async () => {
  const yaml = fs.readFileSync(path.join(__dirname, "..", "examples", "small-team.yaml"), "utf8");
  const session = await dockerAgent.createSession({ yaml, env: { OPENROUTER_API_KEY: "k", GITHUB_TOKEN: "t", USER_NAME: "Ada" } });
  await session.close();
});

it("accepts the current config version only", () => {
  assert.equal(dockerAgent.parseConfig(agent("")).agents.length, 1);
  assert.throws(() => dockerAgent.parseConfig(`version: "2"${agent("")}`), /unsupported config version/);
  assert.throws(() => dockerAgent.listAgents(`version: "2"${agent("")}`), /unsupported config version/);
});

it("rejects what the profile leaves out when the session is created", async () => {
  const env = { OPENAI_API_KEY: "k", ANTHROPIC_API_KEY: "k", GOOGLE_API_KEY: "k" };
  const cases = [
    [agent("").replace("openai/gpt-4o-mini", "anthropic/claude-sonnet-4-6"), /provider "anthropic"/],
    [agent("").replace("openai/gpt-4o-mini", "google/gemini-2.5-flash"), /provider "google"/],
    [agent(`    toolsets:
      - type: openapi
        url: https://example.com/openapi.json
`), /toolset type "openapi"/],
    [agent("    code_mode_tools: true\n"), /feature "code_mode"/],
    [agent(`    hooks:
      turn_start:
        - type: builtin
          command: add_context
          args: ["Be brief."]
`), /builtin "add_context" is not available/],
  ];
  for (const [yaml, want] of cases) {
    await assert.rejects(dockerAgent.createSession({ yaml, env }), want);
  }
});
