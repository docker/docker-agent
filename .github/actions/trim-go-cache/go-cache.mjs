#!/usr/bin/env node
// Usage: node go-cache.mjs <mark|prune|stats> /absolute/cache [--marker PATH]
// Stop Go and other cache writers during mark/prune; the cache must be private.
// The default marker is .go-cache-marker in the cache root. All results are JSON.

import { lstat, open, opendir, realpath, rmdir, unlink, utimes } from 'node:fs/promises';
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from 'node:path';
import { parseArgs } from 'node:util';
import { pathToFileURL } from 'node:url';

const bucketName = /^[0-9a-f]{2}$/;
const entryName = /^[0-9a-f]{64}-[ad]$/;
const coldAgeMs = 2 * 60 * 60 * 1000;
const usage = 'Usage: node go-cache.mjs <mark|prune|stats> /absolute/cache [--marker PATH]';

async function cacheRoot(root) {
  if (!isAbsolute(root)) {
    throw new Error('Cache root must be an absolute path');
  }
  root = resolve(root);
  const info = await lstat(root);
  if (!info.isDirectory() || info.isSymbolicLink()) {
    throw new Error('Cache root must be a directory, not a symlink');
  }
  // Resolve parent aliases (including /tmp on macOS) only after rejecting a root link.
  return realpath(root);
}

async function markerPath(root, marker) {
  marker = resolve(marker ?? join(root, '.go-cache-marker'));
  marker = join(await realpath(dirname(marker)), basename(marker));
  const within = relative(root, marker).split(sep);
  if (marker === root || bucketName.test(within[0])) {
    throw new Error('Marker must not be the cache root or inside a cache bucket');
  }
  return marker;
}

async function markerInfo(marker, required) {
  let info;
  try {
    info = await lstat(marker);
  } catch (error) {
    if (error.code !== 'ENOENT' || required) {
      throw error;
    }
    return undefined;
  }
  if (!info.isFile() || info.isSymbolicLink()) {
    throw new Error('Marker must be a regular file, not a symlink');
  }
  return info;
}

async function* cacheEntries(root) {
  for await (const bucket of await opendir(root)) {
    if (!bucketName.test(bucket.name)) {
      continue;
    }
    const bucketPath = join(root, bucket.name);
    const info = await lstat(bucketPath);
    if (!info.isDirectory() || info.isSymbolicLink()) {
      continue;
    }
    for await (const entry of await opendir(bucketPath)) {
      if (!entryName.test(entry.name) || !entry.name.startsWith(bucket.name)) {
        continue;
      }
      const path = join(bucketPath, entry.name);
      const info = await lstat(path);
      if (info.isFile() || info.isDirectory()) {
        yield { path, info, name: `${bucket.name}/${entry.name}` };
      }
    }
  }
}

// Count file payload bytes only; nested links are unlinked on removal, never followed.
async function visitTree(path, info, remove = false) {
  let bytes = info.isFile() ? info.size : 0;
  if (info.isDirectory()) {
    for await (const child of await opendir(path)) {
      const childPath = join(path, child.name);
      bytes += await visitTree(childPath, await lstat(childPath), remove);
    }
    if (remove) {
      await rmdir(path);
    }
  } else if (remove) {
    await unlink(path);
  }
  return bytes;
}

async function writeMarker(marker, time) {
  const handle = await open(marker, 'wx', 0o600);
  try {
    try {
      const date = new Date(time);
      await handle.utimes(date, date);
      return (await handle.stat()).mtimeMs;
    } finally {
      await handle.close();
    }
  } catch (error) {
    await unlink(marker);
    throw error;
  }
}

export async function run(mode, root, { marker, now = Date.now } = {}) {
  if (!['mark', 'prune', 'stats'].includes(mode)) {
    throw new Error(`Unknown mode: ${mode}`);
  }
  root = await cacheRoot(root);
  if (mode === 'stats') {
    const result = {
      mode, root, count: 0, bytes: 0,
      entries: {
        a: { count: 0, bytes: 0 },
        d: { count: 0, bytes: 0 },
        executable: { count: 0, bytes: 0 },
      },
      largest: [],
    };
    for await (const entry of cacheEntries(root)) {
      const bytes = await visitTree(entry.path, entry.info);
      const kind = entry.info.isDirectory() ? 'executable' : entry.name.at(-1);
      result.count++;
      result.bytes += bytes;
      result.entries[kind].count++;
      result.entries[kind].bytes += bytes;
      // Bound inventory memory regardless of cache size; paths break size ties.
      if (result.largest.length < 20 || bytes >= result.largest.at(-1).bytes) {
        result.largest.push({ path: entry.name, bytes });
        result.largest.sort((a, b) => b.bytes - a.bytes || (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
        if (result.largest.length > 20) {
          result.largest.pop();
        }
      }
    }
    return result;
  }

  marker = await markerPath(root, marker);
  const previous = await markerInfo(marker, mode === 'prune');
  if (mode === 'mark') {
    // An incomplete mark must not leave an old marker that can authorize pruning.
    if (previous) {
      await unlink(marker);
    }
    const cold = new Date(now() - coldAgeMs);
    let marked = 0;
    for await (const entry of cacheEntries(root)) {
      // Go marks the directory itself when reusing a cached executable.
      await utimes(entry.path, entry.info.atime, cold);
      marked++;
    }
    const markerMtimeMs = await writeMarker(marker, now());
    return { mode, root, marker, marked, markerMtimeMs };
  }

  const result = {
    mode, root, marker, markerMtimeMs: previous.mtimeMs,
    scanned: 0, removed: 0, removedBytes: 0,
  };
  for await (const entry of cacheEntries(root)) {
    result.scanned++;
    if (entry.info.mtimeMs < previous.mtimeMs) {
      result.removedBytes += await visitTree(entry.path, entry.info, true);
      result.removed++;
    }
  }
  return result;
}

async function main() {
  const { values, positionals } = parseArgs({
    allowPositionals: true,
    options: { marker: { type: 'string' }, help: { type: 'boolean', short: 'h' } },
  });
  if (values.help) {
    console.error(`${usage}
The marker defaults to .go-cache-marker inside the cache root.
mark ages entries by two hours, then writes the marker; prune removes older entries.
Stop Go and other writers during mark/prune; cache and marker paths must be private.
Results are JSON. stats reports file payload bytes, excluding symlinks and metadata.`);
    return;
  }
  if (positionals.length !== 2) {
    throw new Error(usage);
  }
  const result = await run(positionals[0], positionals[1], { marker: values.marker });
  console.log(JSON.stringify(result));
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  main().catch((error) => {
    console.error(`go-cache: ${error.message}`);
    process.exitCode = 1;
  });
}
