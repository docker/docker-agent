//! Validate the internal tool against the configured schema, not OpenAI's normalized copy.
//! Only bare JSON Pointer references to entries in the root `$defs` are supported.
use serde_json::{json, Value};
use serde_yaml_ng::Value as Instance;
use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;

pub const NAME: &str = "__structured_output__";
pub const REMINDER: &str = "Reminder: this conversation requires structured output. Do not answer in plain text. Call the __structured_output__ tool with your final answer as arguments matching its JSON schema; a valid call ends the turn.";
pub const MAX_REMINDERS: usize = 2;
pub const MAX_ARGS: usize = 64 * 1024;
const MAX_SCHEMA_NODES: usize = 128;
const MAX_DEPTH: usize = 16;
const MAX_EXPANDED_NODES: usize = 512;
const MAX_ALTERNATIVES: usize = 4;
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

// Go's YAML round trip and encoding/json don't preserve every serde scalar spelling.
pub fn check_prompt_schema(name: &str, description: &str, schema: &Value) -> Result<(), String> {
    fn plain(s: &str) -> bool {
        let numeric = s.starts_with(|c: char| c.is_ascii_digit() || matches!(c, '+' | '-' | '.'))
            && s.bytes().any(|b| b.is_ascii_digit());
        !(numeric || s.starts_with("? ") || s.chars().any(char::is_control))
    }
    fn check(v: &Value) -> bool {
        match v {
            Value::Object(o) => o.iter().all(|(k, v)| plain(k) && check(v)),
            Value::Array(a) => a.iter().all(check),
            Value::Number(n) => !n.is_f64(),
            Value::String(s) => plain(s),
            _ => true,
        }
    }
    if !plain(name) || !plain(description) || !check(schema) {
        return Err("Claude schema instruction contains a float or YAML-ambiguous string".into());
    }
    Ok(())
}

pub fn schema_instruction(name: &str, description: &str, schema: &Value) -> String {
    // serde_json::Map uses sorted keys; go_json also escapes HTML and U+2028/U+2029.
    let mut text = format!("Your entire reply MUST be a single JSON object that validates against this JSON schema:\nSchema name: {name}");
    if !description.is_empty() {
        write!(text, "\nSchema description: {description}").unwrap();
    }
    write!(text, "\n\n{}\n\nOutput raw JSON only: no Markdown formatting, no code fences, no explanations, and no text before or after the JSON object.", crate::go_json::encode(schema)).unwrap();
    text
}

