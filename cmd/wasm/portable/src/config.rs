use indexmap::IndexMap;
use serde::{Deserialize, Serialize};
use serde_json::{json, Map, Number, Value};
use std::collections::BTreeMap;

const VERSION: &str = "16";

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Config {
    #[serde(default)]
    version: Option<Value>,
    agents: IndexMap<String, Agent>,
    #[serde(default)]
    models: BTreeMap<String, Model>,
    #[serde(default)]
    providers: BTreeMap<String, Provider>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Agent {
    model: String,
    #[serde(default)]
    description: String,
    #[serde(default)]
    instruction: String,
    #[serde(default)]
    toolsets: Vec<Toolset>,
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
}

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
pub struct Toolset {
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub shared: bool,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<String>,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub readonly: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub instruction: String,
    #[serde(flatten)]
    pub options: BTreeMap<String, Value>,
}

impl Toolset {
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
            "mcp" => &["allow_private_ips", "remote"],
            _ => &[],
        };
        for (name, val) in &self.options {
            if !allowed.contains(&name.as_str()) {
                return Err(format!("unsupported {} option {name:?}", self.kind));
            }
            let valid = match name.as_str() {
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
            "mcp" => {
                let remote = self
                    .options
                    .get("remote")
                    .and_then(Value::as_object)
                    .ok_or("mcp requires remote.url")?;
                if remote
                    .get("url")
                    .and_then(Value::as_str).is_none_or(|s| s.is_empty())
                {
                    return Err("mcp requires remote.url".into());
                }
                if remote
                    .keys()
                    .any(|k| !matches!(k.as_str(), "url" | "headers" | "transport_type" | "oauth"))
                {
                    return Err("unsupported remote MCP configuration".into());
                }
            }
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
                        .and_then(Value::as_str).is_none_or(|s| s.is_empty())
                }) {
                    return Err("api needs a name and endpoint".into());
                }
            }
            _ => {}
        }
        // Only project toolsets that do not require additional configuration.
        match self.kind.as_str() {
            "api" | "mcp" | "calculator" | "datetime" | "fetch" | "file" | "filesystem"
            | "memory" | "plan" | "random" | "session_context" | "shell" | "tasks" | "think"
            | "todo" | "user_prompt" => Ok(()),
            _ => Err(format!("unsupported toolset type {:?}", self.kind)),
        }
    }
}

#[derive(Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct Model {
    provider: String,
    model: String,
    #[serde(default)]
    base_url: String,
    #[serde(default)]
    token_key: String,
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

pub struct Selected {
    pub model_params: Map<String, Value>,
    pub endpoint: String,
    pub token: String,
    pub instruction: String,
    pub toolsets: Vec<Toolset>,
    pub responses: bool,
    pub merge_messages: bool,
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
    for (name, a) in &cfg.agents {
        if name.trim().is_empty() {
            return Err("empty agent name".into());
        }
        for ts in &a.toolsets {
            ts.validate()?;
        }
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
            let (provider, model) = a
                .model
                .split_once('/')
                .filter(|(p, m)| !p.is_empty() && !m.is_empty())
                .ok_or_else(|| {
                    format!("agent {name:?} references non-existent model {:?}", a.model)
                })?;
            cfg.models.insert(
                a.model.clone(),
                Model {
                    provider: provider.into(),
                    model: model.into(),
                    ..Model::default()
                },
            );
        }
    }
    for model in cfg.models.values() {
        if model.model.trim().is_empty() || model.provider.trim().is_empty() {
            return Err("model and provider must be set".into());
        }
    }
    Ok(cfg)
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

