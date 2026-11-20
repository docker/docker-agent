//! Browser-safe subset of pkg/hooks/builtins. Compile and validate the entire
//! configuration before opening a session, including unselected agents.
mod redaction;
pub(crate) use redaction::redact;

use serde::Deserialize;
use serde_json::Value;

#[derive(Default, Clone, Deserialize)]
#[serde(try_from = "Value")]
pub struct Hooks {
    pub add_date: bool,
    pub add_environment_info: bool,
    pub max_calls: Option<u32>,
    pub redact_messages: bool,
    redact_inputs: Vec<Matcher>,
    outputs: Vec<(Matcher, Transform)>,
}

#[derive(Clone, Default)]
struct Matcher(Vec<String>);

impl Matcher {
    fn parse(value: Option<&Value>) -> Result<Self, String> {
        match value.map(Value::as_str) {
            None | Some(Some("" | "*")) => Ok(Self::default()),
            // A deliberately narrow subset of Go's anchored regular expressions.
            Some(Some(s))
                if s.len() <= 256
                    && s.split('|').all(|term| {
                        !term.is_empty()
                            && term.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'_')
                    }) =>
            {
                Ok(Self(s.split('|').map(str::to_owned).collect()))
            }
            _ => Err(
                "unsupported hook matcher: use '*' or literal tool names separated by '|'".into(),
            ),
        }
    }
    fn matches(&self, name: &str) -> bool {
        self.0.is_empty() || self.0.iter().any(|s| s == name)
    }
}

#[derive(Clone, Copy, PartialEq)]
enum Transform {
    Redact,
    Limit,
}

fn entries(value: &Value) -> Result<&[Value], String> {
    match value {
        Value::Object(_) => Ok(std::slice::from_ref(value)),
        Value::Array(a) if a.len() <= 64 && a.iter().all(Value::is_object) => Ok(a),
        _ => Err("expected hook mapping or list of up to 64 mappings".into()),
    }
}

impl TryFrom<Value> for Hooks {
    type Error = String;
    fn try_from(value: Value) -> Result<Self, String> {
        let mut cfg = Self::default();
        for (event, value) in value.as_object().ok_or("hooks must be a mapping")? {
            match event.as_str() {
                "before_llm_call" | "session_start" | "turn_start" | "session_end" => {
                    for hook in entries(value)? {
                        cfg.add(event, hook, Matcher::default())?;
                    }
                }
                "tool_input_transform" | "tool_response_transform" => {
                    for entry in entries(value)? {
                        let obj = entry.as_object().unwrap();
                        if obj
                            .keys()
                            .any(|k| !matches!(k.as_str(), "matcher" | "hooks"))
                        {
                            return Err(format!("unsupported hooks.{event} matcher field"));
                        }
                        let matcher = Matcher::parse(entry.get("matcher"))?;
                        let hooks = entries(&entry["hooks"])?;
                        if hooks.is_empty() {
                            return Err(format!("hooks.{event}: at least one hook is required"));
                        }
                        for hook in hooks {
                            cfg.add(event, hook, matcher.clone())?;
                        }
                    }
                }
                _ => return Err(format!("unsupported hook event {event:?}")),
            }
        }
        Ok(cfg)
    }
}

impl Hooks {
    fn add(&mut self, event: &str, h: &Value, matcher: Matcher) -> Result<(), String> {
        let obj = h.as_object().unwrap();
        if obj
            .keys()
            .any(|k| !matches!(k.as_str(), "command" | "type" | "args"))
            || h["type"] != "builtin"
        {
            return Err(format!(
                "hooks.{event}: expected builtin with command and optional args"
            ));
        }
        let args = match h.get("args") {
            None => &[][..],
            Some(Value::Array(a)) if a.iter().all(Value::is_string) => a.as_slice(),
            _ => return Err(format!("hooks.{event}: args must be a list of strings")),
        };
        match (event, h["command"].as_str().unwrap_or(""), args) {
            ("before_llm_call", "max_iterations", [v]) => {
                let n = v
                    .as_str()
                    .unwrap()
                    .parse::<i32>()
                    .ok()
                    .filter(|&n| n > 0)
                    .ok_or("max_iterations requires a positive integer limit")?
                    as u32;
                if self.max_calls.is_some_and(|old| old != n) {
                    return Err("conflicting max_iterations hooks".into());
                }
                self.max_calls = Some(n);
            }
            ("turn_start", "add_date", []) => self.add_date = true,
            ("session_start", "add_environment_info", []) => self.add_environment_info = true,
            ("tool_input_transform", "redact_secrets", []) => self.redact_inputs.push(matcher),
            ("before_llm_call", "redact_secrets", []) => self.redact_messages = true,
            ("tool_response_transform", "redact_secrets", []) => {
                self.outputs.push((matcher, Transform::Redact))
            }
            ("tool_response_transform", "limit_large_tool_results", []) => {
                self.outputs.push((matcher, Transform::Limit))
            }
            // Go's browser implementation has no spill files to clean up.
            ("session_end", "limit_large_tool_results", []) => (),
            (_, command, _) => {
                return Err(format!(
                    "unsupported hooks.{event} builtin {command:?} or arguments"
                ))
            }
        }
        Ok(())
    }

