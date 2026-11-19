//! Session-local discovery over admitted tool definitions. Never list or execute a source here.

use regex::Regex;
use serde::Serialize;
use serde_json::{Map, Value};
use std::{cmp::Reverse, collections::BTreeMap, fmt::Write};

pub const SEARCH: &str = "search_tool";
pub const ADD: &str = "add_tool";
pub const INSTRUCTIONS: &str = "## Deferred Tools\n\nUse search_tool to discover additional tools by keyword (e.g., \"remote\", \"read\", \"write\"). Use add_tool to activate a discovered tool.";

/// T carries the source's dispatch binding (not just the function name).
#[derive(Debug)]
pub struct Tool<T> {
    pub definition: Value,
    pub target: T,
    pub toon: bool,
}

impl<T> Tool<T> {
    fn description(&self) -> &str {
        self.definition["function"]["description"]
            .as_str()
            .unwrap_or("")
    }
}

#[derive(Debug)]
pub struct Discovery<T> {
    enabled: bool,
    catalog: BTreeMap<String, (Tool<T>, bool)>,
}

impl<T> Default for Discovery<T> {
    fn default() -> Self {
        Self {
            enabled: false,
            catalog: BTreeMap::new(),
        }
    }
}

impl<T> Discovery<T> {
    /// Register static source snapshots in config order, *after* allow-list and read-only filters.
    /// Returns this source's eager tools, preserving order and leaving cross-source dedup to the host.
    pub fn wrap_source(
        &mut self,
        tools: impl IntoIterator<Item = (Value, T)>,
        defer_all: bool,
        defer_names: &[String],
        toon: &ToonSpec,
    ) -> Result<Vec<Tool<T>>, String> {
        let mut eager = Vec::new();
        let mut deferred = Vec::new();
        for (definition, target) in tools {
            let function = &definition["function"];
            let name = function["name"]
                .as_str()
                .filter(|s| !s.is_empty())
                .ok_or("missing tool name")?;
            if function.get("description").is_some_and(|v| !v.is_string()) {
                return Err(format!("invalid tool description for {name:?}"));
            }
            let hidden =
                (defer_all || defer_names.iter().any(|n| n == name)).then(|| name.to_owned());
            let toon = toon.matches(name);
            let tool = Tool {
                definition,
                target,
                toon,
            };
            if let Some(name) = hidden {
                deferred.push((name, tool));
            } else {
                eager.push(tool);
            }
        }
        // Publish only a successfully listed source. A failed snapshot can be retried.
        self.enabled |= defer_all || !defer_names.is_empty();
        for (name, tool) in deferred {
            self.catalog.entry(name).or_insert((tool, false));
        }
        Ok(eager)
    }

    /// A configured source enables the discovery helpers even if no tools matched its defer list.
    pub fn enabled(&self) -> bool {
        self.enabled
    }

    pub fn catalog(&self) -> impl Iterator<Item = (&str, &Tool<T>)> {
        self.catalog
            .iter()
            .map(|(name, (tool, _))| (name.as_str(), tool))
    }

    pub fn activated(&self) -> impl Iterator<Item = (&str, &Tool<T>)> {
        self.catalog
            .iter()
            .filter(|(_, (_, active))| *active)
            .map(|(k, (tool, _))| (k.as_str(), tool))
    }

    /// `Err` denotes a model-facing tool result with IsError, not a fatal turn error.
    pub fn add(&mut self, name: &str) -> Result<String, String> {
        let Some((tool, active)) = self.catalog.get_mut(name) else {
            return Err(format!("Tool '{name}' not found."));
        };
        if *active {
            return Ok(format!("Tool '{name}' is already active"));
        }
        *active = true;
        Ok(format!(
            "Tool '{name}' has been activated and is now available for use.\n\nDescription: {}",
            tool.description()
        ))
    }

    pub fn search(&self, query: &str) -> Result<String, String> {
        #[derive(Serialize)]
        struct Hit<'a> {
            #[serde(skip)]
            rank: Reverse<i32>,
            name: &'a str,
            description: &'a str,
        }
        if query.len() > 64 * 1024 {
            return Err("deferred search query size limit reached".into());
        }
        let pattern: Vec<_> = query.trim().chars().map(simple_lower).collect();
        let mut hits = Vec::new();
        let mut work = MAX_SEARCH_WORK;
        for (name, (tool, active)) in &self.catalog {
            if *active {
                continue;
            }
            let description = tool.description();
            let score = if pattern.is_empty() {
                Some(0)
            } else {
                work = work
                    .checked_sub(name.len() + description.len() + 1)
                    .ok_or("deferred search work limit reached")?;
                score(&format!("{name} {description}"), &pattern, &mut work)?
            };
            if let Some(score) = score {
                hits.push(Hit {
                    rank: Reverse(score),
                    name,
                    description,
                });
            }
        }
        if hits.is_empty() {
            return Err(format!("No deferred tools found matching '{query}'"));
        }
        // Go leaves ties in map iteration order. Keep a deterministic name tiebreak here.
        hits.sort_by_key(|hit| hit.rank);
        let json = serde_json::to_string(&hits)
            .expect("string fields")
            .replace('&', "\\u0026")
            .replace('<', "\\u003c")
            .replace('>', "\\u003e")
            .replace('\u{2028}', "\\u2028")
            .replace('\u{2029}', "\\u2029");
        Ok(format!("Found {} deferred tool(s):\n{json}", hits.len()))
    }
}

