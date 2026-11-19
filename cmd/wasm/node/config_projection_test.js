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

it("local references on an unselected agent need no discovery", async t => {
  const fetch = t.mock.method(globalThis, "fetch", () => { throw new Error("unexpected fetch"); });
  for (const field of [
    "sub_agents: [root]", "handoffs: [root]",
  ]) {
    const yaml = withHelper(field);
    assert.equal(api.parseConfig(yaml).agents.length, 2);
    const session = await api.createSession({ yaml, agentName: "root", env: { OPENAI_API_KEY: "k" } });
    await session.close();
  }
  assert.equal(fetch.mock.callCount(), 0);
});

it("portable sessions do not ignore security settings on an unselected agent", {
  skip: !process.env.DOCKER_AGENT_TEST_PORTABLE,
}, async () => {
  for (const field of ["safety: strict", "hooks: {post_tool_use: {type: builtin, command: redact_secrets}}"]){
    const yaml = withHelper(field);
    await assert.rejects(api.createSession({ yaml, agentName: "root", env: { OPENAI_API_KEY: "k" } }), Error, field);
  }
});

it("projects Go's reusable toolset and remote MCP fixtures without touching their endpoints", t => {
  const fetch = t.mock.method(globalThis, "fetch", () => { throw new Error("unexpected fetch"); });
  const readFixture = name => readFileSync(path.join(__dirname, "../../../pkg/config/testdata", name + ".yaml"), "utf8");
  for (const [file, names] of [["shared_toolsets", ["dev", "ops"]], ["mcp_definitions_remote", ["root", "override"]]]) {
    const yaml = readFixture(file);
    const agents = names.map(name => ({ name, model: "model", description: "", instruction: "" }));
    assert.deepEqual(api.listAgents(yaml), agents);
    assert.deepEqual(api.parseConfig(yaml), {
      version: "16", models: { model: { provider: "anthropic", model: "claude-sonnet-4-0", base_url: "" } },
      agents: agents.map(a => ({ ...a, sub_agents: [], handoffs: [], add_date: false, add_env_info: false })),
    });
  }
  for (const file of ["shared_toolsets_invalid", "shared_toolsets_missing", "mcp_definitions_invalid_ref", "mcp_definitions_multiple_sources"]) {
    const yaml = readFixture(file);
    assert.throws(() => api.parseConfig(yaml), Error, file);
    assert.throws(() => api.listAgents(yaml), Error, file);
  }
  for (const defs of [
    "toolsets: {a: {type: todo}, a: {type: think}}",
    "mcps: {a: {remote: {url: x}}, a: {remote: {url: y}}}",
    "mcps: {a: {remote: {url: x, headers: {X: a, X: b}}}}",
  ]) {
    assert.throws(() => api.parseConfig(`${defs}\nagents: {root: {model: openai/test}}`), Error, defs);
  }
  const unused = "toolsets: {unused: {type: mcp, ref: missing}}\nagents: {root: {model: openai/test}}";
  assert.equal(api.listAgents(unused)[0].name, "root");
  assert.throws(() => api.listAgents(unused.replace("model: openai/test", "model: openai/test, use_toolsets: [unused]")), Error);
  assert.equal(fetch.mock.callCount(), 0);
});

it("joins instruction paragraphs but does not expose model and welcome metadata", () => {
  const yaml = `models: {m: {provider: openai, model: test, description: Capable, provider_opts: {service_tier: default}}}
agents: {root: {model: m, welcome_message: Welcome, instruction: [One, '', 'Two \${env.WHO}']}}`;
  const a = { name: "root", model: "m", description: "", instruction: "One\n\n\n\nTwo ${env.WHO}" };
  assert.deepEqual(api.listAgents(yaml), [a]);
  assert.deepEqual(api.parseConfig(yaml).models, { m: { provider: "openai", model: "test", base_url: "" } });
  assert.throws(() => api.parseConfig(yaml.replace("One, ''", "One, 42")), Error);
});
