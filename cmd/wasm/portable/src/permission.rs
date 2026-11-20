//! Portable rule precedence is deny, mandatory ask, allow, then annotation-based safety.
//! Patterns are ASCII tool globs (`*`, `?`) with optional `:key=glob` string conditions.
//! Unsupported inputs and exhausted match budgets deny the call, never fall back to approval.

use serde::de::{Error, MapAccess, SeqAccess, Visitor};
use serde::{Deserialize, Deserializer};
use serde_json::Value;
use std::collections::BTreeMap;
use std::fmt;

const MAX_RULES_PER_LIST: usize = 32;
const MAX_PATTERN_BYTES: usize = 256;
const MAX_ARGUMENT_BYTES: usize = 64 * 1024;
const MATCH_BUDGET: usize = 1 << 20;

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum Safety {
    #[default]
    Legacy,
    Strict,
    Balanced,
    Restricted,
    Autonomous,
}

impl<'de> Deserialize<'de> for Safety {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        string(d, |s| match s {
            "" => Ok(Self::Legacy),
            "strict" => Ok(Self::Strict),
            "balanced" => Ok(Self::Balanced),
            "restricted" => Ok(Self::Restricted),
            "autonomous" => Ok(Self::Autonomous),
            _ => Err("invalid safety mode (expected strict, balanced, restricted or autonomous)"),
        })
    }
}

#[derive(Clone, Debug, Default)]
pub struct Permissions {
    allow: Vec<Rule>,
    ask: Vec<Rule>,
    deny: Vec<Rule>,
}

#[derive(Default)]
pub struct Policy {
    pub rules: Permissions,
    pub safety: Safety,
}

impl Policy {
    pub fn with_auto_approve(mut self, auto: bool) -> Self {
        // Never promote an explicitly configured safety mode to autonomous.
        if auto && self.safety == Safety::Legacy {
            self.safety = Safety::Autonomous;
        }
        self
    }

    // Go advertises a denied tool and refuses it at dispatch. Only restricted mode, which Go
    // lacks, hides what can never run; argument-dependent rules are left to dispatch.
    pub fn visible(&self, name: &str, safe: bool) -> Result<bool, &'static str> {
        if self.safety != Safety::Restricted {
            return Ok(true);
        }
        let mut budget = MATCH_BUDGET;
        for rule in self.rules.deny.iter().chain(&self.rules.ask) {
            if rule.conditions.is_empty() && glob(&rule.name, name, &mut budget)? {
                return Ok(false);
            }
        }
        if !safe {
            for rule in &self.rules.allow {
                if glob(&rule.name, name, &mut budget)? {
                    return Ok(true);
                }
            }
            return Ok(false);
        }
        Ok(true)
    }

    // Conservative scheduling hint, never an approval decision.
    pub fn may_ask(&self, safe: bool) -> bool {
        self.safety != Safety::Restricted
            && (!self.rules.ask.is_empty()
                || self.safety == Safety::Strict
                || (!safe && self.safety != Safety::Autonomous))
    }

    pub fn grantable(&self) -> bool {
        self.safety == Safety::Legacy
            && self.rules.allow.is_empty()
            && self.rules.deny.is_empty()
            && self.rules.ask.is_empty()
    }
}

#[derive(Debug, Eq, PartialEq)]
pub enum Verdict {
    Allow,
    Ask,
    Deny(String),
}

/// Check at tool registration as well as dispatch. Go classifies command tools by their
/// actual command, not by their annotations; this profile has no shell classifier.
pub fn validate_tool(name: &str) -> Result<(), &'static str> {
    if name.is_empty() || name.len() > 200 || !name.bytes().all(tool_byte) {
        return Err("Unsupported portable tool name.");
    }
    if matches!(name, "shell" | "run_background_job") {
        return Err("Command tools are not supported by the portable policy.");
    }
    Ok(())
}