const MAX_SEARCH_WORK: usize = 4_000_000;

fn simple_lower(c: char) -> char {
    // Go's Unicode 17 additions to the Rust 1.90 (Unicode 16) lowercase table.
    match c as u32 {
        0xa7ce | 0xa7d2 | 0xa7d4 => char::from_u32(c as u32 + 1).unwrap(),
        0x16ea0..=0x16eb8 => char::from_u32(c as u32 + 0x1b).unwrap(),
        _ => c.to_lowercase().next().unwrap(),
    }
}

// Score-only equivalent of pkg/fuzzy's uninitialized FuzzyMatchV2 (no boundary bonuses).
// fzf v0.74.4, copyright (c) 2013-2026 Junegunn Choi, MIT; see pkg/fuzzy/LICENSE.fzf.
fn score(text: &str, pattern: &[char], work: &mut usize) -> Result<Option<i32>, String> {
    if pattern.len() > text.len() {
        return Ok(None);
    }
    let ascii_fast = pattern.len() <= 2 && text.is_ascii();
    let greedy = pattern.len() > 1000;
    let text: Vec<_> = text
        .chars()
        .map(|c| {
            if ascii_fast || greedy {
                normalize(simple_lower(c))
            } else if c.is_ascii() {
                c
            } else {
                // Go checks category Lu, not Unicode's broader Uppercase property.
                let upper = (c.is_uppercase()
                    && !matches!(c as u32, 0x2160..=0x216f | 0x24b6..=0x24cf))
                    || matches!(c as u32, 0xa7ce | 0xa7d2 | 0xa7d4 | 0x16ea0..=0x16eb8);
                normalize(if upper { simple_lower(c) } else { c })
            }
        })
        .collect();
    let mut next = 0;
    let mut first = Vec::new();
    for &p in pattern {
        let Some(pos) = text[next..].iter().position(|&c| c == p) else {
            return Ok(None);
        };
        first.push(next + pos);
        next += pos + 1;
    }
    if pattern.len() == 1 {
        return Ok(Some(16));
    }
    if greedy {
        let mut start = next;
        for p in pattern.iter().rev() {
            start = text[..start].iter().rposition(|c| c == p).unwrap();
        }
        let mut points = 0;
        let mut in_gap = false;
        let mut p = 0;
        for (i, c) in text[start..next].iter().enumerate() {
            if c == &pattern[p] {
                points += if i > 0 && !in_gap { 20 } else { 16 };
                p += 1;
                in_gap = false;
            } else {
                points -= if in_gap { 1 } else { 3 };
                in_gap = true;
            }
        }
        return Ok(Some(points));
    }
    let end = text
        .iter()
        .rposition(|c| Some(c) == pattern.last())
        .unwrap()
        + 1;
    let start = first[0];
    let width = end - start;
    *work = work
        .checked_sub(width.saturating_mul(pattern.len()))
        .ok_or("deferred search work limit reached")?;
    // One row suffices: only the previous diagonal and the new left cell affect a score.
    let mut row = vec![(0i32, false); width];
    let mut left = 0;
    let mut gap = false;
    for (cell, &c) in row.iter_mut().zip(&text[start..end]) {
        let matched = c == pattern[0];
        left = if matched {
            16
        } else {
            (left - if gap { 1 } else { 3 }).max(0)
        };
        *cell = (left, matched);
        gap = !matched;
    }
    let mut best = 0;
    for (&p, &from) in pattern[1..].iter().zip(&first[1..]) {
        let mut diag = row[from - start - 1];
        left = 0;
        gap = false;
        best = 0;
        for (cell, &c) in row[from - start..].iter_mut().zip(&text[from..end]) {
            let skip = left - if gap { 1 } else { 3 };
            let take = if c == p {
                diag.0 + if diag.1 { 20 } else { 16 }
            } else {
                0
            };
            left = skip.max(take).max(0);
            gap = take < skip;
            diag = std::mem::replace(cell, (left, c == p && !gap));
            best = best.max(left);
        }
    }
    Ok(Some(best))
}

