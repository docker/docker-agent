//! Stateless Responses requests and per-request SSE translation for the portable text/tool loop.

use serde_json::{json, Value};
use std::collections::BTreeMap;

fn string<'a>(obj: &'a Value, key: &str) -> Result<&'a str, String> {
    obj[key]
        .as_str()
        .ok_or_else(|| format!("expected string {key}"))
}

fn array<'a>(obj: &'a Value, key: &str) -> Result<&'a [Value], String> {
    match &obj[key] {
        Value::Array(a) => Ok(a),
        Value::Null => Ok(&[]),
        _ => Err(format!("expected array {key}")),
    }
}

// Mirrors modelinfo.UsesReasoningEffort and the default effort for always-reasoning o-series.
fn reasoning(model: &str, effort: &Value) -> Option<Value> {
    let name = model.trim().rsplit('/').next()?.to_ascii_lowercase();
    let o_series = ["o1", "o3", "o4"].iter().any(|p| name.starts_with(p));
    let generation = name.strip_prefix("gpt-").unwrap_or("").split('-').next()?;
    let (major, minor) = generation.split_once('.').unwrap_or((generation, "0"));
    let future_gpt = [major, minor]
        .iter()
        .all(|s| (1..=2).contains(&s.len()) && s.bytes().all(|b| b.is_ascii_digit()))
        && major.parse::<u8>().is_ok_and(|n| n >= 6);
    let gpt5 = name.starts_with("gpt-5") && !name.starts_with("gpt-5-chat");
    if !(o_series || gpt5 || future_gpt) {
        return None;
    }
    let mut reasoning = json!({"summary":"detailed"});
    if let Some(effort) = effort.as_str().or(o_series.then_some("medium")) {
        reasoning["effort"] = effort.into();
    }
    Some(reasoning)
}

pub fn request(chat: Value) -> Result<Value, String> {
    let model = string(&chat, "model")?;
    let mut input = Vec::new();
    for m in array(&chat, "messages")? {
        let role = string(m, "role")?;
        match role {
            "system" | "developer" => input.push(json!({
                "role":role, "content":[{"type":"input_text", "text":string(m,"content")?}]
            })),
            "user" => input.push(json!({"role":role,"content":string(m,"content")?})),
            "assistant" => {
                // The default Go SSE path doesn't replay reasoning_content or opaque provider state.
                if !m["content"].is_null() {
                    let text = string(m, "content")?;
                    if !text.trim().is_empty() {
                        input.push(json!({"role":role,"content":text}));
                    }
                }
                for call in array(m, "tool_calls")? {
                    if call["type"] != "function" {
                        return Err("expected a function call".into());
                    }
                    let f = &call["function"];
                    input.push(json!({
                        "type":"function_call", "call_id":string(call,"id")?,
                        "name":string(f,"name")?, "arguments":string(f,"arguments")?
                    }));
                }
            }
            "tool" => input.push(json!({
                "type":"function_call_output", "call_id":string(m,"tool_call_id")?, "output":string(m,"content")?
            })),
            _ => return Err(format!("unsupported message role {role}")),
        }
    }
    if input.is_empty() {
        return Err("empty Responses input".into());
    }
    let mut body = json!({"model":model,"stream":true});
    body["input"] = input.into();
    // frequency_penalty, presence_penalty and stream_options are Chat-only.
    for key in ["temperature", "top_p", "service_tier"] {
        if !chat[key].is_null() {
            body[key] = chat[key].clone();
        }
    }
    if let Some(cap) = chat.get("max_completion_tokens").or(chat.get("max_tokens")) {
        if cap.as_u64().is_some_and(|n| n > 0) {
            body["max_output_tokens"] = cap.clone();
        }
    }
    if let Some(params) = reasoning(model, &chat["reasoning_effort"]) {
        body["reasoning"] = params;
    }

    let mut tools = Vec::new();
    for tool in array(&chat, "tools")? {
        if tool["type"] != "function" {
            return Err("only function tools are supported".into());
        }
        let f = &tool["function"];
        let name = string(f, "name")?;
        if !f["parameters"].is_object() {
            return Err(format!("missing parameters for tool {name}"));
        }
        // Strictness is determined from the original Go schema, before normalization.
        let strict = crate::turn::response_strict(name);
        tools.push(json!({
            "type":"function", "name":name, "description":string(f,"description")?,
            "parameters":f["parameters"], "strict":strict
        }));
    }
    if !tools.is_empty() {
        body["tools"] = tools.into();
        let choice = &chat["tool_choice"];
        body["tool_choice"] = match choice {
            Value::Null => "auto".into(),
            Value::String(s) if matches!(s.as_str(), "auto" | "none" | "required") => {
                choice.clone()
            }
            _ if choice["type"] == "function" => {
                json!({"type":"function","name":string(&choice["function"],"name")?})
            }
            _ => return Err("unsupported tool_choice".into()),
        };
        if !chat["parallel_tool_calls"].is_null() {
            body["parallel_tool_calls"] = chat["parallel_tool_calls"].clone();
        }
    }
    Ok(body)
}

