// bridge_test.js exercises the JS surface of cmd/wasm under Node: session
// handles, the stateless chat() call and their failure modes. Node has no
// network from inside the Go wasm runtime, so model calls fail; the tests
// check that failures surface as rejections and events, never as hangs.
//
// Run with:  node --test cmd/wasm/node/bridge_test.js
//
// The wasm binary must be built first (see boot.js).

"use strict";

const assert = require("node:assert/strict");
const { before, describe, it } = require("node:test");
const { bootDockerAgent } = require("./boot.js");

const unreachableAgent = `
providers:
  local:
    provider: openai
    base_url: http://127.0.0.1:9/v1
    token_key: LOCAL_KEY
agents:
  root:
    model: local/test-model
    instruction: hi
`;

let dockerAgent;
before(async () => {
  dockerAgent = await bootDockerAgent();
});

describe("createSession", () => {
  it("returns a handle with the documented methods", async () => {
    const session = await dockerAgent.createSession({ yaml: unreachableAgent, env: { LOCAL_KEY: "k" } });
    for (const name of ["send", "confirm", "respondToElicitation", "restart", "abort", "close"]) {
      assert.equal(typeof session[name], "function", name);
    }
    await session.close();
  });

  it("only sees the env it was given, never the process env", async () => {
    process.env.LOCAL_KEY = "leaked";
    await assert.rejects(dockerAgent.createSession({ yaml: unreachableAgent }), /LOCAL_KEY/);
  });

  it("rejects configs that need a host process or local files", async () => {
    const cases = [
      [`
agents:
  root:
    model: openai/gpt-4o
    toolsets:
      - type: mcp
        command: npx
`, /stdio MCP servers/],
      [`
agents:
  root:
    model: openai/gpt-4o
    toolsets:
      - type: shell
`, /toolset type "shell"/],
      [`
agents:
  root:
    model: openai/gpt-4o
    hooks:
      session_start:
        - type: command
          command: echo
`, /command hooks are not supported/],
      [`
agents:
  root:
    model: dmr/ai/qwen3
`, /provider "dmr"/],
      [`
agents:
  root:
    model: openai/gpt-4o
    add_prompt_files: [README.md]
`, /add_prompt_files/],
      [`
agents:
  root:
    model: openai/gpt-4o
    toolsets:
      - type: memory
        path: ./memory.db
`, /memory path/],
      [`
agents:
  root:
    model: openai/gpt-4o
    toolsets:
      - type: openapi
        url: ./openapi.yaml
`, /only http\(s\) specs/],
    ];
    for (const [yaml, want] of cases) {
      await assert.rejects(dockerAgent.createSession({ yaml, env: { OPENAI_API_KEY: "k" } }), want);
    }
  });

  it("accepts the portable example with its builtin toolsets and features", async () => {
    const yaml = require("node:fs").readFileSync(require("node:path").join(__dirname, "..", "examples", "portable-team.yaml"), "utf8");
    const session = await dockerAgent.createSession({ yaml, env: { OPENROUTER_API_KEY: "k", GITHUB_TOKEN: "t" } });
    await session.close();
  });

  it("indexes rag toolsets over the documents option, never over files", async () => {
    const yaml = require("node:fs").readFileSync(require("node:path").join(__dirname, "..", "examples", "handbook-rag.yaml"), "utf8");
    const env = { OPENROUTER_API_KEY: "k" };
    await assert.rejects(dockerAgent.createSession({ yaml, env }), /selects none of the supplied documents/);
    await assert.rejects(dockerAgent.createSession({ yaml, env, documents: "handbook" }), /options.documents: must be an object/);
    await assert.rejects(dockerAgent.createSession({ yaml, env, documents: { "handbook/leave.md": 42 } }), /must be a string/);
    const session = await dockerAgent.createSession({ yaml, env, documents: { "handbook/leave.md": "25 days of vacation" } });
    await session.close();
  });

  it("rejects an unknown agent name and a non-https tool proxy", async () => {
    const env = { LOCAL_KEY: "k" };
    await assert.rejects(dockerAgent.createSession({ yaml: unreachableAgent, env, agentName: "nope" }), /agent "nope" not found/);
    await assert.rejects(dockerAgent.createSession({ yaml: unreachableAgent, env, toolProxy: "http://proxy.example.com" }), /must use https/);
  });

  it("rejects a concurrent send, aborts, restarts and closes cleanly", async () => {
    const events = [];
    const session = await dockerAgent.createSession({ yaml: unreachableAgent, env: { LOCAL_KEY: "k" } }, (ev) => events.push(ev));

    const first = session.send("hello");
    await assert.rejects(session.send("again"), /run is already active/);
    session.abort();
    await assert.rejects(first, /aborted/);
    assert.ok(!events.some((ev) => ev.type === "finish"), "an aborted turn does not finish");

    await session.restart();
    const second = session.send("after restart");
    session.abort();
    await assert.rejects(second, /aborted/);

    await session.close();
    await session.close();
    await assert.rejects(session.send("late"), /session is closed/);
    await assert.rejects(session.restart(), /session is closed/);
    session.abort();
  });

  it("honours an abort issued synchronously after send", async () => {
    const events = [];
    const session = await dockerAgent.createSession({ yaml: unreachableAgent, env: { LOCAL_KEY: "k" } }, (ev) => events.push(ev));

    // No await in between: the turn must already be registered when abort runs.
    const turn = session.send("hello");
    session.abort();
    await assert.rejects(turn, /aborted/);
    assert.deepEqual(events, [], "a turn aborted before it ran emits nothing");

    // The slot is free again and the session still works.
    await assert.rejects(session.send("again"), /127\.0\.0\.1:9/);
    await session.close();
  });

  it("rejects confirm when no tool confirmation is pending", async () => {
    const session = await dockerAgent.createSession({ yaml: unreachableAgent, env: { LOCAL_KEY: "k" } });
    await assert.rejects(session.confirm(true), /no tool confirmation is pending/);
    await assert.rejects(session.send("hello"), /127\.0\.0\.1:9/);
    await assert.rejects(session.confirm("approve"), /no tool confirmation is pending/);
    await session.close();
  });

  it("surfaces a failed model call as an error event and a rejection", async () => {
    const events = [];
    const session = await dockerAgent.createSession({ yaml: unreachableAgent, env: { LOCAL_KEY: "k" } }, (ev) => events.push(ev));
    await assert.rejects(session.send("hello"), /127\.0\.0\.1:9/);
    assert.ok(events.some((ev) => ev.type === "error" && /127\.0\.0\.1:9/.test(ev.message)), JSON.stringify(events));
    assert.ok(!events.some((ev) => ev.type === "finish"));
    await session.close();
  });

  it("validates confirm and respondToElicitation arguments", async () => {
    const session = await dockerAgent.createSession({ yaml: unreachableAgent, env: { LOCAL_KEY: "k" } });
    await assert.rejects(session.confirm("maybe"), /unknown decision "maybe"/);
    await assert.rejects(session.confirm({ decision: "approve_tool" }), /requires toolName/);
    await assert.rejects(session.respondToElicitation({ action: "accept" }), /id is required/);
    await assert.rejects(session.respondToElicitation({ id: "x", action: "shrug" }), /unknown elicitation action/);
    await assert.rejects(session.respondToElicitation({ id: "stale", action: "decline" }));
    await session.close();
  });

  it("ignores what a session's onEvent returns", async () => {
    // A never-settling promise from every event must not stall the turn.
    const session = await dockerAgent.createSession({ yaml: unreachableAgent, env: { LOCAL_KEY: "k" } }, () => new Promise(() => {}));
    await assert.rejects(session.send("hello"), /127\.0\.0\.1:9/);
    await session.close();
  });

  it("survives an onEvent handler that throws or rejects", async () => {
    let calls = 0;
    const session = await dockerAgent.createSession({ yaml: unreachableAgent, env: { LOCAL_KEY: "k" } }, () => {
      calls++;
      if (calls % 2) throw new Error("boom");
      return Promise.reject(new Error("boom"));
    });
    await assert.rejects(session.send("hello"), /127\.0\.0\.1:9/);
    assert.ok(calls > 0);
    // The rejection is logged off the event loop; a second turn proves nothing wedged.
    await assert.rejects(session.send("again"), /127\.0\.0\.1:9/);
    await session.close();
  });
});

