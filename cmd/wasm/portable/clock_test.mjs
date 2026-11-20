import assert from 'node:assert/strict';
import { test } from 'node:test';
import { clockTool, dynamicPrompt } from './clock.mjs';

const random = raw => clockTool('random_int', raw);
const time = (format, timezone) => clockTool('get_datetime', JSON.stringify({ format, timezone }));
const ok = output => ({ output, is_error: false });
const error = output => ({ output, is_error: true });
const lo = -(1n << 63n), hi = (1n << 63n) - 1n;
const range = (min, max) => `{"min":${min},"max":${max}}`;
const rfc = '2006-01-02T15:04:05Z07:00';

function freeze(t, iso) {
  t.mock.timers.enable({ apis: ['Date'], now: Date.parse(iso) });
}

test('random_int preserves both inclusive endpoints, including numbers not representable by JS', () => {
  for (const n of [0n, -1n, 42n, 9007199254740993n, hi, lo]) {
    assert.deepEqual(random(range(n, n)), ok(String(n)));
  }
  for (const [min, max] of [[lo, hi], [lo, 0n], [lo, lo + 1n], [hi - 1n, hi], [-5n, 3n], [1n, 6n]]) {
    for (let i = 0; i < 32; i++) {
      const { output, is_error } = random(range(min, max));
      assert.equal(is_error, false, output);
      assert.match(output, /^-?\d+$/);
      const value = BigInt(output);
      assert.ok(min <= value && value <= max);
    }
  }
});

test('random_int rejects biased draws, uses no entropy for a singleton and propagates entropy failure', t => {
  const draws = [(1n << 64n) - 1n, 5n, 0n];
  const rng = t.mock.method(crypto, 'getRandomValues', array => {
    if (!draws.length) throw new Error('no entropy');
    array[0] = draws.shift();
    return array;
  });
  assert.deepEqual(random(range(hi, hi)), ok(String(hi)));
  assert.equal(rng.mock.callCount(), 0);
  assert.deepEqual(random(range(1, 6)), ok('6'));
  assert.deepEqual(random(range(lo, hi)), ok(String(lo)));
  assert.deepEqual(random(range(0, 1)), error('failed to generate random integer: no entropy'));
});

test('Go argument semantics: raw numbers, duplicate fields in order, null pointers and ignored subtrees', () => {
  const missing = error('min and max are required');
  for (const raw of ['', '{}', 'null', '{"max":6}', '{"min":1,"max":6,"MAX":null}', '{"nest":{"min":2,"max":2}}']) {
    assert.deepEqual(random(raw), missing, raw);
  }
  assert.deepEqual(random(`{"min":null,"\\u006din":${hi},"max":1,"mAx":${hi},"unused":[{"MIN":"bad"}]}`), ok(String(hi)));
  assert.deepEqual(random(`{"__proto__":{"min":0},"max":0}`), missing);
  assert.deepEqual(random(range(hi, lo)), error('min must be less than or equal to max'));
  for (const value of ['1.25', '0.0', '1e2', String(lo - 1n), String(hi + 1n)]) {
    assert.deepEqual(random(`{"MIN":${value},"min":0,"max":2}`),
      error(`Error calling tool: json: cannot unmarshal number ${value} into Go struct field Args.MIN of type int64`));
  }
  assert.deepEqual(random('[1]'), error('Error calling tool: json: cannot unmarshal array into Go value of type random.Args'));
  for (const value of ['"1"', '{}', 'true']) assert.equal(random(`{"min":1,"max":${value}}`).is_error, true);
  assert.deepEqual(random('{"min":1,"max":'), error('Error calling tool: unexpected end of JSON input'));
  assert.equal(random('{"min":1,"max":1}[]').is_error, true);
});

