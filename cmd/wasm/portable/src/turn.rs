use crate::chat_stream::{Chunk, Fragment, Object};
use crate::compaction::{Compactor, Prefix, Undo, Usage};
use crate::discovery::{self, Discovery, Tool};
use crate::permission::{self, Verdict};
use crate::structured_output;
use crate::{
    config::{RequestModel, Selected},
    team::Switch,
};
use indexmap::IndexMap;
use serde::Deserialize;
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
    private_todo: Option<usize>,
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
    shared_todos: Rc<RefCell<crate::todo::Todos>>,
    private_todos: RefCell<BTreeMap<usize, crate::todo::Todos>>,
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
    // Go finds a delegate's last reply through the sub-session recorded beside its tool result.
    delegated: Vec<(usize, Option<String>)>,
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
    draining: bool,
    original: Option<Undo>,
    messages_before: usize,
    scratchpads_before: BTreeMap<String, usize>,
    profile_before: Rc<Profile>,
    id: String,
    message_id: String,
    tool_rounds: usize,
    reminders: usize,
    iterations: usize,
    // Whether this agent's previous model call in the run used tools.
    after_tools: bool,
    input: u64,
    output: u64,
    step: Step,
    queue: VecDeque<Invocation>,
    selection: Option<usize>,
    confirmation: Option<(String, Invocation)>,
    approval_queue: VecDeque<Invocation>,
    logged: Vec<(Reply, String)>,
    prompts: Vec<Value>,
    pending: BTreeMap<String, Reply>,
    host_pending: BTreeMap<String, Reply>,
    atomic_host: Option<String>,
    serial_batch: bool,
}

impl Turn {
    fn token(&self, kind: &str, slot: usize) -> String {
        format!(
            "{}:{kind}:{}:{}:{slot}",
            self.id, self.run_number, self.tool_rounds
        )
    }

    fn waiting(&self) -> bool {
        !self.approval_queue.is_empty()
            || self.confirmation.is_some()
            || !self.pending.is_empty()
            || !self.host_pending.is_empty()
    }
}

struct Invocation {
    call: Call,
    slot: usize,
    // Whether the tool was advertised for this step, before any activation in the same batch.
    available: bool,
    // Go's complaint about arguments that are not JSON; the tool reports it.
    malformed: Option<String>,
}

impl Invocation {
    fn rejected(self) -> Reply {
        Reply {
            id: self.call.id,
            name: self.call.name,
            toon: false,
            slot: self.slot,
        }
    }
}

struct Reply {
    slot: usize,
    id: String,
    name: String,
    toon: bool,
}

#[derive(Default)]
struct Prompt {
    message: String,
    title: String,
    schema: Option<Map<String, Value>>,
}

impl Prompt {
    // userprompt.Args as encoding/json fills it: unknown fields are ignored, a repeated
    // field decodes again and a repeated map merges. The inner error is model-facing.
    fn decode(input: &str) -> Result<Result<Self, String>, String> {
        use crate::go_json::{field_matches, field_path, fields, object, read_string, type_error};
        const TYPE: &str = "userprompt.Args";
        let mut prompt = Self::default();
        let mut unsupported = false;
        let mut decode = || -> Result<(), String> {
            let input = crate::go_json::normalize_json(input)?;
            let Some(obj) = object(&input, TYPE)? else {
                return Ok(());
            };
            fields(obj, |key, val| {
                let path = field_path(TYPE, &key);
                if field_matches(&key, "message") {
                    read_string(val, &mut prompt.message, &path)
                } else if field_matches(&key, "title") {
                    read_string(val, &mut prompt.title, &path)
                } else if !field_matches(&key, "schema") {
                    Ok(())
                } else if val == "null" {
                    prompt.schema = None;
                    Ok(())
                } else if val.starts_with('{') {
                    match serde_json::from_str::<Map<String, Value>>(val) {
                        Ok(schema) => prompt.schema.get_or_insert_default().extend(schema),
                        Err(_) => unsupported = true,
                    }
                    Ok(())
                } else {
                    Err(type_error(val, &path, "map[string]interface {}"))
                }
            })
        };
        let decoded = decode();
        if unsupported {
            // Go would name the offending number's path or accept deeper nesting.
            return Err("user_prompt schema is too deep or has a number out of range".into());
        }
        Ok(decoded.map(|()| prompt))
    }
}

// fetch.ToolArgs as Go decodes it, including aijson's array repairs, re-encoded for the host
// tool. `Err` is the decoder's model-facing complaint.
fn fetch_arguments(input: &str) -> Result<String, String> {
    use crate::go_json::{field_matches, field_path, fields, object, read_string, type_error};
    const TYPE: &str = "fetch.ToolArgs";
    let mut urls = crate::todo::GoSlice::<String>::default();
    let (mut format, mut timeout) = (String::new(), 0_i64);
    let input = crate::go_json::normalize_json(input)?;
    if let Some(obj) = object(&input, TYPE)? {
        let mut decode = |s: &str| {
            fields(s, |key, val| {
                let path = field_path(TYPE, &key);
                if field_matches(&key, "urls") {
                    urls.decode(val, &format!("ToolArgs.{key}"), "[]string", read_string)
                } else if field_matches(&key, "format") {
                    read_string(val, &mut format, &path)
                } else if field_matches(&key, "timeout") && val != "null" {
                    timeout = val.parse().map_err(|_| {
                        if val.starts_with(|c: char| c.is_ascii_digit() || c == '-') {
                            format!("json: cannot unmarshal number {val} into {path} of type int")
                        } else {
                            type_error(val, &path, "int")
                        }
                    })?;
                    Ok(())
                } else {
                    Ok(())
                }
            })
        };
        if let Err(error) = decode(obj) {
            // aijson retries the repaired object into the same partially decoded struct.
            let repaired = (|| {
                let mut raw: Map<String, Value> = serde_json::from_str(obj).ok()?;
                let list = raw
                    .remove("urls")
                    .filter(|v| !v.is_array() && !v.is_null())?;
                raw.insert("urls".into(), crate::todo::repair_slice(list, true)?);
                // Go re-encodes the object through float64: 2.0 comes back as 2.
                if let Some(n) = raw.get("timeout").and_then(Value::as_f64) {
                    if n.fract() == 0.0 && n.abs() < 9e15 {
                        raw.insert("timeout".into(), (n as i64).into());
                    }
                }
                serde_json::to_string(&raw).ok()
            })();
            if repaired.is_none_or(|s| decode(&s).is_err()) {
                return Err(error);
            }
        }
    }
    let mut args = json!({"urls":urls.finish()});
    if !format.is_empty() {
        args["format"] = format.into();
    }
    if timeout != 0 {
        args["timeout"] = timeout.into();
    }
    Ok(args.to_string())
}

fn api_arguments(method: &Value, input: &str) -> Result<String, String> {
    use crate::go_json::{fields, map_field_path, normalize_json, object, read_string};
    if method != "GET" && method != "POST" {
        return Ok(String::new());
    }
    if input.len() > 1 << 20 {
        return Err("API argument size limit reached".into());
    }
    let input = normalize_json(input)?;
    if method == "POST" {
        // The host decodes map[string]any; preserve the original number and duplicate-key tokens.
        return Ok(input.into_owned());
    }
    if input.is_empty() {
        return Ok(String::new()); // Go skips the second expansion entirely.
    }
    let mut values = BTreeMap::new();
    if let Some(obj) = object(&input, "map[string]string")? {
        fields(obj, |key, raw| {
            let mut value = String::new();
            read_string(raw, &mut value, &map_field_path(&key))?;
            values.insert(key, value);
            Ok(())
        })?;
    }
    Ok(serde_json::to_string(&values).unwrap())
}

