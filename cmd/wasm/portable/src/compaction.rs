//! Foreground, same-model LLM compaction (pkg/compaction and pkg/runtime/compactor).
use serde::Deserialize;
use serde_json::{json, Map, Value};
use std::collections::BTreeMap;

const SYSTEM: &str = include_str!("../../../../pkg/compaction/prompts/compaction-system.txt");
const USER: &str = include_str!("../../../../pkg/compaction/prompts/compaction-user.txt");

#[derive(Clone, Copy, Default)]
pub struct Policy {
    pub window: u32,
    threshold: Option<f64>,
}

impl Policy {
    pub fn new(
        window: Option<u32>,
        enabled: Option<bool>,
        threshold: Option<f64>,
    ) -> Result<Self, String> {
        let window = window.unwrap_or(0);
        if window == 0 && enabled != Some(false) && (enabled.is_some() || threshold.is_some()) {
            return Err(
                "portable session_compaction requires a declared model context_window".into(),
            );
        }
        Ok(Self {
            window,
            threshold: (window > 0 && enabled != Some(false)).then(|| threshold.unwrap_or(0.9)),
        })
    }

    pub fn clamp(&self, cap: i64) -> i64 {
        if self.window == 0 {
            cap
        } else {
            cap.min((i64::from(self.window) - 1024).max(1))
        }
    }
}

#[derive(Clone, Copy, Deserialize)]
pub struct Usage {
    pub prompt_tokens: u32,
    pub completion_tokens: u32,
    completion_tokens_details: Option<CompletionDetails>,
}

#[derive(Clone, Copy, Default, Deserialize)]
struct CompletionDetails {
    #[serde(default)]
    reasoning_tokens: u32,
}

impl Usage {
    fn visible_output(self) -> u32 {
        self.completion_tokens.saturating_sub(
            self.completion_tokens_details
                .unwrap_or_default()
                .reasoning_tokens,
        )
    }
}

type Marks = BTreeMap<usize, Usage>;

#[derive(Default)]
pub struct Compactor {
    pub policy: Policy,
    pub context_tokens: u64,
    marks: Marks,
}

pub struct Prefix(usize);

// Keep the pre-compaction arrays only until the enclosing send commits.
// A cancellation can then use the ordinary turn-start truncation boundary.
pub struct Undo(Vec<Value>, Marks);

fn is_instruction(m: &Value) -> bool {
    matches!(m["role"].as_str(), Some("system" | "developer"))
}

struct Entry<'a> {
    index: usize,
    message: &'a Value,
    heuristic: u64,
    reported: u64,
    usage: Option<Usage>,
}

fn bytes(v: &Value) -> usize {
    v.as_str().map_or(0, str::len)
}

fn heuristic(n: usize) -> u64 {
    (n as u64 * 2) / 7 + 5
}

impl<'a> Entry<'a> {
    fn new(index: usize, message: &'a Value, usage: Option<Usage>) -> Self {
        let mut n = bytes(&message["content"]) + bytes(&message["reasoning_content"]);
        for call in message["tool_calls"].as_array().into_iter().flatten() {
            n += bytes(&call["function"]["name"]) + bytes(&call["function"]["arguments"]);
        }
        let reported = usage.map_or(0, |u| {
            if bytes(&message["reasoning_content"]) > 0 {
                u.completion_tokens
            } else {
                u.visible_output()
            }
        });
        Self {
            index,
            message,
            heuristic: heuristic(n),
            reported: reported.into(),
            usage,
        }
    }

    fn tokens(&self, scale: f64) -> u64 {
        if self.reported == 0 {
            (self.heuristic as f64 * scale) as u64
        } else {
            self.reported + 5
        }
    }
}

