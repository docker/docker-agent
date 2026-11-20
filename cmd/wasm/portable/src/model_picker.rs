//! Picker choices change request parameters, not the current agent's transport.
use crate::go_json::{read_string, scalar};
use indexmap::IndexMap;
use serde_json::{Map, Value};
use std::cell::Cell;

#[derive(Default)]
pub struct Picker {
    pub choices: IndexMap<String, Map<String, Value>>,
    pub active: Cell<Option<usize>>,
}

impl Picker {
    pub fn models(&self) -> String {
        self.choices
            .keys()
            .map(String::as_str)
            .collect::<Vec<_>>()
            .join(", ")
    }

    pub fn metadata(&self, instructions: &str, tools: &[Value]) -> (String, Vec<Value>) {
        let models = self.models();
        let mut tools = tools.to_vec();
        let instructions = instructions.replace("{AVAILABLE_MODELS}", &models);
        for tool in &mut tools {
            let description = &mut tool["function"]["description"];
            *description = description
                .as_str()
                .expect("Go catalog description")
                .replace("{AVAILABLE_MODELS}", &models)
                .into();
        }
        (instructions, tools)
    }

    pub fn params(&self) -> Option<&Map<String, Value>> {
        self.choices.get_index(self.active.get()?).map(|(_, v)| v)
    }

    pub fn change(&self, args: &str, selection: &mut Option<usize>) -> Result<String, String> {
        let name = scalar(
            args,
            "modelpicker.ChangeModelArgs",
            "model",
            read_string,
            "invalid arguments: ",
        )?;
        if name.is_empty() {
            return Err("model parameter is required".into());
        }
        *selection = Some(self.choices.get_index_of(&name).ok_or_else(|| {
            format!(
                "model {name:?} is not in the allowed list. Available models: {}",
                self.models()
            )
        })?);
        Ok(format!("Model changed to {name}"))
    }
}

#[cfg(test)]
mod tests {
    use crate::config;
    use std::collections::BTreeMap;

    #[test]
    fn rejects_incompatible_choices_before_opening_a_session() {
        // Identical values under different names still represent different credential scopes.
        let env = BTreeMap::from([("KEY".into(), "k".into()), ("OTHER".into(), "k".into())]);
        for (models, picker) in [
            ("{}", "{type: model_picker}"),
            ("{}", "{type: model_picker, models: []}"),
            ("{}", "{type: model_picker, models: [missing]}"),
            (
                "{}",
                "{type: model_picker, models: [local/fast, local/fast]}",
            ),
            (
                "{}",
                "{type: model_picker, models: [local/fast], defer: {unsupported: true}}",
            ),
            ("{}", "{type: model_picker, models: [other/fast]}"),
            ("{}", "{type: model_picker, models: [local/gpt-5]}"),
            ("{}", "{type: model_picker, models: ['local/a,local/b']}"),
            (
                "{b: {provider: local, model: other, token_key: OTHER}}",
                "{type: model_picker, models: [b]}",
            ),
            (
                "{b: {provider: local, model: other, base_url: 'https://different.invalid'}}",
                "{type: model_picker, models: [b]}",
            ),
            (
                "{b: {provider: local, model: other, temperature: bad}}",
                "{type: model_picker, models: [b]}",
            ),
            ("{}", "{type: session_context, models: [local/fast]}"),
        ] {
            let yaml = format!("providers: {{local: {{token_key: KEY}}, other: {{token_key: KEY}}}}\nmodels: {models}\nagents: {{root: {{model: local/start, toolsets: [{picker}]}}}}");
            assert!(config::select(&yaml, "", &env).is_err(), "{yaml}");
        }
    }
}
