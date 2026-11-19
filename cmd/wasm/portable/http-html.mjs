// A string-only reader for balanced fragments, not HTML5 tree repair or a Markdown
// engine. No DOM, CSS, script evaluation or subresource loading.
const voidTags = new Set('area base br col embed hr img input link meta param source track wbr'.split(' '));
const blocks = new Set('article aside body div footer header html main nav p section'.split(' '));
const hidden = new Set(['head', 'script', 'style']);
const rawEnd = new Map(['script', 'style', 'iframe', 'textarea'].map(name => [name, new RegExp(`</${name}[ \\t\\f\\r\\n]*>`, 'ig')]));
const mdHidden = new Set(['iframe', 'noscript', 'textarea']);
const emphasis = new Map([['em', '*'], ['i', '*'], ['b', '**'], ['strong', '**']]);
const ignoredText = /[\x00-\x08\x0b\x0e-\x1f\x7f-\x9f\xad]/g;
const spaceText = /[ \t\f\n\r\u2028\u2029]+/g;
const entities = { amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", nbsp: '\u00a0', copy: '©', reg: '®',
  ndash: '–', mdash: '—', hellip: '…', laquo: '«', raquo: '»', eacute: 'é' };

function unsupported(reason) {
  throw new Error(`Unsupported portable HTML conversion (${reason}); request format html for the raw body`);
}

function decode(s) {
  return s.replace(/&(#(?:[xX][\da-fA-F]+|\d+)|[A-Za-z][A-Za-z\d]*)(;?)/g, (raw, name, semi, pos) => {
    if (name[0] !== '#') {
      if (Object.hasOwn(entities, name) && semi) return entities[name];
      if (!semi && s[pos + raw.length] === '=' && !Object.hasOwn(entities, name)) return raw;
      unsupported('named entity or missing semicolon');
    }
    const n = Number(name.slice(1).replace(/^x/i, '0x'));
    if (!semi || !n || n > 0x10ffff || (n >= 0xd800 && n <= 0xdfff) || (n >= 128 && n <= 159)) unsupported('numeric entity');
    return String.fromCodePoint(n);
  });
}