#[derive(Default)]
struct Step {
    text: String,
    reasoning: String,
    calls: IndexMap<u64, Call>,
    finish: Option<String>,
    terminal: bool,
    implicit_stop: bool,
    ambiguous: bool,
    usage: Option<Usage>,
}

#[derive(Default)]
struct Call {
    id: String,
    name: String,
    arguments: String,
    renamed: bool,
}

// Schedule only network calls known to reach I/O, not synchronous decoder or policy failures.
// This is a fast-path hint; dispatch still validates arguments and enforces approval and egress.
fn simple_network_call(spec: &crate::config::Toolset, arguments: &str) -> bool {
    if spec.options.get("allow_private_ips") != Some(&Value::Bool(true)) {
        return false;
    }
    // MCP can initiate its own elicitation from inside the transport.
    if spec.kind == "mcp" || spec.options.contains_key("headers") {
        return false;
    }
    let empty = arguments.trim().is_empty() || arguments.trim() == "{}";
    if spec.kind == "api" {
        let api = &spec.options["api_config"];
        return empty
            && api["required"].as_array().is_none_or(Vec::is_empty)
            && api["headers"].is_null()
            && api["endpoint"]
                .as_str()
                .is_some_and(|url| !url.contains('$'));
    }
    if arguments.len() > 16384
        || arguments.contains('\\')
        || spec.options.contains_key("blocked_domains")
        || spec.options.contains_key("allowed_domains")
    {
        return false;
    }
    let Ok(Value::Object(args)) = serde_json::from_str::<Value>(arguments) else {
        return false;
    };
    let Some([url]) = args
        .get("urls")
        .and_then(Value::as_array)
        .map(Vec::as_slice)
    else {
        return false;
    };
    args.len() == 1
        && arguments.matches("\"urls\"").count() == 1
        && url
            .as_str()
            .and_then(|s| s.parse::<http::Uri>().ok())
            .is_some_and(|uri| {
                matches!(uri.scheme_str(), Some("http" | "https"))
                    && uri.host().is_some_and(|h| !h.is_empty())
                    && uri.authority().is_some_and(|a| !a.as_str().contains('@'))
            })
}

impl Profile {
    pub fn new(
        spec: Selected,
        shared_todos: Rc<RefCell<crate::todo::Todos>>,
    ) -> Result<Self, String> {
        Ok(Self {
            catalog: RefCell::new(Catalog::build(&spec, None)?),
            grants: Default::default(),
            spec,
            shared_todos,
            private_todos: Default::default(),
        })
    }

    fn attempt_model(&self, index: usize, source: Option<usize>) -> &RequestModel {
        let configured = &self.spec.models[index].request;
        if index != 0 {
            return configured;
        }
        source
            .and_then(|i| self.spec.tool_models.get(&self.spec.toolsets[i].model))
            .or_else(|| self.spec.picker.selected())
            .unwrap_or(configured)
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
        // Like a blank prompt, a whitespace-only instruction never reaches a request.
        if !agent.instruction.trim().is_empty() {
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
                {
                    continue;
                }
                if !agent.tool_policy.visible(name, safe)? {
                    withheld = true;
                    continue;
                }
                let hidden = all || names.iter().any(|n| n == name);
                let profile = (hosted, hidden, spec.toon.matches(name), spec.shared);
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
                        private_todo: (spec.kind == "todo" && !spec.shared).then_some(index),
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
            // Like Go, a filtered toolset keeps its instructions; one restricted mode hides does not.
            let custom = !spec.instruction.is_empty()
                && !spec.instruction.contains("{ORIGINAL_INSTRUCTIONS}");
            if !text.trim().is_empty() && (!withheld || custom) {
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
                                private_todo: None,
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
                        private_todo: None,
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
            delegated: Vec::new(),
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
        self.hand_off(profile);
        self.messages.push(json!({"role":"user","content":note}));
        self.active
            .as_mut()
            .expect("forced continuation")
            .tool_model_source = None;
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
        self.delegated.clear();
        self.routed_seed = None;
        self.scratchpads.clear();
        self.compaction.truncate(0);
        self.compaction.context_tokens = 0;
        self.compaction.seed_estimates_lost = false;
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
            approval_queue: VecDeque::new(),
            logged: Vec::new(),
            prompts: Vec::new(),
            selection: self.profile.spec.picker.active.get(),
            tool_model_source: None,
            summarizing: None,
            draining: false,
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
            iterations: 0,
            after_tools: false,
            input: 0,
            output: 0,
            step: Step::default(),
            pending: BTreeMap::new(),
            host_pending: BTreeMap::new(),
            atomic_host: None,
            serial_batch: false,
        });
        // Go keeps a whitespace-only prompt out of every request. Under a history limit it
        // still counts as an item, and `project` drops it after trimming.
        if !prompt.trim().is_empty()
            || self
                .frame
                .acting(&self.profile.spec.limits)
                .limits_history()
        {
            self.messages.push(json!({"role":"user", "content":prompt}));
        }
        self.prepare_request()
    }

