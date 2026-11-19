use serde::Serialize;
use serde_json::{json, Value};
use std::io;

// Generic Chat Completions gateways often reject consecutive system/user
// messages. Responses and the first-party chat endpoint retain the boundaries.
pub fn merge_text_messages(request: &mut Value) {
    let Some(messages) = request["messages"].as_array_mut() else {
        return;
    };
    let input = std::mem::take(messages);
    for run in input.chunk_by(|a, b| a["role"] == b["role"]) {
        let role = &run[0]["role"];
        if run.len() > 1
            && matches!(role.as_str(), Some("system" | "user"))
            && run
                .iter()
                .all(|m| m["content"].is_string() && m.as_object().is_some_and(|o| o.len() == 2))
        {
            let mut text = String::new();
            for m in run {
                if !text.is_empty() {
                    text.push('\n');
                }
                text.push_str(m["content"].as_str().unwrap());
            }
            messages.push(json!({"role":role, "content":text}));
        } else {
            messages.extend_from_slice(run);
        }
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
    fn only_merges_plain_text_in_the_same_role() {
        let mut req = json!({"messages":[{"role":"system","content":"a"},{"role":"system","content":"b"},
            {"role":"user","content":"hi","name":"x"},{"role":"user","content":"there"}]});
        merge_text_messages(&mut req);
        assert_eq!(
            req["messages"],
            json!([{"role":"system","content":"a\nb"},
            {"role":"user","content":"hi","name":"x"},{"role":"user","content":"there"}])
        );
    }
}
