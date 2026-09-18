#!/usr/bin/env node
// Browser smoke test for cmd/wasm: boots web/docker-agent.wasm in headless
// Chrome and drives real sessions over the browser's fetch() against a fake
// OpenAI SSE endpoint and a fake HTTPS egress proxy, both served by this
// process on loopback. Nothing leaves the machine; no npm dependencies.
//
//   node cmd/wasm/browser/smoke_test.js [--build] [--verbose] [--chrome <path>]
//
// Builds web/docker-agent.wasm when missing (or with --build). The
// in-browser scenarios live in page.js. Requires Chrome, openssl and the Go
// toolchain that built the binary (for wasm_exec.js).

"use strict";

const { spawn, execFileSync } = require("node:child_process");
const fs = require("node:fs");
const http = require("node:http");
const https = require("node:https");
const os = require("node:os");
const path = require("node:path");

const root = path.join(__dirname, "..");
const wasmPath = path.join(root, "web", "docker-agent.wasm");
const args = process.argv.slice(2);
const chromeBin = args.includes("--chrome")
  ? args[args.indexOf("--chrome") + 1]
  : process.env.CHROME_BIN || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const TIMEOUT_MS = 240_000;
const verbose = args.includes("--verbose");

function goroot() {
  return process.env.GOROOT || execFileSync("go", ["env", "GOROOT"], { encoding: "utf8" }).trim();
}

function ensureWasm() {
  if (fs.existsSync(wasmPath) && !args.includes("--build")) return;
  console.log("building", path.relative(process.cwd(), wasmPath));
  execFileSync("go", ["build", "-o", wasmPath, "./cmd/wasm"], {
    cwd: path.join(root, "..", ".."),
    env: { ...process.env, GOOS: "js", GOARCH: "wasm" },
    stdio: "inherit",
  });
}

// --- servers -----------------------------------------------------------------

const seen = { auth: new Set(), toolNames: null, proxyURLs: [], leaked: 0, slowClosed: 0 };

function cors(req, res) {
  res.setHeader("Access-Control-Allow-Origin", "*");
  res.setHeader("Access-Control-Allow-Methods", "GET, POST, OPTIONS");
  res.setHeader("Access-Control-Allow-Headers", req.headers["access-control-request-headers"] || "*");
  res.setHeader("Access-Control-Expose-Headers", "X-Docker-Agent-Location");
  if (req.method === "OPTIONS") {
    res.writeHead(204).end();
    return true;
  }
  return false;
}

const sse = (res, obj) => res.write(`data: ${JSON.stringify(obj)}\n\n`);
const chunk = (model, delta, finish = null) => ({
  id: "chatcmpl-fake", object: "chat.completion.chunk", created: 1, model,
  choices: [{ index: 0, delta, finish_reason: finish }],
});
function finishText(res, model, text, usage = { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 }) {
  sse(res, chunk(model, { role: "assistant", content: text }));
  sse(res, chunk(model, {}, "stop"));
  sse(res, { id: "chatcmpl-fake", object: "chat.completion.chunk", created: 1, model, choices: [], usage });
  res.end("data: [DONE]\n\n");
}
function toolCall(res, model, name, args) {
  const call = { index: 0, id: `call_${name}`, type: "function", function: { name, arguments: JSON.stringify(args) } };
  sse(res, chunk(model, { role: "assistant", tool_calls: [call] }));
  sse(res, chunk(model, {}, "tool_calls"));
  res.end("data: [DONE]\n\n");
}

// Scripted by model name; the step within a tool loop is the number of tool
// results the runtime has sent back since the last user message.
const toolScript = [
  ["create_todo", { description: "buy milk" }],
  ["write_plan", { name: "trip", content: "go somewhere" }],
  ["add_memory", { memory: "likes tea" }],
  ["get_memories", {}],
  ["delete_plan", { name: "trip" }],
  ["user_prompt", { message: "ok?" }],
  ["user_prompt", { message: "sure?" }],
  ["search_handbook", { query: "vacation days" }],
];
function chatCompletions(req, res, body) {
  seen.auth.add(req.headers.authorization);
  const model = body.model;
  const users = body.messages.filter((m) => m.role === "user");
  const step = body.messages.slice(body.messages.lastIndexOf(users.at(-1))).filter((m) => m.role === "tool").length;
  res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
  switch (model) {
    case "stream":
      if (users.length === 1) {
        sse(res, chunk(model, { role: "assistant", content: "Hello" }));
        setTimeout(() => finishText(res, model, " world"), 50);
      } else {
        finishText(res, model, `Turn ${users.length} saw ${body.messages.length} messages`);
      }
      return;
    case "slow":
      sse(res, chunk(model, { role: "assistant", content: "tick" }));
      req.on("close", () => seen.slowClosed++);
      setTimeout(() => finishText(res, model, " late"), 20_000).unref();
      return;
    case "tools":
      seen.toolNames ??= (body.tools || []).map((t) => t.function.name);
      if (step < toolScript.length) return toolCall(res, model, ...toolScript[step]);
      return finishText(res, model, "done");
    case "fetch":
      if (step === 0) return toolCall(res, model, "fetch", { urls: [users.at(-1).content] });
      return finishText(res, model, "fetched");
  }
  res.end(`data: ${JSON.stringify({ error: { message: "unknown scenario " + model } })}\n\n`);
}

