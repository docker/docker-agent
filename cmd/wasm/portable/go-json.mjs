function kind(raw) {
  switch (raw[0]) {
    case '{': return 'object';
    case '[': return 'array';
    case '"': return 'string';
    case 't': case 'f': return 'bool';
    case 'n': return 'null';
    default: return 'number';
  }
}

// Walk top-level fields in wire order, skipping (but validating) nested values.
// JSON.parse alone discards duplicate fields and rounds large integer guards.
export function decode(raw, spec, args = Object.create(null), retry = true) {
  const [type, fields] = spec;
  if (!raw) return args;
  const whitespace = /[ \r\n\t]*/y, stringEnd = /["\\\x00-\x1f]/g, digits = /\d+/y;
  const stack = [];
  let i = 0, error, overflow = false;
  function space() { whitespace.lastIndex = i; whitespace.exec(raw); i = whitespace.lastIndex; }
  function invalid(reason) {
    if (i === raw.length) throw new Error('unexpected end of JSON input');
    const rune = String.fromCodePoint(raw.codePointAt(i));
    const quoted = rune === "'" ? "\\'" : rune === '"' ? '"' : goQuote(rune).slice(1, -1);
    throw new Error(`invalid character '${quoted}' ${reason}`);
  }
  function endString() {
    stringEnd.lastIndex = ++i;
    for (let token; (token = stringEnd.exec(raw));) {
      i = token.index;
      if (raw[i] === '"') { i++; return; }
      if (raw[i] !== '\\') invalid('in string');
      const escape = raw.slice(i, i + (raw[i + 1] === 'u' ? 6 : 2));
      if (escape.length === 1 || /^\\u[\da-fA-F]{0,3}$/.test(escape)) break;
      if (!/^\\(?:["\\/bfnrt]|u[\da-fA-F]{4})$/.test(escape)) {
        const literal = /^[!-_a-~]+$/.test(escape) ? '`' + escape + '`' : goQuote(escape);
        throw new Error(`invalid escape sequence ${literal} in string`);
      }
      stringEnd.lastIndex = i + escape.length;
    }
    throw new Error('unexpected end of JSON input');
  }
  function numberDigits() {
    digits.lastIndex = i;
    if (!digits.exec(raw)) invalid('in numeric literal');
    i = digits.lastIndex;
  }
  function takeValue() {
    const c = raw[i];
    if (c === '{' || c === '[') {
      if (stack.length === 10_000) throw new Error('exceeded max depth');
      stack.push({ object: c === '{', first: true, key: 0 });
      i++;
    } else if (c === '"') endString();
    else if (c === 'n' || c === 't' || c === 'f') {
      const word = c === 'n' ? 'null' : c === 't' ? 'true' : 'false';
      for (const letter of word) {
        if (raw[i] !== letter) invalid(`in literal ${word} (expecting '${letter}')`);
        i++;
      }
    } else {
      if (!c || !/[\d-]/.test(c)) invalid('looking for beginning of value');
      if (c === '-') i++;
      if (raw[i] === '0') i++; else numberDigits();
      if (raw[i] === '.') { i++; numberDigits(); }
      if (raw[i] === 'e' || raw[i] === 'E') {
        i++;
        if (raw[i] === '-' || raw[i] === '+') i++;
        numberDigits();
      }
    }
  }
  space();
  const top = kind(raw.slice(i));
  if (top !== 'object' && top !== 'null') error = `json: cannot unmarshal ${top} into Go value of type ${type}`;
  takeValue();
  while (stack.length) {
    const frame = stack.at(-1);
    space();
    if (raw[i] === (frame.object ? '}' : ']')) { i++; stack.pop(); continue; }
    if (!frame.first) {
      if (raw[i] !== ',') invalid(frame.object ? 'after object key:value pair' : 'after array element');
      i++; space();
      if (!frame.object) frame.key++;
    }
    frame.first = false;
    if (frame.object) {
      if (raw[i] !== '"') invalid('looking for beginning of object key string');
      const start = i;
      endString();
      if (stack.length === 1 || fields.length === 0) frame.key = JSON.parse(raw.slice(start, i)).toWellFormed();
      space();
      if (raw[i] !== ':') invalid('after object key');
      i++; space();
    }
    const start = i, depth = stack.length;
    takeValue();
    const value = raw.slice(start, i), k = kind(value);
    if (k === 'number' && !Number.isFinite(Number(value))) {
      overflow = true;
      if (!fields.length && !error) {
        const path = '.' + stack.map(f => String(f.key).replaceAll('~', '~0').replaceAll('/', '~1')).join('.');
        const dest = path === '.' ? 'Go value of type float64' : (/\.\d+$/.test(path) ? '' : 'Go struct field ') + path + ' of type float64';
        error = `json: cannot unmarshal number ${value} into ${dest}`;
      }
    }
    if (depth !== 1 || !fields.length || !frame.object) continue;
    const key = frame.key;
    const field = key.replaceAll('ſ', 's').toLowerCase();
    if (!fields.includes(field)) continue;
    const path = `Go struct field ${type.split('.')[1]}.${key}`;
    if (field === 'last_known_revision') {
      if (k === 'null') delete args[field];
      else if (k !== 'number') error ??= `json: cannot unmarshal ${k} into ${path} of type int`;
      else if (!/^-?\d{1,19}$/.test(value) || BigInt.asIntN(64, BigInt(value)) !== BigInt(value)) {
        error ??= `json: cannot unmarshal number ${value} into ${path} of type int`;
      } else args[field] = BigInt(value);
    } else if (k === 'string') args[field] = JSON.parse(value).toWellFormed();
    else if (k !== 'null') error ??= `json: cannot unmarshal ${k} into ${path} of type string`;
  }
  space();
  if (i !== raw.length) invalid('after top-level value');
  if (error) {
    // aijson retries the partially populated struct if a known scalar was null.
    if (retry && fields.length && top === 'object' && !overflow && raw.includes('null')) {
      const obj = JSON.parse(raw);
      if (fields.some(f => obj[f] === null)) {
        const keys = Object.keys(obj).filter(k => !(fields.includes(k) && obj[k] === null)).sort();
        try {
          const pairs = keys.map(k => JSON.stringify(k) + ':' + JSON.stringify(obj[k]));
          return decode('{' + pairs.join(',') + '}', spec, args, false);
        } catch { /* Report the first, strict error if the repair fails. */ }
      }
    }
    throw new Error(error);
  }
  return args;
}

export function escaped(c) { return '\\u' + c.charCodeAt(0).toString(16).padStart(4, '0'); }
const controls = { '\x07': '\\a', '\b': '\\b', '\t': '\\t', '\n': '\\n', '\v': '\\v', '\f': '\\f', '\r': '\\r', '"': '\\"', '\\': '\\\\' };
export function goQuote(s) {
  // Go's %q leaves printable runes intact and uses Go (not JSON) control escapes.
  return '"' + s.replace(/["\\]|[^\p{L}\p{M}\p{N}\p{P}\p{S} ]/gu, c => {
    if (Object.hasOwn(controls, c)) return controls[c];
    const n = c.codePointAt(0);
    if (n < 32 || n === 127) return '\\x' + n.toString(16).padStart(2, '0');
    return n <= 0xffff ? escaped(c) : '\\U' + n.toString(16).padStart(8, '0');
  }) + '"';
}

// Go sorts UTF-8 map keys by code point, JS sorts strings by UTF-16 unit.
export function compareKeys(a, b) {
  for (let i = 0; i < a.length && i < b.length;) {
    const x = a.codePointAt(i), y = b.codePointAt(i);
    if (x !== y) return x - y;
    i += x > 0xffff ? 2 : 1;
  }
  return a.length - b.length;
}
