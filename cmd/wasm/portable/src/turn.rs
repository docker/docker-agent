use indexmap::IndexMap;
use serde::{de::DeserializeOwned, Deserialize};
use serde_json::{json, Map, Value};
use std::collections::{BTreeMap, BTreeSet};
use std::sync::LazyLock;

const MAX_TOOL_ROUNDS: usize = 8;
const MAX_PARALLEL_CALLS: u64 = 16;
// Every think result repeats the scratchpad and becomes part of the next request.
const MAX_SCRATCHPAD_BYTES: usize = 64 * 1024;
const MAX_ELICITATION_BYTES: usize = 64 * 1024;

#[derive(Deserialize)]
struct ToolsetDefinition {
    instructions: String,
    strict: BTreeMap<String, bool>,
    tools: Vec<Value>,
}

static CATALOG: LazyLock<BTreeMap<String, ToolsetDefinition>> = LazyLock::new(|| {
    serde_json::from_str(include_str!("tool_catalog.json")).expect("generated tool catalog")
});
pub fn response_strict(name: &str) -> Result<bool, String> {
    CATALOG
        .values()
        .find_map(|set| set.strict.get(name).copied())
        .ok_or_else(|| format!("no response schema for tool {name:?}"))
}
type Tools = IndexMap<&'static str, &'static Value>;

