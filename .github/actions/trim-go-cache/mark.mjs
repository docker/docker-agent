import { appendFile, mkdir } from 'node:fs/promises';
import { randomUUID } from 'node:crypto';
import { join } from 'node:path';

import { run } from './go-cache.mjs';

try {
  const root = process.env.INPUT_PATH;
  await mkdir(root, { recursive: true });
  const marker = join(process.env.RUNNER_TEMP, `go-cache-${randomUUID()}`);
  console.log(JSON.stringify(await run('mark', root, { marker })));
  await appendFile(process.env.GITHUB_STATE, `cache-root=${root}\nmarker=${marker}\n`);
} catch (error) {
  console.error(error);
  process.exitCode = 1;
}
