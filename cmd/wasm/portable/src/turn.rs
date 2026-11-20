use crate::compaction::{Compactor, Prefix, Undo, Usage};
use crate::discovery::{self, Discovery, Tool};
use crate::permission::{self, Verdict};
use crate::structured_output;
use crate::{config::Selected, team::Switch};
use indexmap::IndexMap;
use serde::{de::DeserializeOwned, Deserialize};
use serde_json::{json, Map, Value};
use std::cell::RefCell;
use std::collections::VecDeque;
use std::collections::{BTreeMap, BTreeSet};
use std::rc::Rc;
use std::sync::LazyLock;

use crate::limits::MAX_TOOL_ROUNDS;
const MAX_PARALLEL_CALLS: u64 = 16;
// Every think result repeats the scratchpad and becomes part of the next request.
const MAX_SCRATCHPAD_BYTES: usize = 64 * 1024;
const MAX_ELICITATION_BYTES: usize = 64 * 1024;

#[derive(Deserialize)]
struct ToolsetDefinition {
    instructions: String,
    strict: BTreeMap<String, bool>,
    read_only: BTreeMap<String, bool>,
    tools: Vec<Value>,
}

static CATALOG: LazyLock<BTreeMap<String, ToolsetDefinition>> = LazyLock::new(|| {
    serde_json::from_str(include_str!("tool_catalog.json")).expect("generated tool catalog")
});
pub fn response_strict(name: &str) -> bool {
    CATALOG
        .values()
        .find_map(|set| set.strict.get(name).copied())
        .unwrap_or(false)
}
#[derive(Clone, Copy, Debug)]
struct Target {
    host: Option<usize>,
    safe: bool,
    model_source: Option<usize>,
}

type Tools = IndexMap<String, Tool<Target>>;

#[derive(Debug, Deserialize)]
pub struct HostSet {
    pub index: usize,
    pub instructions: String,
    pub tools: Vec<Value>,
}

pub struct Profile {
    pub spec: Selected,
    catalog: RefCell<Catalog>,
    grants: RefCell<BTreeSet<String>>,
    todos: Rc<RefCell<crate::todo::Todos>>,
}

struct Catalog {
    tools: Tools,
    discovery: Discovery<Target>,
    instructions: Vec<Value>,
    needs_host: bool,
}

pub struct Conversation {
    pub profile: Rc<Profile>,
    pub attempt: crate::fallback::Attempt,
    pub archive: Option<crate::session_context::Archive>,
    compaction: Compactor,
    // Limits of the agent that opened this session or delegated task; a handoff keeps them.
    frame: crate::limits::Limits,
    opener: String,
    messages: Vec<Value>,
    routed_seed: Option<crate::media::RoutedSeed>,
    scratchpads: BTreeMap<String, String>,
    pub switch: Option<Switch>,
    run_number: u64,
    active: Option<Turn>,
}

struct Turn {
    current_calls: Vec<(String, String)>,
    previous_calls: Vec<(String, String)>,
    consecutive: u32,
    exempt_batch: bool,
    run_number: u64,
    tool_model_source: Option<usize>,
    context_before: u64,
    summarizing: Option<Prefix>,
    original: Option<Undo>,
    messages_before: usize,
    scratchpads_before: BTreeMap<String, usize>,
    profile_before: Rc<Profile>,
    id: String,
    message_id: String,
    tool_rounds: usize,
    reminders: usize,
    forced: usize,
    input: u64,
    output: u64,
    step: Step,
    queue: VecDeque<Invocation>,
    selection: Option<usize>,
    confirmation: Option<(String, Invocation)>,
    pending: BTreeMap<String, Reply>,
    host_pending: BTreeMap<String, Reply>,
}

impl Turn {
    fn token(&self, kind: &str, slot: usize) -> String {
        format!(
            "{}:{kind}:{}:{}:{slot}",
            self.id, self.run_number, self.tool_rounds
        )
    }

    fn waiting(&self) -> bool {
        self.confirmation.is_some() || !self.pending.is_empty() || !self.host_pending.is_empty()
    }
}

struct Invocation {
    call: Call,
    index: usize,
}

struct Reply {
    index: usize,
    name: String,
    toon: bool,
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

#[derive(Default)]
struct Call {
    id: String,
    name: String,
    arguments: String,
    announced: bool,
}

impl Profile {
    pub fn new(spec: Selected, todos: Rc<RefCell<crate::todo::Todos>>) -> Result<Self, String> {
        Ok(Self {
            catalog: RefCell::new(Catalog::build(&spec, None)?),
            grants: Default::default(),
            spec,
            todos,
        })
    }

    pub fn clear_grants(&self) {
        self.grants.borrow_mut().clear();
    }

    pub fn needs_host(&self) -> bool {
        self.catalog.borrow().needs_host
    }