fn model_params(
    id: String,
    m: &Model,
    p: &Provider,
    tools: bool,
) -> Result<Map<String, Value>, String> {
    let mut params = Map::new();
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
        if cap > (1_i64 << 53) - 1 {
            return Err("max_tokens exceeds the JavaScript safe integer range".into());
        }
        // No catalog or context_window declarations in this profile: no known window to clamp to.
        let key = if supports_responses_api(&id) {
            "max_completion_tokens"
        } else {
            "max_tokens"
        };
        params.insert(key.into(), cap.into());
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

pub fn select(yaml: &str, agent: &str, env: &BTreeMap<String, String>) -> Result<Selected, String> {
    let cfg = load(yaml)?;
    for (name, a) in &cfg.agents {
        for (enabled, field) in [
            (!a.sub_agents.is_empty(), "sub_agents"),
            (!a.handoffs.is_empty(), "handoffs"),
            (a.add_date, "add_date"),
            (a.add_environment_info, "add_environment_info"),
        ] {
            if enabled {
                return Err(format!(
                    "agent {name:?}: {field} is not supported in portable sessions"
                ));
            }
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
            ) {
                return Err(format!("unsupported toolset type {:?}", ts.kind));
            }
        }
    }
    for p in cfg.providers.values() {
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
        if !cfg.providers.contains_key(&m.provider)
            && crate::providers::lookup(&m.provider).is_none()
        {
            return Err(format!("unsupported provider {:?}", m.provider));
        }
    }
    let a = if agent.is_empty() {
        cfg.agents
            .get("root")
            .or_else(|| cfg.agents.first().map(|(_, a)| a))
    } else {
        cfg.agents.get(agent)
    }
    .ok_or_else(|| format!("agent {agent:?} not found"))?;
    let m = &cfg.models[&a.model];
    let p = cfg.providers.get(&m.provider);
    let builtin = crate::providers::lookup(&m.provider).filter(|_| p.is_none());
    let default_base = builtin.map_or("https://api.openai.com/v1", |b| b.base_url.as_str());
    let default_key = builtin.map_or("", |b| b.token_env_vars[0].as_str());
    // Go resolves a custom provider's default protocol before expanding the model.
    let id = expand_model_field(&m.model, env)?;
    let responses = match p {
        Some(p) => {
            p.api_type == "openai_responses"
                || (p.api_type.is_empty() && supports_responses_api(&m.model))
        }
        None => builtin.is_some_and(|b| b.auto_responses) && supports_responses_api(&id),
    };
    let custom = p.is_some();
    let default_provider = Provider::default();
    let p = p.unwrap_or(&default_provider);
    let key = first_nonempty([&m.token_key, &p.token_key, default_key]);
    let base = expand_model_field(
        first_nonempty([&m.base_url, &p.base_url, default_base]),
        env,
    )?;
    if id.trim().is_empty() || base.trim().is_empty() {
        return Err("model and endpoint must not expand to an empty string".into());
    }
    let token = if key.is_empty() {
        String::new()
    } else {
        env.get(key)
            .filter(|s| !s.is_empty())
            .ok_or_else(|| format!("{key} environment variable is required"))?
            .clone()
    };
    let merge_messages = !responses
        && (custom
            || builtin.is_some_and(|b| b.merge_consecutive_messages)
            || (m.provider == "openai" && base.trim_end_matches('/') != default_base));
    let route = if responses {
        "responses"
    } else {
        "chat/completions"
    };
    let think = !a.toolsets.is_empty();
    Ok(Selected {
        model_params: model_params(id, m, p, think)?,
        endpoint: format!("{}/{route}", base.trim_end_matches('/')),
        responses,
        merge_messages,
        token,
        instruction: instruction(&a.instruction, env),
        toolsets: a
            .toolsets
            .iter()
            .cloned()
            .map(|mut ts| {
                ts.readonly |= a.readonly;
                ts.instruction = instruction(&ts.instruction, env);
                ts
            })
            .collect(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn strict_loader() {
        for yaml in [
            "agents: {}",
            "agents: {a: {model: nope}}",
            "agents: {a: {model: a/b, hooks: {}}}",
            "agents: {a: {model: a/b}, a: {model: a/c}}",
            "version: 2\nagents: {a: {model: a/b}}",
        ] {
            assert!(parse(yaml).is_err(), "{yaml}");
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
        let select_params = |name| Value::Object(select(CONFIG, name, &env).unwrap().model_params);
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
    fn openrouter_stays_on_chat_completions_for_newer_models() {
        let cfg = "models: {m: {provider: openrouter, model: openai/gpt-4.1-mini, max_tokens: 12}}\nagents: {root: {model: m}}";
        let env = BTreeMap::from([("OPENROUTER_API_KEY".into(), "key".into())]);
        let params = Value::Object(select(cfg, "", &env).unwrap().model_params);
        assert_eq!(
            params,
            json!({"model":"openai/gpt-4.1-mini", "max_completion_tokens":12})
        );
    }

    #[test]
    fn omitted_and_nonpositive_token_caps_are_not_sent() {
        let yaml = "providers: {local: {}}\nagents: {root: {model: local/test}}";
        let params =
            |cfg: &str| Value::Object(select(cfg, "", &BTreeMap::new()).unwrap().model_params);
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
        assert_eq!(s.endpoint, "http://localhost:8080/v1/chat/completions");
        assert_eq!(s.model_params["model"], "org/$LITERAL");
        assert_eq!(s.token, "secret"); // token_key is a name, not a template.
        env.remove("HOST");
        assert!(select(cfg, "root", &env).err().unwrap().contains("HOST"));
        env.insert(" MODEL ".into(), "other".into());
        env.insert("BASE".into(), "https://example.invalid".into());
        let s = select(cfg, "other", &env).unwrap();
        assert_eq!(s.model_params["model"], "other");
        assert_eq!(s.endpoint, "https://example.invalid/v2/chat/completions");
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
            "context_window: 8192",
            "provider_opts: {}",
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
            assert!(selected.responses && selected.endpoint.ends_with("/responses"));
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
        assert_eq!(s.model_params["model"], "org/name");
        assert_eq!(s.token, "");
        assert_eq!(s.instruction, "Ada ${x} ${wrap(${env.WHO})} ");
        assert_eq!(select(cfg, "helper", &env).unwrap().token, "secret");
        assert!(select(cfg, "missing", &env).is_err());
    }
}
