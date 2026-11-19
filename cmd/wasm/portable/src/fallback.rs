use serde::{de::Error, Deserialize, Deserializer};
use serde_json::Value;
use std::cell::Cell;

#[derive(Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Config {
    pub models: Vec<String>,
    #[serde(deserialize_with = "retries")]
    retries: i32,
    #[serde(rename = "cooldown", deserialize_with = "duration")]
    cooldown_ns: u64,
}

impl Config {
    pub fn policy(&self) -> Policy {
        Policy {
            retries: match self.retries {
                -1 => 0,
                0 => 2,
                n => n as u32,
            },
            cooldown_ms: if self.cooldown_ns == 0 {
                60_000.0
            } else {
                self.cooldown_ns as f64 / 1e6
            },
            pinned: Cell::new(None),
        }
    }
}

fn retries<'de, D: Deserializer<'de>>(d: D) -> Result<i32, D::Error> {
    let n = i32::deserialize(d)?;
    if n < -1 {
        return Err(D::Error::custom("fallback.retries must be >= -1"));
    }
    Ok(n)
}

fn duration<'de, D: Deserializer<'de>>(d: D) -> Result<u64, D::Error> {
    #[derive(Deserialize)]
    #[serde(untagged)]
    enum Duration {
        Seconds(u64),
        Text(String),
    }
    let ns = match Duration::deserialize(d)? {
        Duration::Seconds(s) => s.checked_mul(1_000_000_000),
        Duration::Text(text) => parse_duration(&text),
    };
    ns.filter(|&n| n <= i64::MAX as u64).ok_or_else(|| {
        D::Error::custom("invalid fallback.cooldown: expected a non-negative Go duration")
    })
}

// time.ParseDuration accepts compound fractional units, but no days or exponents.
fn parse_duration(text: &str) -> Option<u64> {
    if matches!(text, "" | "0" | "+0" | "-0") {
        return Some(0);
    }
    let negative = text.starts_with('-');
    let mut rest = text.strip_prefix(['+', '-']).unwrap_or(text);
    if rest.is_empty() {
        return None;
    }
    let mut ns = 0u64;
    while !rest.is_empty() {
        let end = rest.find(|c: char| !c.is_ascii_digit() && c != '.')?;
        let (number, tail) = rest.split_at(end);
        let (whole, fraction) = number.split_once('.').unwrap_or((number, ""));
        if (whole.is_empty() && fraction.is_empty())
            || !fraction.bytes().all(|c| c.is_ascii_digit())
        {
            return None;
        }
        let (unit, factor) = [
            ("ns", 1),
            ("us", 1000),
            ("µs", 1000),
            ("μs", 1000),
            ("ms", 1_000_000),
            ("s", 1_000_000_000),
            ("m", 60_000_000_000),
            ("h", 3_600_000_000_000),
        ]
        .into_iter()
        .find(|(unit, _)| tail.starts_with(unit))?;
        if !whole.is_empty() {
            ns = ns.checked_add(whole.parse::<u64>().ok()?.checked_mul(factor)?)?;
        }
        // Reverse long multiplication gives floor(0.fraction * factor) without overflow.
        let fractional_ns = fraction.bytes().rev().fold(0, |carry, digit| {
            (u64::from(digit - b'0') * factor + carry) / 10
        });
        ns = ns.checked_add(fractional_ns)?;
        rest = &tail[unit.len()..];
    }
    (!negative || ns == 0).then_some(ns)
}

pub struct Policy {
    retries: u32,
    cooldown_ms: f64,
    pinned: Cell<Option<(usize, f64)>>,
}

impl Default for Policy {
    fn default() -> Self {
        Config::default().policy()
    }
}

#[derive(Default)]
pub struct Attempt {
    pub current: usize,
    attempt: u32,
    pin_on_success: bool,
}

pub enum Decision {
    Retry(u32),
    Switch,
    Stop,
}

impl Policy {
    pub fn begin(&self, now_ms: f64) -> Attempt {
        Attempt {
            current: self
                .pinned
                .get()
                .filter(|&(_, until)| now_ms < until)
                .map_or(0, |(i, _)| i),
            ..Default::default()
        }
    }
}

impl Attempt {
    pub fn fail(&mut self, policy: &Policy, transient: bool, count: usize) -> Decision {
        if transient && self.attempt < policy.retries {
            let delay = (200 << self.attempt.min(4)).min(2000);
            self.attempt += 1;
            return Decision::Retry(delay);
        }
        if self.current + 1 == count {
            return Decision::Stop;
        }
        self.pin_on_success |= self.current == 0 && !transient;
        self.current += 1;
        self.attempt = 0;
        Decision::Switch
    }

    pub fn success(&mut self, policy: &Policy, now_ms: f64) {
        if self.current == 0 {
            policy.pinned.set(None);
        }
        if self.pin_on_success {
            policy
                .pinned
                .set(Some((self.current, now_ms + policy.cooldown_ms)));
            self.pin_on_success = false;
        }
    }
}

pub fn status(error: &Value) -> Option<u16> {
    [&error["status"], &error["code"]].iter().find_map(|v| {
        v.as_u64()
            .and_then(|n| u16::try_from(n).ok())
            .or_else(|| v.as_str()?.parse().ok())
    })
}