// Consecutive usage anchors measure the actual token cost of intervening messages.
fn scale(entries: &[Entry]) -> f64 {
    let (mut estimated, mut actual) = (0, 0);
    let (mut previous, mut guessed, mut known) = (None, 0, 0);
    for entry in entries {
        if let Some(u) = entry.usage.filter(|u| u.prompt_tokens > 0) {
            let prompt = u64::from(u.prompt_tokens);
            if let Some(total) = previous {
                if prompt > total + known && guessed > 0 {
                    actual += prompt - total - known;
                    estimated += guessed;
                }
            }
            previous = Some(prompt + u64::from(u.visible_output()));
            guessed = 0;
            known = 0;
        } else if entry.reported > 0 {
            known += entry.reported + 5;
        } else {
            guessed += entry.heuristic;
        }
    }
    if estimated < 512 {
        1.0
    } else {
        (actual as f64 / estimated as f64).clamp(0.75, 2.0)
    }
}

fn split(entries: &[Entry], keep: u64, scale: f64) -> usize {
    let mut boundary = entries.len();
    let mut total = 0;
    for (i, m) in entries.iter().enumerate().rev() {
        total += m.tokens(scale);
        if total > keep {
            return boundary;
        }
        if m.message["role"] != "tool" {
            boundary = i;
        }
    }
    entries.len() // Go compacts the whole conversation if all messages fit the tail.
}

impl Compactor {
    pub fn record(&mut self, index: usize, usage: Usage) {
        if self.policy.threshold.is_some() {
            self.context_tokens =
                u64::from(usage.prompt_tokens) + u64::from(usage.completion_tokens);
            self.marks.insert(index, usage);
        }
    }

    pub fn truncate(&mut self, n: usize) {
        while self
            .marks
            .last_key_value()
            .is_some_and(|(&index, _)| index >= n)
        {
            self.marks.pop_last();
        }
    }

    pub fn plan(
        &self,
        messages: &[Value],
        params: &Map<String, Value>,
        merge: bool,
    ) -> Result<Option<(Prefix, Value)>, String> {
        let Some(threshold) = self.policy.threshold else {
            return Ok(None);
        };
        let window = u64::from(self.policy.window);
        let trigger = (window as f64 * threshold) as u64;
        // BeforeTurn checks the last call's usage; AfterMessages also counts completed tool results.
        if self.context_tokens <= trigger && messages.last().is_none_or(|m| m["role"] != "tool") {
            return Ok(None);
        }
        let entries: Vec<_> = messages
            .iter()
            .enumerate()
            .filter(|(_, m)| !is_instruction(m))
            .map(|(i, m)| Entry::new(i, m, self.marks.get(&i).copied()))
            .collect();
        let calibrated = scale(&entries);
        let added: u64 = entries
            .iter()
            .rev()
            .take_while(|e| e.message["role"] == "tool")
            .map(|e| e.tokens(calibrated))
            .sum();
        if self.context_tokens + added <= trigger {
            return Ok(None);
        }
        let end = split(&entries, (window / 5).min(20_000), calibrated);
        let boundary = entries.get(end).map_or(messages.len(), |e| e.index);
        let prefix = &entries[..end];
        let cap = (window / 4).min(16_000);
        let calibrated = scale(prefix);
        let cost: u64 = prefix.iter().map(|m| m.tokens(calibrated)).sum();
        if prefix.is_empty()
            || cap + heuristic(SYSTEM.len()) + heuristic(USER.len()) + cost > window
        {
            // Go can drop older messages to fit. Never replace history the portable summarizer didn't see.
            return Err(
                "compaction: complete history prefix does not fit the summarizer context window"
                    .into(),
            );
        }
        let mut input = Vec::with_capacity(end + 2);
        input.push(json!({"role":"system", "content":SYSTEM}));
        input.extend(prefix.iter().map(|e| e.message.clone()));
        input.push(json!({"role":"user", "content":USER}));
        let mut body = params.clone();
        body.remove("tool_choice");
        body.remove("parallel_tool_calls");
        let key = crate::config::cap_key(params["model"].as_str().expect("configured model"));
        body.insert(key.into(), self.policy.clamp(cap as i64).into());
        body.insert("messages".into(), input.into());
        body.insert("stream".into(), true.into());
        body.insert("stream_options".into(), json!({"include_usage":true}));
        let mut request = Value::Object(body);
        if merge {
            crate::wire::merge_messages(&mut request);
        }
        Ok(Some((Prefix(boundary), request)))
    }

