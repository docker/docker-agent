use serde::Deserialize;
use serde_json::{json, Map, Value};
use std::collections::{BTreeMap, BTreeSet};

const MAX_TOOL_ROUNDS: usize = 8;
const MAX_PARALLEL_CALLS: u64 = 16;
// Every think result repeats the scratchpad and becomes part of the next request.
const MAX_SCRATCHPAD_BYTES: usize = 64 * 1024;

pub struct Conversation {
    model_params: Map<String, Value>,
    think: bool,
    pub merge_messages: bool,
    messages: Vec<Value>,
    scratchpad: String,
    active: Option<Turn>,
}

struct Turn {
    messages_before: usize,
    scratchpad_before: usize,
    id: String,
    message_id: String,
    tool_rounds: usize,
    input: u64,
    output: u64,
    step: Step,
}

#[derive(Default)]
struct Step {
    text: String,
    reasoning: String,
    calls: BTreeMap<u64, Call>,
    finish: Option<String>,
    usage: Option<Usage>,
}

#[derive(Deserialize)]
struct Usage {
    prompt_tokens: u32,
    completion_tokens: u32,
}

#[derive(Default)]
struct Call {
    id: String,
    name: String,
    arguments: String,
    announced: bool,
}

impl Conversation {
    pub fn new(
        model_params: Map<String, Value>,
        instruction: String,
        think: bool,
        mut history: Vec<Value>,
    ) -> Result<Self, String> {
        validate_history(&history)?;
        if !instruction.is_empty() {
            history.insert(0, json!({"role":"system", "content":instruction}));
        }
        Ok(Self {
            model_params,
            think,
            merge_messages: false,
            messages: history,
            scratchpad: "Thoughts:".into(),
            active: None,
        })
    }

    pub fn begin(&mut self, prompt: &str, id: &str) -> Result<Value, String> {
        if self.active.is_some() {
            return Err("a run is already active".into());
        }
        if id.is_empty() {
            return Err("missing message ID".into());
        }
        self.active = Some(Turn {
            messages_before: self.messages.len(),
            scratchpad_before: self.scratchpad.len(),
            id: id.into(),
            message_id: id.into(),
            tool_rounds: 0,
            input: 0,
            output: 0,
            step: Step::default(),
        });
        self.messages.push(json!({"role":"user", "content":prompt}));
        Ok(self.request())
    }

    fn request(&self) -> Value {
        let mut body = Value::Object(self.model_params.clone());
        body["messages"] = json!(self.messages);
        body["stream"] = true.into();
        body["stream_options"] = json!({"include_usage": true});
        if self.think {
            body["tools"] = json!([{"type":"function", "function":{
                "name":"think", "description":"Append a thought to the conversation scratchpad. This obtains no new information and changes no external data.",
                "parameters":{"type":"object", "properties":{"thought":{"type":"string"}}, "required":["thought"], "additionalProperties":false}
            }}]);
        }
        if self.merge_messages {
            crate::wire::merge_text_messages(&mut body);
        }
        body
    }

    pub fn chunk(&mut self, chunk: Value) -> Result<Vec<Value>, String> {
        let turn = self.active.as_mut().ok_or("no active turn")?;
        if let Some(error) = chunk.get("error").filter(|e| !e.is_null()) {
            // Go has already admitted this user message (and completed tool steps).
            // A provider error discards the unfinished response, not that history.
            self.active = None;
            return Err(format!("model failed: error receiving from stream: received error while streaming: {error}"));
        }
        let result = turn.step.chunk(chunk, &turn.message_id, self.think);
        if result.is_err() {
            self.cancel();
        }
        result
    }

    pub fn cancel(&mut self) {
        if let Some(turn) = self.active.take() {
            self.messages.truncate(turn.messages_before);
            self.scratchpad.truncate(turn.scratchpad_before);
        }
    }

    pub fn end(&mut self) -> Result<Value, String> {
        let result = self.finish_step();
        if result.is_err() {
            self.cancel();
        }
        result
    }