pub struct OutputTool {
    name: String,
    schema: Schema,
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
            schema: Schema::link(schema)?,
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
        self.schema
            .validate(&self.schema.root, &value, "")
            .map_err(|e| {
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

fn scalar_union(s: &Value) -> Option<(&'static str, &Value)> {
    ["anyOf", "oneOf"]
        .into_iter()
        .find_map(|key| s.get(key).map(|branches| (key, branches)))
}

// Reject unknown keywords at every node before copying anything into the tool definition.
fn parameters(
    s: &Value,
    depth: usize,
    left: &mut usize,
    strict: &mut bool,
) -> Result<Value, String> {
    if depth > MAX_DEPTH || *left == 0 {
        return Err("tool schema exceeds depth or node limit".into());
    }
    *left -= 1;
    if let Some(reference) = s.get("$ref") {
        if s.as_object().unwrap().len() != 1 {
            return Err("tool schema $ref siblings are unsupported".into());
        }
        let pointer = reference.as_str().ok_or("$ref must be a string")?;
        if !pointer.starts_with('#') {
            return Err("non-local $ref: only same-document pointers are supported".into());
        }
        let token = pointer
            .strip_prefix("#/$defs/")
            .ok_or("$ref must point to a root $defs entry")?;
        // Go's validator URI-decodes fragments; its OpenAI walker does not.
        if token.len() > 256
            || token.contains(['/', '%'])
            || token.chars().any(char::is_control)
            || token.split('~').skip(1).any(|s| !s.starts_with(['0', '1']))
        {
            return Err("invalid or unsupported $ref pointer token".into());
        }
        return Ok(s.clone());
    }
    if let Some((keyword, branches)) = scalar_union(s) {
        if s.as_object().unwrap().len() != 1 {
            return Err(format!("tool schema {keyword} siblings are unsupported"));
        }
        let branches = branches
            .as_array()
            .filter(|a| (2..=MAX_ALTERNATIVES).contains(&a.len()))
            .ok_or_else(|| format!("tool schema {keyword} requires 2-4 scalar alternatives"))?;
        let mut normalized = Vec::with_capacity(branches.len());
        for branch in branches {
            if !matches!(
                branch["type"].as_str(),
                Some("boolean" | "number" | "null" | "string")
            ) || !branch
                .as_object()
                .unwrap()
                .keys()
                .all(|k| matches!(k.as_str(), "type" | "enum" | "title" | "description"))
            {
                return Err(format!("unsupported tool schema {keyword} alternative"));
            }
            normalized.push(parameters(branch, depth + 1, left, strict)?);
        }
        *strict &= keyword != "oneOf";
        return Ok(json!({keyword: normalized}));
    }
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
        if k == "$defs" && depth == 0 {
            let defs = v.as_object().ok_or("tool schema $defs must be an object")?;
            let mut normalized = serde_json::Map::new();
            for (key, child) in defs {
                check_name(key)?;
                normalized.insert(key.clone(), parameters(child, depth + 1, left, strict)?);
            }
            wire[k] = normalized.into();
            continue;
        }
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
            check_name(key)?;
            let mut prop = parameters(child, depth + 1, left, strict)?;
            // Go makes optional composition properties required without rewriting their branches.
            if !required.contains(key.as_str()) && scalar_union(child).is_none() {
                if child["type"] == "null" {
                    return Err("optional null-only tool properties are unsupported".into());
                }
                if matches!(child["type"].as_str(), Some("array" | "object"))
                    || child.get("enum").is_some()
                    || child.get("$ref").is_some()
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

fn check_name(key: &str) -> Result<(), String> {
    if key.len() > 128 || matches!(key, "$ref" | "x-docker-agent-property-order") {
        return Err("unsupported tool schema property or definition name".into());
    }
    Ok(())
}

fn pointer_token(key: &str) -> String {
    key.replace('~', "~0").replace('/', "~1")
}

struct Schema {
    root: Value,
    // Key by the escaped pointer so tool calls never parse URIs or copy a referenced schema.
    defs: BTreeMap<String, Value>,
}

impl Schema {
    fn link(schema: &Value) -> Result<Self, String> {
        let mut root = schema.clone();
        let mut defs = BTreeMap::new();
        if let Some(Value::Object(entries)) = root.as_object_mut().unwrap().remove("$defs") {
            for (key, value) in entries {
                defs.insert(format!("#/$defs/{}", pointer_token(&key)), value);
            }
        }
        let doc = Self { root, defs };
        let mut left = MAX_EXPANDED_NODES;
        let mut stack = Vec::new();
        for node in std::iter::once(&doc.root).chain(doc.defs.values()) {
            doc.check_graph(node, 0, &mut stack, &mut left)?;
        }
        Ok(doc)
    }

    // Charge each edge traversal, including repeated uses, without building an expanded tree.
    fn check_graph<'a>(
        &'a self,
        s: &'a Value,
        depth: usize,
        stack: &mut Vec<&'a str>,
        left: &mut usize,
    ) -> Result<(), String> {
        if depth > MAX_DEPTH {
            return Err("expanded tool schema exceeds depth limit".into());
        }
        if *left == 0 {
            return Err("tool schema exceeds $ref expansion budget".into());
        }
        *left -= 1;
        if let Some(pointer) = s["$ref"].as_str() {
            if stack.contains(&pointer) {
                return Err(format!("cyclic $ref {pointer:?}"));
            }
            let target = self
                .defs
                .get(pointer)
                .ok_or_else(|| format!("unresolved $ref {pointer:?}"))?;
            stack.push(pointer);
            self.check_graph(target, depth + 1, stack, left)?;
            stack.pop();
        } else {
            let branches = scalar_union(s).and_then(|(_, b)| b.as_array());
            for child in s["properties"]
                .as_object()
                .into_iter()
                .flat_map(|p| p.values())
                .chain(s.get("items"))
                .chain(branches.into_iter().flatten())
            {
                self.check_graph(child, depth + 1, stack, left)?;
            }
        }
        Ok(())
    }

    fn validate(&self, s: &Value, v: &Instance, path: &str) -> Result<(), String> {
        if let Some(pointer) = s["$ref"].as_str() {
            let path = &pointer[1..];
            return self
                .validate(&self.defs[pointer], v, path)
                .map_err(|e| format!("validating {path}: {e}"));
        }
        if let Some((keyword, branches)) = scalar_union(s) {
            return validate_union(keyword, branches.as_array().unwrap(), v, path);
        }
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
                        let path = format!("{path}/properties/{}", pointer_token(key));
                        self.validate(child, value, &path)
                            .map_err(|e| format!("validating {path}: {e}"))?;
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
                let path = format!("{path}/items");
                for item in a {
                    self.validate(&s["items"], item, &path)
                        .map_err(|e| format!("validating {path}: {e}"))?;
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
}

fn validate_union(
    keyword: &str,
    branches: &[Value],
    v: &Instance,
    path: &str,
) -> Result<(), String> {
    let got = match v {
        Instance::String(_) => "string",
        Instance::Number(n) if n.as_f64().unwrap().fract() == 0.0 => "integer",
        Instance::Number(_) => "number",
        Instance::Null => "null",
        Instance::Bool(_) => "boolean",
        Instance::Sequence(_) => "array",
        Instance::Mapping(_) => "object",
        Instance::Tagged(_) => unreachable!("JSON instance"),
    };
    let matches_type = |b: &Value| b["type"] == got || (got == "integer" && b["type"] == "number");
    let mut matches = branches.iter().filter(|b| {
        matches_type(b)
            && b["enum"]
                .as_array()
                .is_none_or(|a| a.iter().any(|x| x.as_str() == v.as_str()))
    });
    if matches.next().is_some() {
        if keyword == "oneOf" && matches.next().is_some() {
            return Err(
                "oneOf: validated against both <anonymous schema> and <anonymous schema>".into(),
            );
        }
        return Ok(());
    }
    let names = vec!["<anonymous schema>"; branches.len()].join(" ");
    let mut error = format!("{keyword}: did not validate against any of [{names}]");
    if keyword == "oneOf" {
        return Err(error);
    }

    // Only anyOf includes each branch's failure in jsonschema-go's diagnostic.
    error.push(':');
    let mut value = String::new();
    if v.is_null() {
        value.push_str("<invalid reflect.Value>");
    } else {
        write_go_value(&mut value, v);
    }
    for (i, b) in branches.iter().enumerate() {
        write!(error, "\nvalidating {path}/anyOf/{i}: ").unwrap();
        if matches_type(b) {
            let options: Vec<_> = b["enum"]
                .as_array()
                .unwrap()
                .iter()
                .map(|x| x.as_str().unwrap())
                .collect();
            write!(
                error,
                "enum: {value} does not equal any of: [{}]",
                options.join(" ")
            )
            .unwrap();
        } else {
            write!(error, "type: {value} has type {got:?}, want {}", b["type"]).unwrap();
        }
    }
    Err(error)
}

// Go fmt's %v for a decoded JSON value. Map keys are sorted, strings unquoted.
fn write_go_value(out: &mut String, v: &Instance) {
    match v {
        Instance::Null => out.push_str("<nil>"),
        Instance::String(s) => out.push_str(s),
        Instance::Bool(b) => write!(out, "{b}").unwrap(),
        Instance::Number(n) => out.push_str(&go_float(n.as_f64().unwrap())),
        Instance::Sequence(a) => {
            out.push('[');
            for (i, v) in a.iter().enumerate() {
                if i > 0 {
                    out.push(' ');
                }
                write_go_value(out, v);
            }
            out.push(']');
        }
        Instance::Mapping(m) => {
            out.push_str("map[");
            let mut fields: Vec<_> = m
                .iter()
                .map(|(k, v)| (k.as_str().expect("JSON key"), v))
                .collect();
            fields.sort_unstable_by_key(|&(k, _)| k);
            for (i, (key, v)) in fields.into_iter().enumerate() {
                if i > 0 {
                    out.push(' ');
                }
                write!(out, "{key}:").unwrap();
                write_go_value(out, v);
            }
            out.push(']');
        }
        Instance::Tagged(_) => unreachable!("JSON instance"),
    }
}

fn go_float(f: f64) -> String {
    // Use JSON's shortest round-to-even digits with Go %g's exponent cutoffs and padding.
    let text = serde_json::to_string(&f).unwrap();
    if let Some((mantissa, exp)) = text.split_once('e') {
        return format!("{mantissa}e{:+03}", exp.parse::<i32>().unwrap());
    }
    let decimal = text.strip_suffix(".0").unwrap_or(&text);
    if f == 0.0 || (1e-4..1e6).contains(&f.abs()) {
        return decimal.into();
    }
    let unsigned = decimal.trim_start_matches('-');
    let point = unsigned.find('.').unwrap_or(unsigned.len());
    let digits = unsigned.replace('.', "");
    let significant = digits.trim_start_matches('0');
    let exp = point as i32 - (digits.len() - significant.len()) as i32 - 1;
    let (head, tail) = significant.trim_end_matches('0').split_at(1);
    let sign = if f < 0.0 { "-" } else { "" };
    let dot = if tail.is_empty() { "" } else { "." };
    format!("{sign}{head}{dot}{tail}e{exp:+03}")
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
    fn scalar_any_of_preserves_branch_order_and_optional_property_on_wire() {
        let choice = json!({"anyOf":[{"type":"string","enum":["skip"]},{"type":"number"},{"type":"null"},{"type":"string","title":"Label"}]});
        let mut schema = object(json!({"type":"array","items":{"$ref":"#/$defs/Value"}}));
        schema["properties"]["flag"] =
            json!({"anyOf":[{"type":"boolean"},{"type":"string","enum":["auto"]}]});
        schema["$defs"] = json!({"Value": choice});
        let t = tool(&schema).unwrap();
        assert!(t.strict);
        let mut wire = schema.clone();
        wire["required"] = json!(["flag", "x"]);
        assert_eq!(t.definition["function"]["parameters"], wire);
        assert_eq!(
            t.call(r#"{ "x": ["skip", "label", null, 3, -2.50e+1] }"#, true)
                .unwrap(),
            r#"{"x":["skip","label",null,3,-2.50e+1]}"#
        );
        assert!(t.call(r#"{"x":[],"flag":true}"#, true).is_ok());
        assert!(t
            .call(r#"{"x":[],"flag":null}"#, true)
            .unwrap_err()
            .contains("type: <invalid reflect.Value> has type \"null\""));
        assert_eq!(t.call(r#"{"x":[],"flag":"other"}"#, true).unwrap_err(), concat!(
            "Structured output rejected: arguments do not match the \"report\" schema: validating root: ",
            "validating /properties/flag: anyOf: did not validate against any of [<anonymous schema> <anonymous schema>]:\n",
            "validating /properties/flag/anyOf/0: type: other has type \"string\", want \"boolean\"\n",
            "validating /properties/flag/anyOf/1: enum: other does not equal any of: [auto]. Fix the arguments and call __structured_output__ again."
        ));
    }

    #[test]
    fn scalar_one_of_requires_exactly_one_match() {
        let choice = json!({"oneOf":[
            {"type":"string", "enum":["red","both"]}, {"type":"number"},
            {"type":"string", "title":"Cool", "enum":["blue","both"]}, {"type":"null"}
        ]});
        let mut schema = object(choice.clone());
        schema["required"] = json!([]);
        let t = tool(&schema).unwrap();
        assert!(!t.strict);
        schema["required"] = json!(["x"]);
        assert_eq!(t.definition["function"]["parameters"], schema);
        for valid in ["{}", r#"{"x":"blue"}"#, r#"{"x":1}"#, r#"{"x":null}"#] {
            assert_eq!(t.call(valid, true).unwrap(), valid);
        }
        let prefix = r#"arguments do not match the "report" schema: validating root: validating /properties/x: "#;
        for (input, diagnostic) in [
            (r#"{"x":"none"}"#, "oneOf: did not validate against any of [<anonymous schema> <anonymous schema> <anonymous schema> <anonymous schema>]"),
            (r#"{"x":"both"}"#, "oneOf: validated against both <anonymous schema> and <anonymous schema>"),
        ] {
            assert_eq!(t.validate(input).unwrap_err(), format!("{prefix}{diagnostic}"));
        }
        let mut unused = object(json!({"type":"boolean"}));
        unused["$defs"] = json!({"unused":choice});
        assert!(!tool(&unused).unwrap().strict);
    }

    #[test]
    fn any_of_type_diagnostics_use_go_values() {
        let t = tool(&object(
            json!({"anyOf":[{"type":"boolean"},{"type":"string"}]}),
        ))
        .unwrap();
        for (input, got, text) in [
            ("null", "null", "<invalid reflect.Value>"),
            ("-0", "integer", "-0"),
            ("1.0", "integer", "1"),
            ("9007199254740993", "integer", "9.007199254740992e+15"),
            ("-9200000.125", "number", "-9.200000125e+06"),
            ("0.0001", "number", "0.0001"),
            ("-0.0000123", "number", "-1.23e-05"),
            ("5e-324", "number", "5e-324"),
            (
                r#"{"z":true,"a":[null,"a b",1e56]}"#,
                "object",
                "map[a:[<nil> a b 1e+56] z:true]",
            ),
        ] {
            let err = t.call(&format!(r#"{{"x":{input}}}"#), true).unwrap_err();
            assert!(
                err.contains(&format!("type: {text} has type {got:?}, want \"boolean\"")),
                "{err}"
            );
        }
    }

    #[test]
    fn rejects_unsupported_scalar_unions_even_in_unused_definitions() {
        let string = json!({"type":"string"});
        for key in ["anyOf", "oneOf"] {
            for bad in [
                json!({key:null}),
                json!({key:[]}),
                json!({key:[string]}),
                json!({key:vec![string.clone(); MAX_ALTERNATIVES + 1]}),
                json!({key:[string,string],"description":"sibling"}),
                json!({key:[string, true]}),
                json!({key:[string, {}]}),
                json!({key:[string, {"type":"integer"}]}),
                json!({key:[string, {"type":"string","maxLength":1}]}),
                json!({key:[string, {"type":"boolean","enum":[true]}]}),
                json!({key:[string, {"type":"array","items":string}]}),
                json!({key:[string, {"type":"object","properties":{}}]}),
                json!({key:[string, {"$ref":"#/$defs/s"}]}),
                json!({key:[string, {"anyOf":[string,string]}]}),
                json!({key:[string, {"oneOf":[string,string]}]}),
                json!({key:[string, {"$ref":"https://invalid.test/schema"}]}),
                json!({"anyOf":[string,string],"oneOf":[string,string]}),
                json!({key:[string, {"type":"string","not":string}]}),
            ] {
                let mut schema = object(string.clone());
                schema["$defs"] = json!({"s": string, "unused": {"type":"array","items":bad}});
                assert!(tool(&schema).is_err(), "{bad}");
            }
        }
    }

    #[test]
    fn scalar_union_branches_count_toward_schema_budgets() {
        for key in ["anyOf", "oneOf"] {
            let union = json!({key:vec![json!({"type":"string"}); MAX_ALTERNATIVES]});
            let mut schema = object(union.clone());
            for i in 0..MAX_SCHEMA_NODES / (MAX_ALTERNATIVES + 1) {
                schema["properties"][i.to_string()] = union.clone();
            }
            assert!(tool(&schema).err().unwrap().contains("node limit"));

            let mut schema = object(json!({"$ref":"#/$defs/0"}));
            schema["$defs"] = json!({"0":{"type":"object","properties":{}}, "leaf":union});
            for i in 0..MAX_DEPTH {
                schema["properties"][i.to_string()] = json!({"$ref":"#/$defs/0"});
            }
            for i in 0..5 {
                schema["$defs"]["0"]["properties"][i.to_string()] = json!({"$ref":"#/$defs/leaf"});
            }
            assert!(tool(&schema).err().unwrap().contains("expansion budget"));
        }
    }

    #[test]
    fn refs_are_not_inlined_or_made_nullable_during_validation() {
        let leaf = json!({"$ref":"#/$defs/~01~1~0"});
        let alias = json!({"$ref":"#/$defs/Alias"});
        let mut schema = object(alias.clone());
        schema["properties"]["y"] = alias.clone();
        schema["$defs"] = json!({"Alias":leaf, "~1/~": {"type":"object", "properties":{"tag":{"type":"string", "enum":["ok"]}}}});
        let t = tool(&schema).unwrap();
        let wire = &t.definition["function"]["parameters"];
        assert!(t.strict);
        assert_eq!(wire["required"], json!(["x", "y"]));
        assert_eq!(wire["properties"]["x"], alias);
        assert_eq!(
            wire["properties"]["y"],
            json!({"anyOf":[alias,{"type":"null"}]})
        );
        assert_eq!(wire["$defs"]["Alias"], leaf);
        assert_eq!(
            wire["$defs"]["~1/~"],
            json!({"type":"object", "properties":{"tag":{"anyOf":[{"type":"string", "enum":["ok"]},{"type":"null"}]}}, "required":["tag"], "additionalProperties":false})
        );
        for valid in [r#"{"x":{}}"#, r#"{"x":{"extra":true},"y":{"tag":"ok"}}"#] {
            assert_eq!(t.call(valid, true).unwrap(), valid);
        }
        for invalid in [r#"{}"#, r#"{"x":{},"y":null}"#, r#"{"x":{"tag":"wrong"}}"#] {
            assert!(t.call(invalid, true).is_err(), "{invalid}");
        }
        schema["$defs"]["unused"] =
            json!({"type":"object", "properties":{}, "additionalProperties":true});
        assert!(!tool(&schema).unwrap().strict);
    }

    #[test]
    fn rejects_invalid_refs_including_in_unused_definitions() {
        for (bad, message) in [
            (json!({"$ref":null}), "must be a string"),
            (
                json!({"$ref":"https://schema.invalid/#/$defs/s"}),
                "non-local",
            ),
            (json!({"$ref":"file:///tmp/schema"}), "non-local"),
            (json!({"$ref":"other.json#/$defs/s"}), "non-local"),
            (json!({"$ref":"#anchor"}), "root $defs entry"),
            (json!({"$ref":"#/properties/x"}), "root $defs entry"),
            (json!({"$ref":"#/$defs/missing"}), "unresolved"),
            (json!({"$ref":"#/$defs/s/type"}), "pointer token"),
            (json!({"$ref":"#/$defs/a%2Fb"}), "pointer token"),
            (json!({"$ref":"#/$defs/~2"}), "pointer token"),
            (json!({"$ref":"#/$defs/s~"}), "pointer token"),
            (
                json!({"$ref":"#/$defs/s", "description":"a sibling"}),
                "siblings",
            ),
            (json!({"$ref":"#/$defs/s", "minimum":2}), "siblings"),
            (json!({"type":"string", "pattern":"^a"}), "pattern"),
            (json!({"type":"string", "$defs":{}}), "$defs"),
        ] {
            let mut schema = object(json!({"type":"string"}));
            schema["$defs"] = json!({"s":{"type":"string"}, "unused":{"type":"array","items":bad}});
            assert!(tool(&schema).err().unwrap().contains(message), "{bad}");
        }
        for bad_defs in [
            json!(null),
            json!([]),
            json!({"s":true}),
            json!({"$ref":{"type":"string"}}),
        ] {
            let mut schema = object(json!({"type":"string"}));
            schema["$defs"] = bad_defs;
            assert!(tool(&schema).is_err());
        }
    }

    #[test]
    fn bounds_ref_graph_before_tool_calls_or_serialization() {
        let mut schema = object(json!({"$ref":"#/$defs/0"}));
        schema["$defs"] = json!({"0":{"$ref":"#/$defs/0"}});
        assert!(tool(&schema).err().unwrap().contains("cyclic"));
        schema["$defs"]["0"] = json!({"type":"string"});
        schema["$defs"]["a"] = object(json!({"$ref":"#/$defs/b"}));
        schema["$defs"]["b"] = json!({"type":"array", "items":{"$ref":"#/$defs/a"}});
        assert!(tool(&schema).err().unwrap().contains("cyclic"));

        schema["$defs"] = json!({});
        for i in 0..MAX_DEPTH {
            schema["$defs"][i.to_string()] = json!({"$ref":format!("#/$defs/{}", i+1)});
        }
        schema["$defs"][MAX_DEPTH.to_string()] = json!({"type":"string"});
        assert!(tool(&schema).err().unwrap().contains("depth"));

        // Just four levels of fan-out, well under the source node and depth limits.
        schema["$defs"] = json!({"4":{"type":"string"}});
        for i in 0..4 {
            let child = json!({"$ref":format!("#/$defs/{}", i+1)});
            schema["$defs"][i.to_string()] = json!({"type":"object", "properties":{"a":child,"b":child,"c":child,"d":child,"e":child}});
        }
        assert!(tool(&schema).err().unwrap().contains("expansion budget"));
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
