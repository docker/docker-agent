// Browser counterparts of pkg/tools/builtin/{datetime,random} and the two prompt hooks.
import { goQuote } from './go-json.mjs';
import { states, steps, rules, zones } from './zoneinfo.mjs';

const maxBytes = 64 * 1024;
const encoder = new TextEncoder();
const minInt = -(1n << 63n), maxInt = (1n << 63n) - 1n;
const jsonTokens = /"(?:[^"\\]|\\.)*"|-?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?|true|false|null|[[\]{}]/g;

function jsonType(token) {
  switch (token[0]) {
    case '{': return 'object';
    case '[': return 'array';
    case '"': return 'string';
    case 't': case 'f': return 'bool';
    case 'n': return 'null';
    default: return 'number';
  }
}

function decodeField(key, token, type, int) {
  if (!int && type === 'string') return JSON.parse(token).toWellFormed();
  if (int && type === 'number' && token.length <= 20 && /^-?\d+$/.test(token)) {
    const n = BigInt(token);
    if (n >= minInt && n <= maxInt) return n;
  }
  const source = int && type === 'number' ? `number ${token}` : type;
  throw new Error(`Error calling tool: json: cannot unmarshal ${source} into Go struct field Args.${key} of type ${int ? 'int64' : 'string'}`);
}

function argumentsFor(kind, raw) {
  if (typeof raw !== 'string') throw new Error('expected raw tool arguments');
  if (raw.length > maxBytes || encoder.encode(raw).length > maxBytes) throw new Error('clock argument size limit reached');
  if (!raw) return {};
  try { JSON.parse(raw); } catch (e) {
    const eof = /end of (?:JSON )?(?:input|data)/i.test(e.message) || e.message.includes(`position ${raw.length} `);
    throw new Error(`Error calling tool: ${eof ? 'unexpected end of JSON input' : 'invalid JSON input'}`);
  }
  const args = {};
  let depth = 0, key;
  // The JS object loses duplicate fields, wire order and the original numeric spelling.
  for (const [token] of raw.matchAll(jsonTokens)) {
    if (token === '}' || token === ']') { depth--; continue; }
    const type = jsonType(token);
    if (depth === 0 && type !== 'object' && type !== 'null') {
      throw new Error(`Error calling tool: json: cannot unmarshal ${type} into Go value of type ${kind}.Args`);
    }
    if (depth === 1) {
      if (key === undefined) { key = JSON.parse(token); continue; }
      const field = key.toLowerCase();
      const int = kind === 'random' && (field === 'min' || field === 'max');
      const str = kind === 'datetime' && (field === 'format' || field === 'timezone');
      if (int || str) {
        if (type === 'null') {
          if (int) delete args[field]; // null resets *int64, but leaves string fields alone.
        } else args[field] = decodeField(key, token, type, int);
      }
      key = undefined;
    }
    if ((type === 'object' || type === 'array') && ++depth > 10_000) {
      throw new Error('Error calling tool: exceeded max depth');
    }
  }
  return args;
}

function randomInt({ min, max }) {
  if (min == null || max == null) throw new Error('min and max are required');
  if (min > max) throw new Error('min must be less than or equal to max');
  if (min === max) return String(min);
  const width = max - min + 1n;
  const limit = (1n << 64n) / width * width;
  const draw = new BigUint64Array(1);
  try {
    // Reject the incomplete tail rather than giving some remainders a higher probability.
    do { crypto.getRandomValues(draw); } while (draw[0] >= limit);
  } catch (e) { throw new Error('failed to generate random integer: ' + e.message); }
  return String(min + draw[0] % width);
}

const months = 'January February March April May June July August September October November December'.split(' ');
const weekdays = 'Sunday Monday Tuesday Wednesday Thursday Friday Saturday'.split(' ');
const pad = (n, width = 2) => (n < 0 ? '-' : '') + String(Math.abs(n)).padStart(width, '0');
const layoutTokens = /January|Jan(?![a-z])|Monday|Mon(?![a-z])|MST|_2006|2006|__2|_2|002|0[1-6]|15|[1-5]|PM|pm|[Z-]07(?::00:00|0000|:00|00)?|[.,](?:0+|9+)(?!\d)/g;

function formatLayout(layout, date, offset, name) {
  const year = date.getUTCFullYear(), month = date.getUTCMonth(), day = date.getUTCDate();
  const hour = date.getUTCHours(), minute = date.getUTCMinutes(), second = date.getUTCSeconds();
  const jan1 = new Date(0);
  jan1.setUTCFullYear(year, 0, 1);
  const yday = 1 + Math.floor((date - jan1) / 86400000);
  const meridiem = hour < 12 ? 'AM' : 'PM', weekday = weekdays[date.getUTCDay()];
  const values = {
    January: months[month], Jan: months[month].slice(0, 3), Monday: weekday, Mon: weekday.slice(0, 3),
    '2006': pad(year, 4), _2006: '_' + pad(year, 4), '06': pad(Math.abs(year) % 100), '1': month + 1, '01': pad(month + 1),
    '2': day, '02': pad(day), _2: String(day).padStart(2), '002': pad(yday, 3), __2: String(yday).padStart(3),
    '15': pad(hour), '3': hour % 12 || 12, '03': pad(hour % 12 || 12), '4': minute, '04': pad(minute),
    '5': second, '05': pad(second), PM: meridiem, pm: meridiem.toLowerCase(), MST: name,
  };
  const fraction = pad(date.getUTCMilliseconds(), 3) + '000000'; // Go js/wasm walltime also reads Date in milliseconds.
  // Go chooses the sign after truncating to minutes, even for Accra's historical -52 seconds.
  const negative = Math.trunc(offset / 60) < 0;
  const abs = Math.abs(offset), signHour = (negative ? '-' : '+') + pad(Math.floor(abs / 3600));
  const minutes = pad(Math.floor(abs / 60) % 60), seconds = pad((negative ? -offset : offset) % 60);
  return layout.replace(layoutTokens, token => {
    if (Object.hasOwn(values, token)) return values[token];
    if (token[0] === '.' || token[0] === ',') {
      const digits = fraction.slice(0, (token.length - 1) & 0xfff); // Go packs the requested precision into 12 bits.
      if (token[1] === '0') return token[0] + digits;
      const trimmed = digits.replace(/0+$/, '');
      return trimmed && token[0] + trimmed;
    }
    if (token[0] === 'Z' && !offset) return 'Z';
    const sep = token.includes(':') ? ':' : '';
    return signHour + (token.length > 3 ? sep + minutes : '') + (token.length > 6 ? sep + seconds : '');
  });
}