    fn finish_step(&mut self) -> Result<Value, String> {
        let turn = self.active.as_mut().ok_or("no active turn")?;
        let step = std::mem::take(&mut turn.step);
        let mut events = Vec::new();
        if let Some(usage) = step.usage {
            let input = u64::from(usage.prompt_tokens);
            let output = u64::from(usage.completion_tokens);
            turn.input += input;
            turn.output += output;
            events.push(json!({
                "type": "usage", "input_tokens": input, "output_tokens": output,
                "context_length": input + output, "context_limit": 0, "cost": 0
            }));
        }
        let mut message = json!({"role":"assistant"});
        message["content"] = Value::String(step.text);
        if !step.reasoning.is_empty() {
            message["reasoning_content"] = Value::String(step.reasoning);
        }
        match step.finish.as_deref() {
            None => Err("incomplete stream: missing finish_reason".into()),
            Some("stop") if step.calls.is_empty() => {
                let mut result =
                    json!({"message":{"role":"assistant", "content":message["content"]}});
                self.messages.push(message);
                if turn.input != 0 || turn.output != 0 {
                    result["usage"] =
                        json!({"input_tokens":turn.input, "output_tokens":turn.output});
                }
                events.push(json!({"type":"finish", "reason":"stop"}));
                self.active = None;
                Ok(json!({"events":events, "result":result}))
            }
            Some("tool_calls") if !step.calls.is_empty() => {
                if turn.tool_rounds >= MAX_TOOL_ROUNDS {
                    return Err("tool iteration limit reached".into());
                }
                turn.tool_rounds += 1;
                turn.message_id = format!("{}:{}", turn.id, turn.tool_rounds);
                self.execute_calls(step.calls, message, &mut events)?;
                Ok(json!({"events":events, "request":self.request()}))
            }
            Some(reason) => Err(format!(
                "incomplete or unsupported model response (finish_reason: {reason})"
            )),
        }
    }

    fn execute_calls(
        &mut self,
        calls: BTreeMap<u64, Call>,
        mut message: Value,
        events: &mut Vec<Value>,
    ) -> Result<(), String> {
        let mut ids = BTreeSet::new();
        let mut wire_calls = Vec::with_capacity(calls.len());
        let mut replies = Vec::with_capacity(calls.len());
        for (index, (key, call)) in calls.iter().enumerate() {
            if *key != index as u64 || call.id.is_empty() || !ids.insert(&call.id) {
                return Err("missing or duplicate tool call ID or index".into());
            }
            let thought = call.thought()?;
            if self.scratchpad.len() + 1 + thought.len() > MAX_SCRATCHPAD_BYTES {
                return Err("think scratchpad size limit reached".into());
            }
            self.scratchpad.push('\n');
            self.scratchpad.push_str(&thought);
            events.push(
                json!({"type":"tool_call", "id":call.id, "name":call.name, "args":call.arguments}),
            );
            events.push(json!({"type":"tool_result", "id":call.id, "name":call.name, "output":self.scratchpad, "is_error":false}));
            wire_calls.push(json!({"id":call.id, "type":"function", "function":{"name":call.name, "arguments":call.arguments}}));
            replies.push(json!({"role":"tool", "tool_call_id":call.id, "content":self.scratchpad}));
        }
        message["tool_calls"] = Value::Array(wire_calls);
        self.messages.push(message);
        self.messages.extend(replies);
        Ok(())
    }
}

