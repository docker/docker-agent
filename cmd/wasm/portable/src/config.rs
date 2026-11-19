use indexmap::IndexMap;
use serde::Deserialize;
use serde_json::{json, Value};
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
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Toolset {
    #[serde(rename = "type")]
    kind: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Model {
    provider: String,
    model: String,
    #[serde(default)]
    base_url: String,
    #[serde(default)]
    token_key: String,
}

#[derive(Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
struct Provider {
    provider: String,
    base_url: String,
    token_key: String,
    api_type: String,
}

pub struct Selected {
    pub model: String,
    pub endpoint: String,
    pub token: String,
    pub instruction: String,
    pub think: bool,
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
            if ts.kind != "think" {
                return Err(format!("unsupported toolset type {:?}", ts.kind));
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
                    base_url: String::new(),
                    token_key: String::new(),
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
            v["sub_agents"] = json!([]);
            v["handoffs"] = json!([]);
            v["add_date"] = false.into();
            v["add_env_info"] = false.into();
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
        let var = expr.trim().strip_prefix("env.").filter(|v| {
            !v.is_empty()
                && !v.as_bytes()[0].is_ascii_digit()
                && v.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'_')
        });
        if let Some(key) = var {
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

pub fn select(yaml: &str, agent: &str, env: &BTreeMap<String, String>) -> Result<Selected, String> {
    let cfg = load(yaml)?;
    for p in cfg.providers.values() {
        if !matches!(p.provider.as_str(), "" | "openai")
            || !matches!(p.api_type.as_str(), "" | "openai_chatcompletions")
        {
            return Err("portable providers must use OpenAI Chat Completions".into());
        }
    }
    for m in cfg.models.values() {
        if !cfg.providers.contains_key(&m.provider)
            && !matches!(m.provider.as_str(), "openai" | "openrouter")
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
    let (default_base, default_key) = match (p, m.provider.as_str()) {
        (None, "openrouter") => ("https://openrouter.ai/api/v1", "OPENROUTER_API_KEY"),
        (None, _) => ("https://api.openai.com/v1", "OPENAI_API_KEY"),
        (Some(_), _) => ("https://api.openai.com/v1", ""),
    };
    let first = |values: [&str; 3]| {
        values
            .into_iter()
            .find(|s| !s.is_empty())
            .unwrap_or("")
            .to_owned()
    };
    let key = first([&m.token_key, p.map_or("", |p| &p.token_key), default_key]);
    let base = first([&m.base_url, p.map_or("", |p| &p.base_url), default_base]);
    if base.contains('$') || m.model.contains('$') {
        return Err("placeholders in model names or endpoints are not supported".into());
    }
    let token = if key.is_empty() {
        String::new()
    } else {
        env.get(&key)
            .filter(|s| !s.is_empty())
            .ok_or_else(|| format!("{key} environment variable is required"))?
            .clone()
    };
    Ok(Selected {
        model: m.model.clone(),
        endpoint: format!("{}/chat/completions", base.trim_end_matches('/')),
        token,
        instruction: instruction(&a.instruction, env),
        think: !a.toolsets.is_empty(),
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
    #[test]
    fn selects_root_before_first_and_does_not_leak_keys_to_custom_endpoint() {
        let env = BTreeMap::from([
            ("OPENAI_API_KEY".into(), "secret".into()),
            ("WHO".into(), "Ada".into()),
        ]);
        let cfg = "version: 16\nproviders: {local: {base_url: 'http://localhost:8001/v1'}}\nagents:\n  helper: {model: openai/other}\n  root: {model: local/org/name, instruction: '${env.WHO} ${x} ${wrap(${env.WHO})} ${env.ABSENT}'}";
        let s = select(cfg, "", &env).unwrap();
        assert_eq!(s.model, "org/name");
        assert_eq!(s.token, "");
        assert_eq!(s.instruction, "Ada ${x} ${wrap(${env.WHO})} ");
        assert_eq!(select(cfg, "helper", &env).unwrap().token, "secret");
        assert!(select(cfg, "missing", &env).is_err());
    }
}
