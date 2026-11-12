// boot.js loads web/docker-agent.wasm under Node the way Go's own
// wasm_exec_node.js does, minus its auto-exit, and resolves once the Go side
// has registered globalThis.dockerAgent.
//
// Build the binary first:
//   GOOS=js GOARCH=wasm go build -o cmd/wasm/web/docker-agent.wasm ./cmd/wasm

"use strict";

const { execFileSync } = require("node:child_process");
const fs = require("node:fs");
const path = require("node:path");

// wasm_exec.js is tied to the compiler version, so it must come from the
// toolchain that built the binary.
function goroot() {
  return process.env.GOROOT || execFileSync("go", ["env", "GOROOT"], { encoding: "utf8" }).trim();
}

async function bootDockerAgent() {
  if (globalThis.dockerAgent) return globalThis.dockerAgent;

  globalThis.require = require;
  globalThis.fs = fs;
  globalThis.path = path;
  globalThis.TextEncoder = require("node:util").TextEncoder;
  globalThis.TextDecoder = require("node:util").TextDecoder;
  globalThis.performance ??= require("node:perf_hooks").performance;
  globalThis.crypto ??= require("node:crypto").webcrypto;
  require(path.join(goroot(), "lib", "wasm", "wasm_exec.js"));

  const wasm = fs.readFileSync(path.join(__dirname, "..", "web", "docker-agent.wasm"));
  const go = new Go();
  go.argv = ["docker-agent.wasm"];
  const { instance } = await WebAssembly.instantiate(wasm, go.importObject);
  // main() blocks on select{}, so go.run never settles. Fire and forget.
  go.run(instance);

  for (let i = 0; i < 500 && !globalThis.dockerAgent; i++) {
    await new Promise((r) => setTimeout(r, 10));
  }
  if (!globalThis.dockerAgent) {
    throw new Error("dockerAgent global was never registered");
  }
  return globalThis.dockerAgent;
}

module.exports = { bootDockerAgent };