impl Step {
    fn chunk(&mut self, chunk: Value, id: &str, think: bool) -> Result<Vec<Value>, String> {
        if !chunk["usage"].is_null() {
            if !chunk["usage"].is_object() {
                return Err("invalid token usage".into());
            }
            // Some providers stream cumulative snapshots instead of a single usage trailer.
            self.usage = Some(
                Usage::deserialize(&chunk["usage"])
                    .map_err(|e| format!("invalid token usage: {e}"))?,
            );
        }
        let mut events = Vec::new();
        let choices = chunk["choices"]
            .as_array()
            .ok_or("missing stream choices")?;
        let choice = match choices.as_slice() {
            [] => return Ok(events), // usage-only or prompt annotations
            [choice] if choice["index"].as_u64() == Some(0) || choice.get("index").is_none() => {
                choice
            }
            _ => return Err("expected a single stream choice at index 0".into()),
        };
        if self.finish.is_some() {
            return Err("model sent a choice after finish_reason".into());
        }
        let delta = &choice["delta"];
        if !delta.is_object() || string(delta, "role")?.is_some_and(|role| role != "assistant") {
            return Err("invalid assistant delta".into());
        }
        if !delta["function_call"].is_null()
            || string(delta, "refusal")?.is_some_and(|r| !r.is_empty())
        {
            return Err("refusal or legacy function_call is not supported".into());
        }
        let reasoning = string(delta, "reasoning_content")?
            .filter(|s| !s.is_empty())
            .or(string(delta, "reasoning")?);
        if let Some(s) = reasoning.filter(|s| !s.is_empty()) {
            self.reasoning.push_str(s);
            events.push(json!({"type":"delta", "message_id":id, "reasoning":s}));
        }
        if let Some(s) = string(delta, "content")?.filter(|s| !s.is_empty()) {
            self.text.push_str(s);
            events.push(json!({"type":"delta", "message_id":id, "content":s}));
        }
        if !delta["tool_calls"].is_null() {
            for fragment in delta["tool_calls"]
                .as_array()
                .ok_or("expected tool_calls array")?
            {
                if !think {
                    return Err("tool calls are disabled".into());
                }
                let index = fragment["index"]
                    .as_u64()
                    .filter(|n| *n < MAX_PARALLEL_CALLS)
                    .ok_or("invalid tool call index")?;
                if let Some(event) = self.calls.entry(index).or_default().append(fragment)? {
                    events.push(event);
                }
            }
        }
        self.finish = string(choice, "finish_reason")?.map(str::to_owned);
        Ok(events)
    }
}

impl Call {
    fn append(&mut self, fragment: &Value) -> Result<Option<Value>, String> {
        if string(fragment, "type")?.is_some_and(|t| t != "function") {
            return Err("unsupported tool call type".into());
        }
        if let Some(id) = string(fragment, "id")?.filter(|s| !s.is_empty()) {
            if self.id.is_empty() {
                self.id = id.into();
            } else if self.id != id {
                return Err("tool call ID changed while streaming".into());
            }
        }
        let f = &fragment["function"];
        if !f.is_null() && !f.is_object() {
            return Err("invalid function delta".into());
        }
        if let Some(name) = string(f, "name")? {
            self.name.push_str(name);
            if !"think".starts_with(&self.name) {
                return Err("unsupported tool name; only think is available".into());
            }
        }
        let args = string(f, "arguments")?.unwrap_or_default();
        self.arguments.push_str(args);
        if self.id.is_empty() || self.name != "think" || (self.announced && args.is_empty()) {
            return Ok(None);
        }
        // Arguments can arrive before the name or ID. Flush them once a card can be identified.
        let arguments = if self.announced {
            args
        } else {
            &self.arguments
        };
        self.announced = true;
        Ok(Some(
            json!({"type":"tool_call_delta", "id":self.id, "name":self.name, "arguments":arguments}),
        ))
    }

    fn thought(&self) -> Result<String, String> {
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Args {
            thought: String,
        }

        if self.name != "think" {
            return Err("missing or invalid tool name".into());
        }
        // serde structs also deserialize from positional arrays, which our tool schema forbids.
        if !self.arguments.trim_start().starts_with('{') {
            return Err("think arguments must be a JSON object".into());
        }
        serde_json::from_str::<Args>(&self.arguments)
            .map(|a| a.thought)
            .map_err(|e| format!("invalid think arguments: {e}"))
    }
}

fn string<'a>(object: &'a Value, key: &str) -> Result<Option<&'a str>, String> {
    match &object[key] {
        Value::Null => Ok(None),
        Value::String(s) => Ok(Some(s)),
        _ => Err(format!("expected a string for {key}")),
    }
}

