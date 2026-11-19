import assert from 'node:assert/strict';
import { test } from 'node:test';
import { localTools } from './local-tools.mjs';

function fixture(t) {
  t.mock.timers.enable({ apis: ['Date'], now: Date.parse('2026-11-19T16:00:30.604Z') });
  const { call } = localTools();
  return {
    call,
    ok(name, args = {}) {
      const { output, is_error } = call(name, JSON.stringify(args));
      assert.equal(is_error, false, output.slice(0, 256));
      return output;
    },
    fails(name, args, output) {
      assert.deepEqual(call(name, JSON.stringify(args)), { output, is_error: true });
    },
  };
}

// Exact model-visible results from the Go handlers. Freeze only the wall clock.
test('plan writes, summaries, reads and status views have distinct JSON contracts', t => {
  const { call, ok } = fixture(t);
  const updatedAt = '2026-11-19T16:00:30Z';
  assert.deepEqual(call('list_plans', 'not JSON'), { output: '{"plans":[]}', is_error: false });
  const args = { name: 'release', title: 'Release', content: 'Ship <v2> & test\u2028', author: 'Ada' };
  const written = `{"name":"release","title":"Release","content":"Ship <v2> & test\\u2028","author":"Ada","revision":1,"updatedAt":"${updatedAt}"}`;
  assert.equal(ok('write_plan', { ...args, last_known_revision: 0 }), written);
  assert.equal(ok('read_plan', { name: 'release' }), written);
  assert.equal(ok('get_plan_status', { name: 'release' }), '{"name":"release","status":"","revision":1}');
  assert.equal(ok('set_plan_status', { name: 'release', status: 'in review', last_known_revision: 1 }), '{"name":"release","status":"in review","revision":2}');
  ok('write_plan', { name: '0-first', content: 'other' });
  t.mock.timers.tick(1500);
  const next = '2026-11-19T16:00:32Z';
  assert.equal(ok('write_plan', { name: 'release', content: 'new', title: '', author: null, status: '' }),
    `{"name":"release","title":"Release","content":"new","author":"Ada","status":"in review","revision":3,"updatedAt":"${next}"}`);
  assert.equal(ok('list_plans'), `{"plans":[{"name":"0-first","revision":1,"updatedAt":"${updatedAt}"},{"name":"release","title":"Release","author":"Ada","status":"in review","revision":3,"updatedAt":"${next}"}]}`);
  assert.equal(ok('delete_plan', { name: 'release', last_known_revision: 3 }), '{"deleted":"release"}');
  assert.equal(JSON.parse(ok('write_plan', args)).revision, 1, 'recreating starts over');
});

test('guard and mutation are atomic; missing plans and invalid names are not conflated', async t => {
  const { call, fails, ok } = fixture(t);
  const name = 'release';
  const conflict = rev => `version conflict on plan "release": last_known_revision 1 does not match current revision ${rev}; re-read the plan and retry`;
  fails('write_plan', { name, content: 'v1', last_known_revision: 1 }, conflict(0));
  for (const tool of ['read_plan', 'get_plan_status', 'delete_plan']) fails(tool, { name, last_known_revision: 1 }, 'plan "release" not found');
  fails('set_plan_status', { name, status: 'done', last_known_revision: 1 }, 'plan not found: "release"');
  fails('write_plan', { name: '../x' }, 'content must not be empty');
  fails('set_plan_status', { name: '../x' }, 'status must not be empty');
  for (const [bad, quoted] of [['a/b', '"a/b"'], ['release\n', '"release\\n"'], ['\x00\u00a0', '"\\x00\\u00a0"'], ['-lead', '"-lead"'], ['BAD', '"BAD"']]) {
    fails('write_plan', { name: bad, content: 'x' }, `invalid plan name ${quoted}: use only lowercase letters, digits, '-' and '_', starting with a letter or digit`);
  }
  ok('write_plan', { name, content: 'v1' });
  const writers = Array.from({ length: 16 }, (_, i) => Promise.resolve().then(() => call('write_plan', JSON.stringify({ name, content: `v2-${i}`, last_known_revision: 1 }))));
  const replies = await Promise.all(writers);
  assert.equal(replies.filter(r => !r.is_error).length, 1);
  for (const r of replies.filter(r => r.is_error)) assert.equal(r.output, conflict(2));
  fails('set_plan_status', { name, status: 'lost', last_known_revision: 1 }, conflict(2));
  fails('delete_plan', { name, last_known_revision: 1 }, conflict(2));
  assert.equal(JSON.parse(ok('read_plan', { name })).revision, 2);
});