impl Permissions {
    pub fn verdict(&self, safety: Safety, name: &str, arguments: &str, safe: bool) -> Verdict {
        if let Err(reason) = validate_tool(name) {
            return Verdict::Deny(reason.into());
        }
        self.decide(safety, name, arguments, safe)
            .unwrap_or_else(|reason| Verdict::Deny(format!("Tool '{name}' is denied: {reason}.")))
    }

    fn decide(
        &self,
        mode: Safety,
        name: &str,
        arguments: &str,
        safe: bool,
    ) -> Result<Verdict, &'static str> {
        let mut call = Match {
            name,
            arguments,
            parsed: None,
            budget: MATCH_BUDGET,
        };
        if call.any(&self.deny)? {
            return Ok(Verdict::Deny(format!(
                "Tool '{name}' is denied by permissions configuration."
            )));
        }
        // Portable asks are mandatory, including under autoApprove and overlapping allows.
        if call.any(&self.ask)? {
            return Ok(if mode == Safety::Restricted {
                Verdict::Deny(format!(
                    "Tool '{name}' requires confirmation in restricted mode."
                ))
            } else {
                Verdict::Ask
            });
        }
        if call.any(&self.allow)? {
            return Ok(Verdict::Allow);
        }
        Ok(match mode {
            Safety::Autonomous => Verdict::Allow,
            Safety::Strict => Verdict::Ask,
            _ if safe => Verdict::Allow,
            Safety::Restricted => Verdict::Deny(format!("Tool '{name}' is denied by the restricted safety mode: the call is not classified safe, and the mode's fallback denies instead of asking for confirmation.")),
            _ => Verdict::Ask,
        })
    }
}

struct Match<'a> {
    name: &'a str,
    arguments: &'a str,
    parsed: Option<Arguments>,
    budget: usize,
}

impl Match<'_> {
    fn any(&mut self, rules: &[Rule]) -> Result<bool, &'static str> {
        'rules: for rule in rules {
            if !glob(&rule.name, self.name, &mut self.budget)? {
                continue;
            }
            if rule.conditions.is_empty() {
                return Ok(true);
            }
            let args = match &mut self.parsed {
                Some(parsed) => parsed,
                None => self.parsed.insert(Arguments::parse(self.arguments)?),
            };
            for (key, pattern) in &rule.conditions {
                let Some(value) = args.0.get(key) else {
                    continue 'rules;
                };
                if !glob(
                    pattern,
                    value
                        .as_str()
                        .ok_or("argument rules require string values")?,
                    &mut self.budget,
                )? {
                    continue 'rules;
                }
            }
            return Ok(true);
        }
        Ok(false)
    }
}

fn tool_byte(b: u8) -> bool {
    b.is_ascii_alphanumeric() || b"_-.".contains(&b)
}

#[derive(Clone, Debug)]
struct Rule {
    name: String,
    conditions: Vec<(String, String)>,
}

impl Rule {
    fn parse(pattern: &str) -> Result<Self, &'static str> {
        if pattern.len() > MAX_PATTERN_BYTES
            || !pattern.is_ascii()
            || pattern.contains(['[', ']', '\\'])
        {
            return Err("portable permission rules require at most 256 ASCII bytes and do not support character classes or escapes");
        }
        // Colons in argument values (e.g. URLs) are not separators.
        let mut boundaries = pattern
            .match_indices(':')
            .filter_map(|(i, _)| {
                let tail = &pattern[i + 1..];
                let n = tail
                    .bytes()
                    .take_while(|b| b.is_ascii_alphanumeric() || *b == b'_')
                    .count();
                (n > 0 && tail.as_bytes().get(n) == Some(&b'=')).then(|| (i, &tail[..n]))
            })
            .peekable();
        let name = &pattern[..boundaries.peek().map_or(pattern.len(), |(i, _)| *i)];
        if name.is_empty()
            || !name
                .bytes()
                .all(|b| tool_byte(b) || matches!(b, b'*' | b'?'))
        {
            return Err("invalid portable tool name pattern");
        }
        let mut conditions = Vec::new();
        while let Some((i, key)) = boundaries.next() {
            if key.bytes().any(|b| b.is_ascii_uppercase())
                || conditions.iter().any(|(k, _)| k == key)
            {
                return Err("argument conditions require unique lowercase ASCII keys");
            }
            let end = boundaries.peek().map_or(pattern.len(), |(i, _)| *i);
            conditions.push((key.to_owned(), pattern[i + key.len() + 2..end].to_owned()));
        }
        Ok(Self {
            name: name.into(),
            conditions,
        })
    }
}

