use serde::Serialize;
use serde_json::{json, Value};
use std::io;

// Generic Chat Completions gateways often reject consecutive system/user messages.
// A multipart boundary flushes preceding strings as one text part, just like oaistream.
pub fn merge_messages(request: &mut Value) {
    let Some(messages) = request["messages"].as_array_mut() else {
        return;
    };
    let mut input = std::mem::take(messages);
    for run in input.chunk_by_mut(|a, b| a["role"] == b["role"]) {
        let role = run[0]["role"].clone();
        if run.len() == 1
            || !matches!(role.as_str(), Some("system" | "user"))
            || run
                .iter()
                .any(|m| m.as_object().is_none_or(|o| o.len() != 2))
        {
            messages.extend(run.iter_mut().map(Value::take));
            continue;
        }
        let mut parts = Vec::new();
        let mut text = String::new();
        for content in run.iter_mut().map(|m| m["content"].take()) {
            match content {
                Value::String(s) if !s.is_empty() => {
                    if !text.is_empty() {
                        text.push('\n');
                    }
                    text.push_str(&s);
                }
                Value::Array(p) if !p.is_empty() => {
                    if !text.is_empty() {
                        parts.push(json!({"type":"text","text":std::mem::take(&mut text)}));
                    }
                    parts.extend(p);
                }
                _ => (),
            }
        }
        let content: Value = if parts.is_empty() {
            text.into()
        } else {
            if !text.is_empty() {
                parts.push(json!({"type":"text","text":text}));
            }
            parts.into()
        };
        messages.push(json!({"role":role,"content":content}));
    }
}

// serde_json already matches encoding/json for quotes, backslashes and control characters.
struct GoFormatter;

impl serde_json::ser::Formatter for GoFormatter {
    fn write_string_fragment<W: io::Write + ?Sized>(
        &mut self,
        w: &mut W,
        s: &str,
    ) -> io::Result<()> {
        let mut start = 0;
        for (i, ch) in s.char_indices() {
            let escape = match ch {
                '<' => br"\u003c",
                '>' => br"\u003e",
                '&' => br"\u0026",
                '\u{2028}' => br"\u2028",
                '\u{2029}' => br"\u2029",
                _ => continue,
            };
            w.write_all(&s.as_bytes()[start..i])?;
            w.write_all(escape)?;
            start = i + ch.len_utf8();
        }
        w.write_all(&s.as_bytes()[start..])
    }
}

pub fn encode(value: impl Serialize) -> String {
    let mut bytes = Vec::new();
    value
        .serialize(&mut serde_json::Serializer::with_formatter(
            &mut bytes,
            GoFormatter,
        ))
        .unwrap();
    String::from_utf8(bytes).unwrap()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn does_not_merge_across_message_metadata() {
        let mut req = json!({"messages":[{"role":"system","content":"a"},{"role":"system","content":"b"},
            {"role":"user","content":"hi","name":"x"},{"role":"user","content":"there"}]});
        merge_messages(&mut req);
        assert_eq!(
            req["messages"],
            json!([{"role":"system","content":"a\nb"},
            {"role":"user","content":"hi","name":"x"},{"role":"user","content":"there"}])
        );
    }
}
