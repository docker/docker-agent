import { localTimestamp } from './clock.mjs';
import { decode, escaped, goQuote } from './go-json.mjs';

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

function resultJSON(value) { return JSON.stringify(value).replace(/[\u2028\u2029]/g, escaped); }
function memoryJSON(value) { return JSON.stringify(value).replace(/[<>&\u2028\u2029]/g, escaped); }

// json.MarshalIndent additionally escapes HTML and the two JS line separators.
function storageBytes(value, indent) {
  const s = JSON.stringify(value, null, indent);
  return encoder.encode(s).length + 5 * (s.length - s.replace(/[<>&]/g, '').length)
    + 3 * (s.length - s.replace(/[\u2028\u2029]/g, '').length);
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
    status: a.status || old?.status, revision: current + 1, updatedAt: localTimestamp(new Date(), 0),
  };
  const bytes = encoder.encode(p.content).length;
  if (bytes > maxContent) throw new Error(`plan "${name}" content is too large to store (${bytes} bytes; max ${maxContent})`);
  const size = storageBytes(p, 2);
  if (size > maxEncodedPlan) throw new Error(`plan "${name}" is too large to store (${size} bytes; max ${maxEncodedPlan})`);
  plans.put(name, p, size);
  return tool === 'set_plan_status' ? statusView(p) : resultJSON(p);
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
      id = String(BigInt.asIntN(64, BigInt(now.getTime()) * 1_000_000n));
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
export function localTools(offset = new Date().getTimezoneOffset()) {
  const plans = store('plan', maxEncodedPlan);
  const memories = store('memory', maxMemoryBytes);
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
