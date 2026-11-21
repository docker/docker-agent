import { compareKeys } from './go-json.mjs';

// Port of tools.SchemaToMap and openai.ConvertParametersToSchema for remote definitions.
const object = v => v !== null && typeof v === 'object' && !Array.isArray(v);
const composition = ['anyOf', 'oneOf', 'allOf'];
const arrays = [...composition, 'prefixItems'];
const incompatible = ['oneOf', 'allOf', 'not', 'if', 'then', 'else', 'dependentRequired', 'dependentSchemas'];
const orderKey = 'x-docker-agent-property-order';

function defaultType(node) {
  const types = new Set();
  function add(v) {
    let t = typeof v;
    if (v === null) t = 'null';
    else if (Array.isArray(v)) t = 'array';
    else if (Number.isInteger(v) && v >= -(2 ** 63) && v < 2 ** 63) t = 'integer';
    types.add(t);
  }
  if (Array.isArray(node.enum)) node.enum.forEach(add);
  if ('const' in node) add(node.const);
  if (types.has('number')) types.delete('integer');
  return types.size > 1 ? [...types] : types.values().next().value ?? 'object';
}

// Match SchemaToMap's two property-only passes before the full provider walk.
function prepare(node) {
  if (!object(node?.properties)) return;
  for (const p of Object.values(node.properties)) {
    if (!object(p) || '$ref' in p) continue;
    if (composition.some(k => k in p)) {
      for (const k of composition) if (Array.isArray(p[k])) for (const sub of p[k]) {
        prepare(sub);
        prepare(sub?.items);
      }
    } else {
      p.type ??= defaultType(p);
      prepare(p);
      prepare(p.items);
    }
  }
}

function stripNull(node) {
  if (!object(node?.properties)) return;
  const required = new Set(Array.isArray(node.required) ? node.required : []);
  for (const [key, p] of Object.entries(node.properties)) {
    if (!object(p)) continue;
    if (required.has(key) && Array.isArray(p.type) && p.const !== null && !(Array.isArray(p.enum) && p.enum.includes(null))) {
      const types = p.type.filter(t => t !== 'null');
      if (types.length) p.type = types.length === 1 ? types[0] : types;
    }
    stripNull(p);
    stripNull(p.items);
  }
}

function children(node, visit) {
  for (const k of ['properties', 'patternProperties', '$defs', 'definitions']) if (object(node[k])) {
    for (const child of Object.values(node[k])) if (object(child)) visit(child);
  }
  for (const k of arrays) if (Array.isArray(node[k])) {
    for (const child of node[k]) if (object(child)) visit(child);
  }
  for (const k of ['items', 'additionalProperties', 'not', 'if', 'then', 'else', 'contains', 'propertyNames']) {
    if (object(node[k])) visit(node[k]);
  }
}

function compatibleRef(root, node) {
  const ref = node.$ref;
  if (Object.keys(node).length !== 1 || typeof ref !== 'string') return false;
  if (ref === '#') return true;
  if (!ref.startsWith('#/')) return false;
  let target = root;
  for (const segment of ref.slice(2).split('/')) {
    let key = segment.replaceAll('~1', '/').replaceAll('~0', '~');
    if (Array.isArray(target) && /^[-+]?\d+$/.test(key)) key = Number(key);
    else if (!object(target)) return false;
    if (!Object.hasOwn(target, key)) return false;
    target = target[key];
  }
  return object(target);
}

function compatible(node, root) {
  let strict = !incompatible.some(k => k in node) && !object(node.additionalProperties) && node.additionalProperties !== true;
  if ('$ref' in node && !compatibleRef(root, node)) strict = false;
  children(node, child => { if (strict && !compatible(child, root)) strict = false; });
  return strict;
}

function normalize(node) {
  delete node.format;
  if (!('$ref' in node)) {
    const isObject = node.type === 'object' || (Array.isArray(node.type) && node.type.includes('object'));
    if (isObject && (node.additionalProperties == null || node.additionalProperties === true)) node.additionalProperties = false;
    if (object(node.properties)) {
      const required = new Set(Array.isArray(node.required) ? node.required : []);
      const keys = Object.keys(node.properties).sort(compareKeys);
      const order = Array.isArray(node[orderKey]) ? node[orderKey].filter(k => typeof k === 'string' && Object.hasOwn(node.properties, k)) : [];
      node.required = order.length ? [...new Set([...order, ...keys])] : keys;
      for (const [key, p] of Object.entries(node.properties)) {
        if (!object(p)) continue;
        const t = p.type;
        const container = Array.isArray(t) && t.length === 2 && t.includes('null') && t.find(v => v === 'array' || v === 'object');
        if (container) p.type = container;
        else if (required.has(key)) continue;
        if (container || '$ref' in p || 'enum' in p || 'const' in p || t === 'array' || t === 'object') node.properties[key] = {anyOf: [p, {type: 'null'}]};
        else if (typeof t === 'string') p.type = [t, 'null'];
      }
    }
    if (!('type' in node) && !composition.some(k => k in node)) node.type = defaultType(node);
  }
  delete node[orderKey];
  children(node, normalize);
}

export function mcpParameters(schema) {
  const root = schema ?? {};
  if (!object(root) || (root.type != null && root.type !== 'object')) throw new Error('invalid MCP tool definition');
  root.type = 'object';
  root.properties ??= {};
  prepare(root);
  stripNull(root);
  const strict = compatible(root, root);
  normalize(root);
  if (object(root.properties)) for (const p of Object.values(root.properties)) {
    for (const v of [p, ...(Array.isArray(p?.anyOf) ? p.anyOf : [])]) if (v?.type === 'array' && !('items' in v)) v.items = {type: 'object'};
  }
  return { parameters: root, strict };
}