pub struct Conversation {
    pub hooks: crate::hooks::Hooks,
    model_params: Map<String, Value>,
    tools: Tools,
    merge_messages: bool,
    messages: Vec<Value>,
    instruction_count: usize,
    todos: crate::todo::Todos,
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
    pending: BTreeMap<String, usize>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Think {
    thought: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Prompt {
    message: String,
    title: Option<String>,
    schema: Option<Map<String, Value>>,
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
        toolsets: &[String],
        merge_messages: bool,
        mut history: Vec<Value>,
    ) -> Result<Self, String> {
        validate_history(&history)?;
        let mut tools = Tools::new();
        let mut instructions = Vec::new();
        if !instruction.is_empty() {
            instructions.push(instruction.as_str());
        }
        for kind in toolsets {
            let def = CATALOG
                .get(kind)
                .ok_or_else(|| format!("unsupported toolset {kind:?}"))?;
            if !def.instructions.is_empty() {
                instructions.push(&def.instructions);
            }
            for tool in &def.tools {
                let name = tool["function"]["name"]
                    .as_str()
                    .expect("catalog function name");
                tools.entry(name).or_insert(tool);
            }
        }
        let instruction_count = instructions.len();
        history.splice(
            0..0,
            instructions
                .into_iter()
                .map(|text| json!({"role":"system", "content":text})),
        );
        Ok(Self {
            hooks: crate::hooks::Hooks::default(),
            model_params,
            merge_messages,
            tools,
            messages: history,
            instruction_count,
            todos: crate::todo::Todos::default(),
            scratchpad: "Thoughts:".into(),
            active: None,
        })
    }

    pub fn restart(&mut self) {
        self.cancel();
        self.messages.truncate(self.instruction_count);
        self.scratchpad.truncate("Thoughts:".len());
        // Go restart replaces the conversation, not the runtime's todo toolset.
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
            pending: BTreeMap::new(),
        });
        self.messages.push(json!({"role":"user", "content":prompt}));
        self.request()
    }

    fn request(&self) -> Result<Value, String> {
        if self.hooks.add_date || self.hooks.add_environment_info {
            // The host samples context only when it is actually about to fetch.
            return Ok(Value::Null);
        }
        self.prepare(None, None)
    }

    pub fn prepare(
        &self,
        environment: Option<String>,
        date: Option<String>,
    ) -> Result<Value, String> {
        let turn = self.active.as_ref().ok_or("no active turn")?;
        if !turn.pending.is_empty() {
            return Err("waiting for elicitation".into());
        }
        let mut messages = self.messages.clone();
        let mut pos = self.instruction_count;
        for (enabled, text) in [
            (self.hooks.add_environment_info, environment),
            (self.hooks.add_date, date),
        ] {
            if enabled {
                let text = text
                    .filter(|s| !s.is_empty() && s.len() <= 4096)
                    .ok_or("missing or invalid dynamic prompt")?;
                messages.insert(pos, json!({"role":"system","content":text}));
                pos += 1;
            }
        }
        self.hooks.rewrite_messages(&mut messages)?;
        let mut body = Value::Object(self.model_params.clone());
        body["messages"] = messages.into();
        body["stream"] = true.into();
        body["stream_options"] = json!({"include_usage": true});
        if !self.tools.is_empty() {
            body["tool_choice"] = "auto".into();
            body["tools"] = Value::Array(self.tools.values().map(|tool| (*tool).clone()).collect());
        }
        if self.merge_messages {
            crate::wire::merge_text_messages(&mut body);
        }
        Ok(body)
    }

    pub fn chunk(&mut self, chunk: Value) -> Result<Vec<Value>, String> {
        let turn = self.active.as_mut().ok_or("no active turn")?;
        if !turn.pending.is_empty() {
            return Err("waiting for elicitation".into());
        }
        if let Some(error) = chunk.get("error").filter(|e| !e.is_null()) {
            // Preserve admitted user messages and completed tools on a model error.
            self.active = None;
            return Err(format!("model failed: error receiving from stream: received error while streaming: {error}"));
        }
        let result = turn.step.chunk(chunk, &turn.message_id, &self.tools);
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
        if self.active.as_ref().is_some_and(|t| !t.pending.is_empty()) {
            return Err("waiting for elicitation".into());
        }
        let result = self.finish_step();
        if result.is_err() {
            self.cancel();
        }
        result
    }

    fn finish_step(&mut self) -> Result<Value, String> {
        let turn = self.active.as_mut().ok_or("no active turn")?;
        let mut step = std::mem::take(&mut turn.step);
        let mut events = Vec::new();
        if matches!(
            step.finish.as_deref(),
            Some("length" | "content_filter" | "refusal")
        ) {
            // Never execute (or replay) tool calls from a truncated or refused step.
            if !step.calls.is_empty() {
                let ids: Vec<_> = step.calls.values().map(|c| &c.id).collect();
                events.push(json!({"type":"tool_calls_retracted", "ids":ids}));
                step.calls.clear();
            }
            step.finish = Some("stop".into());
        }
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
        if !step.text.is_empty() || step.calls.is_empty() {
            message["content"] = Value::String(step.text);
        }
        if !step.reasoning.is_empty() {
            message["reasoning_content"] = Value::String(step.reasoning);
        }
        match step.finish.as_deref() {
            None => Err("incomplete stream: missing finish_reason".into()),
            Some("stop") if step.calls.is_empty() => {
                let mut result =
                    json!({"message":{"role":"assistant", "content":message["content"]}});
                if message["content"]
                    .as_str()
                    .is_some_and(|s| !s.trim().is_empty())
                    || message.get("reasoning_content").is_some()
                {
                    self.messages.push(message);
                }
                if turn.input != 0 || turn.output != 0 {
                    result["usage"] =
                        json!({"input_tokens":turn.input, "output_tokens":turn.output});
                }
                events.push(json!({"type":"finish", "reason":"stop"}));
                self.active = None;
                Ok(json!({"events":events, "result":result}))
            }
            Some("stop" | "tool_calls") if !step.calls.is_empty() => {
                if turn.tool_rounds >= MAX_TOOL_ROUNDS {
                    return Err("tool iteration limit reached".into());
                }
                turn.tool_rounds += 1;
                turn.message_id = format!("{}:{}", turn.id, turn.tool_rounds);
                self.execute_calls(step.calls, message, events)
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
        mut events: Vec<Value>,
    ) -> Result<Value, String> {
        let turn = self.active.as_mut().expect("active tool round");
        let mut ids = BTreeSet::new();
        let mut wire_calls = Vec::with_capacity(calls.len());
        let mut replies = Vec::with_capacity(calls.len());
        for (index, (key, mut call)) in calls.into_iter().enumerate() {
            if key != index as u64 || call.id.is_empty() || !ids.insert(call.id.clone()) {
                return Err("missing or duplicate tool call ID or index".into());
            }
            wire_calls.push(json!({"id":call.id, "type":"function", "function":{"name":call.name, "arguments":call.arguments}}));
            if self.hooks.rewrite_input(&call.name, &mut call.arguments)? {
                events.push(json!({"type":"warning", "message":format!("redact_secrets: redacted secret material from arguments of tool {:?}", call.name)}));
            }
            events.push(
                json!({"type":"tool_call", "id":call.id, "name":call.name, "args":call.arguments}),
            );
            let mut reply = json!({"role":"tool", "tool_call_id":call.id});
            let result = match call.name.as_str() {
                "think" => {
                    let Think { thought } = call.parse()?;
                    if self.scratchpad.len() + 1 + thought.len() > MAX_SCRATCHPAD_BYTES {
                        return Err("think scratchpad size limit reached".into());
                    }
                    self.scratchpad.push('\n');
                    self.scratchpad.push_str(&thought);
                    Some(("think", Ok(self.scratchpad.clone())))
                }
                "calculate" => Some(("calculator", crate::calculator::calculate(&call.arguments))),
                "create_todo" | "create_todos" | "update_todos" | "list_todos" => {
                    Some(("todo", self.todos.call(&call.name, &call.arguments)))
                }
                "user_prompt" => {
                    if call.arguments.len() > MAX_ELICITATION_BYTES {
                        return Err("elicitation argument size limit reached".into());
                    }
                    let prompt: Prompt = call.parse()?;
                    // Reserve the tool message in call order even if answers arrive out of order.
                    let id = format!("{}:prompt:{index}", turn.message_id);
                    turn.pending
                        .insert(id.clone(), self.messages.len() + 1 + index);
                    let meta = prompt
                        .title
                        .filter(|t| !t.is_empty())
                        .map(|t| json!({"cagent/title":t}));
                    events.push(json!({
                        "type":"elicitation", "id":id, "message":prompt.message,
                        "schema":prompt.schema, "meta":meta, "server_elicitation_id":"", "mode":"", "url":""
                    }));
                    None
                }
                _ => return Err("missing or invalid tool name".into()),
            };
            if let Some((category, result)) = result {
                let is_error = result.is_err();
                let output =
                    self.hooks
                        .tool_result(&call.name, category, result.unwrap_or_else(|e| e))?;
                events.push(tool_result(&call.id, &call.name, &output, is_error));
                reply["content"] = output.into();
            }
            replies.push(reply);
        }
        message["tool_calls"] = Value::Array(wire_calls);
        self.messages.push(message);
        self.messages.extend(replies);
        Ok(self.after_tools(events))
    }

    fn after_tools(&mut self, events: Vec<Value>) -> Value {
        let turn = self.active.as_ref().expect("active tool round");
        if !turn.pending.is_empty() {
            return json!({"events":events, "waiting":true});
        }
        if let Some(limit) = self
            .hooks
            .max_calls
            .filter(|&n| turn.tool_rounds >= n as usize)
        {
            // The calls were admitted: publish their events and keep their history.
            self.active = None;
            return json!({"events":events, "error":format!("Agent terminated: max_iterations builtin reached its limit of {limit} model call(s).")});
        }
        match self.request() {
            Ok(body) => json!({"events":events, "request":body}),
            Err(error) => {
                self.active = None;
                json!({"events":events, "error":error})
            }
        }
    }

    pub fn answer(
        &mut self,
        id: &str,
        action: &str,
        content: Option<Map<String, Value>>,
    ) -> Result<Value, String> {
        if !matches!(action, "accept" | "decline" | "cancel") {
            return Err(format!("unknown elicitation action {action:?}"));
        }
        let turn = self.active.as_mut().ok_or("no active turn")?;
        let &index = turn
            .pending
            .get(id)
            .ok_or("no pending elicitation with that ID")?;
        let mut response = json!({"action":action});
        if let Some(data) = content.filter(|c| !c.is_empty()) {
            response["content"] = Value::Object(data);
        }
        let output = response.to_string();
        if output.len() > MAX_ELICITATION_BYTES {
            return Err("elicitation answer size limit reached".into());
        }
        let output = self
            .hooks
            .tool_result("user_prompt", "user_prompt", output)?;
        let reply = &mut self.messages[index];
        let call_id = reply["tool_call_id"]
            .as_str()
            .expect("reserved tool message");
        let event = tool_result(call_id, "user_prompt", &output, action != "accept");
        reply["content"] = Value::String(output);
        turn.pending.remove(id);
        Ok(self.after_tools(vec![event]))
    }
}

fn tool_result(id: &str, name: &str, output: &str, is_error: bool) -> Value {
    json!({"type":"tool_result", "id":id, "name":name, "output":output, "is_error":is_error})
}

impl Step {
    fn chunk(&mut self, chunk: Value, id: &str, tools: &Tools) -> Result<Vec<Value>, String> {
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
                if tools.is_empty() {
                    return Err("tool calls are disabled".into());
                }
                let index = fragment["index"]
                    .as_u64()
                    .filter(|n| *n < MAX_PARALLEL_CALLS)
                    .ok_or("invalid tool call index")?;
                if let Some(event) = self
                    .calls
                    .entry(index)
                    .or_default()
                    .append(fragment, tools)?
                {
                    events.push(event);
                }
            }
        }
        self.finish = string(choice, "finish_reason")?.map(str::to_owned);
        Ok(events)
    }
}