function destination(value) {
  const url = decode(value).trim();
  // Complex destinations need Go's URL escaping. Never reproduce active schemes.
  if (/[^\x21-\x7e]|[\\<>()[\]"'`]|%(?![\da-f]{2})/i.test(url)
    || (/^[a-z][\w.+-]*:/i.test(url) && !/^(?:https?|mailto|tel|sms):/i.test(url))) unsupported('link destination');
  if (url && !URL.canParse(url, 'https://example.invalid/')) unsupported('invalid link destination');
  return url;
}

function textRenderer() {
  const out = [];
  let canBreak = false, last = '';
  function write(s) { if (s) { out.push(s); last = s.at(-1); } }
  function text(s) {
    const cleaned = s.replace(ignoredText, '').replace(spaceText, ' ');
    write((!last || last === ' ') && cleaned.startsWith(' ') ? cleaned.slice(1) : cleaned);
  }
  return {
    text(raw) {
      // html2text's paragraph break flag is set by literal text, not entities.
      if (raw.includes('>')) unsupported('literal > in text mode');
      if (raw.replace(/&(?:#\w+|\w+);/g, '').replace(ignoredText, '').replace(spaceText, '')) canBreak = true;
      text(decode(raw));
    },
    tag(node, close, lastTag) {
      const { name, attrs } = node;
      if (name === 'p' || /^h[1-6]$/.test(name)) {
        if (canBreak && !lastTag) write('\r\n\r\n');
        canBreak = false;
      } else if ((!close && (name === 'br' || name === 'li')) || (close && (name === 'ul' || name === 'ol'))) write('\r\n');
      else if (close && name === 'a') write(destination(attrs.href ?? ''));
    },
    finish: () => out.join(''),
  };
}

function markdownRenderer() {
  // Source whitespace is collapsed before rendering. Newlines in the result
  // are structural; a NUL (rejected in input) marks a <br>.
  const root = { parts: [] }, stack = [root];
  function wrap(s, left, right) {
    const trimmed = s.trim();
    return trimmed ? (s.startsWith(' ') ? ' ' : '') + left + trimmed + right + (s.endsWith(' ') ? ' ' : '') : s;
  }
  function render({ name, attrs, parts }, parent) {
    const s = parts.join('');
    if (blocks.has(name)) return '\n\n' + s + '\n\n';
    if (/^h[1-6]$/.test(name)) {
      if (!s.trim()) return '';
      if (s.trimEnd().endsWith('#') || /[\n\0]/.test(s.trim())) unsupported('complex heading');
      return '\n\n' + '#'.repeat(Number(name[1])) + ' ' + s.trim() + '\n\n';
    }
    if (emphasis.has(name)) {
      if (s.includes('*') || /[\n\0]/.test(s) || parent.parts.at(-1)?.endsWith('*')) unsupported('nested or adjacent emphasis');
      return wrap(s, emphasis.get(name), emphasis.get(name));
    }
    if (name === 'span') return s;
    if (name === 'br') return '\0';
    if (name === 'code') {
      if (!s.trim() || /[`\n\0]/.test(s)) unsupported('complex inline code');
      return '`' + s.trim() + '`';
    }
    if (name === 'hr') return '\n\n* * *\n\n';
    if (name === 'a' || name === 'img') {
      if (attrs.title) unsupported('link or image title');
      const url = destination((name === 'a' ? attrs.href : attrs.src) ?? '');
      if (name === 'img') {
        const alt = decode(attrs.alt ?? '');
        if (/[\x00-\x1f[\]\\]/.test(alt)) unsupported('image alt syntax');
        return url ? `![${alt}](${url})` : '';
      }
      if (!s.trim() || /[\n\0[\]]/.test(s)) unsupported('empty or nested link');
      return wrap(s, '[', `](${url})`);
    }
    if (name === 'li') {
      if ('value' in attrs) unsupported('explicit list numbering');
      if (!['ul', 'ol'].includes(parent.name) || /[\n\0]/.test(s.trim())) unsupported('complex list item');
      const marker = parent.name === 'ul' ? '-' : `${++parent.count}.`;
      return marker + ' ' + s.trim() + '\n';
    }
    if (name === 'ol' || name === 'ul') {
      if ('start' in attrs || 'reversed' in attrs || 'type' in attrs || s.includes('\n\n')) unsupported('complex list');
      return '\n\n' + s.trimEnd() + '\n\n';
    }
    if (['meta', 'link', 'input', 'source', 'wbr'].includes(name)) return '';
    unsupported(`<${name}> in Markdown`);
  }
  return {
    text(raw) {
      const s = decode(raw).replace(/[ \t\n\f\r]+/g, ' ');
      if (/[\x00-\x08\x0b\x0e-\x1f]/.test(s)) unsupported('control character');
      if (stack.at(-1).name === 'code') { stack.at(-1).parts.push(s); return; }
      if (['ul', 'ol'].includes(stack.at(-1).name)) { if (s.trim()) unsupported('non-item list content'); return; }
      if (/[\\`*_\[]/.test(s) || /^(?:#{1,6}(?: |$)|[+~-](?: |$)|\d+[.)](?: |$)|[-=~]+$|[-=~]{3})/.test(s.trimStart())) unsupported('Markdown-sensitive literal text');
      stack.at(-1).parts.push(s.replaceAll('<', '&lt;').replaceAll('>', '&gt;'));
    },
    tag(node, close) {
      if (!close) { node.parts = []; node.count = 0; stack.push(node); }
      else { stack.pop(); stack.at(-1).parts.push(render(node, stack.at(-1))); }
    },
    finish() {
      return root.parts.join('').replace(/ +/g, ' ').replace(/ ?([\n\0]) ?/g, '$1')
        .replace(/[\n\0]+/g, run => run.length > 1 ? '\n\n' : run === '\0' ? '  \n' : '\n').trim();
    },
  };
}

export function convertHTML(html, format) {
  if (html.length > (1 << 20)) unsupported('input size limit');
  const md = format === 'markdown', renderer = md ? markdownRenderer() : textRenderer();
  const stack = [];
  let skip = 0, tokens = 0, pos = 0;
  function token() { if (++tokens > 32768) unsupported('HTML token limit'); }
  const tag = /<(\/?)([a-z][\w:-]*)/iy;
  const attr = /[ \t\f\r\n]+([^\s=<>/"']+)(?:[ \t\f\r\n]*=[ \t\f\r\n]*(?:"([^"]*)"|'([^']*)'|([^\s'"=<>`]+)))?/y;
  const stop = /[ \t\f\r\n]*(\/?)>/y;
  while (pos < html.length) {
    const next = html.indexOf('<', pos), end = next === -1 ? html.length : next;
    if (!skip && end > pos) renderer.text(html.slice(pos, end));
    if (next === -1) break;
    pos = next;
    token();
    if (html.startsWith('<!--', pos)) {
      const close = html.indexOf('-->', pos + 4);
      if (close === -1) unsupported('unterminated comment');
      pos = close + 3;
      continue;
    }
    if (html.slice(pos, pos + 15).toLowerCase() === '<!doctype html>') { pos += 15; continue; }
    tag.lastIndex = pos;
    const m = tag.exec(html);
    if (!m) unsupported('malformed markup');
    const name = m[2].toLowerCase(), attrs = Object.create(null);
    pos = tag.lastIndex;
    attr.lastIndex = pos;
    let a;
    while ((a = attr.exec(html))) {
      token();
      const key = a[1].toLowerCase();
      if (['href', 'src', 'alt', 'title', 'start', 'type', 'value', 'reversed'].includes(key) && !Object.hasOwn(attrs, key)) attrs[key] = a[2] ?? a[3] ?? a[4] ?? '';
      pos = attr.lastIndex;
    }
    stop.lastIndex = pos;
    const ending = stop.exec(html);
    if (!ending || (ending[1] && !voidTags.has(name))) unsupported('malformed or self-closing tag');
    pos = stop.lastIndex;
    if (m[1]) {
      const node = stack.pop();
      if (node?.name !== name) unsupported('unbalanced tags');
      if (node.hidden || node.anchor) skip--;
      if (!skip && !node.hidden) renderer.tag(node, true, pos === html.length);
      continue;
    }
    const hide = hidden.has(name) || (md && mdHidden.has(name));
    const node = { name, attrs, hidden: hide, anchor: !md && name === 'a' };
    if (!skip && !hide) renderer.tag(node, false, pos === html.length);
    if (voidTags.has(name)) { if (!skip && md) renderer.tag(node, true); continue; }
    if (stack.length === 64) unsupported('nesting limit');
    stack.push(node);
    if (hide || node.anchor) skip++;
    const closing = rawEnd.get(name);
    if (skip && closing) {
      closing.lastIndex = pos;
      const match = closing.exec(html);
      if (!match) unsupported('unterminated raw text');
      pos = match.index;
    }
  }
  if (stack.length) unsupported('unclosed tags');
  return renderer.finish();
}
