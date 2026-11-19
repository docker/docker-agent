// Session-local counterparts of plan.MemoryStorage and memory/database/inmemory.
const encoder = new TextEncoder();
const maxContent = 10 << 20;
const maxEncodedPlan = 7 * maxContent;
const maxEntries = 256;
const maxMemoryBytes = 1 << 20;
const maxArgs = 16 << 20; // The portable model-stream limit.

const argTypes = new Map([
  ['list_plans', ['plan', []]],
  ['write_plan', ['plan.WritePlanArgs', ['name', 'title', 'content', 'author', 'status', 'last_known_revision']]],
  ['read_plan', ['plan.ReadPlanArgs', ['name']]],
  ['delete_plan', ['plan.DeletePlanArgs', ['name', 'last_known_revision']]],
  ['set_plan_status', ['plan.SetPlanStatusArgs', ['name', 'status', 'last_known_revision']]],
  ['get_plan_status', ['plan.GetPlanStatusArgs', ['name']]],
  ['add_memory', ['memory.AddMemoryArgs', ['memory', 'category']]],
  ['delete_memory', ['memory.DeleteMemoryArgs', ['id']]],
  ['update_memory', ['memory.UpdateMemoryArgs', ['id', 'memory', 'category']]],
  ['get_memories', ['map[string]interface {}', []]],
  ['search_memories', ['memory.SearchMemoriesArgs', ['query', 'category']]],
]);

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
function decode(raw, spec, args = Object.create(null), retry = true) {
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
    if (depth !== 1 || !fields.length) continue;
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

function escaped(c) { return '\\u' + c.charCodeAt(0).toString(16).padStart(4, '0'); }
function resultJSON(value) { return JSON.stringify(value).replace(/[\u2028\u2029]/g, escaped); }
function memoryJSON(value) { return JSON.stringify(value).replace(/[<>&\u2028\u2029]/g, escaped); }

// json.MarshalIndent additionally escapes HTML and the two JS line separators.
function storageBytes(value, indent) {
  const s = JSON.stringify(value, null, indent);
  return encoder.encode(s).length + 5 * (s.length - s.replace(/[<>&]/g, '').length)
    + 3 * (s.length - s.replace(/[\u2028\u2029]/g, '').length);
}

const controls = { '\x07': '\\a', '\b': '\\b', '\t': '\\t', '\n': '\\n', '\v': '\\v', '\f': '\\f', '\r': '\\r', '"': '\\"', '\\': '\\\\' };
function goQuote(s) {
  // Go's %q leaves printable runes intact and uses Go (not JSON) control escapes.
  return '"' + s.replace(/["\\]|[^\p{L}\p{M}\p{N}\p{P}\p{S} ]/gu, c => {
    if (Object.hasOwn(controls, c)) return controls[c];
    const n = c.codePointAt(0);
    if (n < 32 || n === 127) return '\\x' + n.toString(16).padStart(2, '0');
    return n <= 0xffff ? escaped(c) : '\\U' + n.toString(16).padStart(8, '0');
  }) + '"';
}

function store(label, capacity) {
  const rows = new Map();
  let used = 0;
  return {
    rows,
    put(key, value, size) {
      const old = rows.get(key);
      if (!old && rows.size >= maxEntries) throw new Error(`${label} count limit reached`);
      const next = used + size - (old?.size ?? 0);
      if (next > capacity) throw new Error(`${label} storage size limit reached`);
      rows.set(key, { value, size });
      used = next;
    },
    remove(key) { used -= rows.get(key)?.size ?? 0; rows.delete(key); },
  };
}

function planTool(plans, tool, a) {
  if (tool === 'list_plans') {
    return resultJSON({ plans: Array.from(plans.rows.keys()).sort().map(key => {
      const { content, ...summary } = plans.rows.get(key).value;
      return summary;
    }) });
  }
  if (tool === 'write_plan' && !a.content) throw new Error('content must not be empty');
  if (tool === 'set_plan_status' && !a.status) throw new Error('status must not be empty');
  const name = a.name ?? '';
  if (!/^[a-z0-9][a-z0-9_-]*$/.test(name)) {
    throw new Error(`invalid plan name ${goQuote(name)}: use only lowercase letters, digits, '-' and '_', starting with a letter or digit`);
  }
  const old = plans.rows.get(name)?.value;
  if (!old && tool !== 'write_plan') {
    throw new Error(tool === 'set_plan_status' ? `plan not found: "${name}"` : `plan "${name}" not found`);
  }
  const current = old?.revision ?? 0;
  if (a.last_known_revision !== undefined && a.last_known_revision !== BigInt(current)) {
    throw new Error(`version conflict on plan "${name}": last_known_revision ${a.last_known_revision} does not match current revision ${current}; re-read the plan and retry`);
  }
  const statusView = p => resultJSON({ name, status: p.status ?? '', revision: p.revision });
  if (tool === 'get_plan_status') return statusView(old);
  if (tool === 'read_plan') return resultJSON(old);
  if (tool === 'delete_plan') {
    plans.remove(name);
    return resultJSON({ deleted: name });
  }
  const p = {
    name, title: a.title || old?.title, content: a.content ?? old.content, author: a.author || old?.author,
    status: a.status || old?.status, revision: current + 1, updatedAt: new Date().toISOString().slice(0, 19) + 'Z',
  };
  const bytes = encoder.encode(p.content).length;
  if (bytes > maxContent) throw new Error(`plan "${name}" content is too large to store (${bytes} bytes; max ${maxContent})`);
  const size = storageBytes(p, 2);
  if (size > maxEncodedPlan) throw new Error(`plan "${name}" is too large to store (${size} bytes; max ${maxEncodedPlan})`);
  plans.put(name, p, size);
  return tool === 'set_plan_status' ? statusView(p) : resultJSON(p);
}

function localTimestamp(now, offset) {
  const wall = new Date(now.getTime() + offset * 60_000).toISOString().slice(0, 19);
  const pad = n => String(n).padStart(2, '0');
  const mins = Math.abs(offset);
  return wall + (offset === 0 ? 'Z' : `${offset > 0 ? '+' : '-'}${pad(Math.floor(mins / 60))}:${pad(mins % 60)}`);
}

// strings.ToLower uses simple, context-independent mappings (no final sigma or dotted-i expansion).
function lower(s) { return s.replaceAll('Σ', 'σ').replaceAll('İ', 'I').toLowerCase(); }

function equalFold(a, b) {
  if (a === b) return true;
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length;) {
    const x = a.codePointAt(i), y = b.codePointAt(i);
    // A Unicode-insensitive backreference uses the same simple fold as Go's EqualFold.
    if (x !== y && !/^(.)\1$/isu.test(String.fromCodePoint(x, y))) return false;
    i += x > 0xffff ? 2 : 1;
  }
  return true;
}

