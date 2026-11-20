//! Foreground LLM compaction (pkg/compaction and pkg/runtime/compactor).
use serde::Deserialize;
use serde_json::{json, Map, Value};
use std::collections::BTreeMap;

const MAX_OVERFLOW_RETRIES: u8 = 3;
const SYSTEM: &str = include_str!("../../../../pkg/compaction/prompts/compaction-system.txt");
const USER: &str = include_str!("../../../../pkg/compaction/prompts/compaction-user.txt");

#[derive(Clone, Copy, Default, PartialEq)]
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

    pub fn effective_window(self, summary: Self) -> u32 {
        self.window.min(summary.window)
    }

    pub fn enabled(self) -> bool {
        self.threshold.is_some()
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
    prompt_tokens_details: Option<PromptDetails>,
    completion_tokens_details: Option<CompletionDetails>,
}

#[derive(Clone, Copy, Default, Deserialize)]
#[serde(default)]
struct PromptDetails {
    cached_tokens: u32,
    cache_write_tokens: u32,
}

#[derive(Clone, Copy, Default, Deserialize)]
struct CompletionDetails {
    #[serde(default)]
    reasoning_tokens: u32,
}

impl Usage {
    // Go reports as input only what was neither read from nor written to a prompt cache.
    pub fn fresh_input(self) -> u32 {
        let details = self.prompt_tokens_details.unwrap_or_default();
        self.prompt_tokens
            .saturating_sub(details.cached_tokens)
            .saturating_sub(details.cache_write_tokens)
    }

    fn visible_output(self) -> u32 {
        self.completion_tokens.saturating_sub(
            self.completion_tokens_details
                .unwrap_or_default()
                .reasoning_tokens,
        )
    }
}

#[derive(Clone)]
struct Mark {
    model: String,
    usage: Usage,
}

type Marks = BTreeMap<usize, Mark>;

#[derive(Default)]
pub struct Compactor {
    pub policy: Policy,
    pub context_tokens: u64,
    marks: Marks,
    pub seed_estimates_lost: bool,
    prior_summary: Option<usize>,
}

pub struct Prefix {
    boundary: usize,
    tokens: u64,
    pub retries: u8,
}

// Keep the pre-compaction arrays only until the enclosing send commits.
// A cancellation can then use the ordinary turn-start truncation boundary.
pub struct Undo(Vec<Value>, Marks, Option<usize>);

fn is_instruction(m: &Value) -> bool {
    matches!(m["role"].as_str(), Some("system" | "developer"))
}

struct Entry<'a> {
    index: usize,
    message: &'a Value,
    heuristic: u64,
    reported: u64,
    mark: Option<&'a Mark>,
}

fn bytes(v: &Value) -> usize {
    v.as_str().map_or(0, str::len)
}

fn heuristic(n: usize) -> u64 {
    (n as u64 * 2) / 7 + 5
}

