//! Foreground LLM compaction (pkg/compaction and pkg/runtime/compactor).
use crate::config::RequestModel;
use serde::Deserialize;
use serde_json::{json, Value};
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
    pub fn new(window: Option<u32>, enabled: Option<bool>, threshold: Option<f64>) -> Self {
        Self {
            window: window.unwrap_or_default(),
            threshold: (enabled != Some(false)).then(|| threshold.unwrap_or(0.9)),
        }
    }

    pub fn auto_recover(self) -> bool {
        self.threshold.is_some()
    }

    pub fn effective_window(self, summary: Self) -> u32 {
        self.window.min(summary.window)
    }

    pub fn proactive(self) -> bool {
        self.window > 0 && self.auto_recover()
    }

    fn summary_cap(self) -> u32 {
        (self.window / 4).min(16_000)
    }

    pub fn clamp(&self, cap: i64) -> i64 {
        if self.window == 0 {
            cap
        } else {
            cap.min((i64::from(self.window) - 1024).max(1))
        }
    }
}

#[derive(Clone, Copy, Default, Deserialize)]
#[serde(default)]
pub struct Usage {
    #[serde(deserialize_with = "token_count")]
    pub prompt_tokens: u32,
    #[serde(deserialize_with = "token_count")]
    pub completion_tokens: u32,
    prompt_tokens_details: Option<PromptDetails>,
    completion_tokens_details: Option<CompletionDetails>,
}

#[derive(Clone, Copy, Default, Deserialize)]
#[serde(default)]
struct PromptDetails {
    #[serde(deserialize_with = "token_count")]
    cached_tokens: u32,
    #[serde(deserialize_with = "token_count")]
    cache_write_tokens: u32,
}

#[derive(Clone, Copy, Default, Deserialize)]
struct CompletionDetails {
    #[serde(default, deserialize_with = "token_count")]
    reasoning_tokens: u32,
}

fn token_count<'de, D: serde::Deserializer<'de>>(d: D) -> Result<u32, D::Error> {
    Ok(Option::<u32>::deserialize(d)?.unwrap_or_default())
}

impl Usage {
    // Go reports as input only what was neither read from nor written to a prompt cache.
    pub fn fresh_input(self) -> Result<u32, String> {
        let details = self.prompt_tokens_details.unwrap_or_default();
        self.prompt_tokens
            .checked_sub(details.cached_tokens)
            .and_then(|n| n.checked_sub(details.cache_write_tokens))
            .ok_or_else(|| "cached token counts exceed prompt_tokens".into())
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
    // Go resolves a blank final reply against the full session, not the model-facing suffix.
    pub summarized_reply: Option<String>,
}

#[derive(Clone, Copy, PartialEq)]
pub enum Trigger {
    BeforeTurn,
    AfterTools,
    Overflow,
}

pub struct Prefix {
    boundary: usize,
    tokens: u64,
    retries: u8,
    start: usize,
    pinned: bool,
    pub replies: Vec<Value>,
    pub output: u32,
}

impl Prefix {
    pub fn request_kind(&self) -> &'static str {
        if !self.replies.is_empty() {
            "continuation"
        } else if self.retries > 0 {
            "smaller"
        } else {
            "new"
        }
    }
}