    pub fn set_seed_estimates_lost(&mut self, lost: bool) {
        self.compaction.seed_estimates_lost = lost;
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

    pub fn attempt_model(&self, index: usize) -> &RequestModel {
        let turn = self.active.as_ref().expect("active model attempt");
        self.profile.attempt_model(index, turn.tool_model_source)
    }

    fn target(&self) -> &crate::config::Target {
        &self.profile.spec.models[self.attempt.current]
    }

    pub fn summarizing(&self) -> bool {
        self.active
            .as_ref()
            .is_some_and(|t| t.summarizing.is_some())
    }

    fn prepare_request(&mut self) -> Result<Value, String> {
        match self.summary_request()? {
            Some(request) => Ok(request),
            None => self.request(),
        }
    }

    pub fn summary_request(&mut self) -> Result<Option<Value>, String> {
        // The summary never inherits a serving fallback's parameters.
        let (_, summary) = self.profile.spec.summary_target();
        self.compaction.policy = self.profile.spec.models[0].compaction;
        let model = self
            .profile
            .spec
            .picker
            .selected()
            .unwrap_or(&summary.request);
        let turn = self.active.as_mut().expect("active turn");
        if let Some((prefix, mut request)) = self.compaction.plan(
            &self.messages,
            model,
            summary.compaction,
            summary.merge_messages,
            turn.summarizing.as_ref(),
        )? {
            turn.summarizing = Some(prefix);
            turn.step = Step::default();
            self.profile.spec.hooks.rewrite_messages(
                request["messages"]
                    .as_array_mut()
                    .expect("summary messages"),
            )?;
            Ok(Some(request))
        } else {
            Ok(None)
        }
    }

    pub fn discard_summary_output(&mut self) {
        self.active.as_mut().expect("active summary").step = Step::default();
    }

    pub fn skip_summary(&mut self, overflow: bool) -> Result<(), String> {
        self.discard_summary_output();
        let turn = self.active.as_mut().expect("active summary");
        let spec = &self.profile.spec;
        if !overflow
            && turn.summarizing.as_ref().is_some_and(|p| p.retries == 0)
            && (!spec.toolsets.is_empty() || spec.output_tool.is_some())
        {
            return Err("cannot silently continue with tools enabled".into());
        }
        turn.summarizing = None;
        turn.draining = true;
        Ok(())
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
        let prompt_len = messages.len();
        if let Some(seed) = &mut self.routed_seed {
            let target = &self.profile.spec.models[self.attempt.current];
            messages.extend_from_slice(seed.for_target(target.media_projection())?);
        }
        messages.extend_from_slice(&self.messages);
        self.frame
            .acting(&self.profile.spec.limits)
            .project(&mut messages, &self.profile.spec.hooks)?;
        let model = self.attempt_model(self.attempt.current);
        if model.schema_instruction.is_some() {
            // Go normalizes blank messages before selecting the leading role.
            messages.retain(|m| {
                m["role"] == "tool"
                    || !m["tool_calls"].is_null()
                    || m["content"].as_str().is_none_or(|s| !s.trim().is_empty())
            });
        }
        if messages.is_empty() {
            if model.schema_instruction.is_some() {
                return Err("Claude schema instruction needs a nonempty request".into());
            }
            // Go's provider refuses an empty request before any I/O, on each model of a chain.
            let scope = match self.profile.spec.models.len() {
                1 => "model failed",
                _ => "all models failed",
            };
            return Err(format!("{scope}: at least one message is required"));
        }
        if let Some(instruction) = &model.schema_instruction {
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
        if model.cache_breakpoints {
            let part = if self.target().responses {
                "input_text"
            } else {
                "text"
            };
            for count in [catalog.instructions.len(), prompt_len] {
                if let Some(m) = count.checked_sub(1).and_then(|i| messages.get_mut(i)) {
                    if m["role"] == "system" && m["content"].is_string() {
                        m["content"] = json!([{"type":part, "text":m["content"], "prompt_cache_breakpoint":{"mode":"explicit"}}]);
                    }
                }
            }
        }
        let mut body = Value::Object(model.model_params.clone());
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
        model.apply_reasoning(
            body.as_object_mut().unwrap(),
            !catalog.tools.is_empty(),
            model.cap,
        );
        if self.target().merge_messages {
            crate::wire::merge_messages(&mut body);
        }
        Ok(body)
    }

    pub fn chunk(&mut self, chunk: Value) -> Result<Vec<Value>, String> {
        let chunk = serde_json::from_value(chunk).map_err(|_| {
            self.cancel();
            "invalid stream chunk"
        })?;
        self.chat_chunk(chunk)
    }

    pub fn stream_state(&self) -> (bool, bool) {
        let turn = self.active.as_ref().expect("active stream");
        let step = &turn.step;
        (
            step.terminal && (!step.implicit_stop || turn.summarizing.is_some()),
            !step.calls.is_empty(),
        )
    }

    pub fn chat_chunk(&mut self, chunk: Chunk) -> Result<Vec<Value>, String> {
        let turn = self.active.as_mut().ok_or("no active turn")?;
        if turn.waiting() {
            return Err("waiting for elicitation".into());
        }
        if turn.summarizing.is_none() && self.profile.needs_host() {
            return Err("host catalog not registered".into());
        }
        let result = turn.step.chunk(chunk, &turn.message_id).map(|events| {
            if turn.summarizing.is_some() {
                Vec::new()
            } else {
                events
            }
        });
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
            self.delegated.retain(|(at, _)| *at < turn.messages_before);
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
        if turn.draining && !step.calls.is_empty() {
            return Err("compaction: cannot execute an unobserved continuation tool".into());
        }
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
        let reason = step.finish.clone();
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
        let model = self
            .profile
            .attempt_model(self.attempt.current, turn.tool_model_source);
        let blank = step.text.trim().is_empty();
        if step.calls.is_empty() {
            let refused = matches!(reason.as_deref(), Some("content_filter" | "refusal"));
            if refused || blank {
                let model = &model.id;
                let reason = match reason.as_deref() {
                    // Go infers a reason when the stream gave none it can use.
                    Some("tool_calls") => "null",
                    Some(reason) => reason,
                    None if !step.text.is_empty() => "stop",
                    None => "null",
                };
                let message = if refused {
                    format!("Model {model} refused to respond (stop reason: refusal).")
                } else if !step.reasoning.trim().is_empty() {
                    format!("Model {model} produced only reasoning and no reply (stop reason: {reason}). Thinking-mode models can emit reasoning tokens without a final answer; the reasoning is not used as the response. Send a follow-up to continue if the task is unfinished.")
                } else if reason == "stop" && turn.after_tools {
                    format!("Model {model} stopped without a final reply after using tools (stop reason: {reason}). Send a follow-up to continue if the task is unfinished.")
                } else {
                    format!("Model {model} returned an empty response (stop reason: {reason}). Send a follow-up to continue if the task is unfinished.")
                };
                // Go explains a turn without a visible reply before it reports usage.
                let message = self.profile.spec.models[self.attempt.current]
                    .transport
                    .safe_diagnostic(message)?;
                events.push(json!({"type":"warning", "message":message}));
            }
        }
        if let Some(usage) = step.usage {
            let model = if self.profile.spec.routed_summary() {
                &model.id
            } else {
                model.name()
            };
            let input = u64::from(usage.fresh_input()?);
            self.compaction.record(self.messages.len(), model, usage);
            let output = u64::from(usage.completion_tokens);
            turn.input += input;
            turn.output += output;
            events.push(json!({
                "type": "usage", "input_tokens": input, "output_tokens": output,
                "context_length": u64::from(usage.prompt_tokens) + output,
                "context_limit": self.profile.spec.context_window(), "cost": 0
            }));
        }
        turn.iterations += 1;
        let mut message = json!({"role":"assistant"});
        if !step.text.is_empty() || step.calls.is_empty() {
            message["content"] = Value::String(step.text);
        }
        if !step.reasoning.trim().is_empty() {
            message["reasoning_content"] = Value::String(step.reasoning);
        }
        match step.finish.as_deref() {
            // Go ends a turn without tool calls however its stream stopped.
            _ if step.calls.is_empty() => {
                turn.after_tools = false;
                // Go keeps a blank reply only as a usage record, never as history.
                if blank {
                    self.compaction.truncate(self.messages.len());
                } else {
                    self.messages.push(message);
                }
                if self.profile.spec.output_tool.is_some() {
                    let reminders = turn.reminders;
                    if turn.draining {
                        return Err("compaction: cannot drain tool-mode structured output".into());
                    }
                    turn.tool_model_source = None;
                    if let Some(error) = structured_output::stop_error(model.name(), reminders) {
                        self.messages
                            .push(json!({"role":"assistant", "content":error}));
                        self.active = None;
                        return Ok(json!({"events":events,"error":error}));
                    }
                    let turn = self.active.as_mut().unwrap();
                    turn.reminders += 1;
                    return Ok(self.continue_turn(events));
                }
                Ok(self.finish(events))
            }
            Some("stop" | "tool_calls") => {
                if turn.tool_rounds >= MAX_TOOL_ROUNDS {
                    return Err("tool iteration limit reached".into());
                }
                turn.tool_rounds += 1;
                turn.after_tools = true;
                for call in step.calls.values_mut() {
                    let missing = call.name.is_empty();
                    if missing {
                        call.name = "unknown_tool".into();
                    }
                    if (missing || call.renamed)
                        && self.profile.catalog.borrow().tools.contains_key(&call.name)
                    {
                        return Err("ambiguous streamed tool name".into());
                    }
                    if call.name.len() > 64
                        || !call
                            .name
                            .bytes()
                            .all(|b| b.is_ascii_alphanumeric() || b"-_".contains(&b))
                    {
                        return Err("invalid streamed tool name".into());
                    }
                }
                self.execute_calls(step.calls, message, events)
            }
            None => Err("incomplete stream: missing finish_reason".into()),
            Some(_) => Err("incomplete or unsupported tool call finish reason".into()),
        }
    }

    // Go answers with the last assistant message it kept, trimmed: after a blank reply that
    // is an earlier one, possibly a delegate's.
    pub fn last_reply(&self) -> Option<String> {
        for (i, m) in self.messages.iter().enumerate().rev() {
            if let Some((_, Some(reply))) = self.delegated.iter().rev().find(|(at, _)| *at == i) {
                return Some(reply.clone());
            }
            if m["role"] == "assistant" {
                return Some(m["content"].as_str().unwrap_or_default().trim().into());
            }
        }
        self.routed_seed.as_ref().and_then(|seed| seed.last_reply())
    }

    pub fn hand_off(&mut self, profile: Rc<Profile>) {
        // Tool activity and reminders belong to the acting agent, not the frame.
        if let Some(turn) = self
            .active
            .as_mut()
            .filter(|_| !Rc::ptr_eq(&self.profile, &profile))
        {
            turn.after_tools = false;
            turn.reminders = 0;
        }
        self.profile = profile;
    }

    pub fn delegated(&mut self, reply: Option<String>) {
        self.delegated.push((self.messages.len() - 1, reply));
    }

    fn finish(&mut self, mut events: Vec<Value>) -> Value {
        if !self.profile.spec.force_handoff.is_empty() {
            self.switch = Some(Switch::Forced(self.profile.spec.force_handoff.clone()));
            return json!({"events":events});
        }
        let content = self.last_reply().unwrap_or_default();
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
        calls: IndexMap<u64, Call>,
        mut message: Value,
        mut events: Vec<Value>,
    ) -> Result<Value, String> {
        let mut model_source = None;
        let mut ids = BTreeSet::new();
        let mut network = false;
        let mut serial = false;
        let mut interactive = 0;
        let mut malformed = Vec::with_capacity(calls.len());
        let mut wire_calls = Vec::with_capacity(calls.len());
        let mut dispatched = calls.values().filter(|c| {
            self.profile.spec.output_tool.is_none() || c.name != structured_output::NAME
        });
        if dispatched.clone().count() > 1
            && dispatched.any(|c| {
                matches!(c.name.as_str(), "transfer_task" | "handoff")
                    && self.profile.catalog.borrow().tools.contains_key(&c.name)
            })
        {
            return Err("team tools must be called alone, not in a parallel batch".into());
        }
        // Validate against the catalog actually advertised for this step, before any activation.
        for (index, call) in &calls {
            if *index >= calls.len() as u64 || call.id.is_empty() || !ids.insert(&call.id) {
                return Err("missing or duplicate tool call ID or index".into());
            }
            if let Some(tool) = self.profile.catalog.borrow().tools.get(&call.name) {
                // Go routes on offered calls in model order, including calls denied during dispatch.
                model_source = model_source.or(tool.target.model_source);
                if let Some(spec) = tool
                    .target
                    .host
                    .map(|i| &self.profile.spec.toolsets[i])
                    .filter(|s| matches!(s.kind.as_str(), "api" | "fetch" | "mcp"))
                {
                    network = true;
                    if calls.len() > 1 && !simple_network_call(spec, &call.arguments) {
                        serial = true;
                    }
                }
                serial |= matches!(call.name.as_str(), "change_model" | "revert_model");
                interactive += usize::from(
                    call.name == "user_prompt"
                        || self.profile.spec.tool_policy.may_ask(tool.target.safe),
                );
            }
            // Go reads empty arguments as {}.
            let complaint = match call.arguments.as_str() {
                "" => None,
                arguments => crate::go_json::syntax_error(arguments).map_err(|_| {
                    "tool arguments have a JSON error this runtime cannot report like Go"
                })?,
            };
            malformed.push(complaint);
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
        // Picker log writes can interleave; multiple interactive replies can race network I/O.
        turn.serial_batch = !exclusive && (serial || (network && interactive > 1));
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
                events.push(json!({"type":"delta", "message_id":format!("{}:output", turn.message_id), "content":visible}));
                self.messages
                    .push(json!({"role":"assistant", "content":content}));
                if let Some(error) = self.repetition_error() {
                    return Ok(json!({"events":events,"error":error}));
                }
                return Ok(self.finish(events));
            }
        }
        let first_slot = self.messages.len();
        for (call, malformed) in calls.into_values().zip(malformed) {
            if self.profile.spec.output_tool.is_some() && call.name == structured_output::NAME {
                continue;
            }
            let available = self.profile.catalog.borrow().tools.contains_key(&call.name);
            let slot = first_slot + turn.queue.len();
            if turn.serial_batch {
                self.messages
                    .push(json!({"role":"tool", "tool_call_id":call.id}));
            }
            turn.queue.push_back(Invocation {
                call,
                slot,
                available,
                malformed,
            });
        }
        self.drain_calls(&mut events)?;
        Ok(self.continue_turn(events))
    }

    fn drain_calls(&mut self, events: &mut Vec<Value>) -> Result<(), String> {
        loop {
            let turn = self.active.as_mut().unwrap();
            if turn.serial_batch && turn.confirmation.is_some() {
                break;
            }
            // A local host call runs to completion before another Go goroutine can start.
            if turn.atomic_host.is_some() {
                break;
            }
            if let Some(op) = turn.queue.pop_front() {
                self.start_call(op, events)?;
                continue;
            }
            // The elicitation bridge delivers on a newly queued goroutine.
            events.append(&mut turn.prompts);
            if turn.confirmation.is_none()
                && turn.pending.is_empty()
                && turn.approval_queue.is_empty()
            {
                self.flush_logs(events)?;
            }
            break;
        }
        self.profile
            .spec
            .picker
            .active
            .set(self.active.as_ref().unwrap().selection);
        Ok(())
    }

    fn start_call(&mut self, mut op: Invocation, events: &mut Vec<Value>) -> Result<(), String> {
        if !op.available {
            let reason = format!(
                "Tool '{}' is not available. You can only use the tools provided to you.",
                op.call.name
            );
            return self.logged_error(op.rejected(), reason, events);
        }
        // Like Go, input redaction only understands JSON; the message redaction still applies.
        if op.malformed.is_none()
            && self
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
            Verdict::Ask if self.granted(&op.call.name) => self.dispatch(op, events)?,
            Verdict::Ask => {
                if self.active.as_ref().unwrap().confirmation.is_some() {
                    self.active.as_mut().unwrap().approval_queue.push_back(op);
                } else {
                    self.ask(op, events);
                }
            }
        }
        Ok(())
    }