const files = {
  "/wasm_exec.js": [path.join(goroot(), "lib", "wasm", "wasm_exec.js"), "text/javascript"],
  "/page.js": [path.join(__dirname, "page.js"), "text/javascript"],
  "/docker-agent.wasm": [wasmPath, "application/wasm"],
};
function pageHTML(origins) {
  return `<!doctype html><meta charset="utf-8"><title>docker-agent wasm smoke</title>
<script>window.CFG = ${JSON.stringify(origins)};</script>
<script src="/wasm_exec.js"></script><script src="/page.js"></script>`;
}

function httpHandler(origins) {
  return (req, res) => {
    const url = new URL(req.url, origins.openai);
    if (cors(req, res)) return;
    if (url.pathname === "/") return res.writeHead(200, { "Content-Type": "text/html" }).end(pageHTML(origins));
    if (files[url.pathname]) {
      const [file, type] = files[url.pathname];
      res.writeHead(200, { "Content-Type": type, "Content-Length": fs.statSync(file).size });
      return fs.createReadStream(file).pipe(res);
    }
    if (url.pathname === "/v1/chat/completions" && req.method === "POST") {
      let raw = "";
      req.on("data", (d) => (raw += d));
      req.on("end", () => chatCompletions(req, res, JSON.parse(raw)));
      return;
    }
    res.writeHead(404).end("not found: " + url.pathname);
  };
}

// The fake egress proxy and the endpoints page.js probes redirect semantics
// with. Targets under https://target.invalid/ never exist; the proxy answers
// for them.
function proxyHandler(req, res) {
  const url = new URL(req.url, "https://proxy.local");
  if (cors(req, res)) return;
  const location = { "X-Docker-Agent-Location": "https://target.invalid/hello" };
  const leak = { Location: `https://${req.headers.host}/leaked` };
  switch (url.pathname) {
    case "/302-no-location": return res.writeHead(302, location).end();
    case "/302-location": return res.writeHead(302, leak).end();
    case "/leaked": seen.leaked++; return res.writeHead(200).end("leaked");
    case "/proxy": {
      const target = new URL(url.searchParams.get("url"));
      seen.proxyURLs.push(target.toString());
      switch (target.pathname) {
        case "/hello": return res.writeHead(200, { "Content-Type": "text/plain" }).end("hello from target");
        case "/redirect": return res.writeHead(302, location).end();
        case "/leaky-redirect": return res.writeHead(302, leak).end();
        default: return res.writeHead(404).end();
      }
    }
  }
  res.writeHead(404).end();
}

function listen(server) {
  return new Promise((resolve) => server.listen(0, "127.0.0.1", () => resolve(server.address().port)));
}

function selfSignedCert(dir) {
  execFileSync("openssl", ["req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=127.0.0.1",
    "-addext", "subjectAltName=IP:127.0.0.1", "-keyout", path.join(dir, "key.pem"), "-out", path.join(dir, "cert.pem")], { stdio: "ignore" });
  return { key: fs.readFileSync(path.join(dir, "key.pem")), cert: fs.readFileSync(path.join(dir, "cert.pem")) };
}

// --- chrome over CDP ----------------------------------------------------------

function launchChrome(profile) {
  const chrome = spawn(chromeBin, [
    "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check", "--disable-background-networking",
    "--ignore-certificate-errors", "--remote-debugging-port=0", `--user-data-dir=${profile}`, "about:blank",
  ], { stdio: ["ignore", "ignore", "pipe"] });
  const wsURL = new Promise((resolve, reject) => {
    let err = "";
    chrome.stderr.on("data", (d) => {
      err += d;
      const m = /DevTools listening on (ws:\/\/[^\s]+)/.exec(err);
      if (m) resolve(m[1]);
    });
    chrome.on("error", reject);
    chrome.on("exit", (code) => reject(new Error(`chrome exited (${code}) before DevTools came up:\n${err}`)));
  });
  return { chrome, wsURL };
}