fn validate_history(history: &[Value]) -> Result<(), String> {
    let mut pending = BTreeSet::new();
    for message in history {
        let role = message["role"].as_str().ok_or("missing history role")?;
        if role == "tool" {
            let id = message["tool_call_id"]
                .as_str()
                .ok_or("missing tool_call_id in history")?;
            if !pending.remove(id) {
                return Err("unexpected tool result in history".into());
            }
        } else if !pending.is_empty() {
            return Err("missing tool results in history".into());
        }
        if !message["tool_calls"].is_null() {
            if role != "assistant" {
                return Err("only assistant messages can have tool_calls".into());
            }
            for call in message["tool_calls"]
                .as_array()
                .ok_or("invalid tool_calls history")?
            {
                let id = call["id"]
                    .as_str()
                    .filter(|s| !s.is_empty())
                    .ok_or("missing tool ID in history")?;
                if !pending.insert(id)
                    || call["type"] != "function"
                    || call["function"]["name"].as_str().is_none_or(str::is_empty)
                    || !call["function"]["arguments"].is_string()
                {
                    return Err("invalid historical function call".into());
                }
            }
        }
        if !matches!(role, "user" | "assistant" | "system" | "developer" | "tool")
            || !(message["content"].is_string()
                || (!pending.is_empty() && role == "assistant" && message["content"].is_null()))
        {
            return Err("invalid text message in history".into());
        }
    }
    if !pending.is_empty() {
        return Err("missing tool results in history".into());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn model(name: &str) -> Map<String, Value> {
        Map::from_iter([("model".into(), name.into())])
    }

    fn conversation(think: bool) -> Conversation {
        Conversation::new(model("model"), "be concise".into(), think, vec![]).unwrap()
    }

    fn chunk(delta: Value, reason: Option<&str>) -> Value {
        json!({"choices":[{"index":0, "delta":delta, "finish_reason":reason}]})
    }

    fn call(index: u64, id: &str, name: &str, arguments: &str) -> Value {
        json!({"index":index, "id":id, "type":"function", "function":{"name":name, "arguments":arguments}})
    }

    fn think_once(c: &mut Conversation, text: &str) -> Value {
        let args = json!({"thought":text}).to_string();
        let calls = [call(0, "one", "think", &args)];
        c.chunk(chunk(json!({"tool_calls":calls}), Some("tool_calls")))
            .unwrap();
        c.end().unwrap()
    }

    fn answer(c: &mut Conversation, text: &str) -> Value {
        c.chunk(chunk(json!({"content":text}), Some("stop")))
            .unwrap();
        c.end().unwrap()
    }

    #[test]
    fn replay_history_and_stream_separate_text_and_reasoning() {
        let history = vec![
            json!({"role":"user", "content":"before"}),
            json!({"role":"assistant", "tool_calls":[{"id":"old", "type":"function", "function":{"name":"old-tool", "arguments":"{}"}}]}),
            json!({"role":"tool", "tool_call_id":"old", "content":"prior output"}),
        ];
        let mut c = Conversation::new(model("test"), "sys".into(), false, history.clone()).unwrap();
        let request = c.begin("hello", "turn-id").unwrap();
        assert_eq!(
            request["messages"][0],
            json!({"role":"system", "content":"sys"})
        );
        assert_eq!(&request["messages"].as_array().unwrap()[1..4], history);
        assert_eq!(
            request["messages"][4],
            json!({"role":"user", "content":"hello"})
        );
        assert_eq!(request["model"], "test");
        assert_eq!(request["stream"], true);
        assert_eq!(request["stream_options"]["include_usage"], true);
        assert!(request.get("tools").is_none());
        assert!(c.begin("ignored", "other").is_err());
        assert_eq!(
            c.chunk(chunk(json!({"reasoning":"hmm", "content":"Hi"}), None))
                .unwrap(),
            vec![
                json!({"type":"delta", "message_id":"turn-id", "reasoning":"hmm"}),
                json!({"type":"delta", "message_id":"turn-id", "content":"Hi"}),
            ]
        );
        let done = answer(&mut c, " 🌍");
        assert_eq!(
            done,
            json!({"result":{"message":{"role":"assistant", "content":"Hi 🌍"}}, "events":[{"type":"finish", "reason":"stop"}]})
        );
        let request = c.begin("again", "next").unwrap();
        assert_eq!(
            request["messages"][5],
            json!({"role":"assistant", "content":"Hi 🌍", "reasoning_content":"hmm"})
        );
    }

    #[test]
    fn rejects_incomplete_tool_history_before_requesting_a_completion() {
        let assistant = json!({"role":"assistant", "tool_calls":[{"id":"a", "type":"function", "function":{"name":"think", "arguments":"{}"}}]});
        let tool = json!({"role":"tool", "tool_call_id":"a", "content":"prior"});
        for history in [
            vec![tool.clone()],
            vec![assistant.clone()],
            vec![assistant, tool.clone(), tool],
        ] {
            assert!(Conversation::new(model("model"), String::new(), true, history).is_err());
        }
    }

    #[test]
    fn interleaved_tool_fragments_and_usage() {
        let mut c = conversation(true);
        let req = c.begin("go", "turn").unwrap();
        assert_eq!(req["tools"][0]["function"]["name"], "think");
        let events = c
            .chunk(chunk(
                json!({"content":"planning", "reasoning_content":"why", "tool_calls":[
                    call(1, "b", "th", r#"{"th"#), call(0, "a", "think", r#"{"thought":"#)
                ]}),
                None,
            ))
            .unwrap();
        assert_eq!(events[0]["message_id"], "turn");
        assert_eq!(
            events[2],
            json!({"type":"tool_call_delta", "id":"a", "name":"think", "arguments":r#"{"thought":"#})
        );
        let events = c
            .chunk(chunk(
                json!({"tool_calls":[
                    {"index":1, "function":{"name":"ink", "arguments":"ought\":\"B\"}"}},
                    {"index":0, "function":{"arguments":"\"A\"}"}}
                ]}),
                Some("tool_calls"),
            ))
            .unwrap();
        assert_eq!(events[0]["arguments"], r#"{"thought":"B"}"#);
        assert_eq!(events[1]["arguments"], r#""A"}"#);
        let usage = json!({"choices":[], "usage":{"prompt_tokens":7,"completion_tokens":3}});
        c.chunk(usage.clone()).unwrap();
        let next = c.end().unwrap();
        let events = next["events"].as_array().unwrap();
        assert_eq!(events[0]["type"], "usage");
        assert_eq!(
            events[1],
            json!({"type":"tool_call", "id":"a", "name":"think", "args":r#"{"thought":"A"}"#})
        );
        assert_eq!(
            events[4],
            json!({"type":"tool_result", "id":"b", "name":"think", "output":"Thoughts:\nA\nB", "is_error":false})
        );
        let m = &next["request"]["messages"];
        assert_eq!(m[2]["reasoning_content"], "why");
        assert_eq!(m[2]["content"], "planning");
        assert_eq!(
            m[2]["tool_calls"][0],
            json!({"id":"a", "type":"function", "function":{"name":"think", "arguments":r#"{"thought":"A"}"#}})
        );
        assert_eq!(
            m[3],
            json!({"role":"tool", "content":"Thoughts:\nA", "tool_call_id":"a"})
        );
        assert_eq!(
            m[4],
            json!({"role":"tool", "content":"Thoughts:\nA\nB", "tool_call_id":"b"})
        );
        let text = c
            .chunk(chunk(json!({"content":"done"}), Some("stop")))
            .unwrap();
        assert_ne!(text[0]["message_id"], "turn");
        c.chunk(usage).unwrap();
        assert_eq!(
            c.end().unwrap()["result"]["usage"],
            json!({"input_tokens":14, "output_tokens":6})
        );
        assert!(c.end().is_err());
    }

    #[test]
    fn cancel_rolls_back_only_the_active_turn_including_its_tool_results() {
        let mut c = conversation(true);
        c.begin("first", "first").unwrap();
        think_once(&mut c, "committed");
        answer(&mut c, "one");
        let saved = c.messages.clone();
        c.begin("second", "second").unwrap();
        think_once(&mut c, "discard me");
        c.cancel();
        c.cancel();
        assert_eq!(c.messages, saved);
        c.begin("third", "third").unwrap();
        assert_eq!(
            think_once(&mut c, "third")["events"][1]["output"],
            "Thoughts:\ncommitted\nthird"
        );
        let mut other = conversation(true);
        other.begin("go", "go").unwrap();
        assert_eq!(
            think_once(&mut other, "isolated")["events"][1]["output"],
            "Thoughts:\nisolated"
        );
    }

    #[test]
    fn rejects_broken_streams_and_rolls_back() {
        for chunks in [
            vec![chunk(json!({"content":"partial"}), None)],
            vec![chunk(json!({"content":"partial"}), Some("length"))],
            vec![chunk(json!({}), Some("tool_calls"))],
            vec![
                chunk(json!({}), Some("stop")),
                chunk(json!({"content":"late"}), None),
            ],
            vec![chunk(json!({"content":3}), Some("stop"))],
            vec![chunk(
                json!({"tool_calls":[call(0, "a", "think", "{}")]}),
                Some("content_filter"),
            )],
            vec![chunk(
                json!({"tool_calls":[call(0, "a", "think", "{}")]}),
                Some("stop"),
            )],
        ] {
            let mut c = conversation(true);
            let saved = c.messages.clone();
            c.begin("broken", "id").unwrap();
            assert!(chunks
                .into_iter()
                .try_for_each(|v| c.chunk(v).map(drop))
                .and_then(|_| c.end())
                .is_err());
            assert_eq!(c.messages, saved);
            assert_eq!(c.scratchpad, "Thoughts:");
        }
    }

    #[test]
    fn rejects_malformed_or_unavailable_tools_without_retaining_partial_results() {
        let args = r#"{"thought":"ok"}"#;
        for invalid in [
            call(1, "b", "shell", args),
            call(1, "b", "th", args),
            call(1, "a", "think", args),
            call(1, "", "think", args),
            call(1, "b", "think", "{}"),
            call(1, "b", "think", r#"["positional"]"#),
            call(1, "b", "think", "{"),
            call(1, "b", "think", r#"{"thought":3}"#),
            call(1, "b", "think", r#"{"thought":"x","extra":1}"#),
            call(1, "b", "think", r#"{"thought":"x","thought":"y"}"#),
            call(9, "b", "think", args),
            json!({"index":1, "type":"custom", "id":"b"}),
            json!({"index":4294967295_u64}),
        ] {
            let mut c = conversation(true);
            c.begin("go", "go").unwrap();
            let delta = json!({"tool_calls":[call(0, "a", "think", args), invalid]});
            assert!(c
                .chunk(chunk(delta, Some("tool_calls")))
                .and_then(|_| c.end())
                .is_err());
            assert_eq!(c.scratchpad, "Thoughts:");
        }
        let mut c = conversation(false);
        c.begin("go", "go").unwrap();
        assert!(c
            .chunk(chunk(
                json!({"tool_calls":[call(0,"a","think",args)]}),
                Some("tool_calls")
            ))
            .is_err());
    }

    #[test]
    fn repeated_tools_cannot_loop_forever_or_exhaust_the_scratchpad() {
        let mut c = conversation(true);
        c.begin("loop", "loop").unwrap();
        for _ in 0..MAX_TOOL_ROUNDS {
            think_once(&mut c, "");
        }
        c.chunk(chunk(
            json!({"tool_calls":[call(0,"a","think",r#"{"thought":""}"#)]}),
            Some("tool_calls"),
        ))
        .unwrap();
        assert!(c.end().unwrap_err().contains("iteration limit"));
        assert_eq!(c.scratchpad, "Thoughts:");
        c.begin("retry", "retry").unwrap();
        let args = json!({"thought":"x".repeat(MAX_SCRATCHPAD_BYTES)}).to_string();
        c.chunk(chunk(
            json!({"tool_calls":[call(0,"a","think",&args)]}),
            Some("tool_calls"),
        ))
        .unwrap();
        assert!(c.end().unwrap_err().contains("size limit"));
    }
}