impl<'a> Entry<'a> {
    fn new(index: usize, message: &'a Value, mark: Option<&'a Mark>) -> Self {
        let mut n = bytes(&message["content"]) + bytes(&message["reasoning_content"]);
        for call in message["tool_calls"].as_array().into_iter().flatten() {
            n += bytes(&call["function"]["name"]) + bytes(&call["function"]["arguments"]);
        }
        let reported = mark.map_or(0, |m| {
            let u = m.usage;
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
            mark,
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
        if let Some(m) = entry.mark.filter(|m| m.usage.prompt_tokens > 0) {
            let u = m.usage;
            let prompt = u64::from(u.prompt_tokens);
            if let Some((total, model)) = previous {
                if model == m.model && prompt > total + known && guessed > 0 {
                    actual += prompt - total - known;
                    estimated += guessed;
                }
            }
            previous = Some((prompt + u64::from(u.visible_output()), m.model.as_str()));
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

fn suffix(entries: &[Entry], budget: u64, scale: f64, compact_all_if_fit: bool) -> (usize, u64) {
    let mut boundary_tokens = 0;
    let mut boundary = entries.len();
    let mut total = 0;
    for (i, m) in entries.iter().enumerate().rev() {
        total += m.tokens(scale);
        if total > budget {
            return (boundary, boundary_tokens);
        }
        if m.message["role"] != "tool" {
            boundary = i;
            boundary_tokens = total;
        }
    }
    if compact_all_if_fit {
        (entries.len(), 0)
    } else {
        (boundary, boundary_tokens)
    }
}

impl Compactor {
    pub fn record(&mut self, index: usize, model: &str, usage: Usage) {
        if self.policy.threshold.is_some() {
            self.context_tokens =
                u64::from(usage.prompt_tokens) + u64::from(usage.completion_tokens);
            self.marks.insert(
                index,
                Mark {
                    model: model.to_owned(),
                    usage,
                },
            );
        }
    }

    pub fn truncate(&mut self, n: usize) {
        self.prior_summary = self.prior_summary.filter(|&index| index < n);
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
        summary_policy: Policy,
        merge: bool,
        previous: Option<&Prefix>,
    ) -> Result<Option<(Prefix, Value)>, String> {
        if previous.is_some_and(|p| p.retries >= MAX_OVERFLOW_RETRIES || p.tokens < 2) {
            return Ok(None);
        }
        if previous.is_some() && self.seed_estimates_lost {
            return Err(
                "compaction: cannot size projected seed history for an overflow retry".into(),
            );
        }
        let Some(threshold) = self.policy.threshold else {
            return Ok(None);
        };
        let window = u64::from(summary_policy.window);
        let trigger = (f64::from(self.policy.effective_window(summary_policy)) * threshold) as u64;
        if previous.is_none()
            && self.context_tokens <= trigger
            && messages.last().is_none_or(|m| m["role"] != "tool")
        {
            return Ok(None);
        }
        // The kept tail does not change when the child retries a smaller prefix.
        let candidates = previous.map_or(messages, |p| &messages[..p.boundary]);
        let mut entries: Vec<_> = candidates
            .iter()
            .enumerate()
            .filter(|(_, m)| !is_instruction(m))
            .map(|(i, m)| Entry::new(i, m, self.marks.get(&i)))
            .collect();
        let boundary = if let Some(p) = previous {
            p.boundary
        } else {
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
            let (end, _) = suffix(&entries, (window / 5).min(20_000), calibrated, true);
            let boundary = entries.get(end).map_or(messages.len(), |e| e.index);
            entries.truncate(end);
            boundary
        };
        let cap = (window / 4).min(16_000);
        let available =
            window.saturating_sub(cap + heuristic(SYSTEM.len()) + heuristic(USER.len()));
        let budget = previous.map_or(available, |p| available.min(p.tokens / 2));
        let mut rest = entries.as_slice();
        let mut pinned = 0;
        let mut calibrated = scale(rest);
        if previous.is_some() && rest.len() > 1 && Some(rest[0].index) == self.prior_summary {
            let unpinned_scale = scale(&rest[1..]);
            let tokens = rest[0].tokens(unpinned_scale);
            if tokens < budget {
                pinned = tokens;
                calibrated = unpinned_scale;
                rest = &rest[1..];
            }
        }
        let (start, tokens) = suffix(rest, budget - pinned, calibrated, false);
        if previous.is_none()
            && start > 0
            && (start < rest.len()
                || rest.iter().any(|e| e.message["role"] == "tool")
                || self.seed_estimates_lost)
        {
            // Initial partial prefixes and oversized tool output remain outside the supported shape.
            return Err(
                "compaction: complete history prefix does not fit the summarizer context window"
                    .into(),
            );
        }
        let kept = &rest[start..];
        if pinned == 0 && kept.is_empty() {
            return Ok(None);
        }
        let mut input = Vec::with_capacity(kept.len() + 3);
        input.push(json!({"role":"system", "content":SYSTEM}));
        if pinned > 0 {
            input.push(entries[0].message.clone());
        }
        input.extend(kept.iter().map(|e| e.message.clone()));
        input.push(json!({"role":"user", "content":USER}));
        let mut body = params.clone();
        body.remove("response_format");
        body.remove("tool_choice");
        body.remove("parallel_tool_calls");
        let key = crate::config::cap_key(params["model"].as_str().expect("configured model"));
        body.insert(key.into(), summary_policy.clamp(cap as i64).into());
        body.insert("messages".into(), input.into());
        body.insert("stream".into(), true.into());
        body.insert("stream_options".into(), json!({"include_usage":true}));
        let mut request = Value::Object(body);
        if merge {
            crate::wire::merge_messages(&mut request);
        }
        Ok(Some((
            Prefix {
                boundary,
                tokens: pinned + tokens,
                retries: previous.map_or(0, |p| p.retries + 1),
            },
            request,
        )))
    }

    pub fn apply(
        &mut self,
        messages: &mut Vec<Value>,
        Prefix { boundary, .. }: Prefix,
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
        let prior = self.prior_summary.replace(messages.len());
        messages.push(json!({"role":"user", "content":format!("Session Summary: {summary}")}));
        let offset = messages.len();
        messages.extend_from_slice(&original[boundary..]);
        self.marks.extend(
            marks
                .range(boundary..)
                .map(|(i, mark)| (offset + (i - boundary), mark.clone())),
        );
        self.context_tokens = output.into();
        Undo(original, marks, prior)
    }

    pub fn undo(&mut self, messages: &mut Vec<Value>, undo: Undo) {
        (*messages, self.marks, self.prior_summary) = (undo.0, undo.1, undo.2);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn usage(prompt_tokens: u32, completion_tokens: u32) -> Usage {
        Usage {
            prompt_tokens,
            completion_tokens,
            prompt_tokens_details: None,
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
        c.record(2, "a", usage(998, 2));
        assert!(c
            .plan(&messages, &params(), c.policy, false, None)
            .unwrap()
            .is_none());
        c.record(2, "a", usage(999, 2));
        let (prefix, body) = c
            .plan(&messages, &params(), c.policy, false, None)
            .unwrap()
            .unwrap();
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
    fn retry_recalibrates_the_compacted_prefix_without_moving_the_kept_tail() {
        let mut c = Compactor {
            policy: Policy::new(Some(4000), None, Some(0.5)).unwrap(),
            ..Default::default()
        };
        let messages = vec![
            json!({"role":"assistant", "content":"anchor"}),
            json!({"role":"user", "content":"x".repeat(1800)}),
            json!({"role":"assistant", "content":"second anchor"}),
            json!({"role":"user", "content":"y".repeat(1050)}),
            json!({"role":"assistant", "content":"large output"}),
            json!({"role":"user", "content":"kept tail"}),
        ];
        c.record(0, "a", usage(100, 10));
        c.record(2, "a", usage(1210, 30));
        c.record(4, "a", usage(1000, 1100));
        let plan = |prev| {
            c.plan(&messages, &params(), c.policy, false, prev)
                .unwrap()
                .unwrap()
        };
        let (first, _) = plan(None);
        assert_eq!(first.boundary, 5);
        // 2x calibration: 15 + 1038 + 35 + 610 + 1105. Half fits only the last assistant.
        assert_eq!(first.tokens, 2803);
        let (second, body) = plan(Some(&first));
        assert_eq!(second.boundary, first.boundary);
        assert_eq!(second.tokens, 1105);
        assert_eq!(body["messages"].as_array().unwrap()[1..2], messages[4..5]);
        assert!(c
            .plan(&messages, &params(), c.policy, false, Some(&second))
            .unwrap()
            .is_none());
    }

    #[test]
    fn retry_can_send_only_the_prior_summary_but_never_pins_user_supplied_text() {
        let mut c = Compactor {
            policy: Policy::new(Some(2000), None, Some(0.5)).unwrap(),
            context_tokens: 1100,
            ..Default::default()
        };
        let messages = vec![
            json!({"role":"user","content":"Session Summary: prior"}),
            json!({"role":"user","content":"x".repeat(350)}),
        ];
        let (first, _) = c
            .plan(&messages, &params(), c.policy, false, None)
            .unwrap()
            .unwrap();
        assert!(c
            .plan(&messages, &params(), c.policy, false, Some(&first))
            .unwrap()
            .is_none());
        c.prior_summary = Some(0);
        let (_, body) = c
            .plan(&messages, &params(), c.policy, false, Some(&first))
            .unwrap()
            .unwrap();
        assert_eq!(body["messages"].as_array().unwrap().len(), 3);
        assert_eq!(body["messages"][1], messages[0]);
        let too_tight = Prefix {
            tokens: 2 * heuristic("Session Summary: prior".len()),
            ..first
        };
        assert!(c
            .plan(&messages, &params(), c.policy, false, Some(&too_tight))
            .unwrap()
            .is_none());
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
        assert!(c
            .plan(&messages, &params(), c.policy, false, None)
            .unwrap()
            .is_none());
        c.record(0, "a", usage(3000, 100));
        c.record(2, "a", usage(43_110, 100));
        // The 2x calibration crosses 90%; the whole prefix would no longer fit.
        assert!(c.plan(&messages, &params(), c.policy, false, None).is_err());
        // Different tokenizers cannot calibrate each other's history.
        c.record(2, "b", usage(43_110, 100));
        assert!(c
            .plan(&messages, &params(), c.policy, false, None)
            .unwrap()
            .is_none());
    }
}