    pub fn register(&self, sets: &[HostSet]) -> Result<(), String> {
        if !self.needs_host() {
            return Err("host catalog already registered".into());
        }
        *self.catalog.borrow_mut() = Catalog::build(&self.spec, Some(sets))?;
        Ok(())
    }
}

impl Catalog {
    fn build(agent: &Selected, external: Option<&[HostSet]>) -> Result<Self, String> {
        let mut needs_host = false;
        let mut tools = Tools::new();
        let mut discovery = Discovery::default();
        let mut registered = BTreeMap::new();
        let mut seen = BTreeMap::new();
        for set in external.unwrap_or_default() {
            if set.index >= agent.toolsets.len() || registered.insert(set.index, set).is_some() {
                return Err("invalid or duplicate host toolset index".into());
            }
        }
        let mut instructions = agent.team_prompts.clone();
        if !agent.instruction.is_empty() {
            instructions.push(agent.instruction.clone());
        }
        for (index, spec) in agent.toolsets.iter().enumerate() {
            let def = CATALOG.get(&spec.kind);
            let ext = registered.get(&index);
            let dynamic = matches!(spec.kind.as_str(), "api" | "mcp" | "fetch");
            if ext.is_some() && !dynamic {
                return Err("unexpected host tool registration".into());
            }
            if dynamic && ext.is_none() {
                if external.is_some() {
                    return Err("missing host tool registration".into());
                }
                needs_host = true;
            }
            let picker_metadata;
            let api_definition;
            let (original, defs) = if let Some(e) = ext {
                (e.instructions.as_str(), e.tools.as_slice())
            } else if let Some(d) = def {
                if spec.kind == "model_picker" {
                    picker_metadata = agent.picker.metadata(&d.instructions, &d.tools);
                    (picker_metadata.0.as_str(), picker_metadata.1.as_slice())
                } else {
                    (d.instructions.as_str(), d.tools.as_slice())
                }
            } else if spec.kind == "api" {
                // Its name is known before host registration: catch reserved names and collisions at open.
                api_definition = [json!({"function":{"name":spec.options["api_config"]["name"]}})];
                ("", api_definition.as_slice())
            } else if dynamic {
                continue;
            } else {
                return Err(format!("unsupported toolset {:?}", spec.kind));
            };
            let text = if spec.instruction.is_empty() {
                original.into()
            } else {
                spec.instruction
                    .replacen("{ORIGINAL_INSTRUCTIONS}", original, 1)
            };
            let (all, names) = spec.defer.parts();
            let hosted = dynamic
                || matches!(
                    spec.kind.as_str(),
                    "datetime" | "random" | "plan" | "memory"
                );
            let mut withheld = false;
            let mut admitted = Vec::new();
            for tool in defs {
                let name = tool["function"]["name"]
                    .as_str()
                    .ok_or("invalid function name")?;
                permission::validate_tool(name)?;
                if agent.output_tool.is_some() && name == structured_output::NAME {
                    return Err(format!(
                        "tool name {name:?} is reserved for tool-mode structured output"
                    ));
                }
                if dynamic
                    && CATALOG
                        .values()
                        .flat_map(|d| &d.tools)
                        .any(|t| t["function"]["name"] == name)
                    && !(spec.kind == "fetch" && name == "fetch")
                {
                    return Err(format!("reserved tool name {name:?}"));
                }
                // MCP annotations are supplied by the same remote server that runs the handler.
                let safe = match spec.kind.as_str() {
                    "mcp" => false,
                    "api" => spec.options["api_config"]["method"] == "GET",
                    _ => def
                        .and_then(|d| d.read_only.get(name))
                        .copied()
                        .unwrap_or(false),
                };
                if spec.readonly && !safe
                    || !spec.tools.is_empty() && !spec.tools.iter().any(|n| n == name)
                    || !agent.tool_policy.visible(name, safe)?
                {
                    withheld = true;
                    continue;
                }
                let hidden = all || names.iter().any(|n| n == name);
                let profile = (hosted, hidden, spec.toon.matches(name));
                if let Some(previous) = seen.insert(name.to_owned(), profile) {
                    if hosted || previous != profile {
                        return Err(format!("duplicate tool name {name:?}"));
                    }
                    continue;
                }
                admitted.push((
                    tool.clone(),
                    Target {
                        host: hosted.then_some(index),
                        safe,
                        model_source: (!spec.model.is_empty()).then_some(index),
                    },
                ));
            }
            for tool in discovery.wrap_source(admitted, all, names, &spec.toon)? {
                tools.insert(
                    tool.definition["function"]["name"].as_str().unwrap().into(),
                    tool,
                );
            }
            // Do not disclose tools hidden by policy. Deferred tools, unlike filtered tools, are discoverable.
            let custom = !spec.instruction.is_empty()
                && !spec.instruction.contains("{ORIGINAL_INSTRUCTIONS}");
            if !text.is_empty() && (!withheld || custom) {
                instructions.push(text);
            }
        }
        if discovery.enabled() {
            let helpers = &CATALOG["deferred"];
            let mut all_visible = true;
            for definition in &helpers.tools {
                let name = definition["function"]["name"].as_str().unwrap();
                if agent.tool_policy.visible(name, true)? {
                    tools.insert(
                        name.into(),
                        Tool {
                            definition: definition.clone(),
                            target: Target {
                                host: None,
                                safe: helpers.read_only[name],
                                model_source: None,
                            },
                            toon: false,
                        },
                    );
                } else {
                    all_visible = false;
                }
            }
            if all_visible {
                instructions.push(helpers.instructions.clone());
            }
        }
        if let Some(output) = &agent.output_tool {
            tools.insert(
                structured_output::NAME.into(),
                Tool {
                    definition: output.definition.clone(),
                    target: Target {
                        host: None,
                        model_source: None,
                        safe: true,
                    },
                    toon: false,
                },
            );
        }
        Ok(Self {
            instructions: instructions
                .into_iter()
                .map(|text| json!({"role":"system","content":text}))
                .collect(),
            tools,
            discovery,
            needs_host,
        })
    }
}

impl Conversation {
    pub fn open(profile: Rc<Profile>, messages: Vec<Value>) -> Self {
        Self {
            run_number: 0,
            frame: profile.spec.limits,
            opener: profile.spec.name.clone(),
            profile,
            attempt: Default::default(),
            archive: None,
            compaction: Compactor::default(),
            messages,
            routed_seed: None,
            scratchpads: BTreeMap::new(),
            active: None,
            switch: None,
        }
    }

