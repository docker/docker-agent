use crate::permission::{Permissions, Policy, Safety};
use crate::providers::{self, Options, Transport};
use indexmap::IndexMap;
use serde::{de::Error, Deserialize, Deserializer, Serialize};
use serde_json::{json, Map, Number, Value};
use std::collections::BTreeMap;

mod definitions;

const VERSION: &str = "16";
const MAX_SAFE_INTEGER: i64 = (1_i64 << 53) - 1;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Config {
    #[serde(default)]
    version: Option<Value>,
    agents: IndexMap<String, Agent>,
    #[serde(default)]
    toolsets: BTreeMap<String, Toolset>,
    #[serde(default)]
    mcps: BTreeMap<String, Toolset>,
    #[serde(default)]
    permissions: Permissions,
    #[serde(default, deserialize_with = "runtime_defaults")]
    runtime: Runtime,
    #[serde(default)]
    models: BTreeMap<String, Model>,
    #[serde(default)]
    providers: BTreeMap<String, Provider>,
}

#[derive(Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
struct Runtime {
    safety: Safety,
}

fn runtime_defaults<'de, D: serde::Deserializer<'de>>(d: D) -> Result<Runtime, D::Error> {
    let value = serde_yaml_ng::Value::deserialize(d)?;
    if !matches!(value, serde_yaml_ng::Value::Mapping(_)) {
        return Err(serde::de::Error::custom("runtime must be a mapping"));
    }
    serde_yaml_ng::from_value(value).map_err(serde::de::Error::custom)
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Agent {
    #[serde(flatten)]
    limits: crate::limits::Limits,
    model: String,
    session_compaction: Option<bool>,
    redact_secrets: Option<bool>,
    compaction_threshold: Option<f64>,
    structured_output: Option<StructuredOutput>,
    #[serde(default)]
    fallback: crate::fallback::Config,
    #[serde(default)]
    description: String,
    // Neither Go browser projection exposes the welcome message.
    #[serde(rename = "welcome_message")]
    _welcome_message: Option<String>,
    #[serde(default, deserialize_with = "load_instruction")]
    instruction: String,
    #[serde(default)]
    toolsets: Vec<Toolset>,
    #[serde(default)]
    use_toolsets: Vec<String>,
    #[serde(default)]
    sub_agents: Vec<String>,
    #[serde(default)]
    handoffs: Vec<String>,
    #[serde(default)]
    add_date: bool,
    #[serde(default)]
    add_environment_info: bool,
    #[serde(default)]
    readonly: bool,
    #[serde(default)]
    hooks: crate::hooks::Hooks,
    #[serde(default)]
    safety: Safety,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct StructuredOutput {
    name: String,
    #[serde(default)]
    description: String,
    #[serde(default)]
    strict: bool,
    #[serde(default)]
    mode: OutputMode,
    #[serde(deserialize_with = "output_schema")]
    schema: Value,
}

impl StructuredOutput {
    fn check_session(&self, model: &str, env: &BTreeMap<String, String>) -> Result<(), String> {
        if matches!(self.mode, OutputMode::Tool) {
            return Err("tool mode is not supported in portable sessions".into());
        }
        if self.name.is_empty()
            || self.name.len() > 64
            || !self
                .name
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'-'))
        {
            return Err("name must be 1-64 ASCII letters, digits, hyphens or underscores".into());
        }
        let id = expand_model_field(model, env)?;
        if needs_claude_schema_instruction(&id) {
            return Err(format!(
                "the Claude schema instruction for {id:?} is not supported in portable sessions"
            ));
        }
        Ok(())
    }
}

#[derive(Default, Deserialize)]
#[serde(rename_all = "lowercase")]
enum OutputMode {
    #[default]
    #[serde(alias = "")]
    Native,
    Tool,
}

fn output_schema<'de, D: Deserializer<'de>>(de: D) -> Result<Value, D::Error> {
    use serde_yaml_ng::Value as Yaml;

    // serde_json maps non-finite YAML numbers to null, and the JS host rounds large integers.
    fn check(value: &Yaml) -> Result<(), &'static str> {
        match value {
            Yaml::Mapping(map) => {
                for (key, value) in map {
                    let Yaml::String(key) = key else {
                        return Err("schema keys must be strings");
                    };
                    if key == "<<" {
                        return Err("YAML merge keys are unsupported");
                    }
                    check(value)?;
                }
            }
            Yaml::Sequence(seq) => seq.iter().try_for_each(check)?,
            Yaml::Number(n) if !n.is_finite() => return Err("schema numbers must be finite"),
            Yaml::Number(n)
                if n.as_f64()
                    .is_some_and(|v| v.abs() > MAX_SAFE_INTEGER as f64) =>
            {
                return Err("schema number exceeds the JavaScript safe integer range");
            }
            Yaml::Tagged(_) => return Err("tagged schema values are unsupported"),
            _ => (),
        }
        Ok(())
    }

    let yaml = Yaml::deserialize(de)?;
    check(&yaml).map_err(|e| D::Error::custom(format!("structured_output.schema: {e}")))?;
    let schema: Value = serde_yaml_ng::from_value(yaml).map_err(D::Error::custom)?;
    if schema["type"] != "object" || !schema["properties"].is_object() {
        return Err(D::Error::custom(
            "structured_output.schema requires type: object and a properties map",
        ));
    }
    if schema.get("required").is_some_and(|v| {
        !v.as_array()
            .is_some_and(|keys| keys.iter().all(Value::is_string))
    }) {
        return Err(D::Error::custom(
            "structured_output.schema.required must be an array of strings",
        ));
    }
    if schema
        .get("additionalProperties")
        .is_some_and(|v| !v.is_boolean())
    {
        return Err(D::Error::custom(
            "structured_output.schema.additionalProperties must be a boolean",
        ));
    }
    Ok(schema)
}

// Go adds a schema instruction on both OpenAI protocols for modelinfo.IsClaude(nil, id).
fn needs_claude_schema_instruction(model: &str) -> bool {
    let id = model.trim().to_ascii_lowercase();
    let mut segments = id.rsplit('/');
    let name = segments.next().unwrap();
    if segments.next() == Some("openai") {
        return false;
    }
    name.starts_with("claude-")
        || name.starts_with("anthropic.claude-")
        || name.split_once('.').is_some_and(|(region, rest)| {
            !region.is_empty() && rest.starts_with("anthropic.claude-")
        })
}