    pub fn with_defaults(mut self, redact: bool, date: bool, env: bool) -> Self {
        if redact {
            self.redact_inputs.push(Matcher::default());
            self.redact_messages = true;
            self.outputs.push((Matcher::default(), Transform::Redact));
        }
        self.outputs.push((Matcher::default(), Transform::Limit));
        self.add_date |= date;
        self.add_environment_info |= env;
        self
    }

    pub fn rewrite_input(&self, name: &str, args: &mut String) -> Result<bool, String> {
        if !self.redact_inputs.iter().any(|m| m.matches(name)) {
            return Ok(false);
        }
        if args.len() > 256 * 1024 {
            return Err("tool input redaction size limit reached".into());
        }
        // Go walks decoded JSON values, not string keys or the escaped JSON source.
        if args.is_empty() {
            return Ok(false);
        }
        let mut input: Value =
            serde_json::from_str(args).map_err(|_| "cannot redact invalid tool input")?;
        let changed = redact_values(&mut input)?;
        if changed {
            *args = crate::wire::encode(input);
        }
        Ok(changed)
    }

    pub fn check_before_truncation(&self, text: &str) -> Result<(), String> {
        // Truncating first could split a secret; redacting first could expose text outside the cap.
        if self.redact_messages && redaction::redact(text)?.is_some() {
            return Err(
                "cannot safely truncate a tool result containing unredacted secrets".into(),
            );
        }
        Ok(())
    }

    pub fn rewrite_messages(&self, messages: &mut [Value]) -> Result<(), String> {
        if self.redact_messages {
            for msg in messages {
                let fields = msg.as_object_mut().ok_or("invalid message for redaction")?;
                for (field, value) in fields {
                    match field.as_str() {
                        // Wire identifiers must stay paired across calls and replies.
                        "role" | "tool_call_id" => (),
                        "tool_calls" => {
                            if let Some(calls) = value.as_array_mut() {
                                for call in calls {
                                    if let Some(args) = call.pointer_mut("/function/arguments") {
                                        redact_values(args)?;
                                    }
                                }
                            }
                        }
                        _ => {
                            redact_values(value)?;
                        }
                    }
                }
            }
        }
        Ok(())
    }

    pub fn tool_result(
        &self,
        name: &str,
        category: &str,
        mut output: String,
    ) -> Result<String, String> {
        let (mut redacted, mut limited) = (false, false);
        for (matcher, transform) in &self.outputs {
            if !matcher.matches(name) {
                continue;
            }
            let next = match transform {
                Transform::Redact if !redacted => {
                    redacted = true;
                    redaction::redact(&output)?
                }
                Transform::Limit if !limited => {
                    limited = true;
                    limit_large_tool_result(category, &output)
                }
                _ => None,
            };
            if let Some(text) = next {
                output = text;
            }
        }
        Ok(output)
    }
}

fn redact_values(v: &mut Value) -> Result<bool, String> {
    let mut changed = false;
    match v {
        Value::String(s) => {
            if let Some(text) = redaction::redact(s)? {
                *s = text;
                changed = true;
            }
        }
        Value::Object(m) => {
            for v in m.values_mut() {
                changed |= redact_values(v)?;
            }
        }
        Value::Array(a) => {
            for v in a {
                changed |= redact_values(v)?;
            }
        }
        _ => (),
    }
    Ok(changed)
}