test('get_datetime applies Go layout tokens, preserves literals and reads the clock on each call', t => {
  freeze(t, '2024-01-07T00:04:05.120Z');
  for (const [format, output] of [
    ['Monday Mon January Jan _2 2 02 1 01 2006 _2006 06 __2 002', 'Sunday Sun January Jan  7 7 07 1 01 2024 _2024 24   7 007'],
    ['15 3 03 PM pm 04 4 05 5', '00 12 12 AM am 04 4 05 5'],
    ['Janitor Month Januaryx Mondayish Jang MonX', 'Janitor Month Januaryx Sundayish Jang SunX'],
    ['.000000000 .999999999 ,999 .9 ,0 .09', '.120000000 .12 ,12 .1 ,1 .09'],
    ['MST Z07 -07 Z0700 -0700 Z070000 -070000 Z07:00 -07:00 Z07:00:00 -07:00:00', 'UTC Z +00 Z +0000 Z +000000 Z +00:00 Z +00:00:00'],
    ['  YYYY-MM-DD %Y-%m-%d\n', '  YYYY-MM-DD %Y-%m-%d\n'],
  ]) assert.deepEqual(time(format, 'UTC'), ok(output), format);
  t.mock.timers.setTime(Date.parse('2024-12-31T19:02:01Z'));
  assert.deepEqual(time('002 Jan 2 3:04:05.999pm', 'UTC'), ok('366 Dec 31 7:02:01pm'));
  assert.deepEqual(clockTool('get_datetime', '{"Format":"15:04","format":null,"timezone":"UTC"}'), ok('19:02'));
});

test('IANA offsets and abbreviations follow the instant, including DST transitions and fractional zones', t => {
  freeze(t, '2026-03-08T06:59:59Z');
  const ny = 'America/New_York';
  assert.deepEqual(time(rfc + ' MST', ny), ok('2026-03-08T01:59:59-05:00 EST'));
  t.mock.timers.tick(1000);
  assert.deepEqual(time(rfc + ' MST', ny), ok('2026-03-08T03:00:00-04:00 EDT'));
  t.mock.timers.setTime(Date.parse('2026-10-07T23:04:05.123Z'));
  for (const [zone, output] of [
    ['Europe/Paris', '2026-10-08T01:04:05+02:00 CEST'], ['Asia/Tokyo', '2026-10-08T08:04:05+09:00 JST'],
    ['Asia/Kathmandu', '2026-10-08T04:49:05+05:45 +0545'], ['US/Eastern', '2026-10-07T19:04:05-04:00 EDT'],
    ['Etc/GMT+3', '2026-10-07T20:04:05-03:00 -03'], ['GMT', '2026-10-07T23:04:05Z GMT'],
  ]) assert.deepEqual(time(rfc + ' MST', zone), ok(output), zone);
  assert.deepEqual(time('Z07 Z070000 -07:00:00', 'Asia/Kathmandu'), ok('+05 +054500 +05:45:00'));
  assert.deepEqual(time(rfc), time(rfc, Intl.DateTimeFormat().resolvedOptions().timeZone));
  assert.deepEqual(time(rfc, 'Local'), time(rfc));
});

test('when Intl is unavailable, the browser local fallback reads Date offsets for each instant', t => {
  freeze(t, '2026-10-07T23:04:05Z');
  t.mock.method(Intl, 'DateTimeFormat', () => { throw new Error('unavailable'); });
  t.mock.method(Date.prototype, 'getTimezoneOffset', function () { return this.getUTCMonth() === 9 ? 240 : 300; });
  assert.deepEqual(time(rfc + ' MST'), ok('2026-10-07T19:04:05-04:00 Local'));
  t.mock.timers.setTime(Date.parse('2026-11-07T23:04:05Z'));
  assert.deepEqual(time(rfc + ' MST', 'Local'), ok('2026-11-07T18:04:05-05:00 Local'));
  assert.deepEqual(time(rfc, 'UTC'), ok('2026-11-07T23:04:05Z'));
});