    #[cfg(test)]
    fn new(
        instruction: String,
        toolsets: &[String],
        merge: bool,
        history: Vec<Value>,
    ) -> Result<Self, String> {
        let mut spec = crate::config::select(
            "providers: {p: {}}\nagents: {root: {model: p/test}}",
            "",
            &BTreeMap::new(),
        )?;
        spec.models[0].merge_messages = merge;
        spec.hooks = Default::default();
        spec.instruction = instruction;
        spec.toolsets = toolsets
            .iter()
            .map(|kind| crate::config::Toolset {
                kind: kind.clone(),
                ..Default::default()
            })
            .collect();
        Ok(Self::open(
            Rc::new(Profile::new(spec, Default::default())?),
            history,
        ))
    }

    pub fn forced(&mut self, profile: Rc<Profile>, note: String) {
        self.profile = profile;
        self.messages.push(json!({"role":"user","content":note}));
        let t = self.active.as_mut().expect("forced continuation");
        t.message_id = format!(
            "{}:forced:{}:{}",
            t.id, self.profile.spec.name, t.tool_rounds
        );
        t.tool_model_source = None;
        t.forced += 1;
    }
    pub fn restart(&mut self, stamp: crate::session_context::Stamp) -> Result<(), String> {
        self.cancel();
        if let Some(archive) = &mut self.archive {
            if let Some(seed) = &mut self.routed_seed {
                let mut messages = seed
                    .for_target(self.profile.spec.models[self.attempt.current].media_projection())?
                    .to_vec();
                messages.extend_from_slice(&self.messages);
                archive.restart(stamp, &messages)?;
            } else {
                archive.restart(stamp, &self.messages)?;
            }
        }
        self.messages.clear();
        self.routed_seed = None;
        self.scratchpads.clear();
        self.compaction.truncate(0);
        self.compaction.context_tokens = 0;
        Ok(())
    }

    pub fn begin(&mut self, prompt: &str, id: &str) -> Result<Value, String> {
        if self.active.is_some() {
            return Err("a run is already active".into());
        }
        if id.is_empty() {
            return Err("missing message ID".into());
        }
        self.run_number = self
            .run_number
            .checked_add(1)
            .ok_or("turn counter exhausted")?;
        self.active = Some(Turn {
            current_calls: Vec::new(),
            previous_calls: Vec::new(),
            consecutive: 0,
            exempt_batch: false,
            run_number: self.run_number,
            queue: VecDeque::new(),
            confirmation: None,
            selection: self.profile.spec.picker.active.get(),
            tool_model_source: None,
            summarizing: None,
            original: None,
            context_before: self.compaction.context_tokens,
            messages_before: self.messages.len(),
            scratchpads_before: self
                .scratchpads
                .iter()
                .map(|(k, s)| (k.clone(), s.len()))
                .collect(),
            profile_before: Rc::clone(&self.profile),
            id: id.into(),
            message_id: id.into(),
            tool_rounds: 0,
            reminders: 0,
            forced: 0,
            input: 0,
            output: 0,
            step: Step::default(),
            pending: BTreeMap::new(),
            host_pending: BTreeMap::new(),
        });
        self.messages.push(json!({"role":"user", "content":prompt}));
        self.prepare_request()
    }

    pub fn set_routed_seed(&mut self, source: Vec<Value>) {
        self.routed_seed = Some(crate::media::RoutedSeed::new(
            source,
            self.profile.spec.models[0].media_projection(),
            std::mem::take(&mut self.messages),
        ));
    }

    pub fn request_target(&self) -> (usize, &crate::config::Target) {
        let spec = &self.profile.spec;
        if self.summarizing() {
            spec.summary_target()
        } else {
            (self.attempt.current, self.target())
        }
    }

    fn target(&self) -> &crate::config::Target {
        &self.profile.spec.models[self.attempt.current]
    }

    fn override_params(&self, source: Option<usize>) -> Option<&Map<String, Value>> {
        let spec = &self.profile.spec;
        source
            .and_then(|i| spec.tool_models.get(&spec.toolsets[i].model))
            .or_else(|| spec.picker.params())
    }

    pub fn summarizing(&self) -> bool {
        self.active
            .as_ref()
            .is_some_and(|t| t.summarizing.is_some())
    }

    fn prepare_request(&mut self) -> Result<Value, String> {
        // The summary never inherits a serving fallback's parameters.
        let (_, summary) = self.profile.spec.summary_target();
        self.compaction.policy = self.profile.spec.models[0].compaction;
        let params = self
            .profile
            .spec
            .picker
            .params()
            .unwrap_or(&summary.model_params);
        if let Some((prefix, mut request)) = self.compaction.plan(
            &self.messages,
            params,
            summary.compaction,
            summary.merge_messages,
        )? {
            self.active.as_mut().expect("active turn").summarizing = Some(prefix);
            if let Some(msgs) = request["messages"].as_array_mut() {
                self.profile.spec.hooks.rewrite_messages(msgs)?;
            }
            Ok(request)
        } else {
            self.request()
        }
    }

    pub fn discard_summary_output(&mut self) {
        self.active.as_mut().expect("active summary").step = Step::default();
    }

    pub fn skip_summary(&mut self) {
        self.discard_summary_output();
        self.active.as_mut().expect("active summary").summarizing = None;
    }

    // session_start belongs to the agent that opened this run, even after a handoff.
    pub fn add_environment_info(&self) -> bool {
        self.active
            .as_ref()
            .is_some_and(|turn| turn.profile_before.spec.hooks.add_environment_info)
    }

    pub fn request(&mut self) -> Result<Value, String> {
        if self.profile.needs_host()
            || self.profile.spec.hooks.add_date
            || self.add_environment_info()
        {
            // The host samples context only when it is actually about to fetch.
            return Ok(Value::Null);
        }
        self.prepare(None, None)
    }