fn load_instruction<'de, D: Deserializer<'de>>(d: D) -> Result<String, D::Error> {
    #[derive(Deserialize)]
    #[serde(untagged)]
    enum Instruction {
        Text(String),
        Paragraphs(Vec<String>),
    }
    Ok(match Instruction::deserialize(d)? {
        Instruction::Text(text) => text,
        Instruction::Paragraphs(parts) => parts.join("\n\n"),
    })
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
pub struct Toolset {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(default, rename = "type")]
    pub kind: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub shared: bool,
    #[serde(
        default,
        deserialize_with = "tool_filter",
        skip_serializing_if = "Vec::is_empty"
    )]
    pub tools: Vec<String>,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub readonly: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub instruction: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub models: Option<Vec<String>>,
    #[serde(default, skip_serializing)]
    pub defer: Defer,
    #[serde(default, skip_serializing)]
    pub toon: crate::discovery::ToonSpec,
    #[serde(flatten)]
    pub options: BTreeMap<String, Value>,
}

// The typed YAML sequence decoder treats null as an empty (unrestricted) allow-list.
fn tool_filter<'de, D: serde::Deserializer<'de>>(d: D) -> Result<Vec<String>, D::Error> {
    let serde_yaml_ng::Value::Sequence(names) = serde_yaml_ng::Value::deserialize(d)? else {
        return Err(serde::de::Error::custom("tools must be a list of names"));
    };
    names
        .into_iter()
        .map(|name| match name {
            serde_yaml_ng::Value::String(name) => Ok(name),
            _ => Err(serde::de::Error::custom("invalid tool filter name")),
        })
        .collect()
}

#[derive(Clone, Debug, Deserialize)]
#[serde(untagged)]
pub enum Defer {
    All(bool),
    Names(Vec<String>),
}

impl Default for Defer {
    fn default() -> Self {
        Self::All(false)
    }
}

impl Defer {
    pub fn parts(&self) -> (bool, &[String]) {
        match self {
            Self::All(all) => (*all, &[]),
            Self::Names(names) => (false, names),
        }
    }
}

impl Toolset {
    pub fn hosted(&self) -> bool {
        !matches!(
            self.kind.as_str(),
            "calculator"
                | "think"
                | "todo"
                | "user_prompt"
                | "transfer_task"
                | "handoff"
                | "model_picker"
                | "session_context"
        )
    }

    fn validate(&self) -> Result<(), String> {
        if self.shared && self.kind != "todo" {
            return Err("shared can only be used with type 'todo'".into());
        }
        let allowed: &[&str] = match self.kind.as_str() {
            "fetch" => &[
                "allow_private_ips",
                "headers",
                "timeout",
                "allowed_domains",
                "blocked_domains",
                "escape_html",
            ],
            "api" => &["allow_private_ips", "api_config", "timeout"],
            "mcp" => &["allow_private_ips", "remote", "name", "ref"],
            _ => &[],
        };
        for (name, val) in &self.options {
            if !allowed.contains(&name.as_str()) {
                return Err(format!("unsupported {} option {name:?}", self.kind));
            }
            let valid = match name.as_str() {
                "name" | "ref" => val.is_string(),
                "allow_private_ips" | "escape_html" => val.is_boolean(),
                "headers" => val
                    .as_object()
                    .is_some_and(|obj| obj.values().all(Value::is_string)),
                "timeout" => val.as_u64().is_some_and(|n| n <= 300),
                "allowed_domains" | "blocked_domains" => val
                    .as_array()
                    .is_some_and(|a| a.iter().all(Value::is_string)),
                "remote" | "api_config" => val.is_object(),
                _ => false,
            };
            if !valid {
                return Err(format!("invalid {} option {name:?}", self.kind));
            }
        }
        if self
            .options
            .get("allowed_domains")
            .is_some_and(|v| v.as_array().is_some_and(|a| !a.is_empty()))
            && self
                .options
                .get("blocked_domains")
                .is_some_and(|v| v.as_array().is_some_and(|a| !a.is_empty()))
        {
            return Err("allowed_domains and blocked_domains are mutually exclusive".into());
        }
        match self.kind.as_str() {
            "mcp" => definitions::validate_mcp(self)?,
            "api" => {
                let api = self
                    .options
                    .get("api_config")
                    .and_then(Value::as_object)
                    .ok_or("api requires api_config")?;
                if api.keys().any(|k| {
                    !matches!(
                        k.as_str(),
                        "endpoint"
                            | "method"
                            | "name"
                            | "instruction"
                            | "required"
                            | "args"
                            | "headers"
                            | "output_schema"
                    )
                }) {
                    return Err("unsupported api_config field".into());
                }
                if ["name", "endpoint"].iter().any(|k| {
                    api.get(*k)
                        .and_then(Value::as_str)
                        .is_none_or(|s| s.is_empty())
                }) {
                    return Err("api needs a name and endpoint".into());
                }
            }
            _ => {}
        }
        if self.kind == "model_picker" && self.models.as_ref().is_none_or(Vec::is_empty) {
            return Err("model_picker requires a non-empty models list".into());
        }
        if self.kind != "model_picker" && self.models.is_some() {
            return Err("models can only be used with type 'model_picker'".into());
        }
        match self.kind.as_str() {
            "api" | "mcp" | "calculator" | "datetime" | "fetch" | "file" | "filesystem"
            | "memory" | "plan" | "random" | "session_context" | "shell" | "tasks" | "think"
            | "todo" | "user_prompt" | "model_picker" => Ok(()),
            _ => Err(format!("unsupported toolset type {:?}", self.kind)),
        }
    }
}

#[derive(Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct Model {
    context_window: Option<u32>,
    compaction_threshold: Option<f64>,
    #[serde(rename = "description")]
    _description: Option<String>,
    provider: String,
    model: String,
    capabilities: Option<crate::media::Capabilities>,
    #[serde(default)]
    base_url: String,
    #[serde(default)]
    token_key: String,
    #[serde(default)]
    provider_opts: Options,
    temperature: Option<Number>,
    top_p: Option<Number>,
    frequency_penalty: Option<Number>,
    presence_penalty: Option<Number>,
    max_tokens: Option<i64>,
    parallel_tool_calls: Option<bool>,
}