function memoryTool(memories, tool, { id = '', memory = '', category = '', query = '' }, offset) {
  switch (tool) {
    case 'add_memory': {
      const now = new Date();
      // Go's js/wasm wall clock also has millisecond resolution. Collisions are errors, not overwrites.
      id = String(BigInt(now.getTime()) * 1_000_000n);
      if (memories.rows.has(id)) throw new Error(`Error calling tool: failed to add memory: memory ID already exists: ${id}`);
      const row = { id, created_at: localTimestamp(new Date(), offset), memory, category: category || undefined };
      memories.put(id, row, storageBytes(row));
      return `Memory added successfully with ID: ${id}`;
    }
    case 'update_memory': {
      const old = memories.rows.get(id)?.value;
      if (!old) throw new Error('Error calling tool: failed to update memory: ' + (id ? `memory not found: ${id}` : 'memory ID cannot be empty'));
      const row = { ...old, memory, category: category || undefined };
      memories.put(id, row, storageBytes(row));
      return `Memory with ID ${id} updated successfully`;
    }
    case 'delete_memory':
      memories.remove(id);
      return `Memory with ID ${id} deleted successfully`;
    default: {
      const words = lower(query).match(/[^\p{White_Space}]+/gu) ?? [];
      const results = [];
      for (const { value } of memories.rows.values()) {
        if (category && !equalFold(category, value.category ?? '')) continue;
        if (words.length) {
          const text = lower(value.memory);
          if (!words.every(word => text.includes(word))) continue;
        }
        results.push(value);
      }
      return memoryJSON(results.length ? results : null);
    }
  }
}

// Create once per session, outside the restart path. Call only after any required approval.
export function localTools() {
  const plans = store('plan', maxEncodedPlan);
  const memories = store('memory', maxMemoryBytes);
  // time.Local in Go's js/wasm build is a fixed offset, not a live timezone.
  const offset = -new Date().getTimezoneOffset();
  return {
    call(name, argsJSON, signal) {
      try {
        if (signal?.aborted) throw new Error('The tool call was canceled by the user.');
        const spec = argTypes.get(name);
        if (!spec) throw new Error(`Tool '${name}' is not available. You can only use the tools provided to you.`);
        if (typeof argsJSON !== 'string') throw new Error('tool arguments must be a JSON string');
        if (argsJSON.length > maxArgs || encoder.encode(argsJSON).length > maxArgs) throw new Error('local tool argument size limit reached');
        let args;
        try { args = name === 'list_plans' ? {} : decode(argsJSON, spec); }
        catch (e) { throw new Error('Error calling tool: ' + e.message); }
        const output = spec[0].startsWith('plan') ? planTool(plans, name, args) : memoryTool(memories, name, args, offset);
        return { output, is_error: false };
      } catch (e) { return { output: e.message, is_error: true }; }
    },
  };
}
