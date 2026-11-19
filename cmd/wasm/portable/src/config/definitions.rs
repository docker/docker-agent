//! Local references are names in this document, never files, catalog entries or commands.
use super::{Config, Toolset};
use serde_json::Value;
use std::collections::BTreeMap;

pub(super) fn validate_mcp(ts: &Toolset) -> Result<(), String> {
    if let Some(name) = ts.options.get("ref").and_then(Value::as_str) {
        if name.is_empty() || name.starts_with("docker:") || ts.options.contains_key("remote") {
            return Err("mcp requires exactly one remote source or local definition ref".into());
        }
        return Ok(());
    }
    validate_remote(
        ts.options
            .get("remote")
            .ok_or("mcp requires remote.url or ref")?,
    )
}

fn validate_remote(remote: &Value) -> Result<(), String> {
    let map = remote.as_object().ok_or("remote must be an object")?;
    if remote["url"].as_str().is_none_or(|s| s.is_empty()) {
        return Err("mcp requires remote.url".into());
    }
    for (key, val) in map {
        let valid = match key.as_str() {
            "url" => true,
            "transport_type" => {
                matches!(val.as_str(), Some("streamable" | "streamable-http" | "sse"))
            }
            "standalone_sse" => val == false,
            "headers" => val
                .as_object()
                .is_some_and(|h| h.values().all(Value::is_string)),
            "oauth" => val.as_object().is_some_and(|o| {
                o.iter().all(|(k, v)| match k.as_str() {
                    "clientId" | "callbackRedirectURL" => v.is_string(),
                    "scopes" => v.as_array().is_some_and(|a| a.iter().all(Value::is_string)),
                    _ => false,
                })
            }),
            _ => false,
        };
        if !valid {
            return Err(format!("unsupported or invalid remote MCP option {key:?}"));
        }
    }
    Ok(())
}

// Bound the amplification from cloning a large definition through many small refs.
struct Definition {
    toolset: Toolset,
    cost: usize,
}

impl Definition {
    fn new(toolset: Toolset) -> Self {
        let bytes = serde_json::to_vec(&toolset)
            .expect("serializable toolset")
            .len();
        // Defer is Rust-only, so the serialized host spec doesn't account for its names.
        let (_, names) = toolset.defer.parts();
        let cost =
            (bytes + size_of_val(names) + names.iter().map(String::len).sum::<usize>()).max(256);
        Self { toolset, cost }
    }

    fn charge(&self, remaining: &mut usize) -> Result<(), String> {
        *remaining = remaining
            .checked_sub(self.cost)
            .ok_or("toolset reference expansion limit exceeded")?;
        Ok(())
    }
}

fn resolve_mcp(
    ts: &mut Toolset,
    mcps: &BTreeMap<String, Definition>,
    remaining: &mut usize,
) -> Result<(), String> {
    let Some(name) = ts.options.remove("ref") else {
        return Ok(());
    };
    let name = name.as_str().ok_or("MCP ref must be a string")?;
    let def = mcps
        .get(name)
        .ok_or_else(|| format!("non-existent MCP definition {name:?}"))?;
    def.charge(remaining)?;
    let def = &def.toolset;
    if ts.instruction.is_empty() {
        ts.instruction.clone_from(&def.instruction);
    }
    if ts.tools.is_empty() {
        ts.tools.clone_from(&def.tools);
    }
    let (all, names) = ts.defer.parts();
    if !all && names.is_empty() {
        ts.defer.clone_from(&def.defer);
    }
    if ts.options.get("name").and_then(Value::as_str) == Some("") {
        ts.options.remove("name");
    }
    for (key, val) in &def.options {
        ts.options.entry(key.clone()).or_insert_with(|| val.clone());
    }
    Ok(())
}