#[derive(Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
struct Provider {
    provider_opts: Options,
    provider: String,
    base_url: String,
    token_key: String,
    api_type: String,
    temperature: Option<Number>,
    top_p: Option<Number>,
    frequency_penalty: Option<Number>,
    presence_penalty: Option<Number>,
    max_tokens: Option<i64>,
    parallel_tool_calls: Option<bool>,
}

#[derive(Default)]
pub struct Selected {
    pub tool_policy: Policy,
    pub name: String,
    pub team_prompts: Vec<String>,
    pub sub_agents: Vec<String>,
    pub handoffs: Vec<String>,
    pub shared_todo: bool,
    pub limits: crate::limits::Limits,
    pub tool_models: BTreeMap<String, Map<String, Value>>,
    pub hooks: crate::hooks::Hooks,
    pub picker: crate::model_picker::Picker,
    pub models: Vec<Target>,
    pub policy: crate::fallback::Policy,
    pub instruction: String,
    pub toolsets: Vec<Toolset>,
}

pub struct Target {
    pub id: String,
    pub model_params: Map<String, Value>,
    pub transport: Transport,
    pub compaction: crate::compaction::Policy,
    pub capabilities: Option<crate::media::Capabilities>,
    pub key_base: Option<String>,
    pub responses: bool,
    pub merge_messages: bool,
}

impl Target {
    pub fn model(&self) -> &str {
        self.model_params["model"]
            .as_str()
            .expect("configured model")
    }

    fn media_projection(&self) -> (crate::media::Capabilities, bool) {
        crate::media::Capabilities::projection(self.capabilities, self.model())
    }
}

fn load(yaml: &str) -> Result<Config, String> {
    // Reading as YAML values first also rejects duplicate keys in named maps,
    // rather than letting IndexMap/BTreeMap silently replace the first entry.
    let doc = serde_yaml_ng::from_str(yaml).map_err(|e| e.to_string())?;
    let mut cfg: Config = serde_yaml_ng::from_value(doc).map_err(|e| e.to_string())?;
    if let Some(v) = &cfg.version {
        if v.as_str() != Some(VERSION)
            && v.as_u64().map(|n| n.to_string()).as_deref() != Some(VERSION)
        {
            return Err(format!("unsupported config version {v}"));
        }
    }
    if cfg.agents.is_empty() {
        return Err("at least one agent must be configured".into());
    }
    definitions::resolve(&mut cfg)?;
    for (name, a) in &cfg.agents {
        if name.trim().is_empty() {
            return Err("empty agent name".into());
        }
        a.limits.validate()?;
        validate_threshold(a.compaction_threshold)?;
        for (kind, targets) in [("sub-agent", &a.sub_agents), ("handoff agent", &a.handoffs)] {
            for target in targets {
                if target.contains('/') {
                    return Err(format!(
                        "external agent reference {target:?} is not supported"
                    ));
                }
                if !cfg.agents.contains_key(target) {
                    return Err(format!(
                        "agent {name:?} references non-existent {kind} {target:?}"
                    ));
                }
            }
        }
        if !cfg.models.contains_key(&a.model) {
            let m = inline_model(&a.model).ok_or_else(|| {
                format!("agent {name:?} references non-existent model {:?}", a.model)
            })?;
            cfg.models.insert(a.model.clone(), m);
        }
    }
    for model in cfg.models.values() {
        validate_threshold(model.compaction_threshold)?;
        if model.context_window == Some(0) {
            return Err("context_window must be greater than 0".into());
        }
        if model.model.trim().is_empty() || model.provider.trim().is_empty() {
            return Err("model and provider must be set".into());
        }
    }
    Ok(cfg)
}

fn inline_model(name: &str) -> Option<Model> {
    let (provider, model) = name
        .split_once('/')
        .filter(|(p, m)| !p.is_empty() && !m.is_empty())?;
    Some(Model {
        provider: provider.into(),
        model: model.into(),
        ..Model::default()
    })
}

fn validate_threshold(value: Option<f64>) -> Result<(), String> {
    if value.is_some_and(|v| !(v > 0.0 && v <= 1.0)) {
        return Err("compaction_threshold must be greater than 0 and at most 1".into());
    }
    Ok(())
}

fn agent_list(cfg: &Config, full: bool) -> Vec<Value> {
    cfg.agents.iter().map(|(name, a)| {
        let mut v = json!({"name":name, "model":a.model, "description":a.description, "instruction":a.instruction});
        if full {
            v["sub_agents"] = json!(a.sub_agents);
            v["handoffs"] = json!(a.handoffs);
            v["add_date"] = a.add_date.into();
            v["add_env_info"] = a.add_environment_info.into();
        }
        v
    }).collect()
}

pub fn parse(yaml: &str) -> Result<Value, String> {
    let cfg = load(yaml)?;
    let models: BTreeMap<_, _> = cfg
        .models
        .iter()
        .map(|(name, m)| {
            (
                name,
                json!({"provider":m.provider, "model":m.model, "base_url":m.base_url}),
            )
        })
        .collect();
    Ok(json!({"version": VERSION, "agents": agent_list(&cfg, true), "models":models}))
}

pub fn list(yaml: &str) -> Result<Value, String> {
    Ok(Value::Array(agent_list(&load(yaml)?, false)))
}

// Only expand top-level ${env.NAME} placeholders. Never evaluate JavaScript or
// expand the inner placeholder of an otherwise unsupported expression.
fn instruction(text: &str, env: &BTreeMap<String, String>) -> String {
    let mut out = String::new();
    let mut rest = text;
    while let Some((before, tail)) = rest.split_once("${") {
        out.push_str(before);
        let mut depth = 1;
        let end = tail.find(|c| {
            match c {
                '{' => depth += 1,
                '}' => depth -= 1,
                _ => {}
            }
            depth == 0
        });
        let Some(end) = end else {
            out.push_str("${");
            out.push_str(tail);
            return out;
        };
        let expr = &tail[..end];
        if let Some(key) = js_env_name(expr.trim()) {
            if let Some(value) = env.get(key) {
                out.push_str(value);
            }
        } else {
            out.push_str("${");
            out.push_str(expr);
            out.push('}');
        }
        rest = &tail[end + 1..];
    }
    out.push_str(rest);
    out
}

fn js_env_name(expr: &str) -> Option<&str> {
    expr.strip_prefix("env.").filter(|v| {
        !v.is_empty()
            && !v.as_bytes()[0].is_ascii_digit()
            && v.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'_')
    })
}