    pub fn prepare(
        &mut self,
        environment: Option<String>,
        date: Option<String>,
    ) -> Result<Value, String> {
        let turn = self.active.as_ref().ok_or("no active turn")?;
        if turn.waiting() {
            return Err("waiting for elicitation".into());
        }
        let catalog = self.profile.catalog.borrow();
        if catalog.needs_host {
            return Err("host catalog not registered".into());
        }
        let mut messages = catalog.instructions.clone();
        for (enabled, text) in [
            (self.add_environment_info(), environment),
            (self.profile.spec.hooks.add_date, date),
        ] {
            if enabled {
                let text = text
                    .filter(|s| !s.is_empty() && s.len() <= 4096)
                    .ok_or("missing or invalid dynamic prompt")?;
                messages.push(json!({"role":"system","content":text}));
            }
        }
        if turn.reminders > 0 {
            messages.push(json!({"role":"system", "content":structured_output::REMINDER}));
        }
        if let Some(seed) = &mut self.routed_seed {
            let target = &self.profile.spec.models[self.attempt.current];
            messages.extend_from_slice(seed.for_target(target.media_projection())?);
        }
        messages.extend_from_slice(&self.messages);
        self.frame
            .acting(&self.profile.spec.limits)
            .project(&mut messages, &self.profile.spec.hooks)?;
        let override_params = self.override_params(turn.tool_model_source);
        let target = self.target();
        if let Some(instruction) = target
            .schema_instruction
            .as_ref()
            .filter(|_| override_params.is_none())
        {
            // Go normalizes blank messages before selecting the leading role.
            messages.retain(|m| {
                m["role"] == "tool"
                    || !m["tool_calls"].is_null()
                    || m["content"].as_str().is_none_or(|s| !s.trim().is_empty())
            });
            if messages.is_empty() {
                return Err("Claude schema instruction needs a nonempty request".into());
            }
            // Only the outgoing view is amended, after hooks and before either wire protocol.
            if let Some(first) = messages.first_mut().filter(|m| m["role"] == "system") {
                let Value::String(text) = &mut first["content"] else {
                    return Err(
                        "Claude schema instructions require a plain leading system message".into(),
                    );
                };
                text.push_str("\n\n");
                text.push_str(instruction);
            } else {
                messages.insert(0, json!({"role":"system", "content":instruction}));
            }
        }
        let mut body = Value::Object(override_params.unwrap_or(&target.model_params).clone());
        body["messages"] = messages.into();
        body["stream"] = true.into();
        body["stream_options"] = json!({"include_usage": true});
        if !catalog.tools.is_empty() {
            body["tool_choice"] = "auto".into();
            body["tools"] = Value::Array(
                catalog
                    .tools
                    .values()
                    .map(|t| t.definition.clone())
                    .collect(),
            );
        } else {
            body.as_object_mut().unwrap().remove("parallel_tool_calls");
            body.as_object_mut().unwrap().remove("tool_choice");
        }
        if self.target().merge_messages {
            crate::wire::merge_messages(&mut body);
        }
        Ok(body)
    }

    pub fn chunk(&mut self, chunk: Value) -> Result<Vec<Value>, String> {
        let turn = self.active.as_mut().ok_or("no active turn")?;
        if turn.waiting() {
            return Err("waiting for elicitation".into());
        }
        let result = if turn.summarizing.is_some() {
            turn.step
                .chunk(chunk, &turn.message_id, &Tools::new())
                .map(|_| Vec::new())
        } else {
            let catalog = self.profile.catalog.borrow();
            if catalog.needs_host {
                return Err("host catalog not registered".into());
            }
            turn.step.chunk(chunk, &turn.message_id, &catalog.tools)
        };
        if result.is_err() {
            self.cancel();
        }
        result
    }

    // Unlike user cancellation, provider failure retains admitted input and completed tools.
    pub fn fail(&mut self) {
        if self.summarizing() {
            self.cancel();
        } else {
            self.active = None;
        }
    }

    pub fn cancel(&mut self) {
        if let Some(turn) = self.active.take() {
            if let Some(original) = turn.original {
                self.compaction.undo(&mut self.messages, original);
            }
            self.compaction.truncate(turn.messages_before);
            self.compaction.context_tokens = turn.context_before;
            self.messages.truncate(turn.messages_before);
            self.profile = turn.profile_before;
            self.attempt = Default::default();
            self.scratchpads
                .retain(|name, pad| match turn.scratchpads_before.get(name) {
                    Some(&n) => {
                        pad.truncate(n);
                        true
                    }
                    None => false,
                });
            self.switch = None;
        }
    }

