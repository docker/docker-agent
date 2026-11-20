use serde::{Deserialize, Deserializer};
use serde_json::{json, Map, Value};

fn family(model: &str) -> String {
    model
        .trim()
        .rsplit('/')
        .next()
        .unwrap()
        .to_ascii_lowercase()
}

fn generation(name: &str) -> (u8, u8) {
    let version = name
        .strip_prefix("gpt-")
        .unwrap_or("")
        .split('-')
        .next()
        .unwrap();
    let (major, minor) = version.split_once('.').unwrap_or((version, "0"));
    if [major, minor]
        .iter()
        .all(|s| (1..=2).contains(&s.len()) && s.bytes().all(|b| b.is_ascii_digit()))
    {
        (major.parse().unwrap(), minor.parse().unwrap())
    } else {
        (0, 0)
    }
}

fn o_series(name: &str) -> bool {
    ["o1", "o3", "o4"].iter().any(|p| name.starts_with(p))
}

pub(super) fn responses(model: &str) -> bool {
    let name = family(model);
    o_series(&name)
        || ["gpt-4.1", "gpt-5", "codex"]
            .iter()
            .any(|p| name.starts_with(p))
        || name.contains("-codex")
        || generation(&name).0 >= 6
}

pub(super) fn deferred_tools(provider: &str, model: &str) -> bool {
    if !matches!(provider, "openai" | "chatgpt") {
        return false;
    }
    let name = family(model);
    generation(&name).0 >= 6
        || matches!(
            name.as_str(),
            "gpt-5.4"
                | "gpt-5.4-mini"
                | "gpt-5.5"
                | "gpt-5.6-luna"
                | "gpt-5.6-sol"
                | "gpt-5.6-terra"
        )
        || provider == "openai" && name == "gpt-5.4-pro"
}

pub(super) fn explicit_cache(model: &str) -> bool {
    generation(&family(model)) >= (5, 6)
}

#[derive(Debug)]
pub(super) enum Budget {
    Off,
    Effort(String),
    Other,
}

impl<'de> Deserialize<'de> for Budget {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        let v = Value::deserialize(d)?;
        match v {
            Value::Number(n) if n.as_i64() == Some(0) => Ok(Self::Off),
            Value::Number(n) if n.is_i64() => Ok(Self::Other),
            Value::String(s) if s.eq_ignore_ascii_case("none") => Ok(Self::Off),
            Value::String(s) => {
                let s = s.trim().to_ascii_lowercase();
                match s.as_str() {
                    "none" | "minimal" | "low" | "medium" | "high" | "xhigh" | "max" => {
                        Ok(Self::Effort(s))
                    }
                    "adaptive" | "adaptive/low" | "adaptive/medium" | "adaptive/high"
                    | "adaptive/xhigh" | "adaptive/max" => Ok(Self::Other),
                    _ => Err(serde::de::Error::custom("invalid thinking_budget effort")),
                }
            }
            _ => Err(serde::de::Error::custom(
                "thinking_budget must be an integer or effort name",
            )),
        }
    }
}

// Go recognizes only exact Sol/Luna IDs and real date-stamped snapshots for GPT-6's off switch.
fn sol_or_luna(name: &str) -> bool {
    let Some(suffix) = name
        .strip_prefix("gpt-6-sol")
        .or_else(|| name.strip_prefix("gpt-6-luna"))
    else {
        return false;
    };
    if suffix.is_empty() {
        return true;
    }
    let b = suffix.as_bytes();
    if b.len() != 11
        || !b.iter().enumerate().all(|(i, c)| {
            if matches!(i, 0 | 5 | 8) {
                *c == b'-'
            } else {
                c.is_ascii_digit()
            }
        })
    {
        return false;
    }
    let year: u16 = suffix[1..5].parse().unwrap();
    let max = match &suffix[6..8] {
        "02" => {
            if year.is_multiple_of(4) && (!year.is_multiple_of(100) || year.is_multiple_of(400)) {
                29
            } else {
                28
            }
        }
        "04" | "06" | "09" | "11" => 30,
        "01" | "03" | "05" | "07" | "08" | "10" | "12" => 31,
        _ => return false,
    };
    (1..=max).contains(&suffix[9..].parse::<u8>().unwrap())
}

#[derive(Default)]
pub struct Reasoning {
    effort: Option<String>,
    responses: bool,
    disabled: bool,
    omit_with_tools: bool,
}

impl Reasoning {
    pub(super) fn new(
        raw: &str,
        id: &str,
        vendor: bool,
        responses: bool,
        budget: Option<&Budget>,
    ) -> Result<Self, String> {
        let name = family(id);
        let version = generation(&name);
        if !(o_series(&name)
            || name.starts_with("gpt-5") && !name.starts_with("gpt-5-chat")
            || version.0 >= 6)
        {
            return Ok(Self::default());
        }
        let sol = vendor && sol_or_luna(&name);
        let disabled = matches!(budget, Some(Budget::Off));
        let native_off = sol || vendor && name.starts_with("gpt-5.") && version >= (5, 6);
        // The o-series default is chosen before the factory expands the model name.
        let effort = match budget {
            Some(Budget::Off) => Some(if native_off { "none" } else { "low" }.into()),
            Some(Budget::Effort(s)) => Some(s.clone()),
            Some(Budget::Other) => {
                return Err("OpenAI reasoning models require an effort name, not an adaptive or numeric thinking_budget".into());
            }
            None => o_series(&family(raw)).then(|| "medium".into()),
        };
        Ok(Self {
            responses,
            disabled,
            omit_with_tools: !responses
                && version >= (5, 4)
                && !(sol && effort.as_deref() == Some("none")),
            effort,
        })
    }

    pub fn apply(&self, body: &mut Map<String, Value>, tools: bool, cap: Option<i64>) {
        if tools && self.omit_with_tools {
            return;
        }
        if self.responses {
            let mut params = json!({});
            if !self.disabled {
                params["summary"] = "detailed".into();
            }
            if let Some(effort) = &self.effort {
                params["effort"] = effort.as_str().into();
            }
            body.insert("reasoning".into(), params);
        } else if let Some(effort) = &self.effort {
            body.insert("reasoning_effort".into(), effort.as_str().into());
        }
        if self.disabled && cap.is_some_and(|n| n < 256) {
            body.insert("max_completion_tokens".into(), 256.into());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn native_off_requires_vendor_and_exact_model() {
        for (model, vendor, expected) in [
            ("gpt-5.6-terra", true, "none"),
            ("gpt-5.6-terra", false, "low"),
            ("gpt-6-sol-2028-02-29", true, "none"),
            ("gpt-6-sol-2027-02-29", true, "low"),
            ("gpt-6-luna-preview", true, "low"),
            ("gpt-6.1-sol", true, "low"),
            ("gpt-7", true, "low"),
        ] {
            let config = Reasoning::new(model, model, vendor, true, Some(&Budget::Off)).unwrap();
            let mut body = Map::new();
            config.apply(&mut body, false, None);
            assert_eq!(body["reasoning"], json!({"effort":expected}), "{model}");
            assert!(!body.contains_key("max_completion_tokens"));
        }
    }
}