// fzf’s one-rune normalization table, not general Unicode decomposition.
fn normalize(c: char) -> char {
    match c {
        '！'..='～' => char::from_u32(c as u32 - 0xfee0).unwrap(),
        'à' | 'á' | 'â' | 'ã' | 'ä' | 'å' | 'ā' | 'ă' | 'ą' | 'ǎ' | 'ȁ' | 'ȃ' | 'ȧ' | 'ɐ' | 'ɑ'
        | 'ͣ' | 'ḁ' | 'ẚ' | 'ạ' | 'ả' | 'ấ' | 'ầ' | 'ẩ' | 'ẫ' | 'ậ' | 'ắ' | 'ằ' | 'ẳ' | 'ẵ'
        | 'ặ' => 'a',
        'ƀ' | 'ƃ' | 'ɓ' | 'ḃ' | 'ḅ' | 'ḇ' => 'b',
        'ç' | 'ć' | 'ĉ' | 'ċ' | 'č' | 'ƈ' | 'ȼ' | 'ɕ' | 'ʗ' | 'ͨ' | 'ↄ' => 'c',
        'ď' | 'đ' | 'ƌ' | 'ȡ' | 'ɖ' | 'ɗ' | 'ͩ' | 'ḋ' | 'ḍ' | 'ḏ' | 'ḑ' | 'ḓ' => {
            'd'
        }
        'è' | 'é' | 'ê' | 'ë' | 'ē' | 'ĕ' | 'ė' | 'ę' | 'ě' | 'ǝ' | 'ȅ' | 'ȇ' | 'ȩ' | 'ɇ' | 'ɘ'
        | 'ɛ' | 'ɜ' | 'ɝ' | 'ɞ' | 'ʚ' | 'ͤ' | 'ᴈ' | 'ḙ' | 'ḛ' | 'ẹ' | 'ẻ' | 'ẽ' | 'ế' | 'ề'
        | 'ể' | 'ễ' | 'ệ' => 'e',
        'ƒ' | 'ḟ' => 'f',
        'ĝ' | 'ğ' | 'ġ' | 'ģ' | 'ǥ' | 'ǧ' | 'ǵ' | 'ɠ' | 'ɡ' | 'ḡ' => 'g',
        'ĥ' | 'ħ' | 'ȟ' | 'ɥ' | 'ɦ' | 'ʮ' | 'ͪ' | 'ḣ' | 'ḥ' | 'ḧ' | 'ḩ' | 'ḫ' | 'ẖ' | 'ₕ' => {
            'h'
        }
        'ì' | 'í' | 'î' | 'ï' | 'ĩ' | 'ī' | 'ĭ' | 'į' | 'ı' | 'ǐ' | 'ȉ' | 'ȋ' | 'ɨ' | 'ͥ' | 'ᴉ'
        | 'ᵢ' | 'ḭ' | 'ỉ' | 'ị' | 'ⁱ' => 'i',
        'ĵ' | 'ǰ' | 'ȷ' | 'ɉ' | 'ɟ' | 'ʝ' => 'j',
        'ķ' | 'ƙ' | 'ǩ' | 'ʞ' | 'ḱ' | 'ḳ' | 'ḵ' | 'ₖ' => 'k',
        'ĺ' | 'ļ' | 'ľ' | 'ŀ' | 'ł' | 'ƚ' | 'ȴ' | 'ɫ' | 'ɬ' | 'ɭ' | 'ḷ' | 'ḻ' | 'ḽ' | 'ₗ' => {
            'l'
        }
        'ɯ' | 'ɰ' | 'ɱ' | 'ͫ' | 'ᴟ' | 'ḿ' | 'ṁ' | 'ṃ' | 'ₘ' => 'm',
        'ñ' | 'ń' | 'ņ' | 'ň' | 'ƞ' | 'ǹ' | 'ȵ' | 'ɲ' | 'ɳ' | 'ṅ' | 'ṇ' | 'ṉ' | 'ṋ' | 'ₙ' => {
            'n'
        }
        'ò' | 'ó' | 'ô' | 'õ' | 'ö' | 'ø' | 'ō' | 'ŏ' | 'ő' | 'ơ' | 'ǒ' | 'ǫ' | 'ȍ' | 'ȏ' | 'ȯ'
        | 'ɔ' | 'ɵ' | 'ͦ' | 'ᴑ' | 'ᴒ' | 'ᴓ' | 'ᴖ' | 'ᴗ' | 'ọ' | 'ỏ' | 'ố' | 'ồ' | 'ổ' | 'ỗ'
        | 'ộ' | 'ớ' | 'ờ' | 'ở' | 'ỡ' | 'ợ' => 'o',
        'ƥ' | 'ṕ' | 'ṗ' | 'ₚ' => 'p',
        'ɋ' | 'ʠ' => 'q',
        'ŕ' | 'ŗ' | 'ř' | 'ȑ' | 'ȓ' | 'ɍ' | 'ɹ' | 'ɺ' | 'ɻ' | 'ɼ' | 'ɽ' | 'ɾ' | 'ɿ' | 'ͬ' | 'ᵣ'
        | 'ṙ' | 'ṛ' | 'ṟ' => 'r',
        'ß' | 'ś' | 'ŝ' | 'ş' | 'š' | 'ſ' | 'ș' | 'ȿ' | 'ʂ' | 'ṡ' | 'ṣ' | 'ẛ' | 'ₛ' => {
            's'
        }
        'ţ' | 'ť' | 'ŧ' | 'ƫ' | 'ƭ' | 'ț' | 'ȶ' | 'ʇ' | 'ʈ' | 'ͭ' | 'ṫ' | 'ṭ' | 'ṯ' | 'ṱ' | 'ẗ'
        | 'ₜ' => 't',
        'ù' | 'ú' | 'û' | 'ü' | 'ũ' | 'ū' | 'ŭ' | 'ů' | 'ű' | 'ų' | 'ư' | 'ǔ' | 'ȕ' | 'ȗ' | 'ʉ'
        | 'ͧ' | 'ᴝ' | 'ᴞ' | 'ᵤ' | 'ṳ' | 'ṵ' | 'ṷ' | 'ụ' | 'ủ' | 'ứ' | 'ừ' | 'ử' | 'ữ' | 'ự' => {
            'u'
        }
        'ʋ' | 'ʌ' | 'ͮ' | 'ᵥ' | 'ṽ' | 'ṿ' => 'v',
        'ŵ' | 'ʍ' | 'ẁ' | 'ẃ' | 'ẅ' | 'ẇ' | 'ẉ' | 'ẘ' => 'w',
        'ͯ' | 'ẋ' | 'ẍ' => 'x',
        'ý' | 'ÿ' | 'ŷ' | 'ƴ' | 'ȳ' | 'ɏ' | 'ʎ' | 'ẏ' | 'ẙ' | 'ỳ' | 'ỵ' | 'ỷ' | 'ỹ' => {
            'y'
        }
        'ź' | 'ż' | 'ž' | 'ƶ' | 'ȥ' | 'ɀ' | 'ʐ' | 'ʑ' | 'ẑ' | 'ẓ' | 'ẕ' => 'z',
        'À' | 'Á' | 'Â' | 'Ã' | 'Ä' | 'Å' | 'Ⱥ' | 'ᴀ' | 'Ấ' | 'Ầ' | 'Ẩ' | 'Ẫ' | 'Ậ' | 'Ắ' | 'Ằ'
        | 'Ẳ' | 'Ẵ' | 'Ặ' => 'A',
        'Ɓ' | 'Ƀ' | 'ʙ' | 'ᴃ' => 'B',
        'Ç' | 'Ȼ' | 'ᴄ' => 'C',
        'Ɖ' | 'Ɗ' | 'ᴅ' => 'D',
        'È' | 'É' | 'Ê' | 'Ë' | 'Ǝ' | 'Ɛ' | 'Ɇ' | 'ᴇ' | 'Ế' | 'Ề' | 'Ể' | 'Ễ' | 'Ệ' => {
            'E'
        }
        'Ɠ' | 'ɢ' | 'ʛ' => 'G',
        'ʜ' => 'H',
        'Ì' | 'Í' | 'Î' | 'Ï' | 'İ' | 'Ɨ' | 'ɪ' => 'I',
        'Ɉ' | 'ᴊ' => 'J',
        'ᴋ' => 'K',
        'Ƚ' | 'ʟ' | 'ᴌ' => 'L',
        'Ɯ' | 'ᴍ' => 'M',
        'Ñ' | 'Ɲ' | 'Ƞ' | 'ɴ' | 'ᴎ' => 'N',
        'Ò' | 'Ó' | 'Ô' | 'Õ' | 'Ö' | 'Ø' | 'Ɔ' | 'Ɵ' | 'ᴏ' | 'ᴐ' | 'Ố' | 'Ồ' | 'Ổ' | 'Ỗ' | 'Ộ'
        | 'Ớ' | 'Ờ' | 'Ở' | 'Ỡ' | 'Ợ' => 'O',
        'ᴘ' => 'P',
        'Ɋ' => 'Q',
        'Ɍ' | 'ʀ' | 'ʁ' | 'ᴙ' | 'ᴚ' => 'R',
        'Ʈ' | 'Ⱦ' | 'ᴛ' => 'T',
        'Ù' | 'Ú' | 'Û' | 'Ü' | 'Ʉ' | 'ᴜ' | 'Ứ' | 'Ừ' | 'Ử' | 'Ữ' | 'Ự' => 'U',
        'Ʋ' | 'Ʌ' | 'ᴠ' => 'V',
        'ᴡ' => 'W',
        'Ý' | 'Ÿ' | 'Ɏ' | 'ʏ' => 'Y',
        'ᴢ' => 'Z',
        '｡' => '.',
        _ => c,
    }
}