    fn granted(&self, name: &str) -> bool {
        self.profile.spec.tool_policy.grantable() && self.profile.grants.borrow().contains(name)
    }

    fn ask(&mut self, op: Invocation, events: &mut Vec<Value>) {
        let turn = self.active.as_mut().unwrap();
        let token = turn.token("confirm", op.slot);
        events.push(json!({"type":"approval", "agent":self.profile.spec.name, "id":op.call.id, "name":op.call.name, "args":op.call.arguments, "token":token}));
        turn.confirmation = Some((token, op));
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
        events.push(self.complete(op.rejected(), reason, true)?);
        Ok(())
    }

    fn dispatch(&mut self, op: Invocation, events: &mut Vec<Value>) -> Result<(), String> {
        let call = op.call;
        let (source, toon, private_todo) = {
            let catalog = self.profile.catalog.borrow();
            let tool = &catalog.tools[&call.name];
            (tool.target.host, tool.toon, tool.target.private_todo)
        };
        let reply = Reply {
            slot: op.slot,
            id: call.id.clone(),
            name: call.name.clone(),
            toon,
        };
        let kind = source.map(|i| self.profile.spec.toolsets[i].kind.as_str());
        let api_method = source
            .filter(|_| kind == Some("api"))
            .map(|i| &self.profile.spec.toolsets[i].options["api_config"]["method"]);
        // These Go handlers never look at their arguments.
        let ignored = matches!(
            call.name.as_str(),
            "revert_model" | "list_todos" | "list_plans"
        ) || (call.name == "list_sessions" && call.arguments.trim().is_empty())
            || api_method.is_some_and(|m| m != "GET" && m != "POST");
        if let Some(complaint) = op.malformed.as_ref().filter(|_| !ignored) {
            // Every other handler decodes first and answers the model with the decoder's complaint.
            let context = match (kind, call.name.as_str()) {
                (Some("mcp"), _) => "failed to parse tool arguments: ",
                (Some("api"), _)
                | (
                    None,
                    "transfer_task" | "handoff" | "change_model" | "list_sessions" | "read_session",
                ) => "invalid arguments: ",
                _ => "",
            };
            let output = format!("Error calling tool: {context}{complaint}");
            events.push(call.event());
            self.handler_result(reply, Err(output), events)?;
            return Ok(());
        }
        if let Some(source) = source {
            // The host tool reads `input` when what Go decodes differs from the raw text.
            let input = if op.malformed.is_some() {
                Some(String::new())
            } else if kind == Some("mcp") {
                match crate::go_json::normalize_json(&call.arguments)? {
                    std::borrow::Cow::Owned(text) => Some(text),
                    std::borrow::Cow::Borrowed(_) => None,
                }
            } else if kind == Some("fetch") || api_method.is_some() {
                let decoded = match api_method {
                    Some(method) => api_arguments(method, &call.arguments)
                        .map_err(|e| format!("invalid arguments: {e}")),
                    None => fetch_arguments(&call.arguments),
                };
                match decoded {
                    Ok(input) => Some(input),
                    Err(complaint) => {
                        events.push(call.event());
                        let output = format!("Error calling tool: {complaint}");
                        self.handler_result(reply, Err(output), events)?;
                        return Ok(());
                    }
                }
            } else {
                None
            };
            let turn = self.active.as_mut().unwrap();
            let token = turn.token("host", op.slot);
            let network = matches!(kind, Some("api" | "fetch" | "mcp"));
            if !network && !turn.serial_batch {
                turn.atomic_host = Some(token.clone());
            }
            let mut event = json!({"type":"host_call", "agent":self.profile.spec.name, "id":call.id, "name":call.name, "args":call.arguments, "source":source, "network":network, "token":token});
            if let Some(input) = input {
                event["input"] = input.into();
            }
            events.push(event);
            turn.host_pending.insert(token, reply);
            return Ok(());
        }
        let turn = self.active.as_mut().unwrap();
        events.push(call.event());
        let result = match call.name.as_str() {
            "handoff" | "transfer_task" => {
                if call.arguments.len() > 64 * 1024 {
                    return Err(format!("{} arguments exceed 64 KiB", call.name));
                }
                match Switch::parse(&call.name, &call.arguments) {
                    Ok(switch) => {
                        if !turn.serial_batch {
                            self.messages
                                .push(json!({"role":"tool", "tool_call_id":call.id}));
                        }
                        self.switch = Some(switch);
                        return Ok(());
                    }
                    Err(complaint) => Err(complaint),
                }
            }
            "think" => {
                let scratchpad = self
                    .scratchpads
                    .entry(self.profile.spec.name.clone())
                    .or_insert_with(|| "Thoughts:".into());
                if call.arguments.len() > MAX_SCRATCHPAD_BYTES {
                    return Err("think scratchpad size limit reached".into());
                }
                let decoded = crate::go_json::scalar(
                    &call.arguments,
                    "think.Args",
                    "thought",
                    crate::go_json::read_string,
                    "",
                );
                match decoded {
                    Ok(thought) => {
                        if scratchpad.len() + 1 + thought.len() > MAX_SCRATCHPAD_BYTES {
                            return Err("think scratchpad size limit reached".into());
                        }
                        scratchpad.push('\n');
                        scratchpad.push_str(&thought);
                        Ok(scratchpad.clone())
                    }
                    Err(complaint) => Err(complaint),
                }
            }
            "user_prompt" => {
                if call.arguments.len() > MAX_ELICITATION_BYTES {
                    return Err("elicitation argument size limit reached".into());
                }
                match Prompt::decode(&call.arguments)? {
                    Ok(prompt) => {
                        let id = turn.token("prompt", op.slot);
                        turn.pending.insert(id.clone(), reply);
                        let meta = (!prompt.title.is_empty())
                            .then(|| json!({"cagent/title":prompt.title}));
                        let prompts = if turn.serial_batch {
                            &mut *events
                        } else {
                            &mut turn.prompts
                        };
                        prompts.push(json!({"type":"elicitation", "id":id, "message":prompt.message, "schema":prompt.schema, "meta":meta, "server_elicitation_id":"", "mode":"", "url":""}));
                        return Ok(());
                    }
                    Err(complaint) => Err(format!("Error calling tool: {complaint}")),
                }
            }
            discovery::SEARCH | discovery::ADD => {
                let search = call.name == discovery::SEARCH;
                let arg = crate::go_json::scalar(
                    &call.arguments,
                    if search {
                        "deferred.SearchToolArgs"
                    } else {
                        "deferred.AddToolArgs"
                    },
                    if search { "query" } else { "name" },
                    crate::go_json::read_string,
                    "",
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
            "create_todo" | "create_todos" | "update_todos" | "list_todos" => {
                let mut store = if let Some(i) = private_todo {
                    std::cell::RefMut::map(self.profile.private_todos.borrow_mut(), |m| {
                        m.entry(i).or_default()
                    })
                } else {
                    self.profile.shared_todos.borrow_mut()
                };
                store.call(&call.name, &call.arguments)
            }
            _ => return Err("missing or invalid tool name".into()),
        };
        self.handler_result(reply, result, events)
    }

    fn handler_result(
        &mut self,
        reply: Reply,
        result: Result<String, String>,
        events: &mut Vec<Value>,
    ) -> Result<(), String> {
        match result {
            Err(error) if error.starts_with("Error calling tool: ") => {
                self.logged_error(reply, error, events)
            }
            result => {
                let is_error = result.is_err();
                events.push(self.complete(reply, result.unwrap_or_else(|e| e), is_error)?);
                Ok(())
            }
        }
    }

    fn logged_error(
        &mut self,
        reply: Reply,
        error: String,
        events: &mut Vec<Value>,
    ) -> Result<(), String> {
        let turn = self.active.as_mut().unwrap();
        if turn.serial_batch {
            events.push(self.complete(reply, error, true)?);
        } else {
            turn.logged.push((reply, error));
        }
        Ok(())
    }

    // Browser stderr re-enters Go. Writers resume FIFO only once all runnable work is idle.
    fn flush_logs(&mut self, events: &mut Vec<Value>) -> Result<(), String> {
        for (reply, error) in std::mem::take(&mut self.active.as_mut().unwrap().logged) {
            events.push(self.complete(reply, error, true)?);
        }
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
        let event = tool_result(&reply.id, &reply.name, &output, is_error);
        let stored = self
            .frame
            .stored_content(output, &self.profile.spec.hooks)?;
        if self.active.as_ref().unwrap().serial_batch {
            self.messages[reply.slot]["content"] = stored;
        } else {
            self.messages
                .push(json!({"role":"tool", "tool_call_id":reply.id, "content":stored}));
        }
        Ok(event)
    }

    pub fn poll_batch(&mut self) -> Result<Value, String> {
        let turn = self.active.as_mut().ok_or("no active turn")?;
        if !turn.waiting() || turn.atomic_host.is_some() {
            return Err("tool batch is not idle".into());
        }
        let mut events = Vec::new();
        if turn.confirmation.is_none() {
            if let Some(op) = turn.approval_queue.pop_front() {
                if self.granted(&op.call.name) {
                    self.dispatch(op, &mut events)?;
                } else {
                    self.ask(op, &mut events);
                }
                self.drain_calls(&mut events)?;
                return Ok(self.continue_turn(events));
            }
        }
        self.flush_logs(&mut events)?;
        Ok(self.continue_turn(events))
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
        let turn = self.active.as_ref().expect("active turn");
        if turn.waiting() {
            return json!({"events":events, "waiting":true, "serial":turn.serial_batch, "logged":!turn.logged.is_empty(), "unlocked":turn.confirmation.is_none() && !turn.approval_queue.is_empty()});
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
        max > 0 && self.active.as_ref().expect("active turn").iterations >= max
    }

    // Go checks the session cap, then the acting agent's hook, before each model call.
    pub fn resume(&mut self, mut events: Vec<Value>) -> Value {
        let capped = self.at_cap();
        let t = self.active.as_mut().expect("active turn");
        t.message_id = format!("{}:{}", t.id, t.iterations);
        if capped {
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
        if let Some(limit) = self
            .profile
            .spec
            .hooks
            .max_calls
            .filter(|&n| t.iterations >= n as usize)
        {
            self.active = None;
            return json!({"events":events, "error":format!("Agent terminated: max_iterations builtin reached its limit of {limit} model call(s).")});
        }
        if self.profile.spec.separate_model_attempts {
            // A later cancellation or projection error must not erase completed tool effects.
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
        if turn.atomic_host.as_deref() == Some(token) {
            turn.atomic_host = None;
        }
        let mut events = Vec::new();
        self.handler_result(
            reply,
            if is_error { Err(output) } else { Ok(output) },
            &mut events,
        )?;
        if !self.active.as_ref().unwrap().serial_batch {
            self.drain_calls(&mut events)?;
        }
        Ok(self.continue_turn(events))
    }

    pub fn answer_output(action: &str, content: Option<&str>) -> Result<String, String> {
        if !matches!(action, "accept" | "decline" | "cancel") {
            return Err(format!(
                "unknown elicitation action {} (accept, decline, cancel)",
                crate::go_json::quote(action)
            ));
        }
        let mut response = json!({"action":action});
        if let Some(raw) = content {
            let normalized = crate::go_json::normalize_json(raw)?;
            let data: Map<String, Value> =
                serde_json::from_str(&normalized).map_err(|e| e.to_string())?;
            if !data.is_empty() {
                response["content"] = Value::Object(data);
            }
        }
        // Keep the admission limit on the unescaped response, not its expanded Go spelling.
        if response.to_string().len() > MAX_ELICITATION_BYTES {
            return Err("elicitation answer size limit reached".into());
        }
        Ok(crate::go_json::encode_decoded(&response))
    }

    pub fn answer(
        &mut self,
        id: &str,
        action: &str,
        content: Option<&str>,
    ) -> Result<Value, String> {
        let output = Self::answer_output(action, content)?;
        let turn = self.active.as_mut().ok_or("no active turn")?;
        let reply = turn
            .pending
            .remove(id)
            .ok_or("no pending elicitation with that ID")?;
        let mut events = vec![self.complete(reply, output, action != "accept")?];
        if !self.active.as_ref().unwrap().serial_batch {
            self.drain_calls(&mut events)?;
        }
        Ok(self.continue_turn(events))
    }
}

fn tool_result(id: &str, name: &str, output: &str, is_error: bool) -> Value {
    json!({"type":"tool_result", "id":id, "name":name, "output":output, "is_error":is_error})
}

impl Step {
    fn chunk(&mut self, chunk: Chunk, id: &str) -> Result<Vec<Value>, String> {
        let choices = chunk.choices.unwrap_or_default();
        if self.terminal && !self.implicit_stop && !choices.is_empty() {
            return Err("model sent a choice after terminal usage".into());
        }
        let usage = chunk.usage.is_some();
        if usage && !self.terminal {
            self.usage = chunk.usage.map(|u| u.0);
        }
        let mut choices = choices.into_iter();
        let Some(Object(choice)) = choices.next() else {
            if usage && matches!(self.finish.as_deref(), None | Some("null")) {
                // A usage trailer synthesizes stop, but cannot authorize an unfinished call.
                if !self.calls.is_empty() {
                    return Err("incomplete stream: missing finish_reason".into());
                }
                self.finish = Some("stop".into());
                self.implicit_stop = true;
            }
            self.terminal |= usage && self.terminal_reason();
            return Ok(Vec::new());
        };
        // Secondary choices can overwrite the Go adapter's index-to-ID map or held finish reason.
        if choices.any(|Object(c)| {
            c.finish_reason
                .as_ref()
                .is_some_and(|r| !r.is_empty() && Some(r) != choice.finish_reason.as_ref())
                || c.delta
                    .is_some_and(|Object(d)| d.tool_calls.is_some_and(|v| !v.is_empty()))
        }) {
            return Err("ambiguous multi-choice stream".into());
        }
        let mut events = Vec::new();
        let Object(delta) = choice.delta.unwrap_or_default();
        self.ambiguous |= delta.ambiguous()
            || delta
                .role
                .as_deref()
                .is_some_and(|r| !matches!(r, "" | "assistant"));
        if self.ambiguous
            && (!self.calls.is_empty() || delta.tool_calls.as_ref().is_some_and(|v| !v.is_empty()))
        {
            return Err(
                "ambiguous role, refusal or legacy function_call beside a tool call".into(),
            );
        }
        for Object(fragment) in delta.tool_calls.as_deref().unwrap_or_default() {
            if self.finish.is_some() {
                return Err("tool call after finish_reason".into());
            }
            let index = fragment.index.unwrap_or(0);
            if index >= MAX_PARALLEL_CALLS {
                return Err("invalid tool call index".into());
            }
            let call = self.calls.entry(index).or_insert_with(|| Call {
                id: fragment.id.clone().unwrap_or_default(),
                ..Call::default()
            });
            if let Some(event) = call.append(fragment)? {
                events.push(event);
            }
        }
        if let Some(s) = delta.reasoning() {
            self.reasoning.push_str(s);
            events.push(json!({"type":"delta", "message_id":id, "reasoning":s}));
        }
        if !delta.content.is_empty() {
            self.text.push_str(&delta.content);
            events.push(json!({"type":"delta", "message_id":id, "content":delta.content}));
        }
        if let Some(reason) = choice.finish_reason.filter(|s| !s.is_empty()) {
            if self.finish.is_some()
                && !self.calls.is_empty()
                && self.finish.as_deref() != Some(&reason)
            {
                return Err("conflicting tool call finish reasons".into());
            }
            // A held stop survives a non-terminal choice, even one carrying usage.
            self.terminal |= usage && terminal_reason(&reason);
            if !self.terminal_reason() || terminal_reason(&reason) {
                self.finish = Some(reason);
            }
        }
        Ok(events)
    }

    fn terminal_reason(&self) -> bool {
        self.finish.as_deref().is_some_and(terminal_reason)
    }
}

fn terminal_reason(reason: &str) -> bool {
    matches!(reason, "stop" | "length" | "content_filter" | "refusal")
}

impl Call {
    fn event(&self) -> Value {
        json!({"type":"tool_call", "id":self.id, "name":self.name, "args":self.arguments})
    }

    fn append(&mut self, fragment: &Fragment) -> Result<Option<Value>, String> {
        if fragment
            .kind
            .as_deref()
            .is_some_and(|t| !matches!(t, "" | "function"))
        {
            return Err("unsupported tool call type".into());
        }
        if fragment
            .id
            .as_deref()
            .is_some_and(|id| !id.is_empty() && self.id != id)
        {
            return Err("tool call ID changed while streaming".into());
        }
        let Some(Object(f)) = &fragment.function else {
            return Ok(None);
        };
        let learning_name = self.name.is_empty();
        if let Some(name) = f.name.as_ref().filter(|s| !s.is_empty()) {
            self.renamed |= !learning_name && self.name != *name;
            self.name.clone_from(name);
        }
        let args = f.arguments.as_deref().unwrap_or_default();
        self.arguments.push_str(args);
        if self.name.is_empty() || self.id.is_empty() || (!learning_name && args.is_empty()) {
            return Ok(None);
        }
        Ok(Some(
            json!({"type":"tool_call_delta", "id":self.id, "name":self.name, "arguments":args}),
        ))
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
                    call(1, "a", "think", r#"{"thought":"#), call(0, "b", "think", r#"{"th"#)
                ]}),
                None,
            ))
            .unwrap();
        assert_eq!(
            events[0],
            json!({"type":"tool_call_delta", "id":"a", "name":"think", "arguments":r#"{"thought":"#})
        );
        assert_eq!(events[1]["name"], "think");
        assert_eq!(events[2]["reasoning"], "why");
        assert_eq!(events[3]["content"], "planning");
        let events = c
            .chunk(chunk(
                json!({"tool_calls":[
                    {"index":0, "function":{"arguments":"ought\":\"B\"}"}},
                    {"index":1, "function":{"arguments":"\"A\"}"}}
                ]}),
                Some("tool_calls"),
            ))
            .unwrap();
        assert_eq!(events[0]["arguments"], r#"ought":"B"}"#);
        assert_eq!(events[1]["arguments"], r#""A"}"#);
        let usage = json!({"choices":[], "usage":{"prompt_tokens":7,"completion_tokens":3,
            "prompt_tokens_details":{"cached_tokens":4,"cache_write_tokens":1}}});
        c.chunk(usage.clone()).unwrap();
        let next = c.end().unwrap();
        let events = next["events"].as_array().unwrap();
        // Go counts cached prompt tokens toward the context, not as fresh input.
        assert_eq!(events[0]["input_tokens"], 2);
        assert_eq!(events[0]["context_length"], 10);
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
            json!({"input_tokens":4, "output_tokens":6})
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
            vec![chunk(
                json!({"tool_calls":[call(0, "a", "think", r#"{"thought":"x"}"#)]}),
                None,
            )],
            vec![
                chunk(json!({}), Some("stop")),
                chunk(json!({"tool_calls":[call(0,"a","think","{}")] }), None),
            ],
            vec![chunk(json!({"content":3}), Some("stop"))],
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
            call(1, "a", "think", args),
            call(1, "", "think", args),
            // Go would name this character; its printability needs Unicode tables.
            call(1, "b", "think", "\u{2028}"),
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
        // An agent without tools answers a hallucinated call instead of failing the send.
        let mut c = conversation(&[]);
        c.begin("go", "go").unwrap();
        c.chunk(chunk(
            json!({"tool_calls":[call(0,"a","think",args)]}),
            Some("tool_calls"),
        ))
        .unwrap();
        let next = c.end().unwrap();
        assert_eq!(next["events"][0]["is_error"], true);
        assert!(next["request"]["tools"].is_null());
        assert!(c.scratchpads.is_empty());
    }

    #[test]
    fn unavailable_tools_get_a_correctable_error_and_odd_endings_finish_the_turn() {
        let mut c = conversation(&["think"]);
        c.begin("go", "go").unwrap();
        let batch = json!({"tool_calls":[
            call(0, "a", "shell", "{}"), call(1, "b", "think", r#"{"thought":"ok"}"#)
        ]});
        let events = c.chunk(chunk(batch, Some("tool_calls"))).unwrap();
        assert_eq!(events[0]["name"], "shell");
        let next = c.end().unwrap();
        let results: Vec<_> = next["events"]
            .as_array()
            .unwrap()
            .iter()
            .filter(|e| e["type"] == "tool_result")
            .collect();
        assert_eq!(
            results[1]["output"],
            "Tool 'shell' is not available. You can only use the tools provided to you."
        );
        assert_eq!(results[1]["is_error"], true);
        assert_eq!(results[0]["output"], "Thoughts:\nok");
        // Go accepts a stream that stops without a finish reason, or names one it cannot honor.
        c.chunk(chunk(json!({"content":"cut off"}), None)).unwrap();
        assert_eq!(c.end().unwrap()["result"]["message"]["content"], "cut off");
        c.begin("again", "next").unwrap();
        c.chunk(chunk(json!({}), Some("tool_calls"))).unwrap();
        let next = c.end().unwrap();
        let warning = next["events"][0]["message"].as_str().unwrap();
        assert!(warning.contains("returned an empty response (stop reason: null)"));
        assert_eq!(next["result"]["message"]["content"], "cut off");
    }

    #[test]
    fn arguments_decode_like_go_and_mistakes_go_back_to_the_model() {
        let results = |tools: &[&str], name: &str, arguments: &[&str]| -> Vec<Value> {
            let mut c = conversation(tools);
            c.begin("go", "go").unwrap();
            let calls: Vec<_> = arguments
                .iter()
                .enumerate()
                .map(|(i, args)| call(i as u64, &format!("c{i}"), name, args))
                .collect();
            c.chunk(chunk(json!({"tool_calls":calls}), Some("tool_calls")))
                .unwrap();
            let next = c.end().unwrap();
            let mut events = next["events"].as_array().unwrap().clone();
            if next["logged"] == true {
                events.extend(
                    c.poll_batch().unwrap()["events"]
                        .as_array()
                        .unwrap()
                        .iter()
                        .cloned(),
                );
            }
            // Every call is announced, even one whose arguments cannot be decoded.
            let announced = events.iter().filter(|e| e["type"] == "tool_call").count();
            assert_eq!(announced, arguments.len());
            let kept = ["tool_result", "elicitation"];
            events
                .iter()
                .filter(|e| kept.contains(&e["type"].as_str().unwrap()))
                .map(|e| e.get("output").unwrap_or(&e["message"]).clone())
                .collect()
        };
        assert_eq!(
            results(
                &["think"],
                "think",
                &[
                    "",
                    r#"{"THOUGHT":"a","other":[1]}"#,
                    r#"{"thought":"b","thought":"c"}"#,
                    "null",
                    r#"{"thought": oops"#,
                    "   ",
                    r#"["x"]"#,
                    r#"{"thought":3}"#,
                ]
            ),
            [
                "Thoughts:\n",
                "Thoughts:\n\na",
                "Thoughts:\n\na\nc",
                "Thoughts:\n\na\nc\n",
                "Error calling tool: invalid character 'o' looking for beginning of value",
                "Error calling tool: unexpected end of JSON input",
                "Error calling tool: json: cannot unmarshal array into Go value of type think.Args",
                "Error calling tool: json: cannot unmarshal number into Go struct field Args.thought of type string",
            ]
        );
        assert_eq!(
            results(
                &["user_prompt"],
                "user_prompt",
                &[
                    "{}",
                    r#"{"message":"A","Message":"B","url":"ignored"}"#,
                    r#"{"message":"A?","schema":"oops"}"#,
                    r#"{"message": "#,
                ]
            ),
            [
                "",
                "B",
                "Error calling tool: json: cannot unmarshal string into Go struct field Args.schema of type map[string]interface {}",
                "Error calling tool: unexpected end of JSON input",
            ]
        );
    }

    #[test]
    fn fetch_arguments_decode_and_repair_like_go() {
        for (input, expected) in [
            ("", r#"{"urls":[]}"#),
            (
                r#"{"URLS":["a",null],"format":"text","timeout":5,"more":1}"#,
                r#"{"format":"text","timeout":5,"urls":["a",""]}"#,
            ),
            // aijson wraps a bare string, unwraps a stringified list and a one-key object.
            (
                r#"{"urls":"a","timeout":2.0}"#,
                r#"{"timeout":2,"urls":["a"]}"#,
            ),
            (r#"{"urls":"[\"a\",\"b\"]"}"#, r#"{"urls":["a","b"]}"#),
            (r#"{"urls":{"url":"a"}}"#, r#"{"urls":["a"]}"#),
        ] {
            assert_eq!(fetch_arguments(input).unwrap(), expected, "{input}");
        }
        for (input, expected) in [
            ("[1]", "json: cannot unmarshal array into Go value of type fetch.ToolArgs"),
            (r#"{"urls":3}"#, "json: cannot unmarshal number into Go struct field ToolArgs.urls of type []string"),
            (r#"{"urls":["a",3]}"#, "json: cannot unmarshal number into ToolArgs.urls.1 of type string"),
            (r#"{"urls":"a","timeout":1.5}"#, "json: cannot unmarshal string into Go struct field ToolArgs.urls of type []string"),
            (r#"{"Urls":"a"}"#, "json: cannot unmarshal string into Go struct field ToolArgs.Urls of type []string"),
            (r#"{"timeout":1e2}"#, "json: cannot unmarshal number 1e2 into Go struct field ToolArgs.timeout of type int"),
        ] {
            assert_eq!(fetch_arguments(input).unwrap_err(), expected, "{input}");
        }
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
    fn only_idle_log_writers_can_finish_while_approval_is_pending() {
        let mut c = conversation(&["think", "memory"]);
        c.begin("go", "turn").unwrap();
        c.chunk(chunk(
            json!({"tool_calls":[
                call(0,"n","unknown","{}"), call(1,"m","delete_memory","{}"),
                call(2,"b","think","bad"), call(3,"t","think","{}")
            ]}),
            Some("tool_calls"),
        ))
        .unwrap();
        let step = c.end().unwrap();
        let events = step["events"].as_array().unwrap();
        assert_eq!(
            events
                .iter()
                .map(|e| e["id"].as_str().unwrap())
                .collect::<Vec<_>>(),
            ["m", "b", "t", "t"]
        );
        let token = events[0]["token"].as_str().unwrap();
        assert!(c.confirm("wrong", true, true, "").is_err());
        let idle = c.poll_batch().unwrap();
        assert_eq!(idle["waiting"], true);
        assert_eq!(idle["events"][0]["id"], "n");
        assert_eq!(idle["events"][1]["id"], "b");
        let end = c.confirm(token, false, false, "no").unwrap();
        assert_eq!(end["events"][0]["type"], "tool_result");
        let ids: Vec<_> = end["request"]["messages"]
            .as_array()
            .unwrap()
            .iter()
            .filter_map(|m| m["tool_call_id"].as_str())
            .collect();
        assert_eq!(ids, ["t", "n", "b", "m"]);
    }

    #[test]
    fn invalid_batch_never_exposes_a_prompt_or_commits_partial_history() {
        let args = r#"{"message":"A?"}"#;
        let mut c = conversation(&["user_prompt"]);
        let saved = c.messages.clone();
        for invalid in [
            call(1, "a", "user_prompt", args),
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
    fn html_escaping_does_not_reduce_the_accepted_answer_size() {
        let content = json!({"text": "<&>".repeat(18_000)}).to_string();
        let output = Conversation::answer_output("accept", Some(&content)).unwrap();
        assert!(output.len() > MAX_ELICITATION_BYTES);
        assert_eq!(
            serde_json::from_str::<Value>(&output).unwrap()["content"].to_string(),
            content
        );
    }

    #[test]
    fn incomplete_batch_rejects_model_chunks_and_waits_for_its_own_replies() {
        let mut c = conversation(&["user_prompt"]);
        c.begin("go", "turn").unwrap();
        c.chunk(chunk(
            json!({"tool_calls":[call(0,"one","",r#"{"message":"A?"}"#)]}),
            None,
        ))
        .unwrap();
        let events = c
            .chunk(chunk(
                json!({"tool_calls":[{"index":0,"function":{"name":"user_prompt"}}]}),
                Some("tool_calls"),
            ))
            .unwrap();
        assert_eq!(events[0]["arguments"], "");
        let waiting = c.end().unwrap();
        let id = waiting["events"][1]["id"].as_str().unwrap();
        assert!(waiting.get("request").is_none());
        assert!(c.end().is_err());
        assert!(c
            .chunk(chunk(json!({"content":"skip"}), Some("stop")))
            .is_err());
        assert!(c.answer("one", "accept", None).is_err());
        let next = c.answer(id, "accept", Some("{}")).unwrap();
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