#[derive(Default)]
struct Fragments {
    text: String,
    done: bool,
}

impl Fragments {
    // Final snapshots may repeat all streamed bytes or supply a missing suffix.
    fn update<'a>(&mut self, value: &'a str, snapshot: bool) -> Result<&'a str, String> {
        if value.is_empty() || (self.done && !snapshot) {
            return Ok("");
        }
        let suffix = if snapshot {
            self.done = true;
            value
                .strip_prefix(&self.text)
                .ok_or("Responses snapshot contradicts streamed deltas")?
        } else {
            value
        };
        self.text.push_str(suffix);
        Ok(suffix)
    }
}

#[derive(Default)]
struct Call {
    index: Option<usize>,
    id: String,
    name: String,
    args: Fragments,
}

impl Call {
    fn announce(
        &mut self,
        id: Option<&str>,
        name: Option<&str>,
        args: Option<&str>,
        calls: &mut usize,
        out: &mut Vec<Value>,
    ) -> Result<(), String> {
        for (stored, incoming, key) in [(&mut self.id, id, "ID"), (&mut self.name, name, "name")] {
            if let Some(s) = incoming.filter(|s| !s.is_empty()) {
                if stored.is_empty() {
                    stored.push_str(s);
                } else if stored != s {
                    return Err(format!("function call {key} changed"));
                }
            }
        }
        if let Some(args) = args {
            self.arguments(args, true, out)?;
        }
        if self.index.is_none() && !self.id.is_empty() && !self.name.is_empty() {
            self.index = Some(*calls);
            delta(
                out,
                json!({"tool_calls":[{
                    "index":*calls, "id":self.id, "type":"function",
                    "function":{"name":self.name,"arguments":self.args.text}
                }]}),
            );
            *calls += 1;
        }
        Ok(())
    }

    fn arguments(&mut self, text: &str, done: bool, out: &mut Vec<Value>) -> Result<(), String> {
        let suffix = self.args.update(text, done)?;
        if let Some(index) = self.index.filter(|_| !suffix.is_empty()) {
            delta(
                out,
                json!({"tool_calls":[{"index":index,"function":{"arguments":suffix}}]}),
            );
        }
        Ok(())
    }
}

#[derive(Default)]
struct Item {
    parts: BTreeMap<(&'static str, u64), Fragments>,
    call: Option<Call>,
}

#[derive(Default)]
pub struct Stream {
    items: Vec<Item>,
    by_id: BTreeMap<String, usize>,
    by_index: BTreeMap<u64, usize>,
    calls: usize,
    incomplete_item: bool,
    terminal: bool,
}

fn delta(out: &mut Vec<Value>, value: Value) {
    out.push(json!({"choices":[{"index":0,"delta":value}]}));
}

impl Item {
    fn text(
        &mut self,
        channel: &'static str,
        index: u64,
        text: &str,
        snapshot: bool,
        out: &mut Vec<Value>,
    ) -> Result<(), String> {
        let fragment = self
            .parts
            .entry((channel, index))
            .or_default()
            .update(text, snapshot)?;
        if !fragment.is_empty() {
            let field = if channel == "text" {
                "content"
            } else {
                "reasoning_content"
            };
            delta(out, json!({"role":"assistant", field:fragment}));
        }
        Ok(())
    }

