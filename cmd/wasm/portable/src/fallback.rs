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

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Overflow {
    Tokens,
    Wire,
    Media,
}

impl Overflow {
    pub fn message(self) -> &'static str {
        match self {
            Self::Tokens => "The conversation has exceeded the model's context window. Compact the conversation to reduce its size, or start a new session.",
            Self::Wire => "Your message is too large for the AI provider. Try a smaller paste, attach the file separately, or split the content.",
            Self::Media => "An image or file in this conversation is too large for the AI provider. Try a smaller file or remove it from context.",
        }
    }
}

fn truncated(lower: &str) -> bool {
    lower.contains("unexpected eof") || lower.contains("unexpected end of json input")
}

fn status_text(code: u16) -> Option<&'static str> {
    // net/http retains the older names for these statuses.
    match code {
        413 => Some("Request Entity Too Large"),
        414 => Some("Request URI Too Long"),
        416 => Some("Requested Range Not Satisfiable"),
        _ => http::StatusCode::from_u16(code).ok()?.canonical_reason(),
    }
}

// ErrorBodyMiddleware preserves nonstandard bodies; the SDK then unwraps exactly one `error`.
// Only a decoded apierror.Error carries a status into the runtime retry loop.
pub fn http_error(code: u16, endpoint: &str, body: &str) -> Result<(Option<u16>, String), String> {
    use serde_json::value::RawValue;
    const PHASE: &str = "error receiving from stream";
    const UNSUPPORTED: &str = "unsupported HTTP error body";
    let status = status_text(code).unwrap_or_default();
    let typed = |message: String| (Some(code), format!("{PHASE}: HTTP {code}: {message}"));
    let body = if body.is_empty() { status } else { body };
    let raw = body.trim_matches([' ', '\n', '\r', '\t']);
    if crate::go_json::syntax_error(raw)
        .map_err(|_| UNSUPPORTED)?
        .is_some()
        && !body.is_empty()
    {
        let mut detail = body.to_owned();
        if body.contains("(Request-ID:") {
            append_request_id(&mut detail, &crate::go_json::encode(body))?;
        }
        return Ok(typed(detail));
    }
    #[derive(Deserialize)]
    struct Envelope<'a> {
        #[serde(borrow)]
        error: Option<&'a RawValue>,
    }
    let raw = if raw.starts_with('{') {
        serde_json::from_str::<Envelope>(raw)
            .map_err(|_| UNSUPPORTED)?
            .error
            .map(RawValue::get)
            .filter(|s| s.starts_with('{'))
            .unwrap_or(raw)
    } else if body.is_empty() {
        r#"{"message":""}"#
    } else if raw == "null" {
        raw
    } else {
        let error = crate::go_json::type_error(raw, "Go value", "apierror.Error");
        return Ok((None, format!("{PHASE}: {error}")));
    };
    if let Some(mut message) = http_message(raw)? {
        append_request_id(&mut message, raw)?;
        return Ok(typed(message));
    }
    // Do not project a raw diagnostic whose escapes could hide a credential from redaction.
    if raw.contains('\\') {
        return Err(UNSUPPORTED.into());
    }
    Ok(typed(format!("POST \"{endpoint}\": {code} {status} {raw}")))
}

fn http_message(raw: &str) -> Result<Option<String>, String> {
    if raw == "null" {
        return Ok(None);
    }
    let mut message = None;
    let mut invalid = false;
    // fields() borrows slices from validated JSON and expects decodable strings.
    serde_json::from_str::<Value>(raw).map_err(|_| "unsupported HTTP error body")?;
    crate::go_json::fields(raw, |key, val| {
        if crate::go_json::field_matches(&key, "message") {
            if message.is_some() {
                return Err("ambiguous provider error message".into());
            }
            message = Some(serde_json::from_str::<String>(val).unwrap_or_default());
        } else if crate::go_json::field_matches(&key, "error") && val != "null" {
            if val.starts_with('{') {
                return Err("unsupported nested HTTP error".into());
            }
            invalid = true;
        }
        Ok(())
    })?;
    Ok(message.filter(|s| !invalid && !s.is_empty()))
}

fn append_request_id(message: &mut String, raw: &str) -> Result<(), String> {
    static ID: std::sync::OnceLock<regex::Regex> = std::sync::OnceLock::new();
    let re = ID
        .get_or_init(|| regex::Regex::new(r"\(Request-ID:(?-u:\s*)([^)\t\n\x0c\r ]+)\)").unwrap());
    if let Some(id) = re.captures(raw) {
        if id[1].contains('\\') {
            return Err("unsupported escaped request ID".into());
        }
        message.push_str(" (Request-ID: ");
        message.push_str(&id[1]);
        message.push(')');
    }
    Ok(())
}

pub fn model_error(prefix: &str, overflow: Option<Overflow>, reason: &str) -> String {
    if let Some(kind) = overflow {
        return kind.message().into();
    }
    if truncated(&reason.to_ascii_lowercase()) {
        return concat!("The connection to the model was closed unexpectedly before it completed its response. ",
            "With local models (e.g. Docker Model Runner), this can happen when an idle or ",
            "connection timeout in the model server (or a proxy in front of it) drops a silent ",
            "prefill before the first token, or cuts a long response mid-stream. ",
            "Try a shorter prompt, or raise the model server's idle/keep-alive timeout.").into();
    }
    format!("{prefix}: {reason}")
}