pub fn retryable(status: Option<u16>, reason: &str) -> bool {
    let message = reason.to_ascii_lowercase();
    if [
        "prompt is too long",
        "prompt too long",
        "maximum context length",
        "context length exceeded",
        "context_length_exceeded",
        "input is too long",
        "input token count",
        "exceeds the context window",
        "reduce the length of the messages",
        "exceeded model token limit",
        "model_context_window_exceeded",
        "context window exceeds limit",
        "exceeds the available context size",
        "max_tokens must be greater than",
        "maximum number of tokens",
        "content length exceeds",
        "exceeds the model's max token",
        "token limit",
        "reduce your prompt",
        "request_too_large",
        "request too large",
        "payload too large",
        "request entity too large",
        "image exceeds",
        "image dimensions exceed",
        "pdf pages",
        "exceed_context_size_error",
    ]
    .iter()
    .any(|s| message.contains(s))
    {
        return false;
    }
    // Typed status wins over an incidental number in provider diagnostics.
    let status = status.or_else(|| {
        message
            .split(|c: char| c != '_' && !c.is_ascii_alphanumeric())
            .find_map(|s| {
                s.parse::<u16>()
                    .ok()
                    .filter(|n| s.len() == 3 && (400..600).contains(n))
            })
    });
    if let Some(status) = status {
        return !matches!(status, 413 | 429)
            && (matches!(status, 408 | 500 | 502 | 503 | 504 | 529)
                || message.contains("number of function response parts"));
    }
    [
        "timeout",
        "connection reset",
        "connection refused",
        "no such host",
        "temporary failure",
        "service unavailable",
        "internal server error",
        "bad gateway",
        "overloaded",
        "other side closed",
        "fetch failed",
        "reset before headers",
        "upstream connect",
        "internal_error",
        "unexpected eof",
        "unexpected end of json input",
    ]
    .iter()
    .any(|p| message.contains(p))
}

// Once an output item (even an unannounced tool fragment) has arrived we cannot
// replay it. Empty role and stream-lifecycle headers carry no assistant output.
pub fn prelude(chunk: &Value, responses: bool) -> bool {
    if responses {
        matches!(
            chunk["type"].as_str(),
            Some("response.created" | "response.queued" | "response.in_progress")
        ) && chunk["response"]["output"]
            .as_array()
            .is_none_or(Vec::is_empty)
    } else {
        chunk["usage"].is_null()
            && chunk["choices"].as_array().is_some_and(|choices| {
                choices.iter().all(|c| {
                    c["finish_reason"].is_null()
                        && c["delta"].as_object().is_some_and(|d| {
                            d.iter()
                                .all(|(key, v)| key == "role" || v.is_null() || v == "")
                        })
                })
            })
    }
}

pub fn check_responses_history(body: &Value) -> Result<(), String> {
    // Chat history may contain names, opaque provider state or reasoning that
    // the stateless Responses adapter cannot send to a different model.
    for m in body["messages"].as_array().expect("conversation messages") {
        let fields: &[&str] = match m["role"].as_str() {
            Some("assistant") => &["role", "content", "tool_calls"],
            Some("tool") => &["role", "content", "tool_call_id"],
            _ => &["role", "content"],
        };
        let whitespace_only = m["role"] == "assistant"
            && m["content"]
                .as_str()
                .is_some_and(|s| !s.is_empty() && s.trim().is_empty());
        let supported = !whitespace_only
            && has_only(m, fields)
            && m["tool_calls"].as_array().is_none_or(|calls| {
                calls.iter().all(|c| {
                    has_only(c, &["type", "id", "function"])
                        && has_only(&c["function"], &["name", "arguments"])
                })
            });
        if !supported {
            return Err(
                "fallback chain cannot replay this message history losslessly with Responses"
                    .into(),
            );
        }
    }
    Ok(())
}

fn has_only(object: &Value, fields: &[&str]) -> bool {
    object
        .as_object()
        .is_some_and(|o| o.keys().all(|k| fields.contains(&k.as_str())))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cooldown_syntax_and_limits() {
        for (input, expected) in [
            ("'1m30.125s'", 90_125_000_000),
            ("3", 3_000_000_000),
            ("'+.5ms1µs1μs1us'", 503_000),
            ("'0'", 0),
            ("'-0s'", 0),
            ("''", 0),
            ("'9223372036854775807ns'", i64::MAX as u64),
        ] {
            let cfg: Config = serde_yaml_ng::from_str(&format!("cooldown: {input}")).unwrap();
            assert_eq!(cfg.cooldown_ns, expected, "{input}");
        }
        for bad in [
            "-1",
            "-1s",
            "+",
            "1e2s",
            "1.2.3s",
            "1d",
            "1h2",
            "9223372036854775808ns",
            "10h-3s",
        ] {
            assert!(
                serde_yaml_ng::from_str::<Config>(&format!("cooldown: '{bad}'")).is_err(),
                "{bad}"
            );
        }
    }

    #[test]
    fn classifies_before_replaying() {
        for (status, reason, retry) in [
            (None, "unexpected EOF", true),
            (None, "model error 502", true),
            (None, "bad key", false),
            (None, "invalid foo500", false),
            (Some(501), "server error", false),
            (Some(500), "maximum context length", false),
            (Some(429), "timeout", false),
            (Some(408), "", true),
            (Some(529), "", true),
        ] {
            assert_eq!(retryable(status, reason), retry, "{status:?}: {reason}");
        }
    }
}