    fn part(&mut self, part: &Value, index: u64, out: &mut Vec<Value>) -> Result<(), String> {
        let channel = match string(part, "type")? {
            "text" | "output_text" => "text",
            "summary_text" => "summary",
            "reasoning_text" => "reasoning",
            "refusal" => return Err("model refused the request".into()),
            other => return Err(format!("unsupported Responses part: {other}")),
        };
        self.text(channel, index, string(part, "text")?, true, out)
    }
}

impl Stream {
    // item_id joins providers that omit output_index; output_index joins providers that
    // change item_id between deltas and the final snapshot of the same output slot.
    fn slot(&mut self, id: Option<&str>, index: Option<u64>) -> Result<usize, String> {
        let index = index.or(id.is_none().then_some(0));
        let known_id = id.and_then(|id| self.by_id.get(id).copied());
        let known_index = index.and_then(|i| self.by_index.get(&i).copied());
        if matches!((known_id,known_index),(Some(a),Some(b)) if a != b) {
            return Err("conflicting response item identifiers".into());
        }
        let slot = known_index.or(known_id).unwrap_or_else(|| {
            self.items.push(Item::default());
            self.items.len() - 1
        });
        if let Some(id) = id.filter(|_| known_id.is_none()) {
            self.by_id.insert(id.into(), slot);
        }
        if let Some(i) = index.filter(|_| known_index.is_none()) {
            self.by_index.insert(i, slot);
        }
        Ok(slot)
    }

    fn item(
        &mut self,
        slot: usize,
        item: &Value,
        header: &Value,
        done: bool,
        out: &mut Vec<Value>,
    ) -> Result<(), String> {
        if done && !item["status"].is_null() && item["status"] != "completed" {
            self.incomplete_item = true;
        }
        let state = &mut self.items[slot];
        match string(item, "type")? {
            "function_call" => {
                let c = state.call.get_or_insert_with(Call::default);
                let mut id = item["call_id"].as_str().filter(|s| !s.is_empty());
                if id.is_none() && c.id.is_empty() {
                    id = item["id"].as_str().or(header["item_id"].as_str());
                }
                let name = header["name"]
                    .as_str()
                    .filter(|s| !s.is_empty())
                    .or(item["name"].as_str());
                let args = item["arguments"].as_str().filter(|_| done);
                c.announce(id, name, args, &mut self.calls, out)?;
            }
            "message" | "reasoning" => {
                if !item["role"].is_null() && item["role"] != "assistant" {
                    return Err("non-assistant response item".into());
                }
                for field in ["summary", "content"] {
                    for (i, part) in array(item, field)?.iter().enumerate() {
                        if part["type"] == "refusal" {
                            return Err("model refused the request".into());
                        }
                        if done {
                            state.part(part, i as u64, out)?;
                        }
                    }
                }
            }
            other => return Err(format!("unsupported Responses output item: {other}")),
        }
        Ok(())
    }

    pub fn push(&mut self, event: Value) -> Result<Vec<Value>, String> {
        if self.terminal {
            return Err("Responses event after terminal event".into());
        }
        let result = self.translate(&event);
        if result.is_err() {
            self.terminal = true;
        }
        result
    }