/// Compile once when loading config. Invalid/unsupported patterns reject the source, not just a tool.
#[derive(Debug, Default)]
pub struct ToonSpec(Vec<Regex>);

impl ToonSpec {
    pub fn compile(spec: &str) -> Result<Self, String> {
        if spec.is_empty() {
            return Ok(Self::default());
        }
        if spec.len() > 4096 || spec.split(',').count() > 64 {
            return Err("toon pattern limit reached".into());
        }
        spec.split(',')
            .map(str::trim)
            .map(compile_pattern)
            .collect::<Result<Vec<_>, _>>()
            .map(Self)
    }
    pub fn matches(&self, name: &str) -> bool {
        self.0.iter().any(|re| re.is_match(name))
    }
}

// Go and Rust regexes differ on escapes, nested character classes, repetition and flags.
// Accept their shared subset, not a best effort reinterpretation of an unsupported selector.
fn compile_pattern(pattern: &str) -> Result<Regex, String> {
    let unsupported = || format!("unsupported toon regular expression {pattern:?}");
    let mut chars = pattern.chars().peekable();
    let mut class = false;
    let mut repeated = false;
    while let Some(c) = chars.next() {
        match c {
            '\\' => {
                if !chars.next().is_some_and(|c| ".^$|?*+()[]{}\\-".contains(c)) {
                    return Err(unsupported());
                }
            }
            '[' if class => return Err(unsupported()),
            '[' => class = true,
            ']' => class = false,
            '-' | '&' | '~' if class && chars.peek() == Some(&c) => return Err(unsupported()),
            _ if class => (),
            '(' if chars.next_if_eq(&'?').is_some() => {
                if chars.next_if_eq(&':').is_none() {
                    // Plain flag groups, e.g. (?i), or scoped flags, e.g. (?is:...).
                    if !chars.peek().is_some_and(|c| "imsU".contains(*c)) {
                        return Err(unsupported());
                    }
                    while chars.next_if(|c| "imsU".contains(*c)).is_some() {}
                    if !matches!(chars.next(), Some(')' | ':')) {
                        return Err(unsupported());
                    }
                }
            }
            '{' | '}' => return Err(unsupported()),
            '*' | '+' | '?' => {
                if repeated {
                    return Err(unsupported());
                }
                chars.next_if_eq(&'?');
                repeated = true;
                continue;
            }
            _ => (),
        }
        repeated = false;
    }
    regex::RegexBuilder::new(pattern)
        .size_limit(256 * 1024)
        .build()
        .map_err(|e| format!("invalid toon regular expression {pattern:?}: {e}"))
}