    pub fn end(&mut self) -> Result<Value, String> {
        if self.active.as_ref().is_some_and(Turn::waiting) {
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
        if let Some(plan) = turn.summarizing.take() {
            let summary = step.text.trim();
            if summary.is_empty()
                || step.finish.as_deref() != Some("stop")
                || !step.calls.is_empty()
            {
                return Err("compaction: expected a complete, non-empty text summary".into());
            }
            let undo = self.compaction.apply(
                &mut self.messages,
                plan,
                summary,
                step.usage.map_or(0, |u| u.completion_tokens),
            );
            turn.original.get_or_insert(undo);
            return Ok(json!({"events":[], "request":self.request()?}));
        }
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
            self.compaction.record(
                self.messages.len(),
                &self.profile.spec.models[self.attempt.current].id,
                usage,
            );
            let input = u64::from(usage.prompt_tokens);
            let output = u64::from(usage.completion_tokens);
            turn.input += input;
            turn.output += output;
            events.push(json!({
                "type": "usage", "input_tokens": input, "output_tokens": output,
                "context_length": input + output, "context_limit": self.profile.spec.context_window(), "cost": 0
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
                let content = message["content"].clone();
                if message["content"]
                    .as_str()
                    .is_some_and(|s| !s.trim().is_empty())
                    || message.get("reasoning_content").is_some()
                {
                    self.messages.push(message);
                } else {
                    self.compaction.truncate(self.messages.len());
                }
                if self.profile.spec.output_tool.is_some() {
                    let reminders = turn.reminders;
                    let source = turn.tool_model_source.take();
                    let model = self
                        .override_params(source)
                        .map_or_else(|| self.target().model(), |p| p["model"].as_str().unwrap());
                    if let Some(error) = structured_output::stop_error(model, reminders) {
                        self.messages
                            .push(json!({"role":"assistant", "content":error}));
                        self.active = None;
                        return Ok(json!({"events":events,"error":error}));
                    }
                    let turn = self.active.as_mut().unwrap();
                    turn.reminders += 1;
                    turn.message_id = format!("{}:reminder:{}", turn.id, turn.reminders);
                    return Ok(self.continue_turn(events));
                }
                Ok(self.finish(content, events))
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

    fn finish(&mut self, content: Value, mut events: Vec<Value>) -> Value {
        if !self.profile.spec.force_handoff.is_empty() {
            self.switch = Some(Switch::Forced(self.profile.spec.force_handoff.clone()));
            return json!({"events":events});
        }
        let turn = self.active.take().expect("active turn");
        let mut result = json!({"message":{"role":"assistant", "content":content}});
        if turn.input != 0 || turn.output != 0 {
            result["usage"] = json!({"input_tokens":turn.input, "output_tokens":turn.output});
        }
        events.push(json!({"type":"finish", "reason":"stop"}));
        json!({"events":events, "result":result})
    }

    fn execute_calls(
        &mut self,
        calls: BTreeMap<u64, Call>,
        mut message: Value,
        mut events: Vec<Value>,
    ) -> Result<Value, String> {
        let mut model_source = None;
        let mut ids = BTreeSet::new();
        let mut wire_calls = Vec::with_capacity(calls.len());
        if calls.len() != 1
            && calls
                .values()
                .any(|c| matches!(c.name.as_str(), "transfer_task" | "handoff"))
        {
            return Err("team tools must be called alone, not in a parallel batch".into());
        }
        // Validate against the catalog actually advertised for this step, before any activation.
        for (index, (key, call)) in calls.iter().enumerate() {
            if *key != index as u64 || call.id.is_empty() || !ids.insert(&call.id) {
                return Err("missing or duplicate tool call ID or index".into());
            }
            let catalog = self.profile.catalog.borrow();
            let tool = catalog
                .tools
                .get(&call.name)
                .ok_or("missing or invalid tool name")?;
            // Go routes on offered calls in model order, including calls denied during dispatch.
            model_source = model_source.or(tool.target.model_source);
            if call.name == structured_output::NAME
                && self.profile.spec.output_tool.is_some()
                && call.arguments.len() > structured_output::MAX_ARGS
            {
                return Err("structured output arguments exceed 64 KiB".into());
            }
            wire_calls.push(json!({"id":call.id, "type":"function", "function":{"name":call.name, "arguments":call.arguments}}));
        }
        message["tool_calls"] = wire_calls.into();
        self.messages.push(message);
        let turn = self.active.as_mut().expect("active tool round");
        turn.current_calls = calls
            .values()
            .map(|call| {
                let args = serde_json::from_str::<Value>(&call.arguments)
                    .map(|mut value| {
                        value.sort_all_objects();
                        value.to_string()
                    })
                    .unwrap_or_else(|_| call.arguments.clone());
                (call.name.clone(), args)
            })
            .collect();
        turn.exempt_batch = calls
            .values()
            .all(|c| matches!(c.name.as_str(), "random_int" | "get_datetime"));
        turn.tool_model_source = model_source;
        turn.selection = self.profile.spec.picker.active.get();
        let exclusive = calls.len() == 1;
        if let Some(output) = &self.profile.spec.output_tool {
            // Go records intercepted replies first; none can reach approval, hooks or host dispatch.
            let mut final_answer = None;
            for call in calls.values().filter(|c| c.name == structured_output::NAME) {
                let result = output.call(&call.arguments, exclusive);
                let error = result.is_err();
                let content = result.unwrap_or_else(|e| e);
                events.push(call.event());
                events.push(tool_result(&call.id, &call.name, &content, error));
                if !error {
                    final_answer = Some(content.clone());
                }
                let stored = self
                    .frame
                    .stored_content(content, &self.profile.spec.hooks)?;
                self.messages
                    .push(json!({"role":"tool", "tool_call_id":call.id, "content":stored}));
            }
            if let Some(content) = final_answer {
                let visible = content
                    .split_once("<tool_call>")
                    .map_or(content.as_str(), |(text, _)| text);
                events.push(json!({"type":"delta", "message_id":format!("{}:output", turn.id), "content":visible}));
                self.messages
                    .push(json!({"role":"assistant", "content":content}));
                if let Some(error) = self.repetition_error() {
                    return Ok(json!({"events":events,"error":error}));
                }
                return Ok(self.finish(content.into(), events));
            }
        }
        for call in calls.into_values() {
            if self.profile.spec.output_tool.is_some() && call.name == structured_output::NAME {
                continue;
            }
            let index = self.messages.len();
            self.messages
                .push(json!({"role":"tool", "tool_call_id":call.id}));
            turn.queue.push_back(Invocation { call, index });
        }
        self.drain_calls(&mut events)?;
        Ok(self.continue_turn(events))
    }

    fn drain_calls(&mut self, events: &mut Vec<Value>) -> Result<(), String> {
        while let Some(mut op) = self.active.as_mut().unwrap().queue.pop_front() {
            if self
                .profile
                .spec
                .hooks
                .rewrite_input(&op.call.name, &mut op.call.arguments)?
            {
                events.push(json!({"type":"warning", "message":format!("redact_secrets: redacted secret material from arguments of tool {:?}", op.call.name)}));
            }
            let call = &op.call;
            let safe = self.profile.catalog.borrow().tools[&call.name].target.safe;
            match self.profile.spec.tool_policy.rules.verdict(
                self.profile.spec.tool_policy.safety,
                &call.name,
                &call.arguments,
                safe,
            ) {
                Verdict::Allow => self.dispatch(op, events)?,
                Verdict::Deny(reason) => self.reject(op, reason, events)?,
                Verdict::Ask
                    if self.profile.spec.tool_policy.grantable()
                        && self.profile.grants.borrow().contains(&call.name) =>
                {
                    self.dispatch(op, events)?
                }
                Verdict::Ask => {
                    let turn = self.active.as_mut().unwrap();
                    let token = turn.token("confirm", op.index);
                    events.push(json!({"type":"approval", "agent":self.profile.spec.name, "id":call.id, "name":call.name, "args":call.arguments, "token":token}));
                    turn.confirmation = Some((token, op));
                    return Ok(());
                }
            }
        }
        self.profile
            .spec
            .picker
            .active
            .set(self.active.as_ref().unwrap().selection);
        Ok(())
    }

    pub fn confirm(
        &mut self,
        token: &str,
        approved: bool,
        remember: bool,
        reason: &str,
    ) -> Result<Value, String> {
        let turn = self.active.as_mut().ok_or("no active turn")?;
        let (_, op) = turn
            .confirmation
            .take_if(|(id, _)| id == token)
            .ok_or("no matching tool confirmation")?;
        let mut events = Vec::new();
        if approved {
            if remember && self.profile.spec.tool_policy.grantable() {
                self.profile
                    .grants
                    .borrow_mut()
                    .insert(op.call.name.clone());
            }
            self.dispatch(op, &mut events)?;
        } else {
            let mut text = String::from("The user rejected the tool call.");
            if !reason.is_empty() {
                text.push_str(" Reason: ");
                text.push_str(reason);
            }
            self.reject(op, text, &mut events)?;
        }
        self.drain_calls(&mut events)?;
        Ok(self.continue_turn(events))
    }

    fn reject(
        &mut self,
        op: Invocation,
        reason: String,
        events: &mut Vec<Value>,
    ) -> Result<(), String> {
        let reply = Reply {
            index: op.index,
            name: op.call.name,
            toon: false,
        };
        events.push(self.complete(reply, reason, true)?);
        Ok(())
    }

    fn dispatch(&mut self, op: Invocation, events: &mut Vec<Value>) -> Result<(), String> {
        let call = op.call;
        let (source, toon) = {
            let catalog = self.profile.catalog.borrow();
            let tool = &catalog.tools[&call.name];
            (tool.target.host, tool.toon)
        };
        let reply = Reply {
            index: op.index,
            name: call.name.clone(),
            toon,
        };
        let turn = self.active.as_mut().unwrap();
        if let Some(source) = source {
            let token = turn.token("host", op.index);
            events.push(json!({"type":"host_call", "agent":self.profile.spec.name, "id":call.id, "name":call.name, "args":call.arguments, "source":source, "token":token}));
            turn.host_pending.insert(token, reply);
            return Ok(());
        }
        events.push(call.event());
        let result = match call.name.as_str() {
            "handoff" | "transfer_task" => {
                self.switch = Some(Switch::parse(&call.name, &call.arguments)?);
                return Ok(());
            }
            "think" => {
                let scratchpad = self
                    .scratchpads
                    .entry(self.profile.spec.name.clone())
                    .or_insert_with(|| "Thoughts:".into());
                let Think { thought } = call.parse()?;
                if scratchpad.len() + 1 + thought.len() > MAX_SCRATCHPAD_BYTES {
                    return Err("think scratchpad size limit reached".into());
                }
                scratchpad.push('\n');
                scratchpad.push_str(&thought);
                Ok(scratchpad.clone())
            }
            "user_prompt" => {
                if call.arguments.len() > MAX_ELICITATION_BYTES {
                    return Err("elicitation argument size limit reached".into());
                }
                let prompt: Prompt = call.parse()?;
                let id = turn.token("prompt", op.index);
                turn.pending.insert(id.clone(), reply);
                let meta = prompt
                    .title
                    .filter(|t| !t.is_empty())
                    .map(|t| json!({"cagent/title":t}));
                events.push(json!({"type":"elicitation", "id":id, "message":prompt.message, "schema":prompt.schema, "meta":meta, "server_elicitation_id":"", "mode":"", "url":""}));
                return Ok(());
            }
            discovery::SEARCH | discovery::ADD => {
                let search = call.name == discovery::SEARCH;
                let arg = crate::go_json::scalar(
                    &call.arguments,
                    "deferred",
                    if search {
                        "SearchToolArgs"
                    } else {
                        "AddToolArgs"
                    },
                    if search { "query" } else { "name" },
                    crate::go_json::read_string,
                );
                arg.and_then(|arg| {
                    let Catalog {
                        discovery, tools, ..
                    } = &mut *self.profile.catalog.borrow_mut();
                    if search {
                        discovery.search(&arg)
                    } else {
                        let (output, activated) = discovery.add(&arg)?;
                        if let Some(tool) = activated {
                            // The internal output tool follows all user tools, including newly activated ones.
                            if self.profile.spec.output_tool.is_some() {
                                tools.shift_insert(tools.len() - 1, arg, tool.clone());
                            } else {
                                tools.insert(arg, tool.clone());
                            }
                        }
                        Ok(output)
                    }
                })
            }
            "change_model" => self
                .profile
                .spec
                .picker
                .change(&call.arguments, &mut turn.selection),
            "revert_model" => {
                turn.selection = None;
                Ok("Model reverted to the agent's default model".into())
            }
            "list_sessions" | "read_session" => {
                let archive = self
                    .archive
                    .as_ref()
                    .ok_or("session history is not available in this runtime")?;
                if call.name == "list_sessions" {
                    archive.list(&call.arguments)
                } else {
                    archive.read(&call.arguments)
                }
            }
            "calculate" => crate::calculator::calculate(&call.arguments),
            "create_todo" | "create_todos" | "update_todos" | "list_todos" => self
                .profile
                .todos
                .borrow_mut()
                .call(&call.name, &call.arguments),
            _ => return Err("missing or invalid tool name".into()),
        };
        let is_error = result.is_err();
        events.push(self.complete(reply, result.unwrap_or_else(|e| e), is_error)?);
        Ok(())
    }

    fn complete(&mut self, reply: Reply, output: String, is_error: bool) -> Result<Value, String> {
        let (output, is_error) = if reply.toon {
            match discovery::render_toon(output) {
                Ok(text) => (text, is_error),
                Err(error) => (error, true),
            }
        } else {
            (output, is_error)
        };
        let category = self
            .profile
            .catalog
            .borrow()
            .tools
            .get(&reply.name)
            .and_then(|t| t.target.host)
            .and_then(|i| self.profile.spec.toolsets.get(i))
            .map_or(reply.name.as_str(), |s| s.kind.as_str());
        let (output, is_error) =
            match self
                .profile
                .spec
                .hooks
                .tool_result(&reply.name, category, output)
            {
                Ok(o) => (o, is_error),
                Err(e) => (e, true),
            };
        if is_error {
            self.active.as_mut().unwrap().exempt_batch = false;
        }
        let message = &mut self.messages[reply.index];
        let event = tool_result(
            message["tool_call_id"].as_str().unwrap(),
            &reply.name,
            &output,
            is_error,
        );
        message["content"] = self
            .frame
            .stored_content(output, &self.profile.spec.hooks)?;
        Ok(event)
    }

    pub fn include_usage(&mut self, usage: &Value) {
        let turn = self.active.as_mut().expect("suspended caller");
        turn.input += usage["input_tokens"].as_u64().unwrap_or(0);
        turn.output += usage["output_tokens"].as_u64().unwrap_or(0);
    }

    pub fn complete_switch(
        &mut self,
        name: &str,
        output: String,
        error: bool,
    ) -> Result<Value, String> {
        let output = self.profile.spec.hooks.tool_result(name, name, output)?;
        let stored = self
            .frame
            .stored_content(output.clone(), &self.profile.spec.hooks)?;
        let reply = self
            .messages
            .last_mut()
            .expect("reserved team tool message");
        let event = tool_result(
            reply["tool_call_id"].as_str().unwrap(),
            name,
            &output,
            error,
        );
        reply["content"] = stored;
        Ok(event)
    }

    fn repetition_error(&mut self) -> Option<String> {
        let turn = self.active.as_mut().unwrap();
        if !turn.exempt_batch && !turn.current_calls.is_empty() {
            turn.consecutive = if turn.previous_calls == turn.current_calls {
                turn.consecutive + 1
            } else {
                1
            };
            turn.previous_calls = std::mem::take(&mut turn.current_calls);
            if turn.consecutive >= self.frame.repetition_limit() {
                let reason=format!("Agent terminated: detected {} consecutive identical calls to {}. This indicates a degenerate loop where the model is not making progress.",turn.consecutive,turn.previous_calls[0].0);
                self.active = None;
                return Some(reason);
            }
        }
        None
    }

    fn continue_turn(&mut self, events: Vec<Value>) -> Value {
        if self.switch.is_some() {
            return json!({"events":events});
        }
        if self.active.as_ref().is_some_and(Turn::waiting) {
            return json!({"events":events, "waiting":true});
        }
        if let Some(error) = self.repetition_error() {
            return json!({"events":events,"error":error});
        }
        self.resume(events)
    }

    pub fn opener(&self) -> &str {
        &self.opener
    }

    pub fn at_cap(&self) -> bool {
        let max = self.frame.max_iterations as usize;
        max > 0 && self.iterations() >= max
    }

    // Reminders and forced handoffs are model calls without a tool round.
    fn iterations(&self) -> usize {
        let t = self.active.as_ref().expect("active turn");
        t.tool_rounds + t.reminders + t.forced
    }

    // Go checks the session cap, then the acting agent's hook, before each model call.
    pub fn resume(&mut self, mut events: Vec<Value>) -> Value {
        let t = self.active.as_ref().expect("active turn");
        if self.at_cap() {
            let max = self.frame.max_iterations;
            let text = format!(
                "Execution stopped after reaching the configured max_iterations limit ({max})."
            );
            events.push(json!({"type":"delta","content":text,"message_id":t.message_id}));
            let message = json!({"role":"assistant","content":text});
            self.messages.push(message.clone());
            let mut result = json!({"message":message});
            if t.input + t.output > 0 {
                result["usage"] = json!({"input_tokens":t.input,"output_tokens":t.output});
            }
            events.push(json!({"type":"finish","reason":"stop"}));
            self.active = None;
            return json!({"events":events,"result":result});
        }
        let calls = self.iterations();
        if let Some(limit) = self
            .profile
            .spec
            .hooks
            .max_calls
            .filter(|&n| calls >= n as usize)
        {
            self.active = None;
            return json!({"events":events, "error":format!("Agent terminated: max_iterations builtin reached its limit of {limit} model call(s).")});
        }
        if self.profile.spec.separate_model_attempts {
            // A later cancellation or projection error must not erase completed tool effects.
            let t = self.active.as_mut().expect("active tool round");
            t.messages_before = self.messages.len();
            t.scratchpads_before = self
                .scratchpads
                .iter()
                .map(|(k, s)| (k.clone(), s.len()))
                .collect();
            return json!({"events":events, "nextIteration":true});
        }
        match self.prepare_request() {
            Ok(body) => json!({"events":events, "request":body}),
            Err(error) => json!({"events":events,"error":error}),
        }
    }

    pub fn host_result(
        &mut self,
        token: &str,
        output: String,
        is_error: bool,
    ) -> Result<Value, String> {
        if output.len() > 16 * 1024 * 1024 {
            return Err("tool result exceeds 16 MiB".into());
        }
        let turn = self.active.as_mut().ok_or("no active turn")?;
        let reply = turn.host_pending.remove(token).ok_or("unknown host call")?;
        let event = self.complete(reply, output, is_error)?;
        Ok(self.continue_turn(vec![event]))
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
        let mut response = json!({"action":action});
        if let Some(data) = content.filter(|c| !c.is_empty()) {
            response["content"] = Value::Object(data);
        }
        let output = response.to_string();
        if output.len() > MAX_ELICITATION_BYTES {
            return Err("elicitation answer size limit reached".into());
        }
        let reply = turn
            .pending
            .remove(id)
            .ok_or("no pending elicitation with that ID")?;
        let event = self.complete(reply, output, action != "accept")?;
        Ok(self.continue_turn(vec![event]))
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
    fn event(&self) -> Value {
        json!({"type":"tool_call", "id":self.id, "name":self.name, "args":self.arguments})
    }

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

#[cfg(test)]
mod tests {
    use super::*;

    fn conversation(toolsets: &[&str]) -> Conversation {
        Conversation::new(
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
        let mut c = Conversation::new("sys".into(), &[], false, history.clone()).unwrap();
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
            assert!(crate::media::history(
                history,
                crate::media::Projection::new(None, "model", false),
                false
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
        let m = next["request"]["messages"].as_array().unwrap();
        let [assistant, first, second] = m.last_chunk::<3>().unwrap();
        assert_eq!(assistant["reasoning_content"], "why");
        assert_eq!(assistant["content"], "planning");
        assert_eq!(
            assistant["tool_calls"][0],
            json!({"id":"a", "type":"function", "function":{"name":"think", "arguments":r#"{"thought":"A"}"#}})
        );
        assert_eq!(
            *first,
            json!({"role":"tool", "content":"Thoughts:\nA", "tool_call_id":"a"})
        );
        assert_eq!(
            *second,
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
            assert!(c.scratchpads.is_empty());
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
            assert!(c.scratchpads.is_empty());
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
        Rc::get_mut(&mut c.profile).unwrap().spec.hooks = crate::hooks::Hooks::try_from(
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
        Rc::get_mut(&mut c.profile).unwrap().spec.hooks =
            crate::hooks::Hooks::default().with_defaults(true, true, true);
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
        c.restart(crate::session_context::Stamp {
            id: "new".into(),
            created_at: "".into(),
        })
        .unwrap();
        c.begin("new", "b").unwrap();
        let req = c.prepare(Some("env".into()), Some("day 3".into())).unwrap();
        assert_eq!(req["messages"].as_array().unwrap().len(), 2);
    }

    #[test]
    fn stale_commands_cannot_resume_a_reused_model_call_or_host_slot() {
        let mut c = conversation(&["think", "datetime"]);
        Rc::get_mut(&mut c.profile).unwrap().spec.tool_policy.safety = permission::Safety::Strict;
        let start = |c: &mut Conversation| {
            c.begin("same prompt", "reused-id").unwrap();
            c.chunk(chunk(
                json!({"tool_calls":[call(0,"same","get_datetime","{}")]}),
                Some("tool_calls"),
            ))
            .unwrap();
            c.end().unwrap()["events"][0]["token"]
                .as_str()
                .unwrap()
                .to_owned()
        };
        let old = start(&mut c);
        let host = c.confirm(&old, true, false, "").unwrap()["events"][0]["token"]
            .as_str()
            .unwrap()
            .to_owned();
        c.cancel();
        let current = start(&mut c);
        assert_ne!(old, current);
        assert!(c.confirm(&old, true, false, "").is_err());
        let resumed = c.confirm(&current, true, false, "").unwrap();
        assert!(c.confirm(&current, true, false, "").is_err());
        assert!(c.host_result(&host, "stale".into(), false).is_err());
        let host = resumed["events"][0]["token"].as_str().unwrap();
        let done = c.host_result(host, "current".into(), false).unwrap();
        assert_eq!(
            done["request"]["messages"]
                .as_array()
                .unwrap()
                .last()
                .unwrap()["content"],
            "current"
        );
        assert!(c.host_result(host, "duplicate".into(), false).is_err());
    }

    #[test]
    fn repeated_tools_cannot_loop_forever_or_exhaust_the_scratchpad() {
        let mut c = conversation(&["think"]);
        c.begin("loop", "loop").unwrap();
        for i in 0..MAX_TOOL_ROUNDS {
            think_once(&mut c, &i.to_string());
        }
        c.chunk(chunk(
            json!({"tool_calls":[call(0,"a","think",r#"{"thought":""}"#)]}),
            Some("tool_calls"),
        ))
        .unwrap();
        assert!(c.end().unwrap_err().contains("iteration limit"));
        assert!(c.scratchpads.is_empty());
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
