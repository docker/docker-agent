//! Validate the internal tool against the configured schema, not OpenAI's normalized copy.
//! Each schema node has one explicit type and an exhaustive keyword allowlist; there is no resolver.
use serde_json::{json, Value};
use serde_yaml_ng::Value as Instance;
use std::collections::BTreeSet;

pub const NAME: &str = "__structured_output__";
pub const REMINDER: &str = "Reminder: this conversation requires structured output. Do not answer in plain text. Call the __structured_output__ tool with your final answer as arguments matching its JSON schema; a valid call ends the turn.";
const MAX_REMINDERS: usize = 2;
pub const MAX_ARGS: usize = 64 * 1024;
const MAX_SCHEMA_NODES: usize = 128;
const NON_EXCLUSIVE: &str = "Structured output rejected: __structured_output__ must be the only tool call in the response. Finish any other tool use first, then call __structured_output__ alone with the final answer.";

fn safe_integer(n: i64) -> bool {
    n.unsigned_abs() < 1 << 53
}

pub fn stop_error(model: &str, reminders: usize) -> Option<String> {
    let normalized = model.trim().to_ascii_lowercase();
    if let Some(name) = crate::config::claude_name(&normalized) {
        // modelinfo.LookupClaude5: Vertex dates, Bedrock revisions and inference profiles.
        let name = name.split(['@', ':']).next().unwrap();
        let name = name
            .rsplit_once("-v")
            .filter(|(_, rev)| !rev.is_empty() && rev.bytes().all(|b| b.is_ascii_digit()))
            .map_or(name, |(name, _)| name);
        if matches!(
            name,
            "claude-haiku-5-5"
                | "claude-opus-5"
                | "claude-opus-5-5"
                | "claude-sonnet-5"
                | "claude-sonnet-5-5"
                | "claude-fable-5"
                | "claude-fable-5-1"
                | "claude-fable-5-2"
                | "claude-mythos-5"
                | "claude-mythos-5-1"
                | "claude-mythos-5-2"
        ) {
            return Some(format!("Agent terminated: the model did not deliver structured output via the {NAME} tool; {model} cannot be reminded because it rejects a request ending on its own answer."));
        }
    }
    (reminders >= MAX_REMINDERS).then(|| format!("Agent terminated: the model did not deliver structured output via the {NAME} tool after {MAX_REMINDERS} reminders."))
}

pub struct OutputTool {
    name: String,
    schema: Value,
    pub definition: Value,
    pub strict: bool,
}

impl OutputTool {
    pub fn new(name: &str, description: &str, schema: &Value) -> Result<Self, String> {
        if description.len() > 4096 {
            return Err("tool mode description exceeds 4 KiB".into());
        }
        if schema["type"] != "object" {
            return Err("tool output must be an object schema".into());
        }
        let mut strict = true;
        let mut nodes_left = MAX_SCHEMA_NODES;
        let parameters = parameters(schema, 0, &mut nodes_left, &mut strict)?;
        let mut text = format!("Deliver the final answer of this conversation as structured output ({name}). The arguments must be a JSON object matching the tool's parameter schema. Call this tool alone, with no other tool calls in the same response; a valid call ends the turn.");
        if !description.is_empty() {
            text.push_str(" Expected content: ");
            text.push_str(description);
        }
        Ok(Self {
            name: name.into(),
            schema: schema.clone(),
            strict,
            definition: json!({"type":"function", "function":{"name":NAME,"description":text,"parameters":parameters}}),
        })
    }

    pub fn call(&self, raw: &str, exclusive: bool) -> Result<String, String> {
        if !exclusive {
            return Err(NON_EXCLUSIVE.into());
        }
        self.validate(raw).map_err(|e| {
            format!("Structured output rejected: {e}. Fix the arguments and call {NAME} again.")
        })
    }

