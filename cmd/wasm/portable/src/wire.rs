use serde_json::{json, Value};

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
