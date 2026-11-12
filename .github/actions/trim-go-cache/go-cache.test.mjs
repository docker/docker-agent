import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { lstat, mkdir, mkdtemp, readFile, readdir, realpath, rm, symlink, utimes, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { basename, dirname, join, sep } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';

import { run } from './go-cache.mjs';

const exec = promisify(execFile);
const script = fileURLToPath(new URL('./go-cache.mjs', import.meta.url));
const markScript = fileURLToPath(new URL('./mark.mjs', import.meta.url));
const pruneScript = fileURLToPath(new URL('./prune.mjs', import.meta.url));
const epoch = Date.UTC(2025, 0, 2, 12);
const hour = 60 * 60 * 1000;

async function fixture(t) {
  const base = await mkdtemp(join(tmpdir(), 'go-cache-test-'));
  t.after(() => rm(base, { recursive: true, force: true }));
  const root = join(base, 'cache with spaces');
  await mkdir(root);
  return { base, root, marker: join(base, 'marker') };
}

async function entry(root, id, kind = 'd', content = 'cache', directory = false) {
  const hash = id.padEnd(64, '0');
  const path = join(root, hash.slice(0, 2), `${hash}-${kind}`);
  await mkdir(dirname(path), { recursive: true });
  if (directory) {
    await mkdir(path);
  } else {
    await writeFile(path, content);
  }
  return path;
}

async function setTime(path, time) {
  const date = new Date(time);
  await utimes(path, date, date);
}

async function exists(path) {
  try {
    await lstat(path);
    return true;
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
    return false;
  }
}

async function markAction(root, runnerTemp, stateFile) {
  const { stdout, stderr } = await exec(process.execPath, [markScript], {
    env: { ...process.env, INPUT_PATH: root, RUNNER_TEMP: runnerTemp, GITHUB_STATE: stateFile },
  });
  assert.equal(stderr, '');
  const state = Object.fromEntries((await readFile(stateFile, 'utf8')).trimEnd().split('\n').map((line) => {
    const separator = line.indexOf('=');
    return [`STATE_${line.slice(0, separator)}`, line.slice(separator + 1)];
  }));
  return { state, result: JSON.parse(stdout) };
}

async function pruneAction(state) {
  const { stdout, stderr } = await exec(process.execPath, [pruneScript], {
    env: { ...process.env, ...state },
  });
  assert.equal(stderr, '');
  return JSON.parse(stdout);
}

async function makeLink(t, target, path, directory = false) {
  try {
    await symlink(target, path, directory && process.platform === 'win32' ? 'junction' : directory ? 'dir' : 'file');
    return true;
  } catch (error) {
    if (process.platform !== 'win32' || !['EPERM', 'EACCES', 'ENOTSUP'].includes(error.code)) throw error;
    t.skip('Symlink creation requires privileges on this Windows host');
    return false;
  }
}

test('mark ages entries before publishing the marker and prune keeps touched/new entries', async (t) => {
  const { root, marker } = await fixture(t);
  const cold = await entry(root, 'aa1', 'a', 'cold');
  const hot = await entry(root, 'aa2', 'd', 'hot');
  await setTime(cold, epoch + hour);
  await setTime(hot, epoch);
  const before = await lstat(hot);
  let calls = 0;
  const marked = await run('mark', root, {
    marker,
    now: () => {
      if (calls++ === 0) return epoch;
      return epoch + 1000;
    },
  });
  assert.equal(calls, 2);
  assert.equal(marked.marked, 2);
  assert.equal(marked.markerMtimeMs, epoch + 1000);
  for (const path of [cold, hot]) {
    assert.equal((await lstat(path)).mtimeMs, epoch - 2 * hour);
  }
  assert.equal((await lstat(hot)).atimeMs, before.atimeMs);
  await setTime(hot, epoch + 2000);
  const fresh = await entry(root, 'bb1', 'd', 'new');
  await setTime(fresh, epoch + 3000);

  const pruned = await run('prune', root, { marker });
  assert.equal(pruned.scanned, 3);
  assert.equal(pruned.removed, 1);
  assert.equal(pruned.removedBytes, 4);
  assert.equal(await exists(cold), false);
  assert.equal(await readFile(hot, 'utf8'), 'hot');
  assert.equal(await readFile(fresh, 'utf8'), 'new');
  assert.equal((await run('prune', root, { marker })).removed, 0);
});

test('prune uses a strict mtime boundary, retaining equality', async (t) => {
  const { root, marker } = await fixture(t);
  await writeFile(marker, '');
  await setTime(marker, epoch);
  const cutoff = (await lstat(marker)).mtimeMs;
  const older = await entry(root, 'bb1');
  const equal = await entry(root, 'bb2');
  const newer = await entry(root, 'bb3');
  await setTime(older, cutoff - 1000);
  await setTime(equal, cutoff);
  await setTime(newer, cutoff + 1000);
  const result = await run('prune', root, { marker });
  assert.equal(result.removed, 1);
  assert.equal(await exists(older), false);
  assert.equal((await lstat(equal)).mtimeMs, cutoff);
  assert.equal(await exists(newer), true);
});

test('executable directory mtime controls retention and recursive payload bytes', async (t) => {
  const { root, marker } = await fixture(t);
  const cold = await entry(root, 'cc1', 'd', '', true);
  const hot = await entry(root, 'cc2', 'd', '', true);
  const binary = join(hot, 'program.exe');
  await writeFile(binary, 'binary', { mode: 0o755 });
  await mkdir(join(cold, 'nested', 'deeper'), { recursive: true });
  await writeFile(join(cold, 'nested', 'deeper', 'program'), 'cold executable', { mode: 0o755 });
  const mode = (await lstat(binary)).mode;
  const childTime = (await lstat(binary)).mtimeMs;

  await run('mark', root, { marker, now: () => epoch });
  assert.equal((await lstat(binary)).mtimeMs, childTime);
  assert.equal((await lstat(binary)).mode, mode);
  if (process.platform !== 'win32') assert.equal(mode & 0o111, 0o111);
  await setTime(hot, epoch + 1000);
  const stats = await run('stats', root);
  assert.deepEqual(stats.entries.executable, { count: 2, bytes: 21 });
  const pruned = await run('prune', root, { marker });
  assert.equal(pruned.removed, 1);
  assert.equal(pruned.removedBytes, 15);
  assert.equal(await exists(cold), false);
  assert.equal(await readFile(binary, 'utf8'), 'binary');
  assert.equal((await lstat(binary)).mode, mode);
});

test('only lowercase hex buckets and matching 64-hex a/d names are managed', async (t) => {
  const { root, marker } = await fixture(t);
  await entry(root, 'ab1', 'a', 'ok');
  const malformed = [
    ['zz', `${'z'.repeat(64)}-d`],
    ['AB', `${'AB'.padEnd(64, '0')}-d`],
    ['a', `${'a'.repeat(64)}-d`],
    ['abc', `${'a'.repeat(64)}-d`],
    ['ab', 'short-d'],
    ['ab', `${'ab'.padEnd(64, 'z')}-d`],
    ['ab', `${'ab'.padEnd(64, '0')}-x`],
    ['ab', `${'ab'.padEnd(65, '0')}-a`],
    ['ab', `${'cd'.padEnd(64, '0')}-d`],
    ...(process.platform === 'win32' ? [] : [['ab', `${'ab'.padEnd(64, '0')}-d\n`]]),
  ];
  for (const [bucket, name] of malformed) {
    await mkdir(join(root, bucket), { recursive: true });
    await writeFile(join(root, bucket, name), 'untouched');
    await setTime(join(root, bucket, name), epoch + hour);
  }
  await writeFile(join(root, 'README'), 'metadata');
  await writeFile(join(root, 'ee'), 'not a bucket');
  await run('mark', root, { marker, now: () => epoch });
  assert.equal((await run('stats', root)).count, 1);
  assert.equal((await run('prune', root, { marker })).removed, 1);
  for (const [bucket, name] of malformed) {
    assert.equal((await lstat(join(root, bucket, name))).mtimeMs, epoch + hour);
  }
  assert.equal(await readFile(join(root, 'README'), 'utf8'), 'metadata');
  assert.equal(await readFile(join(root, 'ee'), 'utf8'), 'not a bucket');
});

test('bucket and directory-entry symlinks are neither followed nor removed', async (t) => {
  const { root, base, marker } = await fixture(t);
  const outside = join(base, 'outside');
  await mkdir(outside);
  const target = await entry(outside, 'aa1', 'd', 'outside');
  await setTime(target, epoch - hour);
  if (!await makeLink(t, join(outside, 'aa'), join(root, 'aa'), true)) return;
  await mkdir(join(root, 'bb'));
  const link = join(root, 'bb', `${'bb'.padEnd(64, '0')}-d`);
  if (!await makeLink(t, outside, link, true)) return;
  assert.equal((await run('mark', root, { marker, now: () => epoch })).marked, 0);
  assert.equal((await run('stats', root)).count, 0);
  assert.equal((await run('prune', root, { marker })).removed, 0);
  assert.equal((await lstat(target)).mtimeMs, epoch - hour);
  assert.equal(await readFile(target, 'utf8'), 'outside');
  assert.equal((await lstat(link)).isSymbolicLink(), true);
});

test('file-entry symlinks, including dangling links, are ignored', async (t) => {
  const { root, base, marker } = await fixture(t);
  const outside = join(base, 'outside');
  await writeFile(outside, 'outside');
  await setTime(outside, epoch - hour);
  await mkdir(join(root, 'dd'));
  const link = join(root, 'dd', `${'dd1'.padEnd(64, '0')}-a`);
  if (!await makeLink(t, outside, link)) return;
  const dangling = join(root, 'dd', `${'dd2'.padEnd(64, '0')}-d`);
  if (!await makeLink(t, join(base, 'absent'), dangling)) return;
  assert.equal((await run('mark', root, { marker, now: () => epoch })).marked, 0);
  assert.equal((await run('stats', root)).bytes, 0);
  assert.equal((await run('prune', root, { marker })).removed, 0);
  assert.equal((await lstat(outside)).mtimeMs, epoch - hour);
  assert.equal((await lstat(link)).isSymbolicLink(), true);
  assert.equal((await lstat(dangling)).isSymbolicLink(), true);
});

test('recursive removal unlinks nested symlinks without visiting their targets', async (t) => {
  const { root, base, marker } = await fixture(t);
  const executable = await entry(root, 'ee1', 'd', '', true);
  await writeFile(join(executable, 'program'), 'exe');
  const outside = join(base, 'outside');
  await mkdir(outside);
  const sentinel = join(outside, 'sentinel');
  await writeFile(sentinel, 'do not delete');
  await setTime(sentinel, epoch - hour);
  if (!await makeLink(t, outside, join(executable, 'external'), true)) return;
  if (!await makeLink(t, executable, join(executable, 'cycle'), true)) return;
  await run('mark', root, { marker, now: () => epoch });
  assert.equal((await run('stats', root)).bytes, 3);
  const pruned = await run('prune', root, { marker });
  assert.equal(pruned.removedBytes, 3);
  assert.equal(await exists(executable), false);
  assert.equal(await readFile(sentinel, 'utf8'), 'do not delete');
  assert.equal((await lstat(sentinel)).mtimeMs, epoch - hour);
});

test('all modes reject symlink roots, including a trailing separator', async (t) => {
  const { root, base, marker } = await fixture(t);
  const target = await entry(root, 'ff1');
  await setTime(target, epoch);
  const link = join(base, 'linked-root');
  if (!await makeLink(t, root, link, true)) return;
  for (const mode of ['mark', 'prune', 'stats']) {
    for (const path of [link, `${link}${sep}`]) {
      await assert.rejects(run(mode, path, { marker }), /Cache root must be a directory, not a symlink/);
    }
  }
  assert.equal((await lstat(target)).mtimeMs, epoch);
  assert.equal(await exists(marker), false);
});

test('missing marker fails before prune can remove any entries', async (t) => {
  const { root, marker } = await fixture(t);
  const path = await entry(root, 'ff1');
  await setTime(path, epoch - hour);
  await assert.rejects(run('prune', root, { marker }), { code: 'ENOENT' });
  assert.equal(await exists(path), true);
  assert.equal((await lstat(path)).mtimeMs, epoch - hour);
});

test('marker validation prevents following links or modifying a cache entry as marker', async (t) => {
  const { root, base } = await fixture(t);
  const path = await entry(root, 'ff1');
  const marker = join(base, 'directory-marker');
  await mkdir(marker);
  for (const mode of ['mark', 'prune']) {
    await assert.rejects(run(mode, root, { marker }), /Marker must be a regular file/);
    await assert.rejects(run(mode, root, { marker: path }), /inside a cache bucket/);
    await assert.rejects(run(mode, root, { marker: root }), /cache root/);
  }
  const linked = join(base, 'linked-marker');
  if (!await makeLink(t, path, linked)) return;
  for (const mode of ['mark', 'prune']) {
    await assert.rejects(run(mode, root, { marker: linked }), /Marker must be a regular file/);
  }
  assert.equal(await readFile(path, 'utf8'), 'cache');
});

test('default marker, repeated marking, and empty caches work', async (t) => {
  const { root } = await fixture(t);
  const marked = await run('mark', root, { now: () => epoch });
  assert.equal(marked.marked, 0);
  assert.equal(marked.markerMtimeMs, epoch);
  assert.equal((await lstat(marked.marker)).isFile(), true);
  const repeated = await run('mark', root, { now: () => epoch + hour });
  assert.equal(repeated.markerMtimeMs, epoch + hour);
  assert.equal((await run('prune', root)).removed, 0);
  const stats = await run('stats', root);
  assert.equal(stats.count, 0);
  assert.equal(stats.bytes, 0);
  assert.deepEqual(stats.largest, []);
  assert.deepEqual(await readdir(root), ['.go-cache-marker']);
});

test('failed mark removes a stale marker and does not publish an incomplete one', async (t) => {
  const { root, marker } = await fixture(t);
  const path = await entry(root, 'aa1');
  await writeFile(marker, 'stale');
  await setTime(marker, epoch - hour);
  let calls = 0;
  await assert.rejects(run('mark', root, {
    marker,
    now: () => {
      if (calls++ === 0) return epoch;
      throw new Error('clock failed');
    },
  }), /clock failed/);
  assert.equal((await lstat(path)).mtimeMs, epoch - 2 * hour);
  assert.equal(await exists(marker), false);
  await assert.rejects(run('prune', root, { marker }), { code: 'ENOENT' });
});

test('missing roots, regular-file roots, and relative roots fail without creating a marker', async (t) => {
  const { base, marker } = await fixture(t);
  const file = join(base, 'file');
  await writeFile(file, 'not a cache');
  for (const mode of ['mark', 'prune', 'stats']) {
    await assert.rejects(run(mode, file, { marker }), /Cache root must be a directory/);
    await assert.rejects(run(mode, join(base, 'missing'), { marker }), { code: 'ENOENT' });
    await assert.rejects(run(mode, '.', { marker }), /absolute path/);
  }
  assert.equal(await exists(marker), false);
});

test('inventory counts payloads and returns the top 20 in stable size/path order', async (t) => {
  const { root } = await fixture(t);
  const expected = [];
  for (let i = 24; i >= 0; i--) {
    const path = await entry(root, `ab${i.toString(16).padStart(2, '0')}`, i % 2 ? 'a' : 'd', 'x'.repeat(i % 5));
    expected.push({ path: `ab/${basename(path)}`, bytes: i % 5 });
  }
  const stats = await run('stats', root);
  expected.sort((a, b) => b.bytes - a.bytes || (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  assert.equal(stats.count, 25);
  assert.equal(stats.bytes, 50);
  assert.deepEqual(stats.entries.a, { count: 12, bytes: 24 });
  assert.deepEqual(stats.entries.d, { count: 13, bytes: 26 });
  assert.deepEqual(stats.largest, expected.slice(0, 20));
  assert.deepEqual((await run('stats', root)).largest, stats.largest);
});

test('CLI modes emit only JSON without any shell tools on PATH', async (t) => {
  const { root, base } = await fixture(t);
  await entry(root, 'aa1', 'a', 'payload');
  const marker = join(base, 'marker with spaces');
  const options = { env: { ...process.env, PATH: '' } };
  for (const mode of ['stats', 'mark', 'prune']) {
    const { stdout, stderr } = await exec(process.execPath, [script, mode, root, '--marker', marker], options);
    assert.equal(stderr, '');
    assert.equal(stdout.trim().split('\n').length, 1);
    const result = JSON.parse(stdout);
    assert.equal(result.mode, mode);
    if (mode === 'prune') {
      assert.equal(result.removed, 1);
      assert.equal(result.removedBytes, 7);
    }
  }
});

test('action wrappers round-trip paths through runner state and remove the marker after pruning', async (t) => {
  const { root, base } = await fixture(t);
  const cold = await entry(root, 'aa1', 'a', 'cold');
  const hot = await entry(root, 'aa2', 'd', 'hot');
  const runnerTemp = join(base, 'runner temp=with spaces');
  await mkdir(runnerTemp);
  const input = `${root}${sep}`;
  const { state, result: marked } = await markAction(input, runnerTemp, join(base, 'action state'));
  assert.equal(state['STATE_cache-root'], input);
  assert.equal(await realpath(state.STATE_marker), marked.marker);
  assert.equal(dirname(state.STATE_marker), runnerTemp);
  assert.match(basename(state.STATE_marker), /^go-cache-[0-9a-f-]{36}$/);
  assert.equal(marked.marked, 2);
  assert.equal((await lstat(state.STATE_marker)).isFile(), true);
  assert.equal(await exists(join(root, '.go-cache-marker')), false);
  await setTime(hot, marked.markerMtimeMs + 1000);
  const fresh = await entry(root, 'bb1', 'd', 'new');
  await setTime(fresh, marked.markerMtimeMs + 1000);

  const pruned = await pruneAction(state);
  assert.equal(pruned.scanned, 3);
  assert.equal(pruned.removed, 1);
  assert.equal(pruned.removedBytes, 4);
  assert.equal(await exists(cold), false);
  assert.equal(await readFile(hot, 'utf8'), 'hot');
  assert.equal(await readFile(fresh, 'utf8'), 'new');
  assert.equal(await exists(state.STATE_marker), false);
});

test('mark wrapper creates missing roots and keeps action marker state independent', async (t) => {
  const { base } = await fixture(t);
  const actions = [];
  for (const name of ['first cache', 'second cache']) {
    const root = join(base, name);
    const marked = await markAction(root, base, join(base, `${name}.state`));
    assert.equal((await lstat(root)).isDirectory(), true);
    assert.equal(marked.result.marked, 0);
    assert.equal(marked.state['STATE_cache-root'], root);
    actions.push(marked);
  }
  assert.notEqual(actions[0].state.STATE_marker, actions[1].state.STATE_marker);
  assert.equal((await pruneAction(actions[0].state)).removed, 0);
  assert.equal(await exists(actions[1].state.STATE_marker), true);
  assert.equal((await pruneAction(actions[1].state)).removed, 0);
  for (const { state } of actions) assert.equal(await exists(state.STATE_marker), false);
});

test('failed mark wrapper does not publish runner state or mutate a symlink target', async (t) => {
  const { root, base } = await fixture(t);
  const target = await entry(root, 'aa1');
  await setTime(target, epoch);
  const link = join(base, 'linked cache');
  if (!await makeLink(t, root, link, true)) return;
  const stateFile = join(base, 'action state');
  await writeFile(stateFile, 'existing=value\n');
  await assert.rejects(exec(process.execPath, [markScript], {
    env: { ...process.env, INPUT_PATH: link, RUNNER_TEMP: base, GITHUB_STATE: stateFile },
  }), (error) => {
    assert.equal(error.code, 1);
    assert.equal(error.stdout, '');
    assert.match(error.stderr, /Cache root must be a directory, not a symlink/);
    return true;
  });
  assert.equal(await readFile(stateFile, 'utf8'), 'existing=value\n');
  assert.equal((await lstat(target)).mtimeMs, epoch);
  assert.equal((await readdir(base)).some((name) => name.startsWith('go-cache-')), false);
});

test('failed prune wrapper preserves entries and its marker for diagnosis', async (t) => {
  const { root, base } = await fixture(t);
  const cold = await entry(root, 'aa1');
  const { state } = await markAction(root, base, join(base, 'action state'));
  const cases = [
    { ...state, STATE_marker: join(base, 'missing marker') },
    { ...state, 'STATE_cache-root': join(base, 'missing cache') },
    { STATE_marker: '', 'STATE_cache-root': '' },
  ];
  for (const invalid of cases) {
    await assert.rejects(pruneAction(invalid), (error) => {
      assert.equal(error.code, 1);
      assert.equal(error.stdout, '');
      assert.notEqual(error.stderr, '');
      return true;
    });
    assert.equal(await readFile(cold, 'utf8'), 'cache');
    assert.equal(await exists(state.STATE_marker), true);
  }
  assert.equal((await pruneAction(state)).removed, 1);
  assert.equal(await exists(state.STATE_marker), false);
});

test('real Go packages and cached go-run executable survive action mark/prune', { timeout: 180000 }, async (t) => {
  try {
    await exec('go', ['version'], { env: { ...process.env, GOTOOLCHAIN: 'local' } });
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
    t.skip('Go is not installed');
    return;
  }
  const { root, base } = await fixture(t);
  const project = join(base, 'Go project with spaces');
  await mkdir(join(project, 'value'), { recursive: true });
  await writeFile(join(project, 'go.mod'), 'module example.com/cachetest\n\ngo 1.24\n');
  await writeFile(join(project, 'main.go'), `package main
import "example.com/cachetest/value"
func main() { if value.Value() != 42 { panic("wrong value") } }
`);
  await writeFile(join(project, 'value', 'value.go'), 'package value\nfunc Value() int { return 42 }\n');
  await writeFile(join(project, 'value', 'value_test.go'), `package value
import "testing"
func TestValue(t *testing.T) { if Value() != 42 { t.Fatal("wrong value") } }
`);
  const options = {
    cwd: project,
    timeout: 120000,
    maxBuffer: 16 * 1024 * 1024,
    env: {
      ...process.env,
      GOCACHE: root,
      GOPATH: join(base, 'gopath'),
      GOMODCACHE: join(base, 'modules'),
      GOTMPDIR: base,
      GOTOOLCHAIN: 'local',
      GOENV: 'off',
      GOWORK: 'off',
      GOFLAGS: '',
      GODEBUG: '',
      GOOS: process.platform === 'win32' ? 'windows' : process.platform,
      GOARCH: process.arch === 'x64' ? 'amd64' : process.arch,
      CGO_ENABLED: '0',
      GOPROXY: 'off',
      GOSUMDB: 'off',
    },
  };
  await exec('go', ['run', '.'], options);
  await exec('go', ['test', '-count=1', './...'], options);
  assert.ok((await run('stats', root)).entries.executable.count > 0);
  const cold = await entry(root, 'ff123', 'd', 'unused inherited entry');
  const { state, result: marked } = await markAction(root, base, join(base, 'action state'));
  assert.ok(marked.marked > 1);
  for (const stage of ['marked', 'pruned']) {
    if (stage === 'pruned') {
      const pruned = await pruneAction(state);
      assert.ok(pruned.removed >= 1);
      assert.equal(await exists(cold), false);
      assert.ok((await run('stats', root)).entries.executable.count > 0);
    }
    for (const args of [['run', '-x', '.'], ['test', '-x', '-count=1', './...']]) {
      const { stderr } = await exec('go', args, options);
      // go test relinks its executable; go run should reuse its cached executable.
      const tools = args[0] === 'run' ? /[/\\](?:compile|link)(?:\.exe)?"?\s/ : /[/\\]compile(?:\.exe)?"?\s/;
      assert.doesNotMatch(stderr, tools, `${stage}: go ${args.join(' ')}`);
    }
  }
});

test('CLI rejects bad arguments with stderr and no stdout', async (t) => {
  const { root, marker } = await fixture(t);
  for (const args of [[], ['unknown', root], ['stats', 'relative'], ['stats', root, '--bogus'], ['prune', root, '--marker', marker]]) {
    await assert.rejects(exec(process.execPath, [script, ...args]), (error) => {
      assert.equal(error.code, 1);
      assert.equal(error.stdout, '');
      assert.match(error.stderr, /^go-cache: /);
      return true;
    });
  }
});
