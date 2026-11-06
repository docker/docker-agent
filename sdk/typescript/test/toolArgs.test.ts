import assert from 'node:assert/strict';
import { test } from 'node:test';

import {
  EditFileArgsSchema,
  FetchArgsSchema,
  ReadFileArgsSchema,
  ShellArgsSchema,
  UserPromptArgsSchema,
  filesystem,
  toolArgsSchemas,
} from '../src/index.ts';
import type { ReadFileArgs, ShellArgs, ShellToolset, ToolName, Toolset } from '../src/index.ts';

test('valid arguments parse', () => {
  const shell: ShellArgs = ShellArgsSchema.parse({ cmd: 'ls', timeout: 30 });
  assert.deepEqual(shell, { cmd: 'ls', timeout: 30 });
  assert.ok(ShellArgsSchema.safeParse({ cmd: 'ls', description: 'x' }).success);

  const read: ReadFileArgs = ReadFileArgsSchema.parse({ path: 'a.txt', line: null });
  assert.equal(read.path, 'a.txt');

  assert.ok(EditFileArgsSchema.safeParse({ path: 'a', edits: [{ oldText: 'x', newText: 'y' }] }).success);
  assert.ok(EditFileArgsSchema.safeParse({ path: 'a', edits: null }).success);
  assert.ok(FetchArgsSchema.safeParse({ urls: ['https://example.com'], format: 'markdown' }).success);
  assert.ok(UserPromptArgsSchema.safeParse({ message: 'hi', schema: { type: 'object' } }).success);
});

test('invalid arguments fail', () => {
  const invalid: [string, { safeParse(v: unknown): { success: boolean } }, unknown][] = [
    ['missing required', ShellArgsSchema, {}],
    ['unknown property', ShellArgsSchema, { cmd: 'ls', unknown: true }],
    ['description on a tool without it', ReadFileArgsSchema, { path: 'a', description: 'x' }],
    ['wrong type', ShellArgsSchema, { cmd: 1 }],
    ['non-integer', ShellArgsSchema, { cmd: 'ls', timeout: 1.5 }],
    ['nested unknown property', EditFileArgsSchema, { path: 'a', edits: [{ oldText: 'x', newText: 'y', z: 1 }] }],
    ['empty list below minItems', FetchArgsSchema, { urls: [], format: 'text' }],
    ['value outside enum', FetchArgsSchema, { urls: ['u'], format: 'pdf' }],
    ['value above maximum', FetchArgsSchema, { urls: ['u'], format: 'text', timeout: 301 }],
  ];
  for (const [name, schema, value] of invalid) {
    assert.equal(schema.safeParse(value).success, false, name);
  }
});

test('every tool name has an argument schema', () => {
  for (const name of filesystem) {
    assert.ok(name in toolArgsSchemas, name);
  }
  assert.equal(toolArgsSchemas.read_file, ReadFileArgsSchema);
});

test('Toolset narrows on type', () => {
  const toolset: Toolset = { type: 'shell', sudo_askpass: true };
  if (toolset.type === 'shell') {
    const shell: ShellToolset = toolset;
    assert.equal(shell.sudo_askpass, true);
  }

  // @ts-expect-error sudo_askpass is only valid on shell toolsets
  const notShell: Toolset = { type: 'todo', sudo_askpass: true };
  assert.equal(notShell.type, 'todo');

  const api: Toolset = { type: 'api', api_config: { endpoint: 'https://example.com' } };
  assert.equal(api.type, 'api');

  // @ts-expect-error api toolsets need api_config
  const noConfig: Toolset = { type: 'api' };
  assert.equal(noConfig.type, 'api');

  // @ts-expect-error not a builtin tool
  const unknownTool: ToolName = 'not_a_tool';
  assert.equal(unknownTool, 'not_a_tool');
});