/// Transform only a returned ToolCallResult's Output; leave its IsError and metadata untouched.
/// Do not call this on a handler error. Non-object, non-null or malformed JSON is unmodified.
pub fn render_toon(output: String) -> Result<String, String> {
    let trimmed = output.trim_matches([' ', '\t', '\r', '\n']);
    if trimmed == "null" {
        return Ok(String::new());
    }
    if !trimmed.starts_with('{') {
        return Ok(output);
    }
    if trimmed.len() > MAX_TOON_BYTES {
        return Err("TOON input size limit reached".into());
    }
    let obj: Map<String, Value> = match serde_json::from_str(trimmed) {
        Ok(obj) => obj,
        Err(e) => {
            let message = e.to_string();
            if message.starts_with("recursion limit exceeded")
                || message.contains("surrogate")
                || message.starts_with("unexpected end of hex escape")
            {
                return Err(format!("unsupported TOON input: {e}"));
            }
            return Ok(output);
        }
    };
    let mut out = ToonWriter::default();
    out.object(&obj, 0, false)?;
    if out.0.len() > MAX_TOON_BYTES {
        return Err("TOON output size limit reached".into());
    }
    Ok(out.0)
}

const MAX_TOON_BYTES: usize = 2 * 1024 * 1024;

// Default gotoon v0.1.1 dialect: comma-delimited, two-space indentation, no trailing newline.
#[derive(Default)]
struct ToonWriter(String);

impl ToonWriter {
    fn line(&mut self, depth: usize, list_item: bool) -> Result<(), String> {
        if self.0.len() > MAX_TOON_BYTES {
            return Err("TOON output size limit reached".into());
        }
        if !self.0.is_empty() {
            self.0.push('\n');
        }
        self.0.extend(std::iter::repeat_n(' ', depth * 2));
        if list_item {
            self.0.push_str("- ");
        }
        Ok(())
    }