describe("chat", () => {
  it("requires the last message to come from the user", async () => {
    await assert.rejects(
      dockerAgent.chat({ yaml: unreachableAgent, env: { LOCAL_KEY: "k" }, messages: [{ role: "assistant", content: "?" }] }),
      /last message must be from the user/,
    );
    await assert.rejects(dockerAgent.chat({ yaml: unreachableAgent, env: { LOCAL_KEY: "k" }, messages: "nope" }), /messages/);
  });

  it("is cancelled by abort() and does not need an onEvent handler", async () => {
    const pending = dockerAgent.chat({ yaml: unreachableAgent, env: { LOCAL_KEY: "k" }, messages: [{ role: "user", content: "hi" }] });
    dockerAgent.abort();
    await assert.rejects(pending, /aborted/);
  });

  it("carries history with tool calls and reports model errors", async () => {
    const events = [];
    const messages = [
      { role: "user", content: "echo" },
      { role: "assistant", tool_calls: [{ id: "c0", type: "function", function: { name: "echo", arguments: "{}" } }] },
      { role: "tool", tool_call_id: "c0", content: "ok" },
      { role: "user", content: "again" },
    ];
    await assert.rejects(
      dockerAgent.chat({ yaml: unreachableAgent, env: { LOCAL_KEY: "k" }, messages }, (ev) => events.push(ev)),
      /127\.0\.0\.1:9/,
    );
    assert.ok(events.some((ev) => ev.type === "error"));
  });

  it("does not wait on onEvent for events that need no answer", async () => {
    const messages = [{ role: "user", content: "hi" }];
    // A never-settling promise would hang the call if it were awaited; a
    // rejection is logged from the Go side without blocking the event loop.
    await assert.rejects(
      dockerAgent.chat({ yaml: unreachableAgent, env: { LOCAL_KEY: "k" }, messages }, () => new Promise(() => {})),
      /127\.0\.0\.1:9/,
    );
    await assert.rejects(
      dockerAgent.chat({ yaml: unreachableAgent, env: { LOCAL_KEY: "k" }, messages }, () => Promise.reject(new Error("boom"))),
      /127\.0\.0\.1:9/,
    );
  });
});
