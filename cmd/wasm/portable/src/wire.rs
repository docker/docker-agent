use serde_json::{json, Value};

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