    fn validate(&self, raw: &str) -> Result<String, String> {
        if raw.len() > MAX_ARGS {
            return Err("arguments exceed 64 KiB".into());
        }
        let raw = if raw.trim().is_empty() { "{}" } else { raw };
        // JSON syntax only. This value visitor also rejects duplicate keys, including escaped aliases.
        let value: Instance = serde_json::from_str(raw).map_err(|e| {
            if e.is_eof() {
                "arguments are not valid JSON: unexpected end of JSON input".into()
            } else {
                format!("arguments are not valid JSON: {e}")
            }
        })?;
        validate(&self.schema, &value).map_err(|e| {
            format!(
                "arguments do not match the {:?} schema: validating root: {e}",
                self.name
            )
        })?;
        // Like json.Compact, retain key order, string escapes and number spelling.
        let (mut quoted, mut escaped) = (false, false);
        Ok(raw
            .chars()
            .filter(|&c| {
                if escaped {
                    escaped = false;
                } else if c == '"' {
                    quoted = !quoted;
                } else if quoted && c == '\\' {
                    escaped = true;
                }
                quoted || !c.is_ascii_whitespace()
            })
            .collect())
    }
}

// Reject unknown keywords at every node before copying anything into the tool definition.
fn parameters(
    s: &Value,
    depth: usize,
    left: &mut usize,
    strict: &mut bool,
) -> Result<Value, String> {
    if depth > 16 || *left == 0 {
        return Err("tool schema exceeds depth or node limit".into());
    }
    *left -= 1;
    let ty = s["type"]
        .as_str()
        .ok_or("tool schema nodes require a single explicit type")?;
    if !matches!(
        ty,
        "object" | "array" | "string" | "integer" | "number" | "boolean" | "null"
    ) {
        return Err("unsupported tool schema type".into());
    }
    let mut wire = json!({});
    for (k, v) in s.as_object().unwrap() {
        let valid = match (k.as_str(), ty) {
            ("type", _) => true,
            ("description" | "title", _) => v.as_str().is_some_and(|d| d.len() <= 1024),
            ("properties", "object") => v.as_object().is_some_and(|p| p.len() <= 64),
            ("required", "object") => v.as_array().is_some_and(|r| r.len() <= 64),
            ("additionalProperties", "object") => v.is_boolean(),
            ("items", "array") => v.is_object(),
            ("minimum" | "maximum", "integer") => v.as_i64().is_some_and(safe_integer),
            ("minItems" | "maxItems", "array") | ("minLength" | "maxLength", "string") => {
                v.as_u64().is_some_and(|n| n <= MAX_ARGS as u64)
            }
            ("enum", "string") => v.as_array().is_some_and(|a| {
                let mut seen = BTreeSet::new();
                (1..=32).contains(&a.len())
                    && a.iter()
                        .all(|x| x.as_str().is_some_and(|s| s.len() <= 128 && seen.insert(s)))
            }),
            _ => false,
        };
        if !valid {
            return Err(format!(
                "invalid or unsupported tool schema keyword {k:?} for {ty}"
            ));
        }
        if !matches!(k.as_str(), "properties" | "items" | "required") {
            wire[k] = v.clone();
        }
    }
    if ty == "array" {
        wire["items"] = parameters(&s["items"], depth + 1, left, strict)?;
    } else if ty == "object" {
        let props = s["properties"]
            .as_object()
            .ok_or("tool object schemas require properties")?;
        let mut required = BTreeSet::new();
        for key in s["required"].as_array().into_iter().flatten() {
            if !key
                .as_str()
                .is_some_and(|k| props.contains_key(k) && required.insert(k))
            {
                return Err(
                    "tool schema required must contain unique declared property names".into(),
                );
            }
        }
        *strict &= s["additionalProperties"] != true;
        wire["additionalProperties"] = false.into();
        wire["required"] = props.keys().cloned().collect();
        let mut normalized = serde_json::Map::new();
        for (key, child) in props {
            if key.len() > 128 || matches!(key.as_str(), "$ref" | "x-docker-agent-property-order") {
                return Err("unsupported tool schema property name".into());
            }
            let mut prop = parameters(child, depth + 1, left, strict)?;
            if !required.contains(key.as_str()) {
                if child["type"] == "null" {
                    return Err("optional null-only tool properties are unsupported".into());
                }
                if matches!(child["type"].as_str(), Some("array" | "object"))
                    || child.get("enum").is_some()
                {
                    prop = json!({"anyOf":[prop,{"type":"null"}]});
                } else {
                    prop["type"] = json!([child["type"], "null"]);
                }
            }
            normalized.insert(key.clone(), prop);
        }
        wire["properties"] = normalized.into();
    }
    Ok(wire)
}

