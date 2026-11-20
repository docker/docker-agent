//! Text-only projection of pkg/session's history limits. Stored history is not a request view.
use crate::hooks::Hooks;
use serde::Deserialize;
use serde_json::Value;
use std::collections::BTreeSet;

pub const MAX_TOOL_ROUNDS: usize = 8;

#[derive(Clone, Copy, Default, Deserialize)]
#[serde(default)]
pub struct Limits {
    pub max_iterations: u32,
    max_consecutive_tool_calls: u32,
    num_history_items: u32,
    max_tool_result_tokens: i32,
    max_old_tool_call_tokens: i32,
}

impl Limits {
    pub fn check_round_limit(&self) -> Result<(), String> {
        if self.max_iterations as usize > MAX_TOOL_ROUNDS {
            return Err(format!(
                "max_iterations exceeds portable's {MAX_TOOL_ROUNDS} tool round limit"
            ));
        }
        Ok(())
    }

    pub fn validate(&self) -> Result<(), String> {
        if self.max_tool_result_tokens < -1 || self.max_old_tool_call_tokens < -1 {
            return Err("tool token limits must be -1, zero or positive".into());
        }
        Ok(())
    }

    pub fn needs_bounded_fallback(&self, hooks: &Hooks) -> bool {
        self.limits_history() || self.max_iterations > 0 || hooks.max_calls.is_some()
    }

    pub fn repetition_limit(self) -> u32 {
        if self.max_consecutive_tool_calls == 0 {
            5
        } else {
            self.max_consecutive_tool_calls
        }
    }

    pub fn limits_history(&self) -> bool {
        self.num_history_items > 0
            || self.max_old_tool_call_tokens > 0
            || self.max_tool_result_tokens > 0
    }

    pub fn check_seed(&self, history: &[Value], tool_model: bool) -> Result<(), String> {
        if !self.limits_history() && !tool_model {
            return Ok(());
        }
        for m in history {
            if m["multi_content"].as_array().is_some_and(|a| !a.is_empty()) {
                // Protocol projection can split a tool attachment into a separate user message.
                return Err(
                    "history limits and tool model overrides require text-only history".into(),
                );
            }
            if self.num_history_items > 0
                && m["role"] != "tool"
                && blank_text(m)
                && m["tool_calls"].as_array().is_none_or(Vec::is_empty)
            {
                return Err(
                    "num_history_items cannot count blank projected history messages".into(),
                );
            }
        }
        Ok(())
    }

    pub fn stored_content(&self, output: String, hooks: &Hooks) -> Result<Value, String> {
        let mut content = if self.limits_history() && output.trim().is_empty() {
            "(no output)".into()
        } else {
            output.into()
        };
        self.cap(&mut content, hooks)?;
        Ok(content)
    }

    fn cap(&self, content: &mut Value, hooks: &Hooks) -> Result<(), String> {
        if self.max_tool_result_tokens > 0 {
            let text = content
                .as_str()
                .ok_or("tool token limits require text results")?;
            let tokens = self.max_tool_result_tokens as usize;
            if text.len() / 4 > tokens {
                hooks.check_before_truncation(text)?;
                *content = middle_out(text, tokens * 4).into();
            }
        }
        Ok(())
    }

    pub fn project(&self, messages: &mut Vec<Value>, hooks: &Hooks) -> Result<(), String> {
        if self.num_history_items > 0 {
            // Go does not count reasoning-only / usage-only assistant records as history items.
            messages.retain(|m| {
                m["role"] != "assistant" || !blank_text(m) || !m["tool_calls"].is_null()
            });
            trim(messages, self.num_history_items as usize)?;
        }
        if self.max_old_tool_call_tokens > 0 {
            let mut budget = self.max_old_tool_call_tokens as usize;
            for msg in messages.iter_mut().rev().filter(|m| m["role"] == "tool") {
                let cost = msg["content"]
                    .as_str()
                    .ok_or("tool token limits require text results")?
                    .len()
                    / 4;
                if cost > budget {
                    msg["content"] = "[content truncated]".into();
                }
                budget = budget.saturating_sub(cost);
            }
        }
        if self.max_tool_result_tokens > 0 {
            for msg in messages.iter_mut().filter(|m| m["role"] == "tool") {
                self.cap(&mut msg["content"], hooks)?;
            }
        }
        if self.limits_history() {
            messages
                .retain(|m| m["role"] == "tool" || !blank_text(m) || !m["tool_calls"].is_null());
        }
        hooks.rewrite_messages(messages)
    }
}

fn blank_text(msg: &Value) -> bool {
    msg["content"]
        .as_str()
        .is_none_or(|text| text.trim().is_empty())
}

fn trim(messages: &mut Vec<Value>, limit: usize) -> Result<(), String> {
    let count = messages.iter().filter(|m| m["role"] != "system").count();
    if count <= limit {
        return Ok(());
    }
    let mut remove = count - limit;
    let mut ids = BTreeSet::new();
    let mut kept = Vec::new();
    for msg in std::mem::take(messages) {
        if msg["role"] == "system" {
            messages.push(msg);
        } else if remove > 0 && msg["role"] != "user" {
            remove -= 1;
            for call in msg["tool_calls"].as_array().into_iter().flatten() {
                ids.insert(call["id"].as_str().ok_or("missing tool ID")?.to_owned());
            }
        } else if msg["role"] == "tool"
            && msg["tool_call_id"]
                .as_str()
                .is_some_and(|id| ids.contains(id))
        {
            continue;
        } else {
            // Go would drop the newer result too and synthesize a missing-result error.
            if msg["tool_calls"]
                .as_array()
                .into_iter()
                .flatten()
                .any(|c| c["id"].as_str().is_some_and(|id| ids.contains(id)))
            {
                return Err("num_history_items cannot trim across a reused tool call ID".into());
            }
            kept.push(msg);
        }
    }
    messages.append(&mut kept);
    Ok(())
}

fn middle_out(text: &str, budget: usize) -> String {
    const MARKER: &str = "\n[...tool result truncated: middle omitted...]\n";
    for marker in [MARKER, "..."] {
        let Some(keep) = budget.checked_sub(marker.len()) else {
            continue;
        };
        let mut head = keep.div_ceil(2);
        while !text.is_char_boundary(head) {
            head -= 1;
        }
        let mut tail = text.len() - keep / 2;
        while !text.is_char_boundary(tail) {
            tail += 1;
        }
        if marker != MARKER || (head > 0 && tail < text.len()) {
            return format!("{}{marker}{}", &text[..head], &text[tail..]);
        }
    }
    String::new()
}
