import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { createReadStream, readdirSync } from 'node:fs';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const chrome = process.env.CHROME_BIN || (process.platform === 'darwin' ? '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome' : 'google-chrome');
const root = fileURLToPath(new URL('../../../../', import.meta.url));
const goroot = execFileSync('go', ['env', 'GOROOT'], { encoding: 'utf8' }).trim();
const files = new Map([
  ['/go.wasm', [join(root, 'cmd/wasm/web/docker-agent.wasm'), 'application/wasm']],
  ['/wasm_exec.js', [join(goroot, 'lib/wasm/wasm_exec.js'), 'text/javascript']],
  ...readdirSync(join(root, 'bin/wasm-portable')).filter(f => f.endsWith('.mjs')).map(f => ['/' + f, [join(root, 'bin/wasm-portable', f), 'text/javascript']]),
  ['/docker-agent.wasm', [join(root, 'bin/wasm-portable/docker-agent.wasm'), 'application/wasm']],
  ['/replay.mjs', [new URL('./replay.mjs', import.meta.url), 'text/javascript']],
]);

async function capture(runtime) {
  const dir = await mkdtemp(join(tmpdir(), `docker-agent-${runtime}-`));
  let resolve, reject;
  const promise = new Promise((ok, fail) => { resolve = ok; reject = fail; });
  const server = createServer(async (req, res) => {
    try {
      if (req.method === 'POST' && req.url === '/trace') {
        let text = '';
        for await (const chunk of req) text += chunk;
        const { tests, error } = JSON.parse(text);
        res.writeHead(204).end();
        if (error) reject(new Error(error)); else resolve(tests);
      } else if (req.url === `/${runtime}`) {
        res.setHeader('Content-Security-Policy', "default-src 'self'; script-src 'self' 'unsafe-eval'"); // wasm_exec.js uses eval
        res.writeHead(200, { 'Content-Type': 'text/html' }).end('<!doctype html><script type="module" src="/replay.mjs"></script>');
      } else if (files.has(req.url)) {
        const [file, mime] = files.get(req.url);
        res.setHeader('Content-Type', mime);
        createReadStream(file).on('error', e => { res.destroy(); reject(e); }).pipe(res);
      } else { res.writeHead(404).end(); }
    } catch (e) { res.destroy(); reject(e); }
  });
  let child, exited, timeout;
  try {
    await new Promise((ok, fail) => server.once('error', fail).listen(0, '127.0.0.1', ok));
    child = spawn(chrome, ['--headless', '--disable-gpu', '--no-first-run', '--disable-background-networking',
      `--user-data-dir=${dir}`, `http://127.0.0.1:${server.address().port}/${runtime}`], { stdio: 'ignore' });
    exited = new Promise(ok => child.once('close', code => { ok(); reject(new Error(`Chrome exited (${code})`)); }));
    child.once('error', reject);
    timeout = setTimeout(() => reject(new Error(`${runtime} parity replay timed out`)), 45_000);
    return await promise;
  } finally {
    clearTimeout(timeout);
    child?.kill('SIGKILL');
    await exited;
    server.closeAllConnections();
    await new Promise(ok => server.close(ok));
    await rm(dir, { recursive: true, force: true, maxRetries: 3 });
  }
}

try {
  const reference = await capture('go');
  await writeFile(join(root, 'bin/parity-go.json'), JSON.stringify(reference, null, 2));
  const portable = await capture('rust');
  await writeFile(join(root, 'bin/parity-rust.json'), JSON.stringify(portable, null, 2));
  assert.deepEqual(Object.keys(portable), Object.keys(reference));
  for (const [name, trace] of Object.entries(reference)) {
    try { assert.deepEqual(portable[name], trace); }
    catch (error) { console.error(`FAIL ${name}: see bin/parity-{go,rust}.json`); throw error; }
    console.log(`PASS ${name}: ${trace.requests.length} requests, ${trace.events.length} events, ${trace.results.length} results`);
  }
} catch (e) {
  console.error(e);
  process.exitCode = 1;
}