// Go's zone database and the browser's Intl can disagree on both offsets and abbreviations.
const locations = new Map();
for (const [aliases, initial, rule, packed] of zones) {
  const location = { initial, rule: rules[rule], packed };
  for (const name of aliases.split(' ')) locations.set(name, location);
}

// POSIX Mm.w.d rules count weekdays from one; week five means the last such weekday.
function transition(year, [month, week, weekday, seconds], offset) {
  const first = new Date(0);
  first.setUTCFullYear(year, month - 1, 1);
  const day = 1 + (weekday - first.getUTCDay() + 7) % 7 + 7 * (week - 1);
  first.setUTCDate(day);
  if (first.getUTCMonth() !== month - 1) first.setUTCDate(first.getUTCDate() - 7);
  return first.getTime() / 1000 + seconds - offset;
}

function stateAt(location, now) {
  if (!location.history) {
    let when = 0;
    const bytes = atob(location.packed), history = [];
    for (let i = 0; i < bytes.length; i += 2) {
      const [delta, state] = steps[(bytes.charCodeAt(i) << 8) | bytes.charCodeAt(i + 1)];
      history.push([when += delta, state]);
    }
    location.history = history;
  }
  const { history, initial, rule: [standard, summer, start, end] } = location;
  const sec = Math.floor(now / 1000);
  if (history.length && sec < history.at(-1)[0]) {
    let lo = 0, hi = history.length;
    while (lo < hi) {
      const mid = (lo + hi) >>> 1;
      if (history[mid][0] <= sec) lo = mid + 1; else hi = mid;
    }
    return states[lo ? history[lo - 1][1] : initial];
  }
  if (summer === undefined) return states[standard];
  const year = now.getUTCFullYear();
  const from = transition(year, start, states[standard][1]);
  const until = transition(year, end, states[summer][1]);
  const dst = from < until ? sec >= from && sec < until : sec >= from || sec < until;
  return states[dst ? summer : standard];
}

function zoneAt(zone, now) {
  if (!zone || zone === 'Local') {
    try { zone = Intl.DateTimeFormat().resolvedOptions().timeZone; } catch { /* Fall back to Date. */ }
    if (!locations.has(zone)) return ['Local', -now.getTimezoneOffset() * 60];
  }
  const location = locations.get(zone);
  if (location) return stateAt(location, now);
  const error = zone.includes('..') || /^[\\/]/.test(zone) ? 'time: invalid location name'
    : zone.includes('\0') ? 'Invalid argument' : 'not implemented on js'; // wasm_exec's filesystem stub.
  throw new Error(`Error: invalid timezone ${goQuote(zone)}: ${error}`);
}

function datetime({ format, timezone }) {
  if (!format || /^\p{White_Space}+$/u.test(format)) {
    throw new Error('Error: format is required; use a Go time layout such as 2006-01-02 or 15:04:05.');
  }
  const now = new Date();
  const [name, offset] = zoneAt(timezone, now);
  return formatLayout(format, new Date(+now + offset * 1000), offset, name);
}

export function clockTool(name, argsJSON) {
  try {
    let output;
    switch (name) {
      case 'random_int': output = randomInt(argumentsFor('random', argsJSON)); break;
      case 'get_datetime': output = datetime(argumentsFor('datetime', argsJSON)); break;
      default: throw new Error('unknown clock tool');
    }
    return { output, is_error: false };
  } catch (e) { return { output: e.message, is_error: true }; }
}

export function localTimestamp(now, timezoneOffset) {
  return formatLayout('2006-01-02T15:04:05Z07:00', new Date(+now - timezoneOffset * 60_000), -timezoneOffset * 60, '');
}

const browserEnvironment = `Here is useful information about the environment you are running in:
\t<env>
\tWorking directory: /
\tOperating System: js
\tCPU Architecture: wasm
\t</env>`;

let hookTimezoneOffset;

// Insert these strings after invariant instructions, before history and provider-specific merging.
export function dynamicPrompt(flags, meta = {}) {
  const messages = [];
  if (flags.add_environment_info) messages.push(browserEnvironment);
  if (flags.add_date) {
    const now = meta.now == null ? new Date() : new Date(meta.now);
    // Unlike the datetime tool, Go's time.Local on wasm captures a single offset.
    const offset = meta.timezoneOffset ?? (hookTimezoneOffset ??= now.getTimezoneOffset());
    const date = new Date(+now - offset * 60_000);
    messages.push(`Today's date: ${pad(date.getUTCFullYear(), 4)}-${pad(date.getUTCMonth() + 1)}-${pad(date.getUTCDate())}`);
  }
  return messages;
}