async function pageTarget(wsURL) {
  const { host } = new URL(wsURL);
  for (let i = 0; i < 100; i++) {
    const targets = await fetch(`http://${host}/json/list`).then((r) => r.json());
    const page = targets.find((t) => t.type === "page" && t.webSocketDebuggerUrl);
    if (page) return page.webSocketDebuggerUrl;
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error("no page target found");
}

function cdp(wsURL) {
  const ws = new WebSocket(wsURL);
  const pending = new Map();
  const waiters = new Map();
  let nextID = 1;
  ws.onmessage = ({ data }) => {
    const msg = JSON.parse(data);
    if (msg.id) {
      const { resolve, reject } = pending.get(msg.id);
      pending.delete(msg.id);
      msg.error ? reject(new Error(msg.error.message)) : resolve(msg.result);
    } else if (waiters.has(msg.method)) {
      waiters.get(msg.method)(msg.params);
      waiters.delete(msg.method);
    } else if (msg.method === "Runtime.consoleAPICalled") {
      const line = msg.params.args.map((a) => a.value ?? a.description).join(" ");
      // Go's slog output arrives here too; keep it behind --verbose.
      if (verbose || !/^\d{4}\/\d{2}\/\d{2} /.test(line)) console.log("  [page]", line);
    } else if (msg.method === "Runtime.exceptionThrown") {
      console.log("  [page exception]", msg.params.exceptionDetails.exception?.description || msg.params.exceptionDetails.text);
    }
  };
  const send = (method, params = {}) => new Promise((resolve, reject) => {
    const id = nextID++;
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ id, method, params }));
  });
  const open = new Promise((resolve, reject) => { ws.onopen = resolve; ws.onerror = () => reject(new Error("CDP websocket failed")); });
  const once = (method) => new Promise((resolve) => waiters.set(method, resolve));
  return { open, send, once, close: () => ws.close() };
}

// --- main ---------------------------------------------------------------------

async function main() {
  ensureWasm();
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "docker-agent-wasm-"));
  const cleanup = [() => fs.rmSync(tmp, { recursive: true, force: true })];
  let deadline;
  const timeout = new Promise((_, reject) => { deadline = setTimeout(() => reject(new Error(`timed out after ${TIMEOUT_MS}ms`)), TIMEOUT_MS); });
  try {
    const origins = {};
    const httpServer = http.createServer(httpHandler(origins));
    const proxyServer = https.createServer(selfSignedCert(tmp), proxyHandler);
    cleanup.push(() => httpServer.close(), () => proxyServer.close());
    origins.openai = `http://127.0.0.1:${await listen(httpServer)}`;
    origins.proxy = `https://127.0.0.1:${await listen(proxyServer)}`;
    cleanup.push(() => httpServer.closeAllConnections(), () => proxyServer.closeAllConnections());

    const { chrome, wsURL } = launchChrome(path.join(tmp, "profile"));
    cleanup.push(() => {
      chrome.kill("SIGKILL");
      // The profile can only be removed once Chrome has let go of it.
      return new Promise((r) => (chrome.exitCode === null && chrome.pid ? chrome.once("exit", r) : r()));
    });
    const client = cdp(await pageTarget(await wsURL));
    cleanup.push(client.close);
    await client.open;
    await client.send("Runtime.enable");
    await client.send("Page.enable");
    const loaded = client.once("Page.loadEventFired");
    await client.send("Page.navigate", { url: origins.openai + "/" });
    await loaded;
    console.log("page:", origins.openai, "proxy:", origins.proxy, "wasm:", (fs.statSync(wasmPath).size / 1e6).toFixed(0) + " MB");
    const { result } = await Promise.race([timeout, client.send("Runtime.evaluate", {
      expression: "new Promise((r) => { const t = setInterval(() => { if (window.__done) { clearInterval(t); window.__done.then(r); } }, 50); })",
      awaitPromise: true, returnByValue: true,
    })]);
    const results = result.value;

    // Server-side facts the page cannot observe.
    const check = (cond, name) => results.push({ name, ok: !!cond });
    check([...seen.auth].every((a) => a === "Bearer test-key") && seen.auth.size === 1, "session env reached the provider as Authorization: Bearer <LOCAL_KEY>");
    check(["create_todo", "write_plan", "add_memory", "delete_plan", "user_prompt", "search_handbook"].every((t) => seen.toolNames?.includes(t))
      && !seen.toolNames?.includes("export_plan_to_file"), `tool schema advertised portable tools only (${JSON.stringify(seen.toolNames)})`);
    check(seen.slowClosed >= 3, `abort cancelled the in-flight fetch on the server side (${seen.slowClosed} closed)`);
    const expectProxy = ["https://target.invalid/robots.txt", "https://target.invalid/redirect", "https://target.invalid/hello", "https://target.invalid/leaky-redirect"];
    check(expectProxy.every((u) => seen.proxyURLs.includes(u)), `proxy saw target URLs in ?url= (${JSON.stringify(seen.proxyURLs)})`);
    check(seen.leaked === 0, `browser never followed a real Location off the proxy (${seen.leaked} hits)`);

    let failed = 0;
    for (const r of results) {
      failed += r.ok ? 0 : 1;
      console.log(`${r.ok ? "PASS" : "FAIL"} ${r.name}${r.ms ? ` (${r.ms}ms)` : ""}${r.error ? "\n      " + r.error.split("\n").join("\n      ") : ""}`);
    }
    console.log(failed ? `\n${failed} of ${results.length} browser checks FAILED` : `\nOK — ${results.length} browser checks passed`);
    process.exitCode = failed ? 1 : 0;
  } finally {
    clearTimeout(deadline);
    for (const fn of cleanup.reverse()) { try { await fn(); } catch { /* best effort */ } }
  }
}

process.on("SIGINT", () => process.exit(130));
main().catch((e) => { console.error("smoke test failed:", e); process.exit(1); });