test('memory ID, local date, insertion order, nil results and escaping match Go js/wasm', t => {
  const { ok, fails } = fixture(t);
  const id = '1795104030604000000';
  assert.equal(ok('get_memories'), 'null');
  assert.equal(ok('add_memory', { memory: 'likes <tea> & tabs\u2029', category: 'preference' }), `Memory added successfully with ID: ${id}`);
  fails('add_memory', { memory: 'collision' }, `Error calling tool: failed to add memory: memory ID already exists: ${id}`);
  const [row] = JSON.parse(ok('get_memories'));
  assert.match(row.created_at, /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:Z|[+-]\d\d:\d\d)$/);
  assert.equal(Date.parse(row.created_at), Math.floor(Date.now() / 1000) * 1000);
  assert.equal(new Date().getTimezoneOffset() === 0, row.created_at.endsWith('Z'));
  assert.equal(ok('search_memories', { query: 'TABS TEA', category: 'PREFERENCE' }),
    `[{"id":"${id}","created_at":"${row.created_at}","memory":"likes \\u003ctea\\u003e \\u0026 tabs\\u2029","category":"preference"}]`);
  t.mock.timers.tick(1);
  const other = '1795104030605000000';
  assert.equal(ok('add_memory'), `Memory added successfully with ID: ${other}`);
  t.mock.timers.tick(10_000);
  assert.equal(ok('update_memory', { id, memory: 'likes Go' }), `Memory with ID ${id} updated successfully`);
  assert.deepEqual(JSON.parse(ok('get_memories')), [
    { id, created_at: row.created_at, memory: 'likes Go' }, { id: other, created_at: row.created_at, memory: '' },
  ]);
  assert.equal(ok('search_memories', { category: 'preference' }), 'null');
  fails('update_memory', {}, 'Error calling tool: failed to update memory: memory ID cannot be empty');
  fails('update_memory', { id: 'missing' }, 'Error calling tool: failed to update memory: memory not found: missing');
  for (const key of [id, id, other, 'missing', '']) assert.equal(ok('delete_memory', { id: key }), `Memory with ID ${key} deleted successfully`);
  assert.equal(ok('get_memories'), 'null');
});

test('memory search matches all literal Unicode words and a simple-folded category', t => {
  const { ok } = fixture(t);
  for (const [memory, category] of [['Deploy: 100% done_now ΑΣ İ', 'PreferenceΣ'], ['Go backend', 'Key'], ['Project uses Go', 'project']]) {
    ok('add_memory', { memory, category }); t.mock.timers.tick(1);
  }
  for (const [query, category, want] of [
    [' deploy:\u0085ΝΟ ', '', []], [' αΣ\u0085done_now 100% ', 'preferenceς', ['Deploy: 100% done_now ΑΣ İ']],
    ['i\u0307', '', []], ['i', 'preferenceσ', ['Deploy: 100% done_now ΑΣ İ']], ['asad', '', []], ['doneXnow', '', []],
    ['GO', '', ['Go backend', 'Project uses Go']], ['backend go', 'KEY', ['Go backend']], ['go\uFEFFbackend', '', []],
    ['go', '.*', []], [' \t', 'PROJECT', ['Project uses Go']],
  ]) {
    const result = JSON.parse(ok('search_memories', { query, category }));
    assert.deepEqual(result?.map(row => row.memory) ?? [], want);
  }
});

test('typed argument decoding preserves Go order, scalar types, int spelling and nil handling', t => {
  const { call, ok } = fixture(t);
  const valid = String.raw`{"unknown":{"x":["\"}",{}]},"NAME":"release","name":null,"CONTENT":"x","content":"\ud800","\u017ftatus":"ok","__proto__":"ignored","last_known_revision":7,"last_known_revision":null}`;
  assert.deepEqual(JSON.parse(call('write_plan', valid).output), {
    name: 'release', content: '\ufffd', status: 'ok', revision: 1, updatedAt: '2026-11-19T16:00:30Z',
  });
  for (const [tool, raw, diagnostic] of [
    ['write_plan', '{"content":5,"content":"repaired?"}', 'json: cannot unmarshal number into Go struct field WritePlanArgs.content of type string'],
    ['read_plan', '{"NAME":false}', 'json: cannot unmarshal bool into Go struct field ReadPlanArgs.NAME of type string'],
    ['read_plan', '[]', 'json: cannot unmarshal array into Go value of type plan.ReadPlanArgs'],
    ['add_memory', '{"memory":{}}', 'json: cannot unmarshal object into Go struct field AddMemoryArgs.memory of type string'],
    ['get_memories', '[1]', 'json: cannot unmarshal array into Go value of type map[string]interface {}'],
    ['get_memories', '{"x":[1e999]}', 'json: cannot unmarshal number 1e999 into .x.0 of type float64'],
    ['delete_plan', '{"last_known_revision":"1"}', 'json: cannot unmarshal string into Go struct field DeletePlanArgs.last_known_revision of type int'],
    ...['1e0', '1.0', '9223372036854775808'].map(n => ['delete_plan', `{"last_known_revision":${n}}`, `json: cannot unmarshal number ${n} into Go struct field DeletePlanArgs.last_known_revision of type int`]),
    ['add_memory', '{', 'unexpected end of JSON input'],
    ['add_memory', '{not json}', "invalid character 'n' looking for beginning of object key string"],
    ['write_plan', '{"name":5,}', "invalid character '}' looking for beginning of object key string"],
    ['delete_memory', '{"ignored":' + '['.repeat(10_000), 'exceeded max depth'],
  ]) assert.deepEqual(call(tool, raw), { output: 'Error calling tool: ' + diagnostic, is_error: true }, raw.slice(0, 128));
  assert.deepEqual(call('delete_plan', '{"name":"release","last_known_revision":9223372036854775807}'), {
    output: 'version conflict on plan "release": last_known_revision 9223372036854775807 does not match current revision 1; re-read the plan and retry', is_error: true,
  });
  const repaired = call('write_plan', '{"name":"release","content":false,"content":"v2","last_known_revision":1.0,"title":null}');
  assert.equal(repaired.is_error, false);
  assert.equal(JSON.parse(repaired.output).revision, 2);
  assert.equal(JSON.parse(repaired.output).content, 'v2');
  assert.deepEqual(call('get_memories', ''), call('get_memories', 'null'));
  assert.equal(ok('get_memories'), 'null');
});

