// Run against a build with -tags docker_agent_wasm_rag.
"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { before, it } = require("node:test");
const { bootDockerAgent } = require("./boot.js");

const yaml = fs.readFileSync(path.join(__dirname, "..", "examples", "handbook-rag.yaml"), "utf8");
let dockerAgent;
before(async () => {
  dockerAgent = await bootDockerAgent();
});

it("indexes rag toolsets over the documents option, never over files", async () => {
  const env = { OPENROUTER_API_KEY: "k" };
  await assert.rejects(dockerAgent.createSession({ yaml, env }), /selects none of the supplied documents/);
  await assert.rejects(dockerAgent.createSession({ yaml, env, documents: "handbook" }), /options.documents: must be an object/);
  await assert.rejects(dockerAgent.createSession({ yaml, env, documents: { "handbook/leave.md": 42 } }), /must be a string/);
  const session = await dockerAgent.createSession({ yaml, env, documents: { "handbook/leave.md": "25 days of vacation" } });
  await session.close();
});