// ModelConfig.ExpandEnv uses shell references, unlike instruction expansion.
fn expand_model_field(mut text: &str, env: &BTreeMap<String, String>) -> Result<String, String> {
    let mut out = String::with_capacity(text.len());
    while let Some((before, tail)) = text.split_once('$') {
        out.push_str(before);
        let (name, rest) = if let Some(braced) = tail.strip_prefix('{') {
            let (name, rest) = braced
                .split_once('}')
                .ok_or("unclosed environment reference")?;
            if name.is_empty() {
                return Err("empty environment reference".into());
            }
            (js_env_name(name.trim_ascii()).unwrap_or(name), rest)
        } else {
            let n = match tail.as_bytes().first() {
                Some(b'0'..=b'9' | b'*' | b'#' | b'$' | b'@' | b'!' | b'?' | b'-') => 1,
                _ => tail
                    .bytes()
                    .take_while(|b| b.is_ascii_alphanumeric() || *b == b'_')
                    .count(),
            };
            if n == 0 {
                out.push('$');
                text = tail;
                continue;
            }
            tail.split_at(n)
        };
        out.push_str(
            env.get(name)
                .ok_or_else(|| format!("environment variable {name:?} not set"))?,
        );
        text = rest;
    }
    out.push_str(text);
    Ok(out)
}

// Keep in sync with modelinfo.SupportsResponsesAPI (also used for forced Chat Completions).
fn supports_responses_api(id: &str) -> bool {
    let name = id.trim().rsplit('/').next().unwrap().to_ascii_lowercase();
    if ["gpt-4.1", "gpt-5", "o1", "o3", "o4", "codex"]
        .iter()
        .any(|p| name.starts_with(p))
        || name.contains("-codex")
    {
        return true;
    }
    let Some(gpt) = name.strip_prefix("gpt-") else {
        return false;
    };
    let version = gpt.split('-').next().unwrap();
    let (major, minor) = version.split_once('.').unwrap_or((version, "0"));
    [major, minor]
        .iter()
        .all(|s| (1..=2).contains(&s.len()) && s.bytes().all(|b| b.is_ascii_digit()))
        && major.parse::<u8>().is_ok_and(|n| n >= 6)
}

pub fn cap_key(model: &str) -> &'static str {
    if supports_responses_api(model) {
        "max_completion_tokens"
    } else {
        "max_tokens"
    }
}

fn model_params(
    id: String,
    m: &Model,
    p: &Provider,
    tools: bool,
    policy: crate::compaction::Policy,
) -> Result<Map<String, Value>, String> {
    let mut params = Map::new();
    if let Some(tier) = m
        .provider_opts
        .service_tier
        .as_ref()
        .or(p.provider_opts.service_tier.as_ref())
        .filter(|s| !s.is_empty())
    {
        params.insert("service_tier".into(), tier.clone().into());
    }
    for (key, model, provider) in [
        ("temperature", &m.temperature, &p.temperature),
        ("top_p", &m.top_p, &p.top_p),
        (
            "frequency_penalty",
            &m.frequency_penalty,
            &p.frequency_penalty,
        ),
        ("presence_penalty", &m.presence_penalty, &p.presence_penalty),
    ] {
        if let Some(n) = model.as_ref().or(provider.as_ref()) {
            params.insert(key.into(), Value::Number(n.clone()));
        }
    }
    if let Some(cap) = m.max_tokens.or(p.max_tokens).filter(|&n| n > 0) {
        // The JS host parses and re-encodes this request before fetch.
        if cap > MAX_SAFE_INTEGER {
            return Err("max_tokens exceeds the JavaScript safe integer range".into());
        }
        params.insert(cap_key(&id).into(), policy.clamp(cap).into());
    }
    if tools {
        params.insert("tool_choice".into(), "auto".into());
        if let Some(parallel) = m.parallel_tool_calls.or(p.parallel_tool_calls) {
            params.insert("parallel_tool_calls".into(), parallel.into());
        }
    }
    params.insert("model".into(), Value::String(id));
    Ok(params)
}

fn first_nonempty(values: [&str; 3]) -> &str {
    values.into_iter().find(|s| !s.is_empty()).unwrap_or("")
}

fn compaction_policy(a: &Agent, m: &Model) -> Result<crate::compaction::Policy, String> {
    crate::compaction::Policy::new(
        m.context_window,
        a.session_compaction,
        m.compaction_threshold.or(a.compaction_threshold),
    )
}

pub fn team(
    yaml: &str,
    entry: &str,
    env: &BTreeMap<String, String>,
) -> Result<(String, Vec<Selected>), String> {
    let cfg = load(yaml)?;
    for (name, a) in &cfg.agents {
        let primary = &cfg.models[&a.model];
        compaction_policy(a, primary)?;
        a.limits.check_round_limit()?;
        let overrides = a.toolsets.iter().any(|ts| !ts.model.is_empty());
        if a.limits.limits_history() || a.limits.max_iterations > 0 || overrides {
            if primary.context_window.is_some() && a.session_compaction != Some(false) {
                return Err("history/iteration limits and tool model overrides require session_compaction: false when a context_window is declared".into());
            }
            if a.model.contains(',') || primary.model.contains(',') {
                return Err("history/iteration limits and tool model overrides require a single primary model".into());
            }
        }
        if a.limits.limits_history() && a.toolsets.iter().any(|ts| ts.kind == "session_context") {
            return Err("history limits are not supported with session_context".into());
        }
        if let Some(output) = &a.structured_output {
            output
                .check_session(&cfg.models[&a.model].model, env)
                .map_err(|e| format!("agents.{name}.structured_output: {e}"))?;
        }
        for ts in &a.toolsets {
            if !matches!(
                ts.kind.as_str(),
                "think"
                    | "user_prompt"
                    | "calculator"
                    | "todo"
                    | "datetime"
                    | "random"
                    | "plan"
                    | "memory"
                    | "api"
                    | "mcp"
                    | "fetch"
                    | "model_picker"
                    | "session_context"
            ) {
                return Err(format!("unsupported toolset type {:?}", ts.kind));
            }
        }
    }
    for p in cfg.providers.values() {
        p.provider_opts.validate(None)?;
        if !matches!(p.provider.as_str(), "" | "openai")
            || !matches!(
                p.api_type.as_str(),
                "" | "openai_chatcompletions" | "openai_responses"
            )
        {
            return Err("portable providers must use an OpenAI API".into());
        }
    }
    for m in cfg.models.values() {
        check_provider(cfg.providers.get(&m.provider), m)?;
    }
    let entry = if entry.is_empty() {
        if cfg.agents.contains_key("root") {
            "root"
        } else {
            cfg.agents.first().unwrap().0
        }
    } else {
        entry
    };
    let root = cfg
        .agents
        .get(entry)
        .ok_or_else(|| format!("agent {entry:?} not found"))?;
    let team = !root.sub_agents.is_empty() || !root.handoffs.is_empty();
    let mut pending = vec![entry];
    let mut seen = std::collections::BTreeSet::new();
    let mut agents = Vec::new();
    while let Some(name) = pending.pop() {
        if !seen.insert(name) {
            continue;
        }
        let a = &cfg.agents[name];
        pending.extend(a.sub_agents.iter().chain(&a.handoffs).map(String::as_str));
        agents.push(resolve(&cfg, name, a, env, team)?);
    }
    Ok((entry.into(), agents))
}