test('session isolation, cancellation before effect, and unavailable file tools', t => {
  const { call, ok } = fixture(t);
  ok('write_plan', { name: 'release', content: '# Release' });
  ok('add_memory', { memory: 'likes tabs' });
  const independent = localTools();
  assert.deepEqual(independent.call('list_plans', '{}'), { output: '{"plans":[]}', is_error: false });
  assert.deepEqual(independent.call('get_memories', '{}'), { output: 'null', is_error: false });
  for (const name of ['export_plan_to_file', 'update_plan_from_file', '__proto__', 'constructor']) {
    assert.deepEqual(call(name, '{"path":"/outside"}'), { output: `Tool '${name}' is not available. You can only use the tools provided to you.`, is_error: true });
  }
  for (const [tool, args] of [['delete_plan', { name: 'release' }], ['update_memory', { id: '1795104030604000000' }]]) {
    assert.deepEqual(call(tool, JSON.stringify(args), AbortSignal.abort()), { output: 'The tool call was canceled by the user.', is_error: true });
  }
  assert.equal(JSON.parse(ok('read_plan', { name: 'release' })).content, '# Release');
  assert.equal(JSON.parse(ok('get_memories'))[0].memory, 'likes tabs');
});

test('Go content and encoded-size bounds count bytes and never consume a revision on failure', t => {
  const { fails, ok } = fixture(t);
  const max = 10 << 20, encoded = 7 * max;
  const name = 'p', content = 'é'.repeat(max / 2);
  ok('write_plan', { name, content });
  fails('write_plan', { name, content: content + 'x' }, `plan "p" content is too large to store (${max + 1} bytes; max ${max})`);
  assert.equal(JSON.parse(ok('get_plan_status', { name })).revision, 1);
  // A literal '<' costs one transport byte but six bytes in Go's storage encoding.
  const probe = { name, content: 'x', status: 's', revision: 2, updatedAt: '2026-11-19T16:00:30Z' };
  const room = encoded - Buffer.byteLength(JSON.stringify(probe, null, 2)) + 1;
  const status = '<'.repeat(Math.floor(room / 6)) + 's'.repeat(room % 6);
  const atLimit = ok('write_plan', { name, content: 'x', status });
  assert.equal(JSON.parse(atLimit).revision, 2);
  fails('set_plan_status', { name, status: status + 's' }, `plan "p" is too large to store (${encoded + 1} bytes; max ${encoded})`);
  fails('write_plan', { name: 'q', content: 'x' }, 'plan storage size limit reached');
  assert.equal(ok('delete_plan', { name, last_known_revision: 2 }), '{"deleted":"p"}');
  ok('write_plan', { name: 'q', content: 'x' });
});

test('portable budgets also bound tiny entries and memory growth across turns', t => {
  const { ok, fails } = fixture(t);
  for (let i = 0; i < 256; i++) {
    ok('write_plan', { name: `p${i}`, content: 'x' });
    ok('add_memory', { memory: 'x' }); t.mock.timers.tick(1);
  }
  fails('write_plan', { name: 'overflow', content: 'x' }, 'plan count limit reached');
  fails('add_memory', { memory: 'x' }, 'memory count limit reached');
  const id = '1795104030604000000';
  fails('update_memory', { id, memory: 'x'.repeat(1 << 20) }, 'memory storage size limit reached');
  assert.equal(JSON.parse(ok('get_memories'))[0].memory, 'x');
  ok('delete_memory', { id }); ok('add_memory', { memory: 'restored' });
  ok('delete_plan', { name: 'p0' }); ok('write_plan', { name: 'restored', content: 'x' });
  fails('add_memory', { memory: 'x'.repeat(16 << 20) }, 'local tool argument size limit reached');
});
