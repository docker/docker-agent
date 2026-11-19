use serde::Deserialize;
use std::{collections::BTreeMap, sync::OnceLock};

#[derive(Deserialize)]
pub struct Builtin {
    name: String,
    api_type: String,
    #[serde(default)]
    pub base_url: String,
    #[serde(default)]
    pub token_env_vars: Vec<String>,
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
    let catalog = CATALOG.get_or_init(|| {
        serde_json::from_str(include_str!(
            "../../../../pkg/model/provider/catalog/providers.json"
        ))
        .expect("embedded Go provider catalog")
    });
    // Only API-key HTTPS providers. OAuth, SDK-specific headers, dynamic endpoints
    // and local provider discovery need separate ports, not generic Bearer auth.
    catalog.iter().find(|p| {
        p.name == name
            && p.api_type == "openai"
            && p.name != "chatgpt"
            && !p.requires_configuration
            && p.base_url.starts_with("https://")
            && !p.base_url.contains('$')
            && p.token_env_vars.len() == 1
            && p.headers.is_empty()
            && p.query_params.is_empty()
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn key_only_catalog_matches_supported_transports() {
        assert_eq!(lookup("groq").unwrap().token_env_vars[0], "GROQ_API_KEY");
        for name in [
            "chatgpt",
            "github-copilot",
            "azure",
            "cloudflare-workers-ai",
            "anthropic",
            "dmr",
        ] {
            assert!(lookup(name).is_none(), "{name}");
        }
    }
}