fn error_field<'a>(object: &'a Value, key: &str) -> Result<&'a Value, String> {
    let mut found = object
        .as_object()
        .into_iter()
        .flatten()
        .filter(|(k, _)| crate::go_json::field_matches(k, key));
    let value = found.next().map_or(&Value::Null, |(_, v)| v);
    if found.next().is_some() {
        return Err("ambiguous fields in provider error envelope".into());
    }
    Ok(value)
}

pub fn overflow(http_status: Option<u16>, reason: &str) -> Result<Option<Overflow>, String> {
    // The SDK unwraps the provider envelope. Only JSON still present in the
    // diagnostic (not fields discarded by the adapter) is visible to Go.
    for (attempt, (i, _)) in reason.match_indices('{').enumerate() {
        if attempt >= 16 || reason.len() - i > 1 << 20 {
            return Err("provider diagnostic exceeds overflow classification limit".into());
        }
        let Some(Ok(body)) = serde_json::Deserializer::from_str(&reason[i..])
            .into_iter::<Value>()
            .next()
        else {
            continue;
        };
        let error = error_field(&body, "error")?;
        if !error.is_object() {
            break;
        }
        let kind = error_field(error, "type")?;
        if [
            kind,
            error_field(error, "message")?,
            error_field(error, "status")?,
            error_field(&body, "message")?,
        ]
        .into_iter()
        .any(|v| !v.is_null() && !v.is_string())
        {
            break;
        }
        if kind == "request_too_large" {
            return Ok(Some(Overflow::Wire));
        }
        if error_field(error, "code")? == "context_length_exceeded"
            || kind == "exceed_context_size_error"
        {
            return Ok(Some(Overflow::Tokens));
        }
        break;
    }
    if http_status == Some(413) {
        return Ok(Some(Overflow::Wire));
    }
    let lower = reason.to_ascii_lowercase();
    let matches = |patterns: &[&str]| patterns.iter().any(|p| lower.contains(p));
    if matches(&["image exceeds", "image dimensions exceed", "pdf pages"]) {
        return Ok(Some(Overflow::Media));
    }
    if matches(&[
        "request_too_large",
        "request too large",
        "payload too large",
        "request entity too large",
    ]) {
        return Ok(Some(Overflow::Wire));
    }
    Ok(matches(&[
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
    ])
    .then_some(Overflow::Tokens))
}

pub fn retryable(status: Option<u16>, reason: &str) -> bool {
    let message = reason.to_ascii_lowercase();
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
    ]
    .iter()
    .any(|p| message.contains(p))
        || truncated(&message)
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
    fn http_diagnostic_uses_the_unwrapped_wire_object() {
        for (code, body, detail) in [
            (
                400,
                r#"{"error":{"type":"invalid","message":"bad"}}"#,
                "bad",
            ),
            (
                403,
                r#"{"Message":"no","opaque":"(Request-ID: abc)"}"#,
                "no (Request-ID: abc)",
            ),
            (
                400,
                r#"{ "error" : { "extra" : 123 } }"#,
                r#"POST "url": 400 Bad Request { "extra" : 123 }"#,
            ),
            (400, " <html>bad</html>\n", " <html>bad</html>\n"),
            (413, "", "Request Entity Too Large"),
            (529, "", r#"POST "url": 529  {"message":""}"#),
        ] {
            assert_eq!(
                http_error(code, "url", body).unwrap(),
                (
                    Some(code),
                    format!("error receiving from stream: HTTP {code}: {detail}")
                )
            );
        }
        for (body, kind) in [
            ("42", "number"),
            ("false", "bool"),
            ("[]", "array"),
            (r#""x""#, "string"),
        ] {
            let message = format!("error receiving from stream: json: cannot unmarshal {kind} into Go value of type apierror.Error");
            assert_eq!(http_error(503, "url", body).unwrap(), (None, message));
        }
    }

    #[test]
    fn ambiguous_or_escaped_raw_diagnostics_fail_closed() {
        for body in [
            r#"{"error":{"error":{"message":"hidden"}}}"#,
            r#"{"error":{"message":"a"},"error":{"message":"b"}}"#,
            r#"{"message":"a","MESSAGE":"b"}"#,
            r#"{"opaque":"f\u0069xture"}"#,
            r#"{"message":"\ud800"}"#,
        ] {
            assert!(http_error(400, "url", body).is_err(), "{body}");
        }
    }

    #[test]
    fn overflow_uses_only_the_error_visible_to_the_adapter() {
        use Overflow::*;
        for (status, text, kind) in [
            (None, "TOKEN LIMIT", Some(Tokens)),
            (None, "image exceeds token limit", Some(Media)),
            (None, "payload too large: token limit", Some(Wire)),
            (Some(413), "token limit", Some(Wire)),
            (None, r#"{"type":"exceed_context_size_error"}"#, None),
            (
                Some(413),
                r#"inner {"error":{"type":"exceed_context_size_error"}}"#,
                Some(Tokens),
            ),
            (
                None,
                r#"{"error":{"type":"request_too_large","code":"context_length_exceeded"}}"#,
                Some(Wire),
            ),
        ] {
            assert_eq!(overflow(status, text).unwrap(), kind, "{text}");
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
            assert_eq!(
                overflow(status, reason).unwrap().is_none() && retryable(status, reason),
                retry,
                "{status:?}: {reason}"
            );
        }
    }
}