test('rejects invalid fields, non-IANA Intl aliases and oversized input without truncating valid bounds', () => {
  for (const format of ['', null, ' \n\t\u0085']) {
    assert.deepEqual(time(format, 'Bad/Zone'), error('Error: format is required; use a Go time layout such as 2006-01-02 or 15:04:05.'));
  }
  assert.deepEqual(time('\ufeff', 'UTC'), ok('\ufeff')); // Not Unicode White_Space.
  assert.deepEqual(time({}, 'UTC'), error('Error calling tool: json: cannot unmarshal object into Go struct field Args.format of type string'));
  for (const zone of ['Bad/Zone', 'PST', 'utc', 'america/new_york', '+05:00']) {
    assert.deepEqual(time('15:04', zone), error(`Error: invalid timezone "${zone}": not implemented on js`));
  }
  for (const zone of ['../etc/passwd', '/absolute']) {
    assert.deepEqual(time('MST', zone), error(`Error: invalid timezone "${zone}": time: invalid location name`));
  }
  assert.deepEqual(time('MST', 'UTC\0'), error('Error: invalid timezone "UTC\\x00": Invalid argument'));
  assert.deepEqual(time('MST', 'a\\u0000\x7f'), error('Error: invalid timezone "a\\\\u0000\\x7f": not implemented on js'));
  assert.deepEqual(time('🌏'.repeat(20000), 'UTC'), error('clock argument size limit reached'));
  assert.equal(random(range('1'.repeat(70000), 2)).is_error, true);
  assert.equal(random('{"ignored":' + '['.repeat(10000) + ']'.repeat(10000) + '}').is_error, true);
});

test('prompt extras match Go js/wasm, stay ordered and never probe host metadata for the environment', () => {
  const flags = { add_environment_info: true, add_date: true };
  const env = 'Here is useful information about the environment you are running in:\n\t<env>\n\tWorking directory: /\n\tOperating System: js\n\tCPU Architecture: wasm\n\t</env>';
  const noClock = { get now() { assert.fail('date hook disabled'); } };
  assert.deepEqual(dynamicPrompt({}, noClock), []);
  assert.deepEqual(dynamicPrompt({ add_environment_info: true }, noClock), [env]);
  assert.deepEqual(dynamicPrompt(flags, { now: new Date('2026-10-08T03:59:59Z'), timezoneOffset: 240 }), [env, "Today's date: 2026-10-07"]);
  assert.deepEqual(dynamicPrompt(flags, { now: new Date('2026-10-08T04:00:00Z'), timezoneOffset: 240 }), [env, "Today's date: 2026-10-08"]);
});

test('add_date reads a fresh clock with the fixed local offset used by Go wasm', t => {
  freeze(t, '2026-10-08T04:30:00Z');
  const offset = t.mock.method(Date.prototype, 'getTimezoneOffset', function () { return this.getUTCMonth() === 9 ? 240 : 300; });
  assert.deepEqual(dynamicPrompt({ add_date: true }), ["Today's date: 2026-10-08"]);
  t.mock.timers.setTime(Date.parse('2026-11-08T04:30:00Z'));
  assert.deepEqual(dynamicPrompt({ add_date: true }), ["Today's date: 2026-11-08"]);
  assert.equal(offset.mock.callCount(), 1);
});

test('zone transitions from the Go database take precedence over browser ICU', t => {
  const Clock = Date;
  let instant;
  t.mock.method(globalThis, 'Date', class extends Clock {
    constructor(...args) { super(...(args.length ? args : [instant])); }
  });
  t.mock.method(Intl, 'DateTimeFormat', () => { throw new Error('no host zone data'); });
  const layout = '15:04:05 -07:00:00 MST';
  for (const [iso, zone, want] of [
    ['1900-07-01T12:00:00Z', 'Europe/Paris', '12:09:21 +00:09:21 PMT'],
    ['1900-07-01T12:00:00Z', 'Africa/Accra', '11:59:08 +00:00:-52 LMT'],
    ['2026-11-01T08:59:59Z', 'Canada/Pacific', '01:59:59 -07:00:00 PDT'],
    ['2026-11-01T09:00:00Z', 'America/Vancouver', '02:00:00 -07:00:00 MST'],
    ['2050-01-01T00:00:00Z', 'Australia/Lord_Howe', '11:00:00 +11:00:00 +11'],
    ['2050-07-01T00:00:00Z', 'Australia/Lord_Howe', '10:30:00 +10:30:00 +1030'],
  ]) {
    instant = Clock.parse(iso);
    assert.deepEqual(time(layout, zone), ok(want));
  }
});
