// Run against both the Go small and Rust portable builds.
"use strict";

const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const path = require("node:path");
const { before, it } = require("node:test");
const { bootDockerAgent } = require("./boot.js");

let api;
before(async () => { api = await bootDockerAgent(); });

it("projects local team relationships without resolving or executing the team", () => {
  const yaml = `agents:
  helper:
    model: openai/test
    description: Helps
    sub_agents: [root]
    add_environment_info: true
  root:
    model: openai/test
    instruction: 'Delegate for \${env.WHO}'
    sub_agents: [helper, helper]
    handoffs: [helper, root]
    add_date: true
    toolsets: [{type: fetch}, {type: think}]
`;
  const agents = [
    { name: "helper", model: "openai/test", description: "Helps", instruction: "" },
    { name: "root", model: "openai/test", description: "", instruction: "Delegate for ${env.WHO}" },
  ];
  assert.deepEqual(api.listAgents(yaml), agents);
  assert.deepEqual(api.parseConfig(yaml), {
    version: "16",
    models: { "openai/test": { provider: "openai", model: "test", base_url: "" } },
    agents: [
      { ...agents[0], sub_agents: ["root"], handoffs: [], add_date: false, add_env_info: true },
      { ...agents[1], sub_agents: ["helper", "helper"], handoffs: ["helper", "root"], add_date: true, add_env_info: false },
    ],
  });
});

it("can inspect the shared todo and shared plan examples", () => {
  for (const [file, field, names, refs] of [
    ["shared-todo.yaml", "sub_agents", ["root", "first", "second"], [["first", "second"], [], []]],
    ["shared_plan.yaml", "handoffs", ["root", "architect", "builder"], [["architect", "builder"], ["builder"], ["root"]]],
  ]) {
    const yaml = readFileSync(path.join(__dirname, "../../../examples", file), "utf8");
    assert.deepEqual(api.listAgents(yaml).map(a => a.name), names, file);
    assert.deepEqual(api.parseConfig(yaml).agents.map(a => a[field]), refs, file);
  }
});

function withHelper(field) {
  return `agents:\n  root: {model: openai/test}\n  helper:\n    model: openai/test\n    ${field}`;
}

it("still validates references, fields and toolsets during projection", () => {
  for (const field of [
    "sub_agents: [missing]", "handoffs: [missing]", "add_date: not-a-bool",
    "sub_agent: [root]", "toolsets: [{type: todo, typo: true}]",
    "toolsets: [{type: shell, shared: true}]", "toolsets: [{type: mcp}]",
  ]) {
    const yaml = withHelper(field);
    assert.throws(() => api.parseConfig(yaml), Error, field);
    assert.throws(() => api.listAgents(yaml), Error, field);
  }
});

it("session loading checks capabilities even on an unselected agent", async t => {
  const fetch = t.mock.method(globalThis, "fetch", () => { throw new Error("unexpected fetch"); });
  for (const field of [
    "add_date: true", "add_environment_info: true",
  ]) {
    const yaml = withHelper(field);
    assert.equal(api.parseConfig(yaml).agents.length, 2);
    const opening = api.createSession({ yaml, agentName: "root", env: { OPENAI_API_KEY: "k" } });
    if (process.env.DOCKER_AGENT_TEST_PORTABLE) {
      await assert.rejects(opening, /unsupported|not supported/, field);
    } else {
      const session = await opening;
      await session.close();
    }
  }
  assert.equal(fetch.mock.callCount(), 0);
});

it("portable sessions do not ignore security settings on an unselected agent", {
  skip: !process.env.DOCKER_AGENT_TEST_PORTABLE,
}, async () => {
  for (const field of ["readonly: true", "safety: strict", "redact_secrets: true"]) {
    const yaml = withHelper(field);
    await assert.rejects(api.createSession({ yaml, agentName: "root", env: { OPENAI_API_KEY: "k" } }), Error, field);
  }
});