#[cfg(test)]
pub fn select(yaml: &str, agent: &str, env: &BTreeMap<String, String>) -> Result<Selected, String> {
    Ok(team(yaml, agent, env)?.1.remove(0))
}

fn resolve(
    cfg: &Config,
    name: &str,
    a: &Agent,
    env: &BTreeMap<String, String>,
    team: bool,
) -> Result<Selected, String> {
    if team && a.redact_secrets == Some(false) {
        return Err("disabling secret redaction within local teams is not supported".into());
    }
    if team
        && a.readonly
        && a.sub_agents
            .iter()
            .chain(&a.handoffs)
            .any(|n| !cfg.agents[n].readonly)
    {
        return Err("readonly agents cannot delegate or hand off to a writable agent".into());
    }
    if (team || !a.fallback.models.is_empty()) && a.hooks.max_calls.is_some() {
        return Err("max_iterations with fallback or local teams is not supported".into());
    }
    if team && a.toolsets.iter().any(Toolset::hosted) {
        return Err("host toolsets with local delegation or handoff are not supported".into());
    }
    if team && a.toolsets.iter().any(|ts| ts.kind == "session_context") {
        return Err("session_context with local delegation or handoff is not supported".into());
    }
    let mut toolsets = a.toolsets.clone();
    for ts in &mut toolsets {
        ts.readonly |= a.readonly;
        ts.instruction = instruction(&ts.instruction, env);
    }
    let mut todos = a.toolsets.iter().filter(|ts| ts.kind == "todo");
    let shared_todo = todos.next().is_some_and(|ts| ts.shared);
    if todos.any(|ts| ts.shared != shared_todo) {
        return Err(format!(
            "agent {name:?}: mixing shared and private todo toolsets is not supported"
        ));
    }
    let mut team_prompts = Vec::new();
    for (tool, targets) in [("transfer_task", &a.sub_agents), ("handoff", &a.handoffs)] {
        if !targets.is_empty() {
            toolsets.push(Toolset {
                kind: tool.into(),
                ..Default::default()
            });
            let mut roster = String::new();
            for target in targets {
                use std::fmt::Write;
                writeln!(
                    roster,
                    "Name: {target} | Description: {}",
                    instruction(&cfg.agents[target].description, env)
                )
                .unwrap();
            }
            team_prompts.push(crate::team::prompt(tool, &roster, &targets.join(", ")));
        }
    }
    let primary = &cfg.models[&a.model];
    let chain = !a.fallback.models.is_empty();
    if (chain || team)
        && (a.limits.limits_history()
            || a.limits.max_iterations > 0
            || a.toolsets.iter().any(|ts| !ts.model.is_empty()))
    {
        return Err(
            "history limits and toolset model overrides with teams or fallback are not supported"
                .into(),
        );
    }
    let resolve_target =
        |m: &Model| resolve_model(cfg, m, a, env, !toolsets.is_empty(), chain || team);
    let request = resolve_target(primary)?;
    let compatible = |name: &str, feature: &str| -> Result<Target, String> {
        let inline;
        let m = if let Some(m) = cfg.models.get(name) {
            m
        } else {
            inline = inline_model(name)
                .filter(|_| !name.contains(','))
                .ok_or_else(|| format!("unsupported {feature} model {name:?}"))?;
            &inline
        };
        // Do not reuse the open handle's Authorization header at a new destination.
        if m.context_window != primary.context_window
            || m.compaction_threshold != primary.compaction_threshold
        {
            return Err(format!("{feature} cannot change compaction policy"));
        }
        if m.provider != primary.provider {
            return Err(format!("{feature} cannot switch providers"));
        }
        let target = resolve_target(m)?;
        if target.media_projection() != request.media_projection() {
            return Err(format!("{feature} cannot change media capabilities"));
        }
        if target.key_base != request.key_base
            || target.transport != request.transport
            || target.responses != request.responses
            || target.merge_messages != request.merge_messages
        {
            return Err(format!(
                "{feature} cannot change the model endpoint, API or credential"
            ));
        }
        Ok(target)
    };
    let mut tool_models = BTreeMap::new();
    for ts in &a.toolsets {
        let name = &ts.model;
        if name.is_empty() || tool_models.contains_key(name) {
            continue;
        }
        if name.contains(',') || cfg.models.get(name).is_some_and(|m| m.model.contains(',')) {
            return Err("toolset model overrides cannot use multi-model references".into());
        }
        tool_models.insert(name.clone(), compatible(name, "toolset")?.model_params);
    }
    let mut picker = crate::model_picker::Picker::default();
    for ts in a.toolsets.iter().filter(|ts| ts.kind == "model_picker") {
        if chain {
            return Err(
                "model_picker with fallback models on the same agent is not supported".into(),
            );
        }
        if !picker.choices.is_empty() {
            return Err("multiple model_picker toolsets are not supported".into());
        }
        for name in ts.models.as_ref().expect("validated picker") {
            if picker.choices.contains_key(name) {
                return Err(format!("duplicate model_picker choice {name:?}"));
            }
            let target = compatible(name, "model_picker")?;
            picker.choices.insert(name.clone(), target.model_params);
        }
    }
    let mut models = vec![request];
    for name in &a.fallback.models {
        let inline;
        let m = if let Some(m) = cfg.models.get(name) {
            m
        } else {
            inline =
                inline_model(name).ok_or_else(|| format!("fallback model {name:?} not found"))?;
            &inline
        };
        models.push(resolve_target(m)?);
    }
    Ok(Selected {
        limits: a.limits,
        tool_models,
        hooks: a.hooks.clone().with_defaults(
            a.redact_secrets.unwrap_or(true),
            a.add_date,
            a.add_environment_info,
        ),
        tool_policy: Policy {
            rules: cfg.permissions.clone(),
            safety: if a.safety == Safety::Legacy {
                cfg.runtime.safety
            } else {
                a.safety
            },
        },
        picker,
        models,
        policy: a.fallback.policy(),
        instruction: instruction(&a.instruction, env),
        name: name.into(),
        team_prompts,
        sub_agents: a.sub_agents.clone(),
        handoffs: a.handoffs.clone(),
        shared_todo,
        toolsets,
    })
}

