// Exercise the demo's actual run callback, not a duplicate event reducer.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const html = fs.readFileSync(path.join(__dirname, "index.html"), "utf8");
const run = html.slice(html.indexOf("async function run() {"), html.indexOf("// renderAssistant rewrites"));
const render = html.slice(html.indexOf("function renderAssistant("), html.indexOf("// ---- Abort button"));

test("reasoning reset clears only the identified message in the HTML callback", async () => {
  const bubble = { innerHTML: "", classList: { add() {}, remove() {} } };
  const tools = [];
  const emit = [
    { type: "delta", content: "KEPT-TEXT", message_id: "answer" },
    { type: "delta", reasoning: "DECLINED", message_id: "answer" },
    { type: "delta", reasoning: "OTHER-MESSAGE", message_id: "other" },
    { type: "tool_call", id: "kept-tool", name: "shell", args: {} },
    { type: "reasoning_reset", message_id: "answer" },
    { type: "delta", reasoning: "FALLBACK", message_id: "answer" },
  ];
  const frames = [];
  const context = vm.createContext({
    finish: {},
    conversation: [],
    getYaml: () => "agents: {}",
    getKey: () => "",
    agentPicker: { value: "root" },
    setStreaming() {},
    promptEl: { value: "question", focus() {} },
    appendMessage: () => bubble,
    appendSystemMessage: (text) => tools.push(text),
    scrollToBottomIfPinned() {},
    renderMarkdown: (s) => s,
    dockerAgent: {
      parseConfig() {},
      async chat(_opts, onEvent) {
        for (const event of emit) {
          onEvent(event);
          frames.push(bubble.innerHTML);
        }
      },
    },
  });
  await vm.runInContext(`${render}\n${run}\nrun()`, context);
  assert.match(frames[1], /DECLINED/);
  assert.match(frames[4], /KEPT-TEXT/);
  assert.match(frames[4], /OTHER-MESSAGE/);
  assert.doesNotMatch(frames[4], /DECLINED/);
  assert.match(frames[5], /FALLBACK/);
  assert.doesNotMatch(frames[5], /DECLINED/);
  assert.deepEqual(tools, ["🔧 Calling tool: **shell**"]);
});