impl Call {
    fn append(&mut self, fragment: &Value, tools: &Tools) -> Result<Option<Value>, String> {
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
            if !tools.keys().any(|name| name.starts_with(&self.name)) {
                return Err("unsupported tool name".into());
            }
        }
        let args = string(f, "arguments")?.unwrap_or_default();
        self.arguments.push_str(args);
        if self.id.is_empty()
            || !tools.contains_key(self.name.as_str())
            || (self.announced && args.is_empty())
        {
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

    fn parse<T: DeserializeOwned>(&self) -> Result<T, String> {
        // serde structs also deserialize from positional arrays, which our tool schemas forbid.
        if !self.arguments.trim_start().starts_with('{') {
            return Err(format!("{} arguments must be a JSON object", self.name));
        }
        serde_json::from_str(&self.arguments)
            .map_err(|e| format!("invalid {} arguments: {e}", self.name))
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

    fn conversation(toolsets: &[&str]) -> Conversation {
        Conversation::new(
            model("model"),
            "be concise".into(),
            &toolsets.iter().map(|s| s.to_string()).collect::<Vec<_>>(),
            true,
            vec![],
        )
        .unwrap()
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
        let mut c =
            Conversation::new(model("test"), "sys".into(), &[], true, history.clone()).unwrap();
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
            assert!(Conversation::new(
                model("model"),
                String::new(),
                &["think".into()],
                true,
                history
            )
            .is_err());
        }
    }

    #[test]
    fn interleaved_tool_fragments_and_usage() {
        let mut c = conversation(&["think"]);
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
        let mut c = conversation(&["think"]);
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
        let mut other = conversation(&["think"]);
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
            vec![chunk(json!({}), Some("tool_calls"))],
            vec![
                chunk(json!({}), Some("stop")),
                chunk(json!({"content":"late"}), None),
            ],
            vec![chunk(json!({"content":3}), Some("stop"))],
            vec![chunk(
                json!({"tool_calls":[call(0, "a", "think", "{}")]}),
                Some("stop"),
            )],
        ] {
            let mut c = conversation(&["think"]);
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
            let mut c = conversation(&["think"]);
            c.begin("go", "go").unwrap();
            let delta = json!({"tool_calls":[call(0, "a", "think", args), invalid]});
            assert!(c
                .chunk(chunk(delta, Some("tool_calls")))
                .and_then(|_| c.end())
                .is_err());
            assert_eq!(c.scratchpad, "Thoughts:");
        }
        let mut c = conversation(&[]);
        c.begin("go", "go").unwrap();
        assert!(c
            .chunk(chunk(
                json!({"tool_calls":[call(0,"a","think",args)]}),
                Some("tool_calls")
            ))
            .is_err());
    }