// Anchored ASCII * and ?. Retry only the most recent star; charge retries across all rules.
fn glob(pattern: &str, value: &str, budget: &mut usize) -> Result<bool, &'static str> {
    const LIMIT: &str = "permission match budget exhausted";
    *budget = budget.checked_sub(value.len()).ok_or(LIMIT)?;
    if !value.is_ascii() {
        return Err("permission globs require ASCII values");
    }
    let (p, v) = (pattern.as_bytes(), value.as_bytes());
    let (mut i, mut j) = (0, 0);
    let mut star = None;
    while j < v.len() {
        *budget = budget.checked_sub(1).ok_or(LIMIT)?;
        match p.get(i) {
            Some(b'*') => {
                i += 1;
                star = Some((i, j));
            }
            Some(c) if *c == b'?' || c.eq_ignore_ascii_case(&v[j]) => {
                i += 1;
                j += 1;
            }
            _ => match &mut star {
                Some((after, end)) => {
                    *end += 1;
                    i = *after;
                    j = *end;
                }
                None => return Ok(false),
            },
        }
    }
    Ok(p[i..].iter().all(|&c| c == b'*'))
}

// The ordinary map decoder loses duplicate fields. Typed Go-compatible handlers also fold
// field names, so don't let a case variant shadow the value that was checked.
struct Arguments(BTreeMap<String, Value>);

impl Arguments {
    fn parse(raw: &str) -> Result<Self, &'static str> {
        if raw.len() > MAX_ARGUMENT_BYTES {
            return Err("tool arguments exceed the permission size limit");
        }
        serde_json::from_str(raw).map_err(|_| {
            "argument rules require an unambiguous JSON object with lowercase ASCII keys"
        })
    }
}

impl<'de> Deserialize<'de> for Arguments {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct Object;
        impl<'de> Visitor<'de> for Object {
            type Value = Arguments;
            fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str("tool arguments object")
            }
            fn visit_map<M: MapAccess<'de>>(self, mut map: M) -> Result<Arguments, M::Error> {
                let mut args = BTreeMap::new();
                while let Some(key) = map.next_key::<String>()? {
                    if !key.is_ascii() || key.bytes().any(|b| b.is_ascii_uppercase()) {
                        return Err(Error::custom("unsupported argument key"));
                    }
                    if args.insert(key, map.next_value()?).is_some() {
                        return Err(Error::custom("duplicate argument key"));
                    }
                }
                Ok(Arguments(args))
            }
        }
        d.deserialize_map(Object)
    }
}

// Use deserialize_any for policy fields: the YAML backend otherwise accepts null as an
// empty collection, coerces scalars to strings, or strips tags from typed values.
fn string<'de, D, T>(d: D, parse: fn(&str) -> Result<T, &'static str>) -> Result<T, D::Error>
where
    D: Deserializer<'de>,
{
    struct Text<T>(fn(&str) -> Result<T, &'static str>);
    impl<T> Visitor<'_> for Text<T> {
        type Value = T;
        fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
            f.write_str("a policy string")
        }
        fn visit_str<E: Error>(self, s: &str) -> Result<T, E> {
            self.0(s).map_err(Error::custom)
        }
    }
    d.deserialize_any(Text(parse))
}

