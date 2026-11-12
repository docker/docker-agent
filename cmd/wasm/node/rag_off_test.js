// Run against the default build (without docker_agent_wasm_rag).
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

it("rejects RAG configs before checking provider credentials", async () => {
  for (const documents of [undefined, {}, { "handbook/leave.md": "25 days of vacation" }]) {
    await assert.rejects(dockerAgent.createSession({ yaml, documents }), /toolset type "rag"/);
  }
});

it("keeps the documents input validation without RAG", async () => {
  await assert.rejects(dockerAgent.createSession({ yaml, documents: "handbook" }), /options.documents: must be an object/);
  await assert.rejects(dockerAgent.createSession({ yaml, documents: { "handbook/leave.md": 42 } }), /must be a string/);
});
