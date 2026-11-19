use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::{collections::BTreeMap, sync::OnceLock};

#[derive(Deserialize)]
pub struct Builtin {
    name: String,
    api_type: String,
    #[serde(default)]
    pub base_url: String,
    #[serde(default)]
    token_env_vars: Vec<String>,
    #[serde(default)]
    pub auto_responses: bool,
    #[serde(default)]
    pub merge_consecutive_messages: bool,
    #[serde(default)]
    requires_configuration: bool,
    #[serde(default)]
    headers: BTreeMap<String, String>,
    #[serde(default)]
    query_params: BTreeMap<String, String>,
}

pub fn lookup(name: &str) -> Option<&'static Builtin> {
    static CATALOG: OnceLock<Vec<Builtin>> = OnceLock::new();
    CATALOG
        .get_or_init(|| {
            serde_json::from_str(include_str!(
                "../../../../pkg/model/provider/catalog/providers.json"
            ))
            .expect("embedded Go provider catalog")
        })
        .iter()
        .find(|p| {
            p.name == name
                && p.api_type == "openai"
                && !p.token_env_vars.is_empty()
                && (p.scoped()
                    || (p.base_url.starts_with("https://")
                        && !p.base_url.contains('$')
                        && !p.requires_configuration
                        && p.token_env_vars.len() == 1
                        && p.headers.is_empty()
                        && p.query_params.is_empty()))
        })
}

#[derive(Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Options {
    #[serde(deserialize_with = "service_tier")]
    pub service_tier: Option<String>,
    api_version: Option<String>,
    #[serde(rename = "http_headers", deserialize_with = "copilot_header")]
    copilot_header: Option<String>,
}

// A present map key shadows the provider default even if its value is null or empty.
fn service_tier<'de, D: serde::Deserializer<'de>>(d: D) -> Result<Option<String>, D::Error> {
    Ok(Some(Option::<String>::deserialize(d)?.unwrap_or_default()))
}

fn copilot_header<'de, D: serde::Deserializer<'de>>(d: D) -> Result<Option<String>, D::Error> {
    let mut header = None;
    for (name, value) in BTreeMap::<String, String>::deserialize(d)? {
        if !name.eq_ignore_ascii_case("Copilot-Integration-Id") || header.is_some() {
            return Err(serde::de::Error::custom(
                "only Copilot-Integration-Id may be overridden, once",
            ));
        }
        let value = value.replace(['\r', '\n'], "");
        let value = value.trim();
        check_header_value(value).map_err(serde::de::Error::custom)?;
        header = Some(value.into());
    }
    Ok(header)
}

impl Options {
    pub fn validate(&self, builtin: Option<&Builtin>) -> Result<(), String> {
        let name = builtin.map(|p| p.name.as_str());
        if self.api_version.is_some() && name != Some("azure") {
            return Err("api_version is only supported on the built-in azure provider".into());
        }
        if self.copilot_header.is_some() && name != Some("github-copilot") {
            return Err(
                "Copilot-Integration-Id requires the built-in github-copilot provider".into(),
            );
        }
        Ok(())
    }
}

#[derive(Serialize, PartialEq, Eq)]
pub struct Transport {
    pub endpoint: String,
    pub headers: BTreeMap<String, String>,
    query: BTreeMap<String, String>,
    pub chatgpt: bool,
    azure_default_key: bool,
    #[serde(skip)]
    token_key: String,
}

impl Builtin {
    pub fn scoped(&self) -> bool {
        matches!(
            self.name.as_str(),
            "github-copilot"
                | "azure"
                | "chatgpt"
                | "cloudflare-workers-ai"
                | "cloudflare-ai-gateway"
        )
    }

    pub fn validate_base(
        &self,
        base: &str,
        key: &str,
        env: &BTreeMap<String, String>,
    ) -> Result<(), String> {
        if base.is_empty() {
            return Err(format!("{} requires an explicit base_url", self.name));
        }
        if base.trim_end_matches('/') != self.base_url.trim_end_matches('/') {
            if self.name != "azure"
                && (self.scoped() || key.is_empty() || self.token_env_vars.iter().any(|k| k == key))
            {
                return Err("cannot use built-in credentials on an overridden endpoint; configure a custom provider and token_key".into());
            }
            return Ok(());
        }
        let segments: &[&str] = match self.name.as_str() {
            "cloudflare-workers-ai" => &["CLOUDFLARE_ACCOUNT_ID"],
            "cloudflare-ai-gateway" => &["CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_GATEWAY_ID"],
            _ => return Ok(()),
        };
        for key in segments {
            let value = env
                .get(*key)
                .ok_or_else(|| format!("environment variable {key} not set"))?;
            if value.is_empty()
                || matches!(value.as_str(), "." | "..")
                || !value
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b"-_.~".contains(&b))
            {
                return Err(format!("{key} must be a single unescaped path segment"));
            }
        }
        Ok(())
    }
}