fn check_provider(
    custom: Option<&Provider>,
    m: &Model,
) -> Result<Option<&'static providers::Builtin>, String> {
    let builtin = providers::lookup(&m.provider);
    if custom.is_none() && builtin.is_none() {
        return Err(format!("unsupported provider {:?}", m.provider));
    }
    if custom.is_some_and(|p| p.provider.is_empty()) && builtin.is_some_and(|p| p.scoped()) {
        return Err(
            "custom providers shadowing this built-in must declare their underlying provider"
                .into(),
        );
    }
    let builtin = builtin.filter(|_| custom.is_none());
    m.provider_opts.validate(builtin)?;
    Ok(builtin)
}

fn resolve_model(
    cfg: &Config,
    m: &Model,
    a: &Agent,
    env: &BTreeMap<String, String>,
    tools: bool,
    multi_target: bool,
) -> Result<Target, String> {
    let p = cfg.providers.get(&m.provider);
    let builtin = check_provider(p, m)?;
    let default_base = builtin.map_or("https://api.openai.com/v1", |b| b.base_url.as_str());
    let chatgpt = p.is_none() && m.provider == "chatgpt";
    // Go resolves a custom provider's default protocol before expanding the model.
    let id = expand_model_field(&m.model, env)?;
    let responses = match p {
        Some(p) => {
            p.api_type == "openai_responses"
                || (p.api_type.is_empty() && supports_responses_api(&m.model))
        }
        None => {
            chatgpt || (builtin.is_some_and(|b| b.auto_responses) && supports_responses_api(&id))
        }
    };
    let custom = p.is_some();
    let default_provider = Provider::default();
    let p = p.unwrap_or(&default_provider);
    let key = if m.token_key.is_empty() {
        &p.token_key
    } else {
        &m.token_key
    };
    let base_field = first_nonempty([&m.base_url, &p.base_url, default_base]);
    if let Some(b) = builtin {
        b.validate_base(base_field, key, env)?;
    }
    let base = expand_model_field(base_field, env)?;
    if id.trim().is_empty() || base.trim().is_empty() {
        return Err("model and endpoint must not expand to an empty string".into());
    }
    let merge_messages = !responses
        && (custom
            || builtin.is_some_and(|b| b.merge_consecutive_messages)
            || (m.provider == "openai" && base.trim_end_matches('/') != default_base));
    let route = if responses {
        "responses"
    } else {
        "chat/completions"
    };
    let provider_id = if p.provider.is_empty() {
        &m.provider
    } else {
        &p.provider
    };
    let model_id = format!("{provider_id}/{id}");
    let compaction = compaction_policy(a, m)?;
    if multi_target && compaction.enabled() {
        return Err("compaction with fallback or local teams is not supported; set session_compaction: false".into());
    }
    let mut params = model_params(id, m, p, tools, compaction)?;
    if let Some(output) = &a.structured_output {
        output.check_session(&m.model, env)?;
        params.insert("response_format".into(),json!({"type":"json_schema","json_schema":{"name":output.name,"description":output.description,"strict":output.strict,"schema":output.schema}}));
    }
    if multi_target
        && responses
        && ["frequency_penalty", "presence_penalty"]
            .iter()
            .any(|k| params.contains_key(*k))
    {
        return Err(
            "Responses does not support frequency_penalty or presence_penalty when switching model targets"
                .into(),
        );
    }
    // An override on one model must not redirect an inherited provider key to another origin.
    let key_base = if custom
        && !m.base_url.is_empty()
        && multi_target
        && m.token_key.is_empty()
        && !key.is_empty()
    {
        Some(expand_model_field(
            first_nonempty([&p.base_url, default_base, ""]),
            env,
        )?)
    } else {
        None
    };
    Ok(Target {
        id: model_id,
        model_params: params,
        transport: providers::transport(builtin, &base, route, key, &m.provider_opts, env)?,
        responses,
        merge_messages,
        key_base,
        compaction,
        capabilities: m.capabilities,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn primary_params(cfg: &str, agent: &str, env: &BTreeMap<String, String>) -> Value {
        Value::Object(
            select(cfg, agent, env)
                .unwrap()
                .models
                .remove(0)
                .model_params,
        )
    }

    #[test]
    fn strict_loader() {
        for yaml in [
            "agents: {}",
            "agents: {a: {model: nope}}",
            "agents: {a: {model: a/b, hooks: {bogus: []}}}",
            "agents: {a: {model: a/b}, a: {model: a/c}}",
            "version: 2\nagents: {a: {model: a/b}}",
        ] {
            assert!(parse(yaml).is_err(), "{yaml}");
        }
    }

    #[test]
    fn native_schema_is_per_agent_and_projected_without_normalization() {
        let cfg = r"providers: {local: {}}
agents:
  root:
    model: local/test
    toolsets: [{type: think}]
    structured_output:
      name: result
      schema:
        type: object
        properties: {values: {type: array, items: {$ref: '#/$defs/result'}}}
        $defs: {result: {type: [string, 'null'], enum: [null, 'a']}}
        required: []
  other: {model: local/test}
";
        let params = |agent| primary_params(cfg, agent, &BTreeMap::new());
        let schema = json!({"type":"object", "properties":{"values":{"type":"array","items":{"$ref":"#/$defs/result"}}},
            "$defs":{"result":{"type":["string","null"], "enum":[null,"a"]}}, "required":[]});
        assert_eq!(
            params("root"),
            json!({"model":"test", "tool_choice":"auto", "response_format":{
            "type":"json_schema", "json_schema":{"name":"result", "description":"", "strict":false, "schema":schema}}})
        );
        assert_eq!(params("other"), json!({"model":"test"}));
        assert!(parse(cfg).unwrap()["agents"][0]
            .get("structured_output")
            .is_none());
    }

    #[test]
    fn rejects_malformed_output_configs_and_lossy_schema_values() {
        for output in [
            "[]", "{}", "{name: out}", "{schema: {type: object, properties: {}}}",
            "{name: out, schema: null}", "{name: out, schema: true}", "{name: out, schema: {type: string}}",
            "{name: out, schema: {type: object, properties: []}}",
            "{name: out, schema: {type: object, properties: {}, required: [2]}}",
            "{name: out, schema: {type: object, properties: {}, additionalProperties: null}}",
            "{name: out, schema: {type: object, properties: {}, $defs: {s: {const: [9007199254740993]}}}}",
            "{name: out, schema: {type: object, properties: {}, enum: [-9007199254740993]}}",
            "{name: out, schema: {type: object, properties: {x: {enum: [.nan]}}}}",
            "{name: out, schema: {type: object, properties: {<<: {x: {type: string}}}}}",
            "{name: out, schema: {type: object, properties: {false: {type: string}}}}",
            "{name: out, schema: {type: object, properties: {}}, strict: 'true'}",
            "{name: out, schema: {type: object, properties: {}}, typo: false}",
            "{name: out, schema: {type: object, properties: {}}, mode: Tool}",
        ] {
            assert!(parse(&format!("agents: {{root: {{model: openai/test, structured_output: {output}}}}}")).is_err(), "{output}");
        }
    }

    #[test]
    fn rejects_unimplemented_output_modes_and_claude_fallback_before_open() {
        let cfg = "providers: {local: {}}\nagents:\n  root: {model: local/test}\n  other: {model: 'local/$MODEL', structured_output: {mode: native, name: output, schema: {type: object, properties: {}}}}";
        let tool = cfg.replace("mode: native", "mode: tool");
        assert!(list(&tool).is_ok());
        assert!(select(&tool, "root", &BTreeMap::new())
            .err()
            .unwrap()
            .contains("tool mode"));
        for model in [
            " claude-opus-4.8 ",
            "org/anthropic/CLAUDE-sonnet",
            "anthropic.claude-3",
            "foo/global.anthropic.claude-test",
        ] {
            let env = BTreeMap::from([("MODEL".into(), model.into())]);
            assert!(
                select(cfg, "root", &env).err().unwrap().contains("Claude"),
                "{model}"
            );
        }
        let env = BTreeMap::from([("MODEL".into(), "org/openai/claude-test".into())]);
        assert!(select(cfg, "other", &env).is_ok()); // Go preserves an explicit openai/ qualifier.
        assert!(select(&cfg.replace("mode: native", "mode: ''"), "other", &env).is_ok());
        for name in ["''", "two words", &"x".repeat(65)] {
            assert!(select(
                &cfg.replace("name: output", &format!("name: {name}")),
                "other",
                &env
            )
            .err()
            .unwrap()
            .contains("name must be"));
        }
    }

    const CONFIG: &str = "
providers:
  local:
    api_type: openai_chatcompletions
    temperature: 0.75
    top_p: 0.5
    frequency_penalty: -0.5
    presence_penalty: 0.25
    parallel_tool_calls: true
    max_tokens: 500000
models:
  custom:
    provider: local
    model: 'openai/${env.MODEL}'
    temperature: 0
    top_p: null
    frequency_penalty: 0
    parallel_tool_calls: false
agents:
  root: {model: custom, toolsets: [{type: think}]}
  no_tools: {model: custom}
  inherited: {model: local/gpt-4o-mini, toolsets: [{type: think}]}
";

    #[test]
    fn model_options_override_provider_defaults_field_by_field() {
        let env = BTreeMap::from([("MODEL".into(), "gpt-5".into())]);
        let select_params = |name| primary_params(CONFIG, name, &env);
        assert_eq!(
            select_params("root"),
            json!({"model":"openai/gpt-5", "temperature":0,
            "top_p":0.5, "frequency_penalty":0, "presence_penalty":0.25,
            "max_completion_tokens":500000, "tool_choice":"auto", "parallel_tool_calls":false})
        );
        assert_eq!(
            select_params("inherited"),
            json!({"model":"gpt-4o-mini", "temperature":0.75,
            "top_p":0.5, "frequency_penalty":-0.5, "presence_penalty":0.25,
            "max_tokens":500000, "tool_choice":"auto", "parallel_tool_calls":true})
        );
        let no_tools = select_params("no_tools");
        assert!(no_tools.get("parallel_tool_calls").is_none());
        assert!(no_tools.get("tool_choice").is_none());
        assert_eq!(
            parse(CONFIG).unwrap()["models"]["custom"],
            json!({"provider":"local", "model":"openai/${env.MODEL}", "base_url":""})
        );
    }

    #[test]
    fn service_tier_uses_present_model_option_before_provider() {
        for (option, expected) in [
            ("", Some("priority")),
            (", provider_opts: {}", Some("priority")),
            (", provider_opts: {service_tier: flex}", Some("flex")),
            (", provider_opts: {service_tier: ''}", None),
            (", provider_opts: {service_tier: null}", None),
        ] {
            let cfg = format!("providers: {{p: {{provider_opts: {{service_tier: priority}}}}}}\nmodels: {{m: {{model: test, provider: p{option}}}}}\nagents: {{root: {{model: m}}}}");
            let m = select(&cfg, "root", &BTreeMap::new()).unwrap();
            assert_eq!(
                m.models[0]
                    .model_params
                    .get("service_tier")
                    .and_then(Value::as_str),
                expected,
                "{option}"
            );
        }
    }

    #[test]
    fn openrouter_stays_on_chat_completions_for_newer_models() {
        let cfg = "models: {m: {provider: openrouter, model: openai/gpt-4.1-mini, max_tokens: 12}}\nagents: {root: {model: m}}";
        let env = BTreeMap::from([("OPENROUTER_API_KEY".into(), "key".into())]);
        let params = primary_params(cfg, "", &env);
        assert_eq!(
            params,
            json!({"model":"openai/gpt-4.1-mini", "max_completion_tokens":12})
        );
    }

    #[test]
    fn omitted_and_nonpositive_token_caps_are_not_sent() {
        let yaml = "providers: {local: {}}\nagents: {root: {model: local/test}}";
        let params = |cfg: &str| primary_params(cfg, "", &BTreeMap::new());
        assert_eq!(params(yaml), json!({"model":"test"}));
        for cap in [0, -1] {
            let cfg = format!("providers: {{local: {{max_tokens: 42}}}}\nmodels: {{m: {{provider: local, model: test, max_tokens: {cap}}}}}\nagents: {{root: {{model: m}}}}");
            assert_eq!(params(&cfg), json!({"model":"test"}));
        }
    }

    #[test]
    fn token_budget_wire_key_matches_go_model_families() {
        for (name, expected) in [
            ("gpt-4o-mini", false),
            ("org/test", false),
            ("openrouter/openai/GPT-4.1-mini", true),
            ("gpt-5-chat-latest", true),
            (" o3-mini ", true),
            ("o1-preview", true),
            ("o4-mini", true),
            ("codex-mini", true),
            ("x-codex", true),
            ("gpt-6", true),
            ("gpt-10.2-mini", true),
            ("gpt-6.20260709", false),
            ("gpt-6foo", false),
            ("gpt-100-astra", false),
        ] {
            assert_eq!(supports_responses_api(name), expected, "{name}");
        }
    }

    #[test]
    fn expands_selected_fields_only_once_after_provider_defaults() {
        let cfg = "
providers:
  local: {base_url: '${env.HOST}/$VERSION/', token_key: '${TOKEN_KEY}'}
models:
  override: {provider: local, model: '${ MODEL }', base_url: '${BASE}/v2'}
agents:
  root: {model: 'local/org/${ env.MODEL }'}
  other: {model: override}
";
        let mut env = BTreeMap::from([
            ("HOST".into(), "http://localhost:8080".into()),
            ("VERSION".into(), "v1".into()),
            ("MODEL".into(), "$LITERAL".into()),
            ("${TOKEN_KEY}".into(), "secret".into()),
        ]);
        let s = select(cfg, "root", &env).unwrap();
        assert_eq!(
            s.models[0].transport.endpoint,
            "http://localhost:8080/v1/chat/completions"
        );
        assert_eq!(s.models[0].model_params["model"], "org/$LITERAL");
        assert_eq!(
            s.models[0].transport.headers["Authorization"],
            "Bearer secret"
        ); // token_key is a name, not a template.
        env.remove("HOST");
        assert!(select(cfg, "root", &env).err().unwrap().contains("HOST"));
        env.insert(" MODEL ".into(), "other".into());
        env.insert("BASE".into(), "https://example.invalid".into());
        let s = select(cfg, "other", &env).unwrap();
        assert_eq!(s.models[0].model_params["model"], "other");
        assert_eq!(
            s.models[0].transport.endpoint,
            "https://example.invalid/v2/chat/completions"
        );
        assert!(expand_model_field("${env.PATH}", &env)
            .unwrap_err()
            .contains("PATH"));
        env.insert("MODEL".into(), String::new());
        assert_eq!(
            expand_model_field("pre${MODEL}post", &env).unwrap(),
            "prepost"
        );
        env.insert("BASE".into(), String::new());
        assert!(select(&cfg.replace("${BASE}/v2", "${BASE}"), "other", &env).is_err());
    }

    #[test]
    fn rejects_invalid_options_and_selects_responses_when_required() {
        for option in [
            "temperature: .nan",
            "top_p: nope",
            "max_tokens: 1.5",
            "parallel_tool_calls: 1",
            "top_p: 1, top_p: 0",
            "context_window: 0",
            "provider_opts: {transport: websocket}",
            "provider_opts: {extra_body: {model: other}}",
            "provider_opts: {service_tier: 42}",
            "provider_opts: {service_tier: priority, service_tier: flex}",
            "thinking_budget: low",
            "max_completion_tokens: 42",
        ] {
            let provider =
                format!("providers: {{p: {{{option}}}}}\nagents: {{root: {{model: p/test}}}}");
            let model = format!("models: {{m: {{provider: openai, model: test, {option}}}}}\nagents: {{root: {{model: m}}}}");
            for cfg in [provider, model] {
                assert!(parse(&cfg).is_err(), "{cfg}");
            }
        }
        let env = BTreeMap::from([("OPENAI_API_KEY".into(), "key".into())]);
        for cfg in [
            "agents: {root: {model: openai/gpt-5}}",
            "providers: {local: {}}\nagents: {root: {model: local/gpt-4.1-mini}}",
            "providers: {local: {api_type: openai_responses}}\nagents: {root: {model: local/test}}",
        ] {
            let selected = select(cfg, "", &env).unwrap();
            assert!(
                selected.models[0].responses
                    && selected.models[0]
                        .transport
                        .endpoint
                        .ends_with("/responses")
            );
        }
        let cfg = "providers: {local: {max_tokens: 9007199254740993}}\nagents: {root: {model: local/test}}";
        assert!(select(cfg, "", &BTreeMap::new())
            .err()
            .unwrap()
            .contains("safe integer"));
    }

    #[test]
    fn selects_root_before_first_and_does_not_leak_keys_to_custom_endpoint() {
        let env = BTreeMap::from([
            ("OPENAI_API_KEY".into(), "secret".into()),
            ("WHO".into(), "Ada".into()),
        ]);
        let cfg = "version: 16\nproviders: {local: {base_url: 'http://localhost:8001/v1'}}\nagents:\n  helper: {model: openai/other}\n  root: {model: local/org/name, instruction: '${env.WHO} ${x} ${wrap(${env.WHO})} ${env.ABSENT}'}";
        let s = select(cfg, "", &env).unwrap();
        assert_eq!(s.models[0].model_params["model"], "org/name");
        assert_eq!(s.models[0].transport.headers.get("Authorization"), None);
        assert_eq!(s.instruction, "Ada ${x} ${wrap(${env.WHO})} ");
        assert_eq!(
            select(cfg, "helper", &env).unwrap().models[0]
                .transport
                .headers["Authorization"],
            "Bearer secret"
        );
        assert!(select(cfg, "missing", &env).is_err());
    }
}