fn validate(s: &Value, v: &Instance) -> Result<(), String> {
    let ty = s["type"].as_str().unwrap();
    let bounds = |n: usize, min: &str, max: &str| {
        if s[min].as_u64().is_some_and(|v| (n as u64) < v) {
            Err(format!("{min}: length {n} is less than {}", s[min]))
        } else if s[max].as_u64().is_some_and(|v| n as u64 > v) {
            Err(format!("{max}: length {n} is greater than {}", s[max]))
        } else {
            Ok(())
        }
    };
    match (ty, v) {
        ("object", Instance::Mapping(m)) => {
            for (key, value) in m {
                let key = key.as_str().expect("JSON key");
                if let Some(child) = s["properties"].get(key) {
                    validate(child, value).map_err(|e| format!("property {key:?}: {e}"))?;
                } else if s["additionalProperties"] == false {
                    return Err(format!("unexpected additional properties [{key:?}]"));
                }
            }
            let missing: Vec<_> = s["required"]
                .as_array()
                .into_iter()
                .flatten()
                .filter_map(Value::as_str)
                .filter(|k| !m.contains_key(*k))
                .map(|s| format!("{s:?}"))
                .collect();
            if !missing.is_empty() {
                return Err(format!(
                    "required: missing properties: [{}]",
                    missing.join(" ")
                ));
            }
        }
        ("array", Instance::Sequence(a)) => {
            bounds(a.len(), "minItems", "maxItems")?;
            for (i, item) in a.iter().enumerate() {
                validate(&s["items"], item).map_err(|e| format!("item {i}: {e}"))?;
            }
        }
        ("string", Instance::String(text)) => {
            if s["enum"]
                .as_array()
                .is_some_and(|a| !a.iter().any(|v| v == text.as_str()))
            {
                return Err("string is not a member of enum".into());
            }
            bounds(text.chars().count(), "minLength", "maxLength")?;
        }
        ("null", Instance::Null)
        | ("boolean", Instance::Bool(_))
        | ("number", Instance::Number(_)) => (),
        ("integer", Instance::Number(n)) => {
            let n = n
                .as_i64()
                .filter(|&n| safe_integer(n))
                .ok_or("expected a safe integer literal")?;
            if s["minimum"].as_i64().is_some_and(|m| n < m) {
                return Err(format!("minimum: {n} is less than {}", s["minimum"]));
            }
            if s["maximum"].as_i64().is_some_and(|m| n > m) {
                return Err(format!("maximum: {n} is greater than {}", s["maximum"]));
            }
        }
        _ => return Err(format!("expected type {ty:?}")),
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn object(child: Value) -> Value {
        json!({"type":"object", "properties":{"x":child}, "required":["x"], "additionalProperties":false})
    }

    fn tool(schema: &Value) -> Result<OutputTool, String> {
        OutputTool::new("report", "", schema)
    }

    #[test]
    fn validates_original_schema_not_nullable_wire_properties() {
        let schema = json!({"type":"object", "properties":{
            "tags":{"type":"array", "minItems":1,"maxItems":2,"items":{"type":"string", "enum":["🌍","AB"], "maxLength":1}},
            "label":{"type":"string", "minLength":2}, "size":{"type":"integer","minimum":-2,"maximum":2},
            "ok":{"type":"boolean"}, "count":{"type":"number"}, "empty":{"type":"null"}}, "required":["tags","empty"], "additionalProperties":false});
        let t = tool(&schema).unwrap();
        assert!(t.strict);
        assert_eq!(
            t.definition["function"]["parameters"]["properties"]["size"]["type"],
            json!(["integer", "null"])
        );
        let valid = r#"{ "tags" : [ "🌍" ], "size": -2,"count":2.5e+1,"ok":false,"empty":null,"label":"\u0041\\\" B" }"#;
        assert_eq!(
            t.call(valid, true).unwrap(),
            r#"{"tags":["🌍"],"size":-2,"count":2.5e+1,"ok":false,"empty":null,"label":"\u0041\\\" B"}"#
        );
        for invalid in [
            valid.replace("🌍", "AB"),
            valid.replace("🌍", "X"),
            valid.replace("\"🌍\"", "true"),
            valid.replace("\"🌍\"", ""),
            valid.replace("\"🌍\"", "\"🌍\",\"🌍\",\"🌍\""),
            valid.replace("-2", "3"),
            valid.replace("-2", "-3"),
            valid.replace("-2", "1.2"),
            valid.replace("-2", "1.00000000000000001"),
            valid.replace("-2", "9007199254740993"),
            valid.replace("-2", "null"),
            valid.replace("false", "\"false\""),
            valid.replace("2.5e+1", "true"),
            valid.replace("null", "0"),
            valid.replace("label", "extra"),
            valid.replace(r#"\u0041\\\" B"#, "é"),
            "{}".into(),
            "null".into(),
            "[]".into(),
            "{".into(),
        ] {
            let err = t.call(&invalid, true).unwrap_err();
            assert!(err.starts_with("Structured output rejected:"), "{err}");
            assert!(err.ends_with("call __structured_output__ again."), "{err}");
        }
    }

    #[test]
    fn rejects_ambiguous_or_oversize_arguments_and_nonexclusive_calls() {
        let t =
            tool(&json!({"type":"object", "properties":{}, "additionalProperties":true})).unwrap();
        assert!(!t.strict);
        assert_eq!(t.call("\n\t ", true).unwrap(), "{}");
        assert_eq!(
            t.call(r#"{ "x": [1,true,null] }"#, true).unwrap(),
            r#"{"x":[1,true,null]}"#
        );
        assert!(t
            .call(r#"{"extra":{"x":0,"\u0078":1}}"#, true)
            .unwrap_err()
            .contains("duplicate"));
        assert!(t
            .call(&" ".repeat(MAX_ARGS + 1), true)
            .unwrap_err()
            .contains("64 KiB"));
        let deep = format!("{{\"a\":{}0{}}}", "[".repeat(128), "]".repeat(128));
        assert!(t.call(&deep, true).is_err());
        assert_eq!(t.call("not even JSON", false), Err(NON_EXCLUSIVE.into()));
    }

    #[test]
    fn rejects_every_unsupported_subschema_or_constraint_before_inference() {
        for (keyword, v) in [
            ("$ref", json!("#")),
            ("$ref", json!("https://invalid.test/s")),
            ("$ref", json!("file:///schema")),
            ("$defs", json!({})),
            (
                "$schema",
                json!("https://json-schema.org/draft/2020-12/schema"),
            ),
            ("pattern", json!(".*")),
            ("patternProperties", json!({})),
            ("format", json!("email")),
            ("not", json!({})),
            ("if", json!({})),
            ("then", json!({})),
            ("else", json!({})),
            ("dependentSchemas", json!({})),
            ("anyOf", json!([{}])),
            ("allOf", json!([{}])),
            ("oneOf", json!([{}])),
            ("prefixItems", json!([{}])),
            ("additionalProperties", json!({})),
            ("propertyNames", json!({})),
            ("minLength", json!(1)),
            ("unknown", Value::Null),
        ] {
            let mut nested = object(json!({"type":"number"}));
            nested[keyword] = v;
            let schema = object(json!({"type":"array", "items":nested}));
            assert!(tool(&schema).err().unwrap().contains(keyword), "{keyword}");
        }
        for child in [
            json!(true),
            json!({}),
            json!({"type":["null","string"]}),
            json!({"type":"array"}),
            json!({"type":"array","items":false}),
            json!({"type":"array","items":[{}]}),
            json!({"type":"object"}),
            json!({"type":"object", "properties":{}, "required":["missing"]}),
            json!({"type":"object", "properties":{"y":{"type":"string"}}, "required":["y","y"]}),
            json!({"type":"integer","minimum":0.5}),
            json!({"type":"number","maximum":1}),
            json!({"type":"number","enum":[1,2]}),
            json!({"type":"string","enum":["A","A"]}),
            json!({"type":"string","enum":[]}),
            json!({"type":"string","minLength":-1}),
            json!({"type":"string","minLength":0.1}),
            json!({"type":"object","properties":{"$ref":{"type":"number"}}}),
        ] {
            assert!(tool(&object(child.clone())).is_err(), "{child}");
        }
    }

    #[test]
    fn rejects_oversized_schema_before_normalizing() {
        let mut deep = json!({"type":"string"});
        for _ in 0..17 {
            deep = json!({"type":"array", "items":deep});
        }
        let mut props = serde_json::Map::new();
        for n in 0..64 {
            props.insert(
                n.to_string(),
                json!({"type":"array", "items":{"type":"string"}}),
            );
        }
        for child in [
            deep,
            json!({"type":"object", "properties":props}),
            json!({"type":"string", "enum":["a".repeat(129)]}),
            json!({"type":"string", "title":"x".repeat(1025)}),
        ] {
            assert!(tool(&object(child)).is_err());
        }
    }
}