    pub fn apply(
        &mut self,
        messages: &mut Vec<Value>,
        Prefix(boundary): Prefix,
        summary: &str,
        output: u32,
    ) -> Undo {
        let original = std::mem::take(messages);
        let marks = std::mem::take(&mut self.marks);
        messages.extend(
            original[..boundary]
                .iter()
                .filter(|m| is_instruction(m))
                .cloned(),
        );
        messages.push(json!({"role":"user", "content":format!("Session Summary: {summary}")}));
        let offset = messages.len();
        messages.extend_from_slice(&original[boundary..]);
        self.marks.extend(
            marks
                .range(boundary..)
                .map(|(i, u)| (offset + (i - boundary), *u)),
        );
        self.context_tokens = output.into();
        Undo(original, marks)
    }

    pub fn undo(&mut self, messages: &mut Vec<Value>, undo: Undo) {
        (*messages, self.marks) = (undo.0, undo.1);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn usage(prompt_tokens: u32, completion_tokens: u32) -> Usage {
        Usage {
            prompt_tokens,
            completion_tokens,
            completion_tokens_details: None,
        }
    }

    fn params() -> Map<String, Value> {
        Map::from_iter([("model".into(), "test".into())])
    }

    #[test]
    fn strict_threshold_and_tail_boundary_never_orphan_a_parallel_call() {
        let mut c = Compactor {
            policy: Policy::new(Some(2000), None, Some(0.5)).unwrap(),
            ..Default::default()
        };
        let mut messages = vec![
            json!({"role":"system", "content":"rules"}),
            json!({"role":"user", "content":"old"}),
            json!({"role":"assistant", "tool_calls":[{"id":"1", "function":{"name":"a","arguments":"{}"}},{"id":"2", "function":{"name":"b","arguments":"{}"}}]}),
            json!({"role":"tool", "tool_call_id":"1", "content":"abc".repeat(512)}),
            json!({"role":"tool", "tool_call_id":"2", "content":"b"}),
            json!({"role":"developer", "content":"do not remove"}),
            json!({"role":"user", "content":"continue"}),
        ];
        c.record(2, usage(998, 2));
        assert!(c.plan(&messages, &params(), false).unwrap().is_none());
        c.record(2, usage(999, 2));
        let (prefix, body) = c.plan(&messages, &params(), false).unwrap().unwrap();
        assert_eq!(body["messages"].as_array().unwrap()[1..5], messages[1..5]);
        assert_eq!(body["messages"].as_array().unwrap().len(), 6);
        let saved = messages.clone();
        let undo = c.apply(&mut messages, prefix, "key facts", 7);
        assert_eq!(
            messages,
            vec![
                saved[0].clone(),
                saved[5].clone(),
                json!({"role":"user", "content":"Session Summary: key facts"}),
                saved[6].clone()
            ]
        );
        assert_eq!(c.context_tokens, 7);
        assert!(c.marks.is_empty());
        c.undo(&mut messages, undo);
        assert_eq!(messages, saved);
        assert!(c.marks.contains_key(&2));
    }

    #[test]
    fn added_tool_tokens_use_calibrated_provider_usage() {
        let mut c = Compactor {
            policy: Policy::new(Some(100_000), None, None).unwrap(),
            ..Default::default()
        };
        let messages = vec![
            json!({"role":"assistant"}),
            json!({"role":"tool", "content":"z".repeat(70_000)}),
            json!({"role":"assistant"}),
            json!({"role":"tool", "content":"z".repeat(120_000)}),
        ];
        c.context_tokens = 43_210;
        assert!(c.plan(&messages, &params(), false).unwrap().is_none());
        c.record(0, usage(3000, 100));
        c.record(2, usage(43_110, 100));
        // The 2x calibration crosses 90%; the whole prefix would no longer fit.
        assert!(c.plan(&messages, &params(), false).is_err());
    }
}
