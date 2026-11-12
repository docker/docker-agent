import { unlink } from 'node:fs/promises';

import { run } from './go-cache.mjs';

try {
  const marker = process.env.STATE_marker;
  console.log(JSON.stringify(await run('prune', process.env['STATE_cache-root'], { marker })));
  await unlink(marker);
} catch (error) {
  console.error(error);
  process.exitCode = 1;
}