    fn object(
        &mut self,
        obj: &Map<String, Value>,
        depth: usize,
        list_item: bool,
    ) -> Result<(), String> {
        if obj.is_empty() && list_item {
            self.line(depth, false)?;
            self.0.push('-');
        }
        // serde_json::Map is ordered by key (no preserve_order feature).
        for (i, (k, v)) in obj.iter().enumerate() {
            let d = depth + usize::from(list_item && i > 0);
            self.line(d, list_item && i == 0)?;
            if !v.is_array() || !k.is_empty() {
                write_key(&mut self.0, k);
            }
            match v {
                Value::Array(items) => self.array(items, d, list_item && i == 0 && k.is_empty())?,
                Value::Object(fields) => {
                    self.0.push(':');
                    self.object(fields, d + 1 + usize::from(list_item && i == 0), false)?;
                }
                _ => {
                    self.0.push_str(": ");
                    self.primitive(v)?;
                }
            }
        }
        Ok(())
    }

    fn array(
        &mut self,
        items: &[Value],
        depth: usize,
        empty_first_key: bool,
    ) -> Result<(), String> {
        let inline = items.iter().all(is_primitive);
        let header = items.first().and_then(Value::as_object).filter(|h| {
            !h.is_empty()
                && items.iter().all(|v| {
                    v.as_object().is_some_and(|o| {
                        o.len() == h.len() && h.keys().all(|k| o.get(k).is_some_and(is_primitive))
                    })
                })
        });
        if empty_first_key && !inline && header.is_none() {
            self.0.push_str("\"\"");
        }
        write!(self.0, "[{}]", items.len()).unwrap();
        if inline {
            self.0.push(':');
            if !items.is_empty() {
                self.0.push(' ');
                self.row(items.iter())?;
            }
        } else if let Some(header) = header {
            self.0.push('{');
            for (i, key) in header.keys().enumerate() {
                if i > 0 {
                    self.0.push(',');
                }
                write_key(&mut self.0, key);
            }
            self.0.push_str("}:");
            for v in items {
                self.line(depth + 1, false)?;
                self.row(header.keys().map(|k| &v[k]))?;
            }
        } else {
            self.0.push(':');
            for v in items {
                if let Some(obj) = v.as_object() {
                    self.object(obj, depth + 1, true)?;
                } else {
                    self.line(depth + 1, true)?;
                    if let Some(a) = v.as_array() {
                        // gotoon drops this item entirely; don't return a lossy result.
                        if !a.iter().all(is_primitive) {
                            return Err("unsupported TOON nested list item".into());
                        }
                        self.array(a, depth + 1, false)?;
                    } else {
                        self.primitive(v)?;
                    }
                }
            }
        }
        Ok(())
    }

    fn row<'a>(&mut self, values: impl Iterator<Item = &'a Value>) -> Result<(), String> {
        for (i, v) in values.enumerate() {
            if i > 0 {
                self.0.push(',');
            }
            self.primitive(v)?;
        }
        Ok(())
    }

    fn primitive(&mut self, v: &Value) -> Result<(), String> {
        match v {
            Value::String(s) => {
                let bare = !s.is_empty()
                    && s.trim() == s
                    && !matches!(s.as_str(), "true" | "false" | "null")
                    && !s.starts_with('-')
                    && !s.contains([':', '"', '\\', '[', ']', '{', '}', '\n', '\r', '\t', ','])
                    && !numeric_like(s);
                if bare {
                    self.0.push_str(s);
                } else {
                    quoted(&mut self.0, s);
                }
            }
            Value::Number(n) => {
                let f = n.as_f64().ok_or("unsupported TOON number")?;
                write_number(&mut self.0, f);
            }
            Value::Bool(b) => self.0.push_str(if *b { "true" } else { "false" }),
            Value::Null => self.0.push_str("null"),
            _ => unreachable!("only JSON primitives in TOON row"),
        }
        if self.0.len() > MAX_TOON_BYTES {
            return Err("TOON output size limit reached".into());
        }
        Ok(())
    }
}

fn write_number(out: &mut String, f: f64) {
    let i = f as i64;
    if i as f64 == f {
        write!(out, "{i}").unwrap();
    } else {
        // JSON and Go both break shortest-decimal ties to even; Rust Display doesn't.
        let decimal = serde_json::to_string(&f).unwrap();
        if let Some((mantissa, exp)) = decimal.split_once('e') {
            let exp: i32 = exp.parse().unwrap();
            let mantissa = mantissa.strip_prefix('-').map_or(mantissa, |s| {
                out.push('-');
                s
            });
            let point = mantissa.find('.').unwrap_or(mantissa.len()) as i32 + exp;
            let digits = mantissa.replace('.', "");
            if point <= 0 {
                out.push_str("0.");
                out.extend(std::iter::repeat_n('0', (-point) as usize));
                out.push_str(&digits);
            } else {
                let point = point as usize;
                let (int, frac) = digits.split_at(point.min(digits.len()));
                out.push_str(int);
                out.extend(std::iter::repeat_n('0', point - int.len()));
                if !frac.is_empty() {
                    out.push('.');
                    out.push_str(frac);
                }
            }
        } else {
            out.push_str(&decimal);
        }
    }
}