    fn translate(&mut self, event: &Value) -> Result<Vec<Value>, String> {
        if let Some((_, reason)) = failure(event) {
            return Err(reason);
        }
        let ty = string(event, "type")?;
        let mut out = Vec::new();
        match ty {
            "response.completed"
            | "response.done"
            | "response.incomplete"
            | "response.failed"
            | "response.cancelled" => {
                self.finish(ty, &event["response"], &mut out)?;
                return Ok(out);
            }
            "response.refusal.delta" | "response.refusal.done" => {
                return Err("model refused the request".into())
            }
            "response.created"
            | "response.queued"
            | "response.in_progress"
            | "response.output_text.annotation.added"
            | "response.reasoning_summary_part.added" => return Ok(out),
            _ => (),
        }
        let item = &event["item"];
        let id = event["item_id"]
            .as_str()
            .or(item["id"].as_str())
            .or(item["call_id"].as_str());
        let slot = self.slot(id, event["output_index"].as_u64())?;
        let state = &mut self.items[slot];
        let index = event["content_index"].as_u64().unwrap_or(0);
        let summary = event["summary_index"].as_u64().unwrap_or(0);
        match ty {
            "response.output_item.added" | "response.output_item.done" => {
                self.item(slot, item, event, ty.ends_with(".done"), &mut out)?;
            }
            "response.output_text.delta" | "response.content_part.delta" => {
                if event["part"]["type"] == "refusal" {
                    return Err("model refused the request".into());
                }
                let text = [
                    &event["delta"],
                    &event["text"],
                    &event["code"],
                    &event["part"]["text"],
                ]
                .into_iter()
                .filter_map(Value::as_str)
                .reduce(|s, t| if s.is_empty() { t } else { s })
                .ok_or("expected text delta")?;
                state.text("text", index, text, false, &mut out)?;
            }
            "response.output_text.done" => {
                state.text("text", index, string(event, "text")?, true, &mut out)?
            }
            "response.content_part.added" => {
                if event["part"]["type"] == "refusal" {
                    return Err("model refused the request".into());
                }
            }
            "response.content_part.done" | "response.reasoning_summary_part.done" => state.part(
                &event["part"],
                if ty.contains("summary") {
                    summary
                } else {
                    index
                },
                &mut out,
            )?,
            "response.reasoning_text.delta"
            | "response.reasoning_text.done"
            | "response.reasoning_summary_text.delta"
            | "response.reasoning_summary_text.done" => {
                let done = ty.ends_with(".done");
                let (channel, index) = if ty.contains("summary") {
                    ("summary", summary)
                } else {
                    ("reasoning", index)
                };
                state.text(
                    channel,
                    index,
                    string(event, if done { "text" } else { "delta" })?,
                    done,
                    &mut out,
                )?;
            }
            "response.function_call_arguments.delta" | "response.function_call_arguments.done" => {
                let done = ty.ends_with(".done");
                let text = if done {
                    event["arguments"].as_str().unwrap_or("")
                } else {
                    event["delta"]
                        .as_str()
                        .filter(|s| !s.is_empty())
                        .or(event["arguments"].as_str())
                        .or(event["delta"].as_str())
                        .ok_or("expected arguments delta")?
                };
                let c = state.call.get_or_insert_with(Call::default);
                c.arguments(text, done, &mut out)?;
            }
            _ => return Err(format!("unsupported Responses stream event: {ty}")),
        }
        Ok(out)
    }