// Keep the pre-compaction arrays only until the enclosing send commits.
// A cancellation can then use the ordinary turn-start truncation boundary.
pub struct Undo(Vec<Value>, Marks, Option<usize>, Option<String>);

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
        let mut binary = 0;
        for part in message["content"].as_array().into_iter().flatten() {
            n += bytes(&part["text"]);
            if matches!(part["type"].as_str(), Some("image_url" | "input_image")) {
                binary += 1500;
            }
        }
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
            heuristic: heuristic(n) + binary,
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
        if self.policy.proactive() {
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
        summary_policy: Policy,
        trigger: Trigger,
        previous: Option<&Prefix>,
    ) -> Result<Option<Prefix>, String> {
        if previous.is_some_and(|p| p.retries >= MAX_OVERFLOW_RETRIES || p.tokens < 2) {
            return Ok(None);
        }
        if previous.is_some() && self.seed_estimates_lost {
            return Err(
                "compaction: cannot size projected seed history for an overflow retry".into(),
            );
        }
        let Some(threshold) = self.policy.threshold.filter(|_| self.policy.window > 0) else {
            return Ok(None);
        };
        let window = u64::from(summary_policy.window);
        let limit = (f64::from(self.policy.effective_window(summary_policy)) * threshold) as u64;
        if previous.is_none()
            && self.context_tokens <= limit
            && (trigger == Trigger::BeforeTurn
                || (trigger == Trigger::AfterTools
                    && messages.last().is_none_or(|m| m["role"] != "tool")))
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
            if trigger != Trigger::Overflow && self.context_tokens <= limit {
                let added: u64 = entries
                    .iter()
                    .rev()
                    .take_while(|e| e.message["role"] == "tool")
                    .map(|e| e.tokens(calibrated))
                    .sum();
                if self.context_tokens + added <= limit {
                    return Ok(None);
                }
            }
            let (end, _) = suffix(&entries, (window / 5).min(20_000), calibrated, true);
            let boundary = entries.get(end).map_or(messages.len(), |e| e.index);
            entries.truncate(end);
            boundary
        };
        let cap = u64::from(summary_policy.summary_cap());
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
        Ok(Some(Prefix {
            boundary,
            start: kept.first().map_or(boundary, |e| e.index),
            pinned: pinned > 0,
            tokens: pinned + tokens,
            retries: previous.map_or(0, |p| p.retries + 1),
            replies: Vec::new(),
            output: 0,
        }))
    }

    pub fn request(
        &self,
        messages: &[Value],
        plan: &Prefix,
        model: &RequestModel,
        summary_policy: Policy,
    ) -> Value {
        let mut input = Vec::with_capacity(plan.boundary - plan.start + plan.replies.len() + 3);
        input.push(json!({"role":"system", "content":SYSTEM}));
        if plan.pinned {
            input.push(messages[self.prior_summary.expect("pinned summary")].clone());
        }
        input.extend(
            messages[plan.start..plan.boundary]
                .iter()
                .filter(|m| !is_instruction(m))
                .cloned(),
        );
        input.push(json!({"role":"user", "content":USER}));
        input.extend_from_slice(&plan.replies);
        let cap = i64::from(summary_policy.summary_cap());
        let mut body = model.model_params.clone();
        body.remove("response_format");
        body.remove("tool_choice");
        body.remove("parallel_tool_calls");
        let key = crate::config::cap_key(model.name());
        body.insert(key.into(), summary_policy.clamp(cap).into());
        body.insert("messages".into(), input.into());
        body.insert("stream".into(), true.into());
        body.insert("stream_options".into(), json!({"include_usage":true}));
        model.apply_reasoning(&mut body, false, Some(cap));
        Value::Object(body)
    }

    pub fn apply(
        &mut self,
        messages: &mut Vec<Value>,
        Prefix {
            boundary, output, ..
        }: Prefix,
        summary: &str,
    ) -> Undo {
        let original = std::mem::take(messages);
        let marks = std::mem::take(&mut self.marks);
        let prior = self.prior_summary.replace(0);
        let reply = self.summarized_reply.take();
        self.summarized_reply = original[..boundary]
            .iter()
            .rev()
            .find(|m| m["role"] == "assistant")
            .map(|m| m["content"].as_str().unwrap_or_default().trim().into())
            .or_else(|| reply.clone());
        messages.push(json!({"role":"user", "content":format!("Session Summary: {summary}")}));
        messages.extend_from_slice(&original[boundary..]);
        self.marks.extend(
            marks
                .range(boundary..)
                .map(|(i, mark)| (1 + (i - boundary), mark.clone())),
        );
        self.context_tokens = output.into();
        Undo(original, marks, prior, reply)
    }

    pub fn undo(&mut self, messages: &mut Vec<Value>, undo: Undo) {
        (*messages, self.marks, self.prior_summary) = (undo.0, undo.1, undo.2);
        self.summarized_reply = undo.3;
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

    fn params() -> RequestModel {
        RequestModel {
            model_params: serde_json::Map::from_iter([("model".into(), "test".into())]),
            replay_reasoning: true,
            ..Default::default()
        }
    }

    #[test]
    fn strict_threshold_and_tail_boundary_never_orphan_a_parallel_call() {
        let mut c = Compactor {
            policy: Policy::new(Some(2000), None, Some(0.5)),
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
            .plan(&messages, c.policy, Trigger::AfterTools, None)
            .unwrap()
            .is_none());
        c.record(2, "a", usage(999, 2));
        let mut prefix = c
            .plan(&messages, c.policy, Trigger::AfterTools, None)
            .unwrap()
            .unwrap();
        let body = c.request(&messages, &prefix, &params(), c.policy);
        assert_eq!(body["messages"].as_array().unwrap()[1..5], messages[1..5]);
        assert_eq!(body["messages"].as_array().unwrap().len(), 6);
        let saved = messages.clone();
        prefix.output = 7;
        let undo = c.apply(&mut messages, prefix, "key facts");
        assert_eq!(
            messages,
            vec![
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
            policy: Policy::new(Some(4000), None, Some(0.5)),
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
            c.plan(&messages, c.policy, Trigger::BeforeTurn, prev)
                .unwrap()
                .unwrap()
        };
        let first = plan(None);
        assert_eq!(first.boundary, 5);
        // 2x calibration: 15 + 1038 + 35 + 610 + 1105. Half fits only the last assistant.
        assert_eq!(first.tokens, 2803);
        let second = plan(Some(&first));
        let body = c.request(&messages, &second, &params(), c.policy);
        assert_eq!(second.boundary, first.boundary);
        assert_eq!(second.tokens, 1105);
        assert_eq!(body["messages"].as_array().unwrap()[1..2], messages[4..5]);
        assert!(c
            .plan(&messages, c.policy, Trigger::AfterTools, Some(&second))
            .unwrap()
            .is_none());
    }

    #[test]
    fn retry_can_send_only_the_prior_summary_but_never_pins_user_supplied_text() {
        let mut c = Compactor {
            policy: Policy::new(Some(2000), None, Some(0.5)),
            context_tokens: 1100,
            ..Default::default()
        };
        let messages = vec![
            json!({"role":"user","content":"Session Summary: prior"}),
            json!({"role":"user","content":"x".repeat(350)}),
        ];
        let first = c
            .plan(&messages, c.policy, Trigger::AfterTools, None)
            .unwrap()
            .unwrap();
        assert!(c
            .plan(&messages, c.policy, Trigger::AfterTools, Some(&first))
            .unwrap()
            .is_none());
        c.prior_summary = Some(0);
        let second = c
            .plan(&messages, c.policy, Trigger::AfterTools, Some(&first))
            .unwrap()
            .unwrap();
        let body = c.request(&messages, &second, &params(), c.policy);
        assert_eq!(body["messages"].as_array().unwrap().len(), 3);
        assert_eq!(body["messages"][1], messages[0]);
        let too_tight = Prefix {
            tokens: 2 * heuristic("Session Summary: prior".len()),
            ..first
        };
        assert!(c
            .plan(&messages, c.policy, Trigger::AfterTools, Some(&too_tight))
            .unwrap()
            .is_none());
    }

    #[test]
    fn added_tool_tokens_use_calibrated_provider_usage() {
        let mut c = Compactor {
            policy: Policy::new(Some(100_000), None, None),
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
            .plan(&messages, c.policy, Trigger::AfterTools, None)
            .unwrap()
            .is_none());
        c.record(0, "a", usage(3000, 100));
        c.record(2, "a", usage(43_110, 100));
        // The 2x calibration crosses 90%; the whole prefix would no longer fit.
        assert!(c
            .plan(&messages, c.policy, Trigger::AfterTools, None)
            .is_err());
        // Different tokenizers cannot calibrate each other's history.
        c.record(2, "b", usage(43_110, 100));
        assert!(c
            .plan(&messages, c.policy, Trigger::AfterTools, None)
            .unwrap()
            .is_none());
    }
}