fn is_primitive(v: &Value) -> bool {
    !v.is_array() && !v.is_object()
}

fn numeric_like(s: &str) -> bool {
    let digits = |s: &str| !s.is_empty() && s.bytes().all(|b| b.is_ascii_digit());
    let (mantissa, exp) = s
        .split_once(['e', 'E'])
        .map_or((s, None), |(a, b)| (a, Some(b)));
    let (int, frac) = mantissa
        .split_once('.')
        .map_or((mantissa, None), |(a, b)| (a, Some(b)));
    digits(int)
        && frac.is_none_or(digits)
        && exp.is_none_or(|e| digits(e.strip_prefix(['+', '-']).unwrap_or(e)))
}

fn write_key(out: &mut String, key: &str) {
    let mut b = key.bytes();
    if b.next()
        .is_some_and(|b| b.is_ascii_alphabetic() || b == b'_')
        && b.all(|c| c.is_ascii_alphanumeric() || matches!(c, b'_' | b'.'))
    {
        out.push_str(key);
    } else {
        quoted(out, key);
    }
}

fn quoted(out: &mut String, s: &str) {
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            _ => out.push(c),
        }
    }
    out.push('"');
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn tool(name: &str, description: &str, source: usize) -> (Value, usize) {
        (
            json!({"type":"function","function":{"name":name,"description":description}}),
            source,
        )
    }

    #[test]
    fn source_partition_and_activation_preserve_wrappers() {
        let mut d = Discovery::default();
        let toon = ToonSpec::compile("create_.*").unwrap();
        let all = [
            tool("create_file", "Creates <new> & fresh file", 1),
            tool("read_file", "Reads file content", 1),
        ];
        let eager = d
            .wrap_source(all, false, &["create_file".into()], &toon)
            .unwrap();
        assert!(d.enabled());
        assert_eq!(eager.len(), 1);
        assert_eq!(eager[0].definition["function"]["name"], "read_file");
        assert!(!eager[0].toon);
        assert_eq!(d.activated().count(), 0);
        assert_eq!(
            d.catalog().map(|(n, _)| n).collect::<Vec<_>>(),
            ["create_file"]
        );
        d.wrap_source(
            [tool("create_file", "Other", 2)],
            true,
            &[],
            &ToonSpec::default(),
        )
        .unwrap();
        assert_eq!(d.search(" crFil ").unwrap(), "Found 1 deferred tool(s):\n[{\"name\":\"create_file\",\"description\":\"Creates \\u003cnew\\u003e \\u0026 fresh file\"}]");
        assert_eq!(
            d.add("read_file").unwrap_err(),
            "Tool 'read_file' not found."
        );
        assert_eq!(d.add("create_file").unwrap(), "Tool 'create_file' has been activated and is now available for use.\n\nDescription: Creates <new> & fresh file");
        assert_eq!(
            d.add("create_file").unwrap(),
            "Tool 'create_file' is already active"
        );
        let (name, tool) = d.activated().next().unwrap();
        assert_eq!((name, tool.target, tool.toon), ("create_file", 1, true));
        assert_eq!(d.catalog().count(), 1);
        assert_eq!(
            d.search(" c ").unwrap_err(),
            "No deferred tools found matching ' c '"
        );
    }

    #[test]
    fn rejected_source_never_publishes_partial_catalog() {
        let mut d = Discovery::default();
        assert!(d
            .wrap_source(
                [tool("one", "", 1), (Value::Null, 1)],
                true,
                &[],
                &ToonSpec::default()
            )
            .is_err());
        assert!(!d.enabled());
        assert_eq!(d.catalog().count(), 0);
    }

    #[test]
    fn empty_source_and_deterministic_ranking() {
        let mut d = Discovery::<usize>::default();
        assert!(!d.enabled());
        d.wrap_source([], false, &["missing".into()], &ToonSpec::default())
            .unwrap();
        assert!(d.enabled());
        d.wrap_source(
            [
                tool("zzz", "r e a d", 1),
                tool("bbb", "read", 1),
                tool("aaa", "read", 1),
            ],
            true,
            &[],
            &ToonSpec::default(),
        )
        .unwrap();
        for query in ["READ", "  "] {
            let result = d.search(query).unwrap();
            let hits: Vec<Value> =
                serde_json::from_str(result.split_once('\n').unwrap().1).unwrap();
            let names: Vec<_> = hits.iter().map(|v| v["name"].as_str().unwrap()).collect();
            assert_eq!(names, ["aaa", "bbb", "zzz"]);
        }
        assert_eq!(
            Discovery::<usize>::default().add("aaa").unwrap_err(),
            "Tool 'aaa' not found."
        );
    }

    // Recorded by pkg/fuzzy.TestScore (including fzf's uninitialized case-folding behavior).
    #[test]
    fn fuzzy_go_scores() {
        let t = "read_file Reads a file from disk";
        let mut budget = MAX_SEARCH_WORK;
        for (text, pattern, expected) in [
            (t, "r", Some(16)),
            (t, "rf", Some(26)),
            (t, "read", Some(76)),
            (t, "read file", Some(157)),
            (t, "xyz", None),
            ("ReadFile", "rf", Some(27)),
            ("ReadFile", "rea", None),
            ("ReadFile", "file", None),
            ("café latte", "cafe", Some(76)),
            ("café latte", "café", None),
            ("Ｆｕｌｌｗｉｄｔｈ tool", "full", Some(76)),
            ("straße", "stras", Some(96)),
            ("ⅩⓈ", "ⅹⓢ", None),
            ("straße", "strasse", None),
            ("日本語 検索", "本索", Some(27)),
            ("🚀 launch rocket", "🚀r", Some(22)),
            ("r e a d", "read", Some(55)),
        ] {
            assert_eq!(
                score(text, &pattern.chars().collect::<Vec<_>>(), &mut budget).unwrap(),
                expected,
                "{text:?}/{pattern:?}"
            );
        }
        assert_eq!(
            score(&"A".repeat(1001), &vec!['a'; 1001], &mut budget).unwrap(),
            Some(20016)
        );
    }

    #[test]
    fn toon_selectors_are_unanchored_regexp_not_globs() {
        let s = ToonSpec::compile(" ^(ab|c)[0-9]+$, (?i:list)_.*").unwrap();
        assert!(s.matches("ab32"));
        assert!(!s.matches("xab32"));
        assert!(s.matches("prefix_LIST_tools"));
        assert!(!ToonSpec::compile("").unwrap().matches("test"));
        for spec in [",", "  ", "test,", ".*"] {
            assert!(ToonSpec::compile(spec).unwrap().matches("anything"));
        }
        for spec in [
            "[", "(a)\\1", "(?x)test", "[a&&b]", "\\p{L}", "a**", "a{1200}",
        ] {
            assert!(ToonSpec::compile(spec).is_err(), "{spec}");
        }
    }

    // Exact outputs from toon.Wrap, including maps decoded via float64 and the nil-map `null` case.
    #[test]
    fn toon_go_outputs() {
        for (input, expected) in [
            (r#"{"key":"value", "number":42}"#, "key: value\nnumber: 42"),
            ("null", ""),
            (" {} ", ""),
            (r#"{"nil":null,"empty":{}}"#, "empty:\nnil: null"),
            (r#"{"x":1574272208324070.2}"#, "x: 1574272208324070.2"),
            (r#"{"mixed":[1,{"data":{"nested":2},"z":false},{"a":[{"q":1}]}]}"#,
                "mixed[3]:\n  - 1\n  - data:\n      nested: 2\n    z: false\n  - a[1]{q}:\n    1"),
            (r#"{"n":-0.00,"large":9007199254740993,"twop63":9223372036854775808,"decimal":0.10000000000000101,"tiny":1e-9}"#,
                "decimal: 0.100000000000001\nlarge: 9007199254740992\nn: 0\ntiny: 0.000000001\ntwop63: 9223372036854775807"),
            (r#"{"tags":["3e+8","01.2","1.","a,b","val ue"," a","t\n",null],"no-key":{}}"#,
                "\"no-key\":\ntags[8]: \"3e+8\",\"01.2\",1.,\"a,b\",val ue,\" a\",\"t\\n\",null"),
            (r#"{"todos":[{"status":"pending","id":"todo_1","description":"ship it"}], "reminder":"Remember"}"#,
                "reminder: Remember\ntodos[1]{description,id,status}:\n  ship it,todo_1,pending"),
            (r#"{"arr":[[1,2],[],[3]]}"#, "arr[3]:\n  - [2]: 1,2\n  - [0]:\n  - [1]: 3"),
            (r#"{"mixed":[{"":[1,{}]}]}"#, "mixed[1]:\n  - \"\"[2]:\n    - 1\n    -"),
        ] {
            assert_eq!(render_toon(input.into()).unwrap(), expected, "{input}");
        }
        for input in [
            "",
            "plain text output",
            " [1,2] ",
            " true",
            "12",
            "\"null\"",
            "{oops",
            "{\"n\":1e400}",
        ] {
            assert_eq!(render_toon(input.into()).unwrap(), input);
        }
        assert!(render_toon(r#"{"a":[[[1]],null]}"#.into()).is_err());
        assert!(render_toon(r#"{"s":"\uD800"}"#.into()).is_err());
        assert!(render_toon(format!("{{\"a\":{}0{}}}", "[".repeat(128), "]".repeat(128))).is_err());
    }
}