    #[test]
    fn official_openai_keeps_system_messages_separate_and_deduplicates_advertised_tools() {
        let mut c = Conversation::new(
            model("model"),
            "agent".into(),
            &["user_prompt".into(), "think".into(), "user_prompt".into()],
            false,
            vec![],
        )
        .unwrap();
        let req = c.begin("go", "turn").unwrap();
        assert_eq!(
            req["tools"],
            json!([CATALOG["user_prompt"].tools[0], CATALOG["think"].tools[0]])
        );
        let texts: Vec<_> = req["messages"]
            .as_array()
            .unwrap()
            .iter()
            .map(|m| m["content"].as_str().unwrap())
            .collect();
        assert_eq!(
            texts,
            [
                "agent",
                &CATALOG["user_prompt"].instructions,
                &CATALOG["think"].instructions,
                &CATALOG["user_prompt"].instructions,
                "go"
            ]
        );
    }

    #[test]
    fn invalid_batch_never_exposes_a_prompt_or_commits_partial_history() {
        let args = r#"{"message":"A?"}"#;
        let mut c = conversation(&["user_prompt"]);
        let saved = c.messages.clone();
        for invalid in [
            call(1, "b", "think", r#"{"thought":"no"}"#),
            call(1, "b", "user_", args),
            call(1, "a", "user_prompt", args),
            call(1, "b", "user_prompt", "{}"),
            call(1, "b", "user_prompt", r#"["question"]"#),
            call(1, "b", "user_prompt", r#"{"message":"A","message":"B"}"#),
            call(1, "b", "user_prompt", r#"{"message":"A?","url":"no"}"#),
            call(1, "b", "user_prompt", r#"{"message":"A?","schema":"oops"}"#),
            call(
                1,
                "b",
                "user_prompt",
                &json!({"message":"x".repeat(MAX_ELICITATION_BYTES)}).to_string(),
            ),
        ] {
            c.begin("go", "turn").unwrap();
            let delta = json!({"tool_calls":[call(0, "a", "user_prompt", args), invalid]});
            assert!(c
                .chunk(chunk(delta, Some("tool_calls")))
                .and_then(|_| c.end())
                .is_err());
            assert_eq!(c.messages, saved);
            assert!(c.active.is_none());
        }
    }

    #[test]
    fn incomplete_batch_rejects_model_chunks_and_waits_for_its_own_replies() {
        let mut c = conversation(&["user_prompt"]);
        c.begin("go", "turn").unwrap();
        c.chunk(chunk(
            json!({"tool_calls":[call(0,"one","user_",r#"{"message":"A?"}"#)]}),
            None,
        ))
        .unwrap();
        let events = c
            .chunk(chunk(
                json!({"tool_calls":[{"index":0,"function":{"name":"prompt"}}]}),
                Some("tool_calls"),
            ))
            .unwrap();
        assert_eq!(events[0]["arguments"], r#"{"message":"A?"}"#);
        let waiting = c.end().unwrap();
        let id = waiting["events"][1]["id"].as_str().unwrap();
        assert!(waiting.get("request").is_none());
        assert!(c.end().is_err());
        assert!(c
            .chunk(chunk(json!({"content":"skip"}), Some("stop")))
            .is_err());
        assert!(c.answer("one", "accept", None).is_err());
        let next = c.answer(id, "accept", Some(Default::default())).unwrap();
        assert_eq!(next["events"][0]["output"], r#"{"action":"accept"}"#);
        assert!(c.answer(id, "decline", None).is_err());
        assert_eq!(answer(&mut c, "ok")["result"]["message"]["content"], "ok");
    }

    #[test]
    fn max_calls_checks_between_rounds_including_pending_elicitations() {
        let mut c = conversation(&["user_prompt", "think"]);
        c.hooks = crate::hooks::Hooks::try_from(
            json!({"before_llm_call":{"type":"builtin","command":"max_iterations","args":["1"]}}),
        )
        .unwrap();
        c.begin("go", "a").unwrap();
        c.chunk(chunk(
            json!({"tool_calls":[call(0,"q","user_prompt",r#"{"message":"ok?"}"#)]}),
            Some("tool_calls"),
        ))
        .unwrap();
        let waiting = c.end().unwrap();
        assert_eq!(waiting["waiting"], true);
        let stop = c
            .answer(
                waiting["events"][1]["id"].as_str().unwrap(),
                "decline",
                None,
            )
            .unwrap();
        assert_eq!(stop["events"][0]["type"], "tool_result");
        assert!(stop["error"]
            .as_str()
            .unwrap()
            .contains("limit of 1 model call(s)"));
        assert!(stop.get("request").is_none());
        c.cancel();
        let req = c.begin("new run", "b").unwrap();
        assert_eq!(req["messages"][3]["role"], "tool");
        assert_eq!(answer(&mut c, "ok")["result"]["message"]["content"], "ok");
    }

    #[test]
    fn dynamic_context_is_transient_and_redacted_on_each_model_call() {
        let mut c = conversation(&["think"]);
        c.hooks = crate::hooks::Hooks::default().with_defaults(true, true, true);
        let secret = format!("dckr_pat_{}", "a".repeat(27));
        assert_eq!(c.begin(&secret, "a").unwrap(), Value::Null);
        assert!(c.prepare(None, None).is_err());
        let req = c.prepare(Some("env".into()), Some("day 1".into())).unwrap();
        assert!(req["messages"][0]["content"]
            .as_str()
            .unwrap()
            .ends_with("env\nday 1"));
        assert_eq!(req["messages"][1]["content"], "[REDACTED]");
        let step = think_once(&mut c, &secret);
        assert_eq!(step["events"][0]["type"], "warning");
        assert!(step["events"][1]["args"]
            .as_str()
            .unwrap()
            .contains("[REDACTED]"));
        assert_eq!(step["events"][2]["output"], "Thoughts:\n[REDACTED]");
        let req = c.prepare(Some("env".into()), Some("day 2".into())).unwrap();
        assert!(!req.to_string().contains(&secret));
        assert!(!req.to_string().contains("day 1"));
        assert!(req["messages"][0]["content"]
            .as_str()
            .unwrap()
            .ends_with("day 2"));
        assert!(c.messages.iter().all(|m| !m.to_string().contains("day 2")));
        c.restart();
        c.begin("new", "b").unwrap();
        let req = c.prepare(Some("env".into()), Some("day 3".into())).unwrap();
        assert_eq!(req["messages"].as_array().unwrap().len(), 2);
    }

    #[test]
    fn repeated_tools_cannot_loop_forever_or_exhaust_the_scratchpad() {
        let mut c = conversation(&["think"]);
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