fn limit_large_tool_result(category: &str, payload: &str) -> Option<String> {
    const BYTES: usize = 50 * 1024;
    const LINES: usize = 2000;
    if !matches!(
        category,
        "filesystem" | "shell" | "background_jobs" | "mcp" | "a2a"
    ) {
        return None;
    }
    let lines = payload.split_terminator('\n').count();
    let reason = if payload.len() > BYTES {
        format!("{} bytes; limit {BYTES} bytes", payload.len())
    } else if lines > LINES {
        format!("{lines} lines; limit {LINES} lines")
    } else {
        return None;
    };
    let mut prefix = format!("Tool call result was too large ({reason}). The full result is not available in the browser; narrow the tool query to get the part you need.\n\n");
    if category == "background_jobs" {
        if let Some((header, _)) = payload.split_once("--- Output ---\n") {
            for line in header.split('\n') {
                let mut end = line.len().min(512);
                while !line.is_char_boundary(end) {
                    end -= 1;
                }
                prefix.push_str(&line[..end]);
                prefix.push('\n');
                if prefix.len() > BYTES / 4 {
                    break;
                }
            }
        }
    }
    prefix.push_str(&format!(
        "Showing the last {LINES} lines (up to {BYTES} bytes, including this notice):\n\n"
    ));
    let tail = payload
        .rmatch_indices('\n')
        .nth(LINES)
        .map_or(payload, |(i, _)| &payload[i + 1..]);
    let mut start = tail.len().saturating_sub(BYTES - prefix.len());
    while !tail.is_char_boundary(start) {
        start += 1;
    }
    prefix.push_str(&tail[start..]);
    Some(prefix)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn strict_shape_events_args_and_options() {
        let h = |command| json!({"type":"builtin", "command":command});
        for value in [
            json!([]),
            json!({"post_tool_use":[]}),
            json!({"turn_start":h("redact_secrets")}),
            json!({"pre_tool_use":{"matcher":"*", "hooks":h("redact_secrets")}}),
            json!({"before_llm_call":h("max_iterations")}),
            json!({"before_llm_call":{"command":"redact_secrets"}}),
            json!({"before_llm_call":{"type":"command", "command":"redact_secrets"}}),
            json!({"before_llm_call":{"type":"builtin", "command":"redact_secrets", "on_error":"ignore"}}),
            json!({"tool_response_transform":h("redact_secrets")}),
            json!({"tool_response_transform":{"hooks":[]}}),
            json!({"tool_input_transform":{"matcher":"(think)", "hooks":h("redact_secrets")}}),
            json!({"tool_input_transform":{"hooks":h("redact_secrets"), "preempt_yolo":false}}),
        ] {
            assert!(Hooks::try_from(value.clone()).is_err(), "{value}");
        }
        for args in [
            json!([0]),
            json!(["0"]),
            json!(["-1"]),
            json!(["9999999999999"]),
            json!(["2", "3"]),
        ] {
            let value = json!({"before_llm_call":{"command":"max_iterations","type":"builtin","args":args}});
            assert!(Hooks::try_from(value).is_err());
        }
    }

    #[test]
    fn matches_only_selected_tool_names_and_legs() {
        let cfg = Hooks::try_from(json!({"tool_input_transform":[{"matcher":"think|calculate", "hooks":{"command":"redact_secrets","type":"builtin"}}]})).unwrap().with_defaults(false,false,false);
        let secret = format!("dckr_pat_{}", "a".repeat(27));
        let input = json!({"n":42, "nested":[{"argument":secret}]}).to_string();
        let mut args = input.clone();
        assert!(!cfg.rewrite_input("rethink", &mut args).unwrap());
        assert_eq!(args, input);
        assert!(cfg.rewrite_input("think", &mut args).unwrap());
        assert_eq!(args, r#"{"n":42,"nested":[{"argument":"[REDACTED]"}]}"#);
        assert_eq!(
            cfg.tool_result("think", "think", secret.clone()).unwrap(),
            secret
        );
        let mut messages = [json!({"content":secret})];
        cfg.rewrite_messages(&mut messages).unwrap();
        assert_eq!(messages[0]["content"], secret);
    }

    #[test]
    fn limits_go_categories_and_respects_utf8_bytes_and_line_boundaries() {
        let cfg = Hooks::default().with_defaults(false, false, false);
        let text = format!("old\n{}last\n", "猫😀".repeat(8192));
        for category in ["think", "todo", "user_prompt", "fetch"] {
            assert_eq!(
                cfg.tool_result("mcp", category, text.clone()).unwrap(),
                text
            );
        }
        for category in ["mcp", "a2a", "shell", "filesystem", "background_jobs"] {
            let out = cfg
                .tool_result("read_file", category, text.clone())
                .unwrap();
            assert!(out.len() <= 50 * 1024);
            assert!(out.starts_with("Tool call result was too large (57353 bytes; limit 51200 bytes). The full result is not available in the browser;"));
            assert!(out.ends_with("猫😀last\n"));
            assert!(!out.contains("old\n"));
        }
        let at_limit = "line\n".repeat(2000);
        assert!(limit_large_tool_result("mcp", &at_limit).is_none());
        let out = limit_large_tool_result("mcp", &format!("first\n{at_limit}")).unwrap();
        assert!(out.contains("(2001 lines; limit 2000 lines)"));
        assert!(out.ends_with(&at_limit));
        assert!(!out.contains("first"));
        let out = limit_large_tool_result(
            "background_jobs",
            &format!("exit: 9\n--- Output ---\n{text}"),
        )
        .unwrap();
        assert!(out.contains("exit: 9\n"));
        assert!(limit_large_tool_result("mcp", &"x".repeat(51200)).is_none());
    }
}
