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

pub fn request(
    mut chat: Value,
    output_tool: Option<&crate::structured_output::OutputTool>,
) -> Result<Value, String> {
    let model = string(&chat, "model")?;
    let mut input = Vec::new();
    for m in array(&chat, "messages")? {
        let role = string(m, "role")?;
        match role {
            "system" | "user" => {
                match &m["content"] {
                    Value::String(s) if role == "system" => input.push(json!({"role":role,"content":[{"type":"input_text","text":s}]})),
                    // media::history supplies protocol-native arrays, not Chat Completions parts.
                    content @ (Value::Array(_) | Value::String(_)) => input.push(json!({"role":role,"content":content})),
                    _ => return Err("invalid message content".into()),
                }
            }
            "assistant" => {
                // The default Go SSE path doesn't replay reasoning_content or opaque provider state.
                if !m["content"].is_null() {
                    let text = string(m, "content")?;
                    if !text.trim().is_empty() || (m["tool_calls"].is_null() && m["reasoning_content"].is_null()) {
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
                "type":"function_call_output", "call_id":string(m,"tool_call_id")?, "output":m["content"]
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

    if let Some(format) = chat.get_mut("response_format") {
        if format["type"] != "json_schema" || !format["json_schema"].is_object() {
            return Err("unsupported response_format".into());
        }
        let mut schema = format["json_schema"].take();
        schema["type"] = "json_schema".into();
        body["text"]["format"] = schema;
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
        let strict = output_tool
            .filter(|_| name == crate::structured_output::NAME)
            .map_or_else(|| crate::turn::response_strict(name), |t| t.strict);
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
struct Call {
    index: Option<usize>,
    id: String,
    name: String,
    args: String,
    final_args: bool,
}

impl Call {
    fn announce(
        &mut self,
        next: &mut usize,
        id: &str,
        name: &str,
        out: &mut Vec<Value>,
    ) -> Result<(), String> {
        if id.is_empty() || name.is_empty() {
            return Err("function call missing name or ID".into());
        }
        if self.index.is_some() {
            if self.id != id || self.name != name {
                return Err("function call identity changed".into());
            }
            return Ok(());
        }
        let index = *next;
        *next += 1;
        self.index = Some(index);
        self.id = id.into();
        self.name = name.into();
        delta(
            out,
            json!({"tool_calls":[{"index":index,"id":id,"type":"function",
            "function":{"name":name,"arguments":self.args}}]}),
        );
        Ok(())
    }

    fn check_identity(&self, item: &Value) -> Result<(), String> {
        if nonempty(item, "name").is_some_and(|s| s != self.name)
            || nonempty(item, "call_id").is_some_and(|s| s != self.id)
        {
            return Err("function call identity changed".into());
        }
        Ok(())
    }

    fn arguments(
        &mut self,
        args: &str,
        snapshot: bool,
        out: &mut Vec<Value>,
    ) -> Result<(), String> {
        if args.is_empty() || (!snapshot && self.final_args) {
            return Ok(());
        }
        if snapshot {
            self.final_args = true;
            if self.index.is_none() {
                // Nothing has been emitted: .done replaces the entire pending buffer.
                self.args.clear();
            } else if !self.args.is_empty() {
                if self.args != args {
                    return Err("Responses snapshot contradicts streamed arguments".into());
                }
                return Ok(());
            }
        }
        self.args.push_str(args);
        if let Some(index) = self.index {
            delta(
                out,
                json!({"tool_calls":[{"index":index,"function":{"arguments":args}}]}),
            );
        }
        Ok(())
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum ItemEvent {
    Added,
    Done,
    Terminal,
}

#[derive(Default)]
pub struct Stream {
    // Text snapshots may change item ID while retaining the same output slot.
    parts: Vec<BTreeMap<u64, bool>>,
    by_id: BTreeMap<String, usize>,
    by_index: BTreeMap<u64, usize>,
    // Function arguments, unlike text, are routed strictly by item_id in Go.
    functions: BTreeMap<String, Call>,
    calls: usize,
    // Unknown events and unfinished messages are harmless only on a text-only step.
    ambiguous_output: bool,
    incomplete_item: bool,
    terminal: bool,
}

fn delta(out: &mut Vec<Value>, value: Value) {
    out.push(json!({"choices":[{"index":0,"delta":value}]}));
}

fn nonempty<'a>(obj: &'a Value, key: &str) -> Option<&'a str> {
    obj[key].as_str().filter(|s| !s.is_empty())
}

impl Stream {
    fn text(
        &mut self,
        id: Option<&str>,
        index: Option<u64>,
        part: u64,
        text: &str,
        snapshot: bool,
        out: &mut Vec<Value>,
    ) -> Result<(), String> {
        if text.is_empty() {
            return Ok(());
        }
        let index = index.or(id.is_none().then_some(0));
        let known_id = id.and_then(|id| self.by_id.get(id).copied());
        let known_index = index.and_then(|i| self.by_index.get(&i).copied());
        if matches!((known_id,known_index),(Some(a),Some(b)) if a != b) {
            return Err("conflicting response item identifiers".into());
        }
        let slot = known_index.or(known_id).unwrap_or_else(|| {
            self.parts.push(Default::default());
            self.parts.len() - 1
        });
        if let Some(id) = id.filter(|_| known_id.is_none()) {
            self.by_id.insert(id.into(), slot);
        }
        if let Some(i) = index.filter(|_| known_index.is_none()) {
            self.by_index.insert(i, slot);
        }
        if let Some(was_snapshot) = self.parts[slot].get(&part) {
            if snapshot {
                return Ok(());
            }
            if *was_snapshot {
                return Err("text delta after Responses snapshot".into());
            }
        }
        self.parts[slot].insert(part, snapshot);
        delta(out, json!({"role":"assistant","content":text}));
        Ok(())
    }

    fn item(
        &mut self,
        item: &Value,
        header: &Value,
        index: Option<u64>,
        source: ItemEvent,
        out: &mut Vec<Value>,
    ) -> Result<(), String> {
        let id = nonempty(item, "id");
        let header_id = nonempty(header, "item_id");
        if source != ItemEvent::Added && !item["status"].is_null() && item["status"] != "completed"
        {
            if item["type"] == "message" && item["status"] == "in_progress" {
                self.ambiguous_output = true;
            } else {
                self.incomplete_item = true;
            }
        }
        match string(item, "type")? {
            "function_call" => {
                if source == ItemEvent::Added {
                    let key = id.or(header_id).ok_or("function call missing item ID")?;
                    let call_id = nonempty(item, "call_id").unwrap_or(key);
                    let name = nonempty(header, "name")
                        .or(nonempty(item, "name"))
                        .unwrap_or_default();
                    let call = self.functions.entry(key.into()).or_default();
                    return call.announce(&mut self.calls, call_id, name, out);
                }
                let call = header_id
                    .or(id)
                    .and_then(|key| self.functions.get_mut(key))
                    .filter(|c| c.index.is_some());
                let Some(call) = call else {
                    // Terminal output selects a finish reason but never announces a tool.
                    return if source == ItemEvent::Terminal {
                        Ok(())
                    } else {
                        Err("unannounced function call snapshot".into())
                    };
                };
                call.check_identity(item)?;
                if let Some(args) = nonempty(item, "arguments") {
                    // Only item.done can supply missing bytes. It doesn't finalize prior deltas.
                    if source == ItemEvent::Done && call.args.is_empty() {
                        call.arguments(args, true, out)?;
                    } else if args != call.args {
                        return Err("function call snapshot contradicts streamed arguments".into());
                    }
                }
            }
            "message" | "reasoning" => {
                if !item["role"].is_null() && item["role"] != "assistant" {
                    return Err("non-assistant response item".into());
                }
                for (i, part) in array(item, "content")?.iter().enumerate() {
                    match part["type"].as_str() {
                        Some("refusal") => return Err("model refused the request".into()),
                        Some("text" | "output_text")
                            if source != ItemEvent::Added && item["type"] == "message" =>
                        {
                            self.text(
                                header_id.or(id),
                                index,
                                i as u64,
                                string(part, "text")?,
                                true,
                                out,
                            )?;
                        }
                        Some("reasoning_text" | "text" | "output_text") => (),
                        _ => return Err("unsupported Responses content part".into()),
                    }
                }
            }
            // This is server-side activity, never a request to dispatch a local tool.
            "web_search_call" => (),
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
        if event["type"] == "response.failed"
            || event["type"] == "error"
            || !event["error"].is_null()
        {
            return Err("Responses provider error".into());
        }
        let ty = string(event, "type")?;
        let mut out = Vec::new();
        let id = nonempty(event, "item_id");
        let index = event["output_index"].as_u64();
        let part = &event["part"];
        if part["type"] == "refusal" {
            return Err("model refused the request".into());
        }
        match ty {
            "response.completed" | "response.done" | "response.incomplete" => {
                self.finish(ty, &event["response"], &mut out)?
            }
            "response.refusal.delta" | "response.refusal.done" => {
                return Err("model refused the request".into())
            }
            "response.cancelled" => return Err(format!("unexpected Responses stream event: {ty}")),
            "response.output_item.added" | "response.output_item.done" => self.item(
                &event["item"],
                event,
                index,
                if ty.ends_with(".done") {
                    ItemEvent::Done
                } else {
                    ItemEvent::Added
                },
                &mut out,
            )?,
            "response.output_text.delta"
            | "response.content_part.delta"
            | "response.output_text.done"
            | "response.content_part.done" => {
                let text = match ty {
                    "response.output_text.delta" => {
                        nonempty(event, "delta").or(nonempty(event, "text"))
                    }
                    "response.content_part.delta" => nonempty(event, "delta")
                        .or(nonempty(event, "text"))
                        .or(nonempty(event, "code"))
                        .or(nonempty(part, "text")),
                    "response.content_part.done"
                        if matches!(part["type"].as_str(), Some("output_text" | "text")) =>
                    {
                        nonempty(part, "text")
                    }
                    "response.output_text.done" => nonempty(event, "text"),
                    _ => None,
                };
                if let Some(text) = text {
                    self.text(
                        id,
                        index,
                        event["content_index"].as_u64().unwrap_or(0),
                        text,
                        ty.ends_with(".done"),
                        &mut out,
                    )?;
                }
            }
            "response.reasoning_text.delta" | "response.reasoning_summary_text.delta" => {
                if let Some(fragment) = nonempty(event, "delta") {
                    delta(
                        &mut out,
                        json!({"role":"assistant","reasoning_content":fragment}),
                    );
                }
            }
            "response.function_call_arguments.delta" | "response.function_call_arguments.done" => {
                let done = ty.ends_with(".done");
                let args = if done { None } else { nonempty(event, "delta") }
                    .or(nonempty(event, "arguments"));
                if let Some(args) = args {
                    let key = id.ok_or("arguments missing item ID")?;
                    self.functions
                        .entry(key.into())
                        .or_default()
                        .arguments(args, done, &mut out)?;
                }
            }
            "response.created"
            | "response.queued"
            | "response.in_progress"
            | "response.content_part.added"
            | "response.output_text.annotation.added"
            | "response.reasoning_summary_text.done"
            | "response.reasoning_text.done"
            | "response.reasoning_summary_part.added"
            | "response.reasoning_summary_part.done" => (),
            _ => self.ambiguous_output = true,
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
        if status == Some("failed") || !response["error"].is_null() {
            return Err("failed terminal response".into());
        }
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
            if status.is_some_and(|s| s != "completed") {
                return Err("unexpected terminal Responses status".into());
            }
            let output = array(response, "output")?;
            for (i, item) in output.iter().enumerate() {
                self.item(item, &Value::Null, Some(i as u64), ItemEvent::Terminal, out)?;
            }
            if self.incomplete_item || (self.ambiguous_output && self.calls > 0) {
                return Err("unfinished or unsupported response output".into());
            }

            if self.functions.values().any(|c| c.index.is_none()) {
                return Err("function call missing announcement".into());
            }
            if output.iter().any(|item| item["type"] == "function_call") {
                "tool_calls"
            } else {
                "stop"
            }
        };
        let mut chunk = json!({"choices":[{"index":0,"delta":{},"finish_reason":reason}]});
        let usage = &response["usage"];
        if !usage.is_null() && !usage.is_object() {
            return Err("invalid Responses usage".into());
        }
        let count = |obj: &Value, key: &str| match &obj[key] {
            Value::Null => Ok(0),
            n => n.as_u64().ok_or_else(|| format!("invalid {key}")),
        };
        let total = match &usage["total_tokens"] {
            Value::Null => 0,
            n => n.as_i64().ok_or("invalid total_tokens")?,
        };
        if total > 0 {
            let prompt = count(usage, "input_tokens")?;
            let details = &usage["input_tokens_details"];
            let reads = count(details, "cached_tokens")?;
            let writes = count(details, "cache_write_tokens")?;
            if reads
                .checked_add(writes)
                .is_none_or(|cached| cached > prompt)
            {
                return Err("cached token counts exceed input_tokens".into());
            }
            chunk["usage"] =
                json!({"prompt_tokens":prompt,"completion_tokens":count(usage,"output_tokens")?});
            for (from, to) in [
                ("input_tokens_details", "prompt_tokens_details"),
                ("output_tokens_details", "completion_tokens_details"),
            ] {
                if !usage[from].is_null() {
                    chunk["usage"][to] = usage[from].clone();
                }
            }
        }
        out.push(chunk);
        Ok(())
    }
}

pub fn failure(event: &Value) -> Option<String> {
    (event["type"] == "response.failed").then(|| {
        let response = &event["response"];
        let error = &response["error"];
        format!(
            "openai response failed ({}): {} [response_id={}]",
            error["code"].as_str().unwrap_or_default(),
            error["message"].as_str().unwrap_or_default(),
            response["id"].as_str().unwrap_or_default()
        )
    })
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
            request(chat, None).unwrap(),
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
            request(chat.clone(), None).unwrap(),
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
            assert_eq!(request(chat.clone(), None).unwrap()["reasoning"], expected);
        }
        chat["messages"][0]["content"] = json!({"type":"image_url"});
        assert!(request(chat, None).is_err());
    }

    #[test]
    fn native_format_uses_text_instead_of_chat_response_format() {
        let schema = json!({"type":"object", "properties":{"codes":{"type":"array","items":{"type":"string"}}}, "required":["codes"], "additionalProperties":false});
        let chat = json!({"model":"test", "messages":[{"role":"user","content":"hi"}], "response_format":{
            "type":"json_schema", "json_schema":{"name":"result","description":"Status codes","strict":true,"schema":schema}}});
        let response = request(chat, None).unwrap();
        assert!(response.get("response_format").is_none());
        assert_eq!(
            response["text"],
            json!({"format":{"type":"json_schema", "name":"result", "description":"Status codes", "strict":true,"schema":schema}})
        );
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
        assert_eq!(chunks.len(), 3);
        for (c, text) in chunks.iter().zip(["Hi ", "!"]) {
            assert_eq!(c["choices"][0]["delta"]["content"], text);
        }
        assert_eq!(chunks[2]["choices"][0]["finish_reason"], "stop");
        assert_eq!(
            chunks[2]["usage"],
            json!({"prompt_tokens":8,"completion_tokens":4,
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
        assert_eq!(chunks.len(), 4);
        for (c, text) in chunks.iter().zip(["sum", "tra"]) {
            assert_eq!(
                c["choices"][0]["delta"],
                json!({"role":"assistant","reasoning_content":text})
            );
        }
        assert_eq!(chunks[2]["choices"][0]["delta"]["content"], "answer");
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
            json!({"type":"response.function_call_arguments.delta","item_id":"fc_a","delta":"\"a\"}"}),
            json!({"type":"response.function_call_arguments.done","item_id":"fc_a","arguments":"{\"thought\":\"a\"}"}),
            json!({"type":"response.function_call_arguments.delta","item_id":"fc_a","delta":"obsolete"}),
            json!({"type":"response.output_item.done","item":b}),
            completed(json!([{"type":"reasoning","id":"r"},a,b])),
        ];
        let mut chunks = Vec::new();
        for event in events {
            chunks.extend(s.push(event).unwrap());
        }
        assert_eq!(chunks.len(), 5);
        for (c, (index, id, name, bytes)) in chunks.iter().zip([
            (0, Some("a"), Some("think"), "{\"thought\":"),
            (1, Some("b"), Some("user_prompt"), ""),
            (0, None, None, "\"a\"}"),
            (1, None, None, "{}"),
        ]) {
            let f = &c["choices"][0]["delta"]["tool_calls"][0];
            assert_eq!(f["index"], index);
            assert_eq!(f["id"].as_str(), id);
            assert_eq!(f["function"]["name"].as_str(), name);
            assert_eq!(f["function"]["arguments"], bytes);
        }
        assert_eq!(chunks[4]["choices"][0]["finish_reason"], "tool_calls");
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
            .push(json!({"type":"response.done", "response":{"output":[{"type":"function_call"}]}}))
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
            let c = s.push(json!({"type":ty,"response":{"status":"incomplete", "incomplete_details":{"reason":reason},"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}})).unwrap();
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
        assert_eq!(
            failure(&json!({"type":"response.failed","response":failed})).unwrap(),
            "openai response failed (server_error): upstream failed [response_id=r]"
        );
        for event in [
            json!({"type":"response.failed","response":failed}),
            json!({"type":"response.completed","response":failed}),
            json!({"type":"response.done","response":failed}),
            json!({"type":"error","message":"failed"}),
            json!({"error":{"message":"failed"}}),
            json!({"type":"response.refusal.delta","delta":"no"}),
            json!({"type":"response.completed"}),
            json!({"type":"response.done","response":{"status":"in_progress"}}),
            completed(json!([{"type":"message","content":[{"type":"refusal","refusal":"no"}]}])),
            completed(json!([{"type":"unknown_action"}])),
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
    }

    #[test]
    fn only_added_can_announce_a_call() {
        let item = json!({"type":"function_call","id":"item","call_id":"call","name":"think","arguments":"{}"});
        let c = Stream::default().push(completed(json!([item]))).unwrap();
        assert_eq!(c.len(), 1);
        assert_eq!(c[0]["choices"][0]["delta"], json!({}));
        assert_eq!(c[0]["choices"][0]["finish_reason"], "tool_calls");
        assert!(Stream::default()
            .push(json!({"type":"response.output_item.done","item":item}))
            .is_err());
    }

    #[test]
    fn snapshots_cannot_complete_or_change_emitted_arguments() {
        let item = json!({"id":"fc","name":"think","type":"function_call"});
        for snapshot in [
            json!({"type":"response.function_call_arguments.done","item_id":"fc","arguments":"{}"}),
            json!({"type":"response.output_item.done","item": {"type":"function_call","id":"fc","arguments":"{}"}}),
            completed(json!([{"type":"function_call","id":"fc","arguments":"{}"}])),
        ] {
            let mut s = Stream::default();
            s.push(json!({"type":"response.output_item.added","item":item}))
                .unwrap();
            s.push(
                json!({"type":"response.function_call_arguments.delta","item_id":"fc","delta":"{"}),
            )
            .unwrap();
            assert!(s.push(snapshot).is_err());
        }
    }

    #[test]
    fn terminal_usage_requires_a_positive_total_not_both_counts() {
        for (usage, expected) in [
            (json!({}), Value::Null),
            (json!({"total_tokens":-3}), Value::Null),
            (json!({"input_tokens":8,"output_tokens":3}), Value::Null),
            (json!({"input_tokens":8,"total_tokens":0}), Value::Null),
            (
                json!({"total_tokens":4,"output_tokens":4}),
                json!({"prompt_tokens":0,"completion_tokens":4}),
            ),
            (
                json!({"total_tokens":7}),
                json!({"prompt_tokens":0,"completion_tokens":0}),
            ),
        ] {
            let c = Stream::default()
                .push(json!({"type":"response.completed","response":{"usage":usage}}))
                .unwrap();
            assert_eq!(c[0]["usage"], expected);
        }
    }
}