pub(super) fn resolve(cfg: &mut Config) -> Result<(), String> {
    let mut remaining = 16 * 1024 * 1024;
    let mut mcps = BTreeMap::new();
    for (name, mut ts) in std::mem::take(&mut cfg.mcps) {
        if ts.kind.is_empty() {
            ts.kind = "mcp".into();
        }
        if ts.kind != "mcp" || ts.options.contains_key("ref") {
            return Err(format!("mcps.{name}: requires a remote MCP definition"));
        }
        if ts.readonly {
            // applyMCPDefaults does not inherit readonly. Never silently drop it.
            return Err(format!(
                "mcps.{name}: set readonly on the referencing toolset"
            ));
        }
        ts.validate().map_err(|e| format!("mcps.{name}: {e}"))?;
        mcps.insert(name, Definition::new(ts));
    }
    let mut toolsets = BTreeMap::new();
    for (name, ts) in std::mem::take(&mut cfg.toolsets) {
        ts.validate().map_err(|e| format!("toolsets.{name}: {e}"))?;
        toolsets.insert(name, Definition::new(ts));
    }
    for (name, agent) in &mut cfg.agents {
        for ts in &agent.toolsets {
            ts.validate().map_err(|e| format!("agent {name:?}: {e}"))?;
        }
        for key in &agent.use_toolsets {
            let def = toolsets
                .get(key)
                .ok_or_else(|| format!("agent {name:?} references non-existent toolset {key:?}"))?;
            def.charge(&mut remaining)?;
            agent.toolsets.push(def.toolset.clone());
        }
        for ts in &mut agent.toolsets {
            if ts.kind == "mcp" {
                resolve_mcp(ts, &mcps, &mut remaining)
                    .map_err(|e| format!("agent {name:?}: {e}"))?;
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use crate::config::{load, parse};
    use serde_json::json;

    #[test]
    fn resolves_in_declaration_order_with_independent_overrides() {
        let cfg = load("\
mcps:
  same: {name: base, tools: [base_echo], instruction: defaults, remote: {url: 'https://example.test/mcp', headers: {X-Test: '${env.HEADER}'}}, allow_private_ips: true}
toolsets:
  same: {type: mcp, ref: same, allow_private_ips: false, tools: [n_echo], instruction: override, name: n, readonly: true}
agents:
  first: {model: openai/test, toolsets: [{type: think}], use_toolsets: [same, same]}
  other: {model: openai/test, toolsets: [{type: mcp, ref: same, name: '', instruction: '', tools: []}]}
").unwrap();
        let first = &cfg.agents["first"].toolsets;
        assert_eq!(
            first.iter().map(|ts| ts.kind.as_str()).collect::<Vec<_>>(),
            ["think", "mcp", "mcp"]
        );
        let base = json!({"url":"https://example.test/mcp", "headers":{"X-Test":"${env.HEADER}"}});
        for ts in &first[1..] {
            assert_eq!(
                json!(ts),
                json!({"type":"mcp", "name":"n", "remote":base, "tools":["n_echo"], "readonly":true, "instruction":"override", "allow_private_ips":false})
            );
        }
        assert_eq!(
            json!(cfg.agents["other"].toolsets),
            json!([{"type":"mcp", "name":"base", "remote":base, "tools":["base_echo"], "instruction":"defaults", "allow_private_ips":true}])
        );
    }

    #[test]
    fn rejects_invalid_unused_definitions_and_unsafe_refs() {
        for yaml in [
            "toolsets: {a: {type: think}, a: {type: todo}}",
            "mcps: {a: {}, a: {remote: {url: x}}}",
            "toolsets: {a: {ref: foo}}",
            "toolsets: {a: {type: mcp, ref: docker:foo}}",
            "toolsets: {a: {type: mcp, ref: s, remote: {url: x}}}",
            "toolsets: {a: {type: think, ref: s}}",
            "mcps: {a: {ref: s}}",
            "mcps: {a: {command: program, args: [a]}}",
            "mcps: {a: {remote: {url: x}, working_dir: .}}",
            "mcps: {a: {remote: {url: x}, env: {X: y}}}",
            "mcps: {a: {remote: {url: x}, readonly: true}}",
            "mcps: {a: {remote: {url: x}, allow_private_ips: null}}",
            "mcps: {a: {remote: {url: x}, allow_private_ips: .nan}}",
            "mcps: {a: {remote: {url: x, headers: {X: nope, X: yep}}}}",
            "mcps: {a: {remote: {url: x, headers: {X: []}}}}",
            "mcps: {a: {remote: {url: x, oauh: {}}}}",
            "mcps: {a: {remote: {url: x, oauth: {scope: read}}}}",
            "mcps: {a: {remote: {url: x, oauth: {clientSecret: secret}}}}",
            "mcps: {a: {remote: {url: x, oauth: {scopes: not-a-list}}}}",
        ] {
            assert!(
                parse(&format!("agents: {{root: {{model: openai/test}}}}\n{yaml}")).is_err(),
                "{yaml}"
            );
        }
    }

    #[test]
    fn bounded_expansion_rejects_repeated_large_definitions() {
        let refs = ["large"; 1024].join(",");
        let description = "x".repeat(32 * 1024);
        let yaml = format!("toolsets: {{large: {{type: think, instruction: '{description}'}}}}\nagents: {{root: {{model: openai/test, use_toolsets: [{refs}]}}}}");
        assert!(parse(&yaml).unwrap_err().contains("expansion limit"));
    }
}