pub fn transport(
    builtin: Option<&Builtin>,
    base: &str,
    route: &str,
    key: &str,
    opts: &Options,
    env: &BTreeMap<String, String>,
) -> Result<Transport, String> {
    let name = builtin.map_or("", |p| p.name.as_str());
    let defaults = builtin.map_or(&[][..], |p| p.token_env_vars.as_slice());
    let mut headers = builtin.map(|b| b.headers.clone()).unwrap_or_default();
    if let Some(value) = &opts.copilot_header {
        headers.insert("Copilot-Integration-Id".into(), value.clone());
    }
    let chatgpt = name == "chatgpt";
    // Explicit token_key is required by Go's config preflight, even for GITHUB_TOKEN.
    let key = if !key.is_empty() || defaults.is_empty() {
        key
    } else {
        defaults
            .iter()
            .find(|k| env.get(*k).is_some_and(|v| !v.is_empty()))
            .ok_or_else(|| format!("{} environment variable is required", defaults.join(" or ")))?
    };
    if !key.is_empty() {
        let token = env
            .get(key)
            .filter(|v| !v.is_empty())
            .ok_or_else(|| format!("{key} environment variable is required"))?;
        check_header_value(token)?;
        if chatgpt {
            if let Some(id) = account_id(token)? {
                headers.insert("ChatGPT-Account-Id".into(), id);
            }
            headers.insert("OpenAI-Beta".into(), "responses=experimental".into());
            headers.insert("originator".into(), "codex_cli_rs".into());
        }
        headers.insert("Authorization".into(), format!("Bearer {token}"));
    }
    let mut query = BTreeMap::new();
    if let (Some(b), Some(version)) = (builtin, &opts.api_version) {
        query.insert(
            b.query_params
                .get("api_version")
                .ok_or("unknown api_version query mapping")?
                .clone(),
            version.clone(),
        );
    }
    Ok(Transport {
        endpoint: format!("{}/{route}", base.trim_end_matches('/')),
        headers,
        chatgpt,
        query,
        token_key: key.into(),
        azure_default_key: name == "azure" && defaults.iter().any(|k| k == key),
    })
}

fn check_header_value(value: &str) -> Result<(), String> {
    if !value.bytes().all(|b| (b' '..=b'~').contains(&b)) {
        return Err("unsupported character in provider credential or header".into());
    }
    Ok(())
}

// Read the optional routing claim, not proof of identity. Only the Codex backend verifies the token.
fn account_id(token: &str) -> Result<Option<String>, String> {
    if !token.contains('.') {
        return Ok(None);
    }
    let invalid = "unsupported ChatGPT access token";
    let mut parts = token.split('.');
    let mut decode = || {
        URL_SAFE_NO_PAD
            .decode(parts.next().ok_or(invalid)?)
            .map_err(|_| invalid)
    };
    let header: Value = serde_json::from_slice(&decode()?).map_err(|_| invalid)?;
    let claims: Value = serde_json::from_slice(&decode()?).map_err(|_| invalid)?;
    decode()?;
    if parts.next().is_some()
        || !matches!(header["alg"].as_str(), Some("RS256" | "ES256"))
        || !claims.is_object()
    {
        return Err(invalid.into());
    }
    let id = claims["https://api.openai.com/auth"]["chatgpt_account_id"]
        .as_str()
        .filter(|s| !s.is_empty());
    if let Some(id) = id {
        check_header_value(id)?;
    }
    Ok(id.map(str::to_owned))
}

pub fn apply_chatgpt_policy(body: &mut Value) {
    let mut instructions = Vec::new();
    body["input"]
        .as_array_mut()
        .expect("Responses input list")
        .retain(|item| {
            if item["role"] != "system" {
                return true;
            }
            if let Some(parts) = item["content"].as_array() {
                for part in parts.iter().filter(|p| p["type"] == "input_text") {
                    if let Some(text) = part["text"].as_str().filter(|s| !s.trim().is_empty()) {
                        instructions.push(text.to_owned());
                    }
                }
            }
            false
        });
    body["instructions"] = if instructions.is_empty() {
        "You are a helpful assistant.".into()
    } else {
        instructions.join("\n\n").into()
    };
    body["store"] = false.into();
    if !body["reasoning"]["summary"].is_null() {
        body["reasoning"]["summary"] = "auto".into();
    }
    let params = body.as_object_mut().expect("Responses request");
    for k in ["temperature", "top_p", "max_output_tokens"] {
        params.remove(k);
    }
}
