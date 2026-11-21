//! Chat Completions wire records. Decode before JS can collapse duplicate routing fields.
use crate::compaction::Usage;
use serde::{
    de::{value::MapAccessDeserializer, MapAccess, Visitor},
    Deserialize, Deserializer,
};
use serde_json::{value::RawValue, Value};
use std::{fmt, marker::PhantomData};

// serde also decodes structs positionally from arrays; the SDK never reads tool fields that way.
#[derive(Default)]
pub struct Object<T>(pub T);

impl<'de, T: Deserialize<'de>> Deserialize<'de> for Object<T> {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct Map<T>(PhantomData<T>);
        impl<'de, T: Deserialize<'de>> Visitor<'de> for Map<T> {
            type Value = Object<T>;
            fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str("a JSON object")
            }
            fn visit_map<A: MapAccess<'de>>(self, map: A) -> Result<Self::Value, A::Error> {
                T::deserialize(MapAccessDeserializer::new(map)).map(Object)
            }
        }
        d.deserialize_map(Map(PhantomData))
    }
}

#[derive(Default, Deserialize)]
pub struct Chunk {
    #[serde(default, rename = "error", deserialize_with = "present")]
    pub has_error: bool,
    pub choices: Option<Vec<Object<Choice>>>,
    pub usage: Option<Object<Usage>>,
}

fn present<'de, D: Deserializer<'de>>(d: D) -> Result<bool, D::Error> {
    serde::de::IgnoredAny::deserialize(d).map(|_| true)
}

#[derive(Deserialize)]
pub struct Choice {
    pub delta: Option<Object<Delta>>,
    pub finish_reason: Option<String>,
}

#[derive(Default, Deserialize)]
#[serde(default)]
pub struct Delta {
    pub role: Option<String>,
    #[serde(deserialize_with = "content")]
    pub content: String,
    pub reasoning: Value,
    pub reasoning_content: Value,
    pub tool_calls: Option<Vec<Object<Fragment>>>,
    pub refusal: Option<String>,
    pub function_call: Option<Value>,
}

// The SDK's string decoder returns arrays as their original JSON, not joined text parts.
fn content<'de, D: Deserializer<'de>>(d: D) -> Result<String, D::Error> {
    let raw = Box::<RawValue>::deserialize(d)?;
    match raw.get().as_bytes()[0] {
        b'n' => Ok(String::new()),
        b'[' => Ok(raw.get().into()),
        _ => serde_json::from_str(raw.get()).map_err(serde::de::Error::custom),
    }
}

#[derive(Deserialize)]
pub struct Fragment {
    pub index: Option<u64>,
    pub id: Option<String>,
    #[serde(rename = "type")]
    pub kind: Option<String>,
    pub function: Option<Object<Function>>,
}

#[derive(Default, Deserialize)]
pub struct Function {
    pub name: Option<String>,
    pub arguments: Option<String>,
}

impl Chunk {
    pub fn decode(raw: &str) -> serde_json::Result<Self> {
        serde_json::from_str::<Object<Self>>(raw).map(|o| o.0)
    }

    pub fn prelude(&self) -> bool {
        self.usage.is_none()
            && self.choices.iter().flatten().all(|Object(choice)| {
                choice.finish_reason.as_deref().is_none_or(str::is_empty)
                    && choice.delta.as_ref().is_none_or(|Object(d)| {
                        d.content.is_empty()
                            && d.reasoning().is_none()
                            && d.tool_calls.as_ref().is_none_or(Vec::is_empty)
                            && !d.ambiguous()
                    })
            })
    }
}

impl Delta {
    pub fn reasoning(&self) -> Option<&str> {
        [&self.reasoning_content, &self.reasoning]
            .into_iter()
            .filter_map(Value::as_str)
            .find(|s| !s.is_empty())
    }

    pub fn ambiguous(&self) -> bool {
        self.function_call.is_some() || self.refusal.as_deref().is_some_and(|s| !s.is_empty())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn wire_fields_cannot_be_supplied_positionally_or_twice() {
        for raw in [
            r#"[null,[],null]"#,
            r#"{"choices":[[{},"stop"]]}"#,
            r#"{"choices":[{"delta":{"tool_calls":[{"function":["think","{}"]}]}}]}"#,
            r#"{"choices":[{"delta":{"tool_calls":[{"function":{"name":"a","n\u0061me":"b"}}]}}]}"#,
            r#"{"choices":[],"choices":[]}"#,
            r#"{"error":null,"error":null}"#,
        ] {
            assert!(Chunk::decode(raw).is_err());
        }
    }
}