    fn finish(&mut self, ty: &str, response: &Value, out: &mut Vec<Value>) -> Result<(), String> {
        self.terminal = true;
        if !response.is_object() {
            return Err("missing terminal response object".into());
        }
        let status = response
            .get("status")
            .map(|_| string(response, "status"))
            .transpose()?;
        let reason = if ty == "response.incomplete"
            || status == Some("incomplete")
            || !response["incomplete_details"].is_null()
        {
            if response["incomplete_details"]["reason"] == "content_filter" {
                "content_filter"
            } else {
                "length"
            }
        } else {
            if !matches!(
                (ty, status),
                (
                    "response.completed" | "response.done",
                    None | Some("completed")
                )
            ) {
                return Err(format!(
                    "unexpected terminal Responses status: {status:?} ({ty})"
                ));
            }
            let mut listed_calls = 0;
            for (i, item) in array(response, "output")?.iter().enumerate() {
                if item["type"] == "function_call" {
                    listed_calls += 1;
                }
                // Some gateways send type-only markers instead of snapshots in response.completed.
                if item["type"] == "function_call" && item.as_object().is_some_and(|m| m.len() == 1)
                {
                    continue;
                }
                let slot = self.slot(
                    item["id"].as_str().or(item["call_id"].as_str()),
                    Some(i as u64),
                )?;
                self.item(slot, item, &Value::Null, true, out)?;
            }
            if self.incomplete_item {
                return Err("unfinished response output item".into());
            }
            if listed_calls > self.calls {
                return Err("missing function call from completed response".into());
            }
            if self
                .items
                .iter()
                .any(|item| item.call.as_ref().is_some_and(|c| c.index.is_none()))
            {
                return Err("function call missing name or ID".into());
            }
            if self.calls > 0 {
                "tool_calls"
            } else {
                "stop"
            }
        };
        let mut chunk = json!({"choices":[{"index":0,"delta":{},"finish_reason":reason}]});
        let usage = &response["usage"];
        if !usage.is_null() {
            let mut u = json!({
                "prompt_tokens":usage["input_tokens"].as_u64().ok_or("invalid input_tokens")?,
                "completion_tokens":usage["output_tokens"].as_u64().ok_or("invalid output_tokens")?
            });
            for (from, to) in [
                ("total_tokens", "total_tokens"),
                ("input_tokens_details", "prompt_tokens_details"),
                ("output_tokens_details", "completion_tokens_details"),
            ] {
                if !usage[from].is_null() {
                    u[to] = usage[from].clone();
                }
            }
            chunk["usage"] = u;
        }
        out.push(chunk);
        Ok(())
    }
}

pub fn failure(event: &Value) -> Option<(&Value, String)> {
    let (error, response) = if !event["error"].is_null() {
        (&event["error"], event)
    } else if event["type"] == "error" {
        (event, event)
    } else if matches!(
        event["type"].as_str(),
        Some("response.failed" | "response.completed" | "response.done" | "response.incomplete")
    ) && event["response"].is_object()
        && (event["type"] == "response.failed"
            || event["response"]["status"] == "failed"
            || !event["response"]["error"].is_null())
    {
        (&event["response"]["error"], &event["response"])
    } else {
        return None;
    };
    Some((error, provider_error(error, response)))
}

fn provider_error(error: &Value, response: &Value) -> String {
    format!(
        "openai response failed ({}): {} [response_id={}]",
        error["code"].as_str().unwrap_or("unknown"),
        error["message"].as_str().unwrap_or("unknown error"),
        response["id"].as_str().unwrap_or("unknown")
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stateless_request_replays_text_then_function_calls_and_results() {
        let schema = json!({"type":"object","additionalProperties":false,"properties":{"thought":{"type":"string"}},"required":["thought"]});
        let prompt = json!({"type":"object","properties":{"schema":{"anyOf":[{"type":"object","additionalProperties":false},{"type":"null"}]}}});
        let chat = json!({
            "model":"gpt-5", "stream":true, "stream_options":{"include_usage":true},
            "max_completion_tokens":350, "temperature":0, "top_p":0.8, "frequency_penalty":1, "presence_penalty":1,
            "parallel_tool_calls":false, "reasoning_effort":"high",
            "messages":[
                {"role":"system","content":"instructions"},{"role":"system","content":"tools"},
                {"role":"user","content":"hello"},{"role":"assistant","content":" ","reasoning_content":"hidden"},
                {"role":"assistant","content":"Checking.","tool_calls":[
                    {"type":"function","id":"call_a","function":{"name":"think","arguments":"{}"}}]},
                {"role":"tool","tool_call_id":"call_a","content":"Thoughts:\nHi"}],
            "tools":[
                {"type":"function","function":{"name":"think","description":"Think!","parameters":schema}},
                {"type":"function","function":{"name":"user_prompt","description":"Ask!","parameters":prompt}}]
        });
        assert_eq!(
            request(chat).unwrap(),
            json!({
                "model":"gpt-5","stream":true,"max_output_tokens":350,"temperature":0,"top_p":0.8,
                "parallel_tool_calls":false,"tool_choice":"auto","reasoning":{"summary":"detailed","effort":"high"},
                "input":[
                    {"role":"system","content":[{"type":"input_text","text":"instructions"}]},
                    {"role":"system","content":[{"type":"input_text","text":"tools"}]},
                    {"role":"user","content":"hello"},{"role":"assistant","content":"Checking."},
                    {"type":"function_call","call_id":"call_a","name":"think","arguments":"{}"},
                    {"type":"function_call_output","call_id":"call_a","output":"Thoughts:\nHi"}],
                "tools":[
                    {"type":"function","name":"think","description":"Think!","parameters":schema,"strict":true},
                    {"type":"function","name":"user_prompt","description":"Ask!","parameters":prompt,"strict":false}]
            })
        );
    }

    #[test]
    fn optional_parameters_do_not_change_plain_model_requests() {
        let mut chat = json!({"model":"gpt-4.1", "messages":[{"role":"user","content":"hi"}],
            "tools":[],"parallel_tool_calls":true,"max_tokens":32});
        assert_eq!(
            request(chat.clone()).unwrap(),
            json!({"model":"gpt-4.1","input":[{"role":"user","content":"hi"}],"max_output_tokens":32,"stream":true})
        );
        for (model, expected) in [
            (
                "openai/O3-mini",
                json!({"summary":"detailed","effort":"medium"}),
            ),
            ("gpt-5.1", json!({"summary":"detailed"})),
            ("gpt-6.2-pro", json!({"summary":"detailed"})),
            ("gpt-5-chat-latest", Value::Null),
            ("gpt-6abc", Value::Null),
        ] {
            chat["model"] = model.into();
            assert_eq!(request(chat.clone()).unwrap()["reasoning"], expected);
        }
        chat["messages"][0]["content"] = json!([{"type":"image_url"}]);
        assert!(request(chat).is_err());
    }

    fn completed(output: Value) -> Value {
        json!({"type":"response.completed","response":{"id":"resp", "status":"completed","output":output,
            "usage":{"input_tokens":8,"output_tokens":4,"total_tokens":12,
                "input_tokens_details":{"cached_tokens":1},"output_tokens_details":{"reasoning_tokens":2}}}})
    }

    #[test]
    fn copilot_snapshots_do_not_duplicate_deltas_or_hide_snapshot_only_parts() {
        let mut s = Stream::default();
        let parts =
            json!([{"type":"output_text","text":"Hi 🌍"},{"type":"output_text","text":"!"}]);
        let item = json!({"id":"msg_A","type":"message","role":"assistant","content":parts,"status":"completed"});
        let events = [
            json!({"type":"response.content_part.added","item_id":"msg_B","output_index":0,"content_index":0,"part":parts[0]}),
            json!({"type":"response.output_text.delta","item_id":"msg_C","output_index":0,"delta":"Hi "}),
            json!({"type":"response.output_text.done","item_id":"msg_C","output_index":0,"text":"Hi 🌍"}),
            json!({"type":"response.output_item.done","output_index":0,"item":item}),
            completed(json!([item])),
        ];
        let mut chunks = Vec::new();
        for event in events {
            chunks.extend(s.push(event).unwrap());
        }
        assert_eq!(chunks.len(), 4);
        for (c, text) in chunks.iter().zip(["Hi ", "🌍", "!"]) {
            assert_eq!(c["choices"][0]["delta"]["content"], text);
        }
        assert_eq!(chunks[3]["choices"][0]["finish_reason"], "stop");
        assert_eq!(
            chunks[3]["usage"],
            json!({"prompt_tokens":8,"completion_tokens":4,"total_tokens":12,
            "prompt_tokens_details":{"cached_tokens":1},"completion_tokens_details":{"reasoning_tokens":2}})
        );
        assert!(s.push(completed(json!([]))).is_err());
    }

    #[test]
    fn reasoning_summary_and_text_are_not_visible_content() {
        let mut s = Stream::default();
        let r = json!({"type":"reasoning","id":"r","summary":[{"type":"summary_text","text":"sum"}],"content":[{"type":"reasoning_text","text":"trace"}]});
        let mut chunks = Vec::new();
        for event in [
            json!({"type":"response.reasoning_summary_text.delta","item_id":"r","delta":"sum"}),
            json!({"type":"response.reasoning_text.delta","item_id":"r","delta":"tra"}),
            completed(
                json!([r,{"type":"message","id":"m","content":[{"type":"output_text","text":"answer"}]}]),
            ),
        ] {
            chunks.extend(s.push(event).unwrap());
        }
        assert_eq!(chunks.len(), 5);
        for (c, text) in chunks.iter().zip(["sum", "tra", "ce"]) {
            assert_eq!(
                c["choices"][0]["delta"],
                json!({"role":"assistant","reasoning_content":text})
            );
        }
        assert_eq!(chunks[3]["choices"][0]["delta"]["content"], "answer");
    }

    #[test]
    fn interleaved_calls_use_dense_indices_and_deliver_each_argument_byte_once() {
        let mut s = Stream::default();
        let a = json!({"type":"function_call","id":"fc_a","name":"think","call_id":"a"});
        let b = json!({"type":"function_call","id":"fc_b","name":"user_prompt","call_id":"b","arguments":"{}"});
        let events = [
            json!({"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"r"}}),
            json!({"type":"response.function_call_arguments.delta","item_id":"fc_a","delta":"{\"thought\":"}),
            json!({"type":"response.output_item.added","output_index":1,"item":a}),
            json!({"type":"response.output_item.added","output_index":2,"item":b}),
            json!({"type":"response.function_call_arguments.delta","item_id":"fc_a","delta":"\"a\""}),
            json!({"type":"response.function_call_arguments.done","item_id":"fc_a","arguments":"{\"thought\":\"a\"}"}),
            json!({"type":"response.function_call_arguments.delta","item_id":"fc_a","delta":"obsolete"}),
            json!({"type":"response.output_item.done","item":b}),
            completed(json!([{"type":"reasoning","id":"r"},a,b])),
        ];
        let mut chunks = Vec::new();
        for event in events {
            chunks.extend(s.push(event).unwrap());
        }
        assert_eq!(chunks.len(), 6);
        for (c, (index, id, name, bytes)) in chunks.iter().zip([
            (0, Some("a"), Some("think"), "{\"thought\":"),
            (1, Some("b"), Some("user_prompt"), ""),
            (0, None, None, "\"a\""),
            (0, None, None, "}"),
            (1, None, None, "{}"),
        ]) {
            let f = &c["choices"][0]["delta"]["tool_calls"][0];
            assert_eq!(f["index"], index);
            assert_eq!(f["id"].as_str(), id);
            assert_eq!(f["function"]["name"].as_str(), name);
            assert_eq!(f["function"]["arguments"], bytes);
        }
        assert_eq!(chunks[5]["choices"][0]["finish_reason"], "tool_calls");
    }

    #[test]
    fn done_only_arguments_and_type_only_terminal_marker() {
        let mut s = Stream::default();
        let item = json!({"type":"function_call","id":"fc","call_id":"c","name":"think"});
        assert!(s.push(json!({"type":"response.function_call_arguments.done","item_id":"fc","arguments":"{}"})).unwrap().is_empty());
        let c = s
            .push(json!({"type":"response.output_item.added","item":item}))
            .unwrap();
        assert_eq!(
            c[0]["choices"][0]["delta"]["tool_calls"][0]["function"],
            json!({"name":"think","arguments":"{}"})
        );
        assert!(s.push(json!({"type":"response.function_call_arguments.delta","item_id":"fc","delta":"late"})).unwrap().is_empty());
        let c = s
            .push(completed(json!([{"type":"function_call"}])))
            .unwrap();
        assert_eq!(c.len(), 1);
        assert_eq!(c[0]["choices"][0]["finish_reason"], "tool_calls");
    }

    #[test]
    fn only_terminal_response_can_finish_the_step() {
        let mut s = Stream::default();
        for event in [
            json!({"type":"response.created", "response":{"status":"in_progress"}}),
            json!({"type":"response.output_item.added", "item_id":"fc", "name":"think", "item":{"type":"function_call"}}),
            json!({"type":"response.function_call_arguments.done", "item_id":"fc", "arguments":"{}"}),
            json!({"type":"response.output_item.done", "item_id":"fc", "item":{"type":"function_call", "status":"completed"}}),
        ] {
            for c in s.push(event).unwrap() {
                assert!(c["choices"][0]["finish_reason"].is_null());
            }
        }
        let c = s
            .push(json!({"type":"response.done", "response":{"output":[]}}))
            .unwrap();
        assert_eq!(c[0]["choices"][0]["finish_reason"], "tool_calls");
    }

    #[test]
    fn incomplete_is_not_a_success_and_preserves_usage() {
        for (ty, reason, finish) in [
            ("response.incomplete", "max_output_tokens", "length"),
            ("response.done", "content_filter", "content_filter"),
        ] {
            let mut s = Stream::default();
            let c = s.push(json!({"type":ty,"response":{"status":"incomplete", "incomplete_details":{"reason":reason},"usage":{"input_tokens":1,"output_tokens":2}}})).unwrap();
            assert_eq!(c[0]["choices"][0]["finish_reason"], finish);
            assert_eq!(
                c[0]["usage"],
                json!({"prompt_tokens":1,"completion_tokens":2})
            );
        }
    }

    #[test]
    fn failed_refused_or_unsupported_outputs_cannot_commit_a_turn() {
        let failed = json!({"id":"r","status":"failed","error":{"code":"server_error","message":"upstream failed"}});
        for ty in ["response.failed", "response.completed", "response.done"] {
            assert!(Stream::default()
                .push(json!({"type":ty,"response":failed}))
                .unwrap_err()
                .contains("server_error): upstream failed [response_id=r]"));
        }
        for event in [
            json!({"type":"error","message":"failed"}),
            json!({"error":{"message":"failed"}}),
            json!({"type":"response.refusal.delta","delta":"no"}),
            json!({"type":"response.completed"}),
            json!({"type":"response.done","response":{"status":"in_progress"}}),
            completed(json!([{"type":"message","content":[{"type":"refusal","refusal":"no"}]}])),
            completed(json!([{"type":"function_call"}])),
            completed(json!([{"type":"web_search_call"}])),
            completed(
                json!([{"type":"function_call","id":"fc","call_id":"c","status":"incomplete"}]),
            ),
        ] {
            let mut s = Stream::default();
            assert!(s.push(event.clone()).is_err(), "{event}");
            assert!(s.push(completed(json!([]))).is_err());
        }
        let mut unfinished = Stream::default();
        unfinished.push(json!({"type":"response.output_item.done", "item":{"type":"message", "status":"incomplete"}})).unwrap();
        assert!(unfinished.push(completed(json!([]))).is_err());
        let mut s = Stream::default();
        s.push(json!({"type":"response.output_text.delta","item_id":"m","delta":"a"}))
            .unwrap();
        assert!(s
            .push(json!({"type":"response.output_text.done","item_id":"m","text":"b"}))
            .is_err());
    }
}