impl<'de> Deserialize<'de> for Rule {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        string(d, Self::parse)
    }
}

enum Field {
    Allow,
    Ask,
    Deny,
}

impl<'de> Deserialize<'de> for Field {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        string(d, |s| match s {
            "allow" => Ok(Self::Allow),
            "ask" => Ok(Self::Ask),
            "deny" => Ok(Self::Deny),
            _ => Err("unknown permissions field"),
        })
    }
}

struct List(Vec<Rule>);

impl<'de> Deserialize<'de> for List {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct Rules;
        impl<'de> Visitor<'de> for Rules {
            type Value = List;
            fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str("a list of permission patterns")
            }
            fn visit_seq<S: SeqAccess<'de>>(self, mut seq: S) -> Result<List, S::Error> {
                let mut rules = Vec::new();
                while let Some(rule) = seq.next_element()? {
                    if rules.len() == MAX_RULES_PER_LIST {
                        return Err(Error::custom("too many permission rules"));
                    }
                    rules.push(rule);
                }
                Ok(List(rules))
            }
        }
        d.deserialize_any(Rules)
    }
}

impl<'de> Deserialize<'de> for Permissions {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct Policy;
        impl<'de> Visitor<'de> for Policy {
            type Value = Permissions;
            fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str("permissions mapping with allow, ask or deny lists")
            }
            fn visit_map<M: MapAccess<'de>>(self, mut map: M) -> Result<Permissions, M::Error> {
                let (mut allow, mut ask, mut deny) = (None, None, None);
                while let Some(field) = map.next_key()? {
                    let slot = match field {
                        Field::Allow => &mut allow,
                        Field::Ask => &mut ask,
                        Field::Deny => &mut deny,
                    };
                    if slot.is_some() {
                        return Err(Error::custom("duplicate permissions field"));
                    }
                    *slot = Some(map.next_value::<List>()?.0);
                }
                Ok(Permissions {
                    allow: allow.unwrap_or_default(),
                    ask: ask.unwrap_or_default(),
                    deny: deny.unwrap_or_default(),
                })
            }
        }
        d.deserialize_any(Policy)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use Verdict::{Allow, Ask, Deny};

    // Use the same YAML Value -> typed fields path as config::load.
    fn load<T: serde::de::DeserializeOwned>(yaml: &str) -> Result<T, serde_yaml_ng::Error> {
        serde_yaml_ng::from_value(serde_yaml_ng::from_str(yaml)?)
    }

    #[test]
    fn only_restricted_mode_hides_what_dispatch_refuses() {
        let rules: Permissions = load("{deny: [blocked], ask: [gated], allow: [write]}").unwrap();
        for (safety, shown) in [
            (
                Safety::Legacy,
                ["blocked", "gated", "write", "other"].as_slice(),
            ),
            (Safety::Strict, &["blocked", "gated", "write", "other"]),
            (Safety::Restricted, &["write"]),
        ] {
            let policy = Policy {
                rules: rules.clone(),
                safety,
            };
            for name in ["blocked", "gated", "write", "other"] {
                assert_eq!(
                    policy.visible(name, false).unwrap(),
                    shown.contains(&name),
                    "{safety:?} {name}"
                );
            }
            // Offering a denied tool never lets it run.
            assert!(matches!(
                rules.verdict(safety, "blocked", "{}", true),
                Deny(_)
            ));
        }
    }

    #[test]
    fn config_fields_validate_even_if_a_rule_would_never_match() {
        #[derive(Default, Deserialize)]
        #[serde(default, deny_unknown_fields)]
        struct Config {
            permissions: Permissions,
            safety: Safety,
        }
        let defaults: Config = load("{}").unwrap();
        assert_eq!(defaults.safety, Safety::Legacy);
        assert!(defaults.permissions.deny.is_empty());
        for s in ["''", "strict", "restricted", "autonomous", "balanced"] {
            assert!(load::<Config>(&format!(
                "safety: {s}\npermissions: {{allow: [], deny: [], ask: []}}"
            ))
            .is_ok());
        }
        for s in [
            "legacy",
            "safe-auto",
            "safer",
            "unsafe",
            "yolo",
            "Strict",
            "false",
            "null",
            "[strict]",
            "{strict: null}",
            "!other strict",
        ] {
            assert!(load::<Config>(&format!("safety: {s}")).is_err(), "{s}");
        }
        for v in [
            "null",
            "[]",
            "strict",
            "{deny: think}",
            "{DENY: []}",
            "{allow: [], extra: []}",
            "{deny: null}",
            "{!tag deny: []}",
            "{deny: [false]}",
            "{deny: [42]}",
            "{allow: [~]}",
            "{ask: [{}]}",
            "{deny: [!tag think]}",
            "{deny: !tag [think]}",
            "!tag {deny: [think]}",
            "{deny: [], deny: []}",
        ] {
            assert!(load::<Config>(&format!("permissions: {v}")).is_err(), "{v}");
        }
        for rule in [
            "",
            " ",
            ":k=v",
            "tool:a-b=c",
            "mcp:git:*",
            "think[abc]",
            "tool:k=\\?",
            "tool:k=κ",
            "tool:a=1:a=2",
            "tool:A=1",
        ] {
            assert!(
                serde_json::from_value::<Permissions>(json!({"allow": ["*"], "deny": [rule]}))
                    .is_err(),
                "{rule}"
            );
        }
        assert!(serde_json::from_value::<Permissions>(
            json!({"allow": ["a".repeat(MAX_PATTERN_BYTES)]})
        )
        .is_ok());
        assert!(serde_json::from_value::<Permissions>(
            json!({"ask": ["a".repeat(MAX_PATTERN_BYTES + 1)]})
        )
        .is_err());
        assert!(serde_json::from_value::<Permissions>(
            json!({"deny": vec!["t"; MAX_RULES_PER_LIST]})
        )
        .is_ok());
        assert!(serde_json::from_value::<Permissions>(
            json!({"deny": vec!["t"; MAX_RULES_PER_LIST + 1]})
        )
        .is_err());
    }

    #[test]
    fn portable_asks_are_mandatory_and_denies_take_precedence() {
        let empty = Permissions::default();
        let deny: Permissions = load("{deny: ['*'], allow: [tool], ask: [tool]}").unwrap();
        let allow: Permissions = load("{allow: ['t*']}").unwrap();
        let ask: Permissions = load("{allow: [tool], ask: [tool]}").unwrap();
        for (mode, annotated, other) in [
            (Safety::Legacy, Allow, Ask),
            (Safety::Strict, Ask, Ask),
            (Safety::Balanced, Allow, Ask),
            (Safety::Restricted, Allow, Deny("Tool 'tool' is denied by the restricted safety mode: the call is not classified safe, and the mode's fallback denies instead of asking for confirmation.".into())),
            (Safety::Autonomous, Allow, Allow),
        ] {
            for (ro, destructive) in [(true, false), (false, false), (false, true), (true, true)] {
                let verdict = |p: &Permissions| p.verdict(mode, "tool", "{}", ro && !destructive);
                let fallback = if ro && !destructive { &annotated } else { &other };
                assert_eq!(&verdict(&empty), fallback);
                if mode == Safety::Restricted { assert!(matches!(verdict(&ask), Deny(_))); }
                else { assert_eq!(verdict(&ask), Ask); }
                assert_eq!(verdict(&deny), Deny("Tool 'tool' is denied by permissions configuration.".into()));
                assert_eq!(verdict(&allow), Allow);
            }
        }
    }

    #[test]
    fn wire_names_and_string_conditions() {
        let p: Permissions =
            load("{deny: ['git_*:url=https://host:8443/pr*?x:method=P?ST']}").unwrap();
        let blocked = |n, url, method| {
            let raw = json!({"url":url,"method":method}).to_string();
            matches!(p.verdict(Safety::Autonomous, n, &raw, true), Deny(_))
        };
        assert!(blocked("GIT_query", "https://HOST:8443/pr/a\nX", "post"));
        assert!(!blocked("GIT_query", "https://HOST:8443/pr/a\nX#", "post"));
        assert!(!blocked("GIT_query", "https://HOST:8443/pr/a\nX", "get"));
        assert!(!blocked(
            "GITLAB_query",
            "https://HOST:8443/pr/a\nX",
            "post"
        ));
        assert_eq!(
            p.verdict(Safety::Autonomous, "git_query", "{}", false),
            Allow
        );
        let p: Permissions = load("{allow: ['do:arg=']}").unwrap();
        assert_eq!(
            p.verdict(Safety::Strict, "do", r#"{"arg":""}"#, false),
            Allow
        );
        assert_eq!(p.verdict(Safety::Strict, "do", "{}", false), Ask);
    }

    #[test]
    fn ambiguous_or_unsupported_input_cannot_skip_a_deny() {
        let p: Permissions = load("{deny: ['*:mode=special'], allow: ['*']}").unwrap();
        for raw in [
            "",
            "bogus",
            "null",
            "[]",
            "{}{}",
            r#"{"mode":42}"#,
            r#"{"mode":true}"#,
            r#"{"mode":null}"#,
            r#"{"mode":{}}"#,
            r#"{"mode":["special"]}"#,
            r#"{"mode":"sp€cial"}"#,
            r#"{"møde":"special"}"#,
            r#"{"mode":"special","m\u006fde":"fine"}"#,
            r#"{"MODE":"special"}"#,
            r#"{"mode":"fine","Mode":"special"}"#,
        ] {
            assert!(
                matches!(p.verdict(Safety::Autonomous, "x", raw, false), Deny(_)),
                "{raw}"
            );
        }
        // No checked condition, no need to interpret (or normalize) handler arguments.
        let p: Permissions = load("{deny: ['other:mode=x'], allow: [x]}").unwrap();
        assert_eq!(p.verdict(Safety::Strict, "x", "ignored", false), Allow);
    }

    #[test]
    fn match_limits_deny_without_falling_through_to_an_allow() {
        let p: Permissions = load(
            "{deny: ['*:s=*aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaab'], allow: ['*']}",
        )
        .unwrap();
        for (size, reason) in [
            (MAX_ARGUMENT_BYTES, "size limit"),
            (MAX_ARGUMENT_BYTES / 2, "match budget"),
        ] {
            let raw = json!({"s": "a".repeat(size)}).to_string();
            let Deny(msg) = p.verdict(Safety::Autonomous, "x", &raw, true) else {
                panic!("must deny");
            };
            assert!(msg.contains(reason), "{msg}");
        }
        for name in [
            "shell",
            "run_background_job",
            "",
            "K",
            "t\n",
            &"x".repeat(MAX_PATTERN_BYTES + 1),
        ] {
            let reason = validate_tool(name).unwrap_err();
            assert_eq!(
                p.verdict(Safety::Autonomous, name, "{}", true),
                Deny(reason.into())
            );
        }
    }

    #[test]
    fn anchored_wildcards() {
        for (pat, text, expected) in [
            ("", "", true),
            ("", "x", false),
            ("*?", "", false),
            ("?*?", "x", false),
            ("?", "\n", true),
            ("x", "X/y", false),
            ("x?z", "X/Z", true),
            ("**a*ab?***", "aa//\naabZ", true),
            ("**a*ab?", "aa//\naabZ?", false),
            ("a*b*", "a", false),
            ("*a*b", "xxaab", true),
            ("*z", "zzab", false),
        ] {
            assert_eq!(
                glob(pat, text, &mut { MATCH_BUDGET }),
                Ok(expected),
                "{pat:?} ~ {text:?}"
            );
        }
    }
}
