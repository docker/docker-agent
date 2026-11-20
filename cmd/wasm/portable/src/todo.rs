//! Session-local counterpart of pkg/tools/builtin/todo.

use crate::go_json::{
    encode, field_matches, fields, normalize_json, object, read_string, take, type_error,
};
use serde::Serialize;
use serde_json::Value;
use std::fmt::Write as _;

const MAX_ARGUMENT_BYTES: usize = 64 * 1024;
// Every successful call includes the whole list, often also its text in a reminder.
const MAX_TEXT_BYTES: usize = 64 * 1024;
const MAX_TODOS: usize = 256;
const REMINDER: &str = "The following todos are still incomplete and MUST be completed:";

#[derive(Default)]
pub struct Todos {
    items: Vec<Todo>,
}

#[derive(Serialize)]
struct Todo {
    id: String,
    description: String,
    status: String,
}

#[derive(Default, Serialize)]
struct Update {
    id: String,
    status: String,
}

#[derive(Serialize)]
struct Created<'a, T> {
    created: T,
    all_todos: &'a [Todo],
    #[serde(skip_serializing_if = "String::is_empty")]
    reminder: String,
}

#[derive(Serialize)]
struct Updated<'a> {
    #[serde(skip_serializing_if = "Vec::is_empty")]
    updated: Vec<&'a Update>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    not_found: Vec<&'a str>,
    all_todos: Option<&'a [Todo]>,
    #[serde(skip_serializing_if = "String::is_empty")]
    reminder: String,
}

#[derive(Serialize)]
struct Listed<'a> {
    todos: &'a [Todo],
    #[serde(skip_serializing_if = "String::is_empty")]
    reminder: String,
}

impl Todos {
    /// `Err` is model-facing error output, including Go's JSON for all-missing updates.
    pub fn call(&mut self, name: &str, arguments: &str) -> Result<String, String> {
        if arguments.len() > MAX_ARGUMENT_BYTES {
            return Err("todo argument size limit reached".into());
        }
        if name == "list_todos" {
            // Unlike the typed Go handlers, list_todos ignores its arguments.
            return Ok(encode(Listed {
                todos: &self.items,
                reminder: self.reminder(),
            }));
        }
        // The runtime prefixes a typed handler's decoding error.
        match arguments_for(name, arguments).map_err(|e| format!("Error calling tool: {e}"))? {
            Args::Create(description) => {
                let start = self.append(vec![description])?;
                Ok(self.created(&self.items[start]))
            }
            Args::CreateMany(descriptions) => {
                let start = self.append(descriptions.finish())?;
                Ok(self.created(&self.items[start..]))
            }
            Args::Update(updates) => self.update(&updates.finish()),
        }
    }

    fn created(&self, created: impl Serialize) -> String {
        encode(Created {
            created,
            all_todos: &self.items,
            reminder: self.reminder(),
        })
    }

    fn append(&mut self, descriptions: Vec<String>) -> Result<usize, String> {
        let start = self.items.len();
        if start + descriptions.len() > MAX_TODOS {
            return Err("todo count limit reached".into());
        }
        let used: usize = self
            .items
            .iter()
            .map(|t| t.description.len() + t.status.len())
            .sum();
        let extra: usize = descriptions.iter().map(|s| s.len() + "pending".len()).sum();
        if used + extra > MAX_TEXT_BYTES {
            return Err("todo text size limit reached".into());
        }
        for description in descriptions {
            self.items.push(Todo {
                id: format!("todo_{}", self.items.len() + 1),
                description,
                status: "pending".into(),
            });
        }
        Ok(start)
    }

    fn update(&mut self, updates: &[Update]) -> Result<String, String> {
        let mut updated = Vec::new();
        let mut not_found = Vec::new();
        let mut statuses = vec![None; self.items.len()];
        for u in updates {
            if let Some(i) = self.items.iter().position(|t| t.id == u.id) {
                // Emit every successful update in call order; only the last status is stored.
                statuses[i] = Some(u.status.as_str());
                updated.push(u);
            } else {
                not_found.push(u.id.as_str());
            }
        }
        if updated.is_empty() && !not_found.is_empty() {
            return Err(encode(Updated {
                updated,
                not_found,
                all_todos: None,
                reminder: String::new(),
            }));
        }
        let bytes: usize = self
            .items
            .iter()
            .zip(&statuses)
            .map(|(t, s)| t.description.len() + s.unwrap_or(&t.status).len())
            .sum();
        if bytes > MAX_TEXT_BYTES {
            return Err("todo text size limit reached".into());
        }
        for (t, status) in self.items.iter_mut().zip(statuses) {
            if let Some(status) = status {
                t.status = status.into();
            }
        }
        Ok(encode(Updated {
            updated,
            not_found,
            all_todos: Some(&self.items),
            reminder: self.reminder(),
        }))
    }

    fn reminder(&self) -> String {
        let mut s = String::new();
        for status in ["in-progress", "pending"] {
            for t in self.items.iter().filter(|t| t.status == status) {
                if s.is_empty() {
                    s.push_str(REMINDER);
                }
                write!(s, " ({status}) [{}] {}", t.id, t.description).unwrap();
            }
        }
        s
    }
}

enum Args {
    Create(String),
    CreateMany(GoSlice<String>),
    Update(GoSlice<Update>),
}

fn arguments_for(name: &str, input: &str) -> Result<Args, String> {
    let (ty, field, mut args) = match name {
        "create_todo" => ("CreateTodoArgs", "description", Args::Create(String::new())),
        "create_todos" => (
            "CreateTodosArgs",
            "descriptions",
            Args::CreateMany(GoSlice::default()),
        ),
        "update_todos" => (
            "UpdateTodosArgs",
            "updates",
            Args::Update(GoSlice::default()),
        ),
        _ => return Err("unknown todo tool".into()),
    };
    let input = normalize_json(input)?;
    let Some(input) = object(&input, &format!("todo.{ty}"))? else {
        return Ok(args);
    };
    let mut decode = |s: &str| {
        fields(s, |key, val| {
            if field_matches(&key, field) {
                args.decode(ty, &key, val)
            } else {
                Ok(())
            }
        })
    };
    if let Err(error) = decode(input) {
        // aijson retries the repaired object into the same partially decoded struct.
        let repaired = repaired_args(input, field);
        if repaired.is_none_or(|s| decode(&s).is_err()) {
            return Err(error);
        }
    }
    Ok(args)
}

impl Args {
    fn decode(&mut self, ty: &str, key: &str, val: &str) -> Result<(), String> {
        let path = format!("{ty}.{key}");
        match self {
            Args::Create(s) => read_string(val, s, &format!("Go struct field {path}")),
            Args::CreateMany(v) => v.decode(val, &path, "[]string", read_string),
            Args::Update(v) => v.decode(val, &path, "[]todo.Update", |val, u, path| {
                if val == "null" {
                    return Ok(());
                }
                if !val.starts_with('{') {
                    return Err(type_error(val, path, "todo.Update"));
                }
                fields(val, |key, val| {
                    let s = if field_matches(&key, "id") {
                        &mut u.id
                    } else if field_matches(&key, "status") {
                        &mut u.status
                    } else {
                        return Ok(());
                    };
                    read_string(val, s, &format!("Go struct field {path}.{key}"))
                })
            }),
        }
    }
}

// encoding/json resets a slice's length, but reuses its backing elements on duplicate fields.
#[derive(Default)]
struct GoSlice<T> {
    items: Vec<T>,
    len: usize,
}

impl<T: Default> GoSlice<T> {
    fn finish(mut self) -> Vec<T> {
        self.items.truncate(self.len);
        self.items
    }

    fn decode(
        &mut self,
        s: &str,
        path: &str,
        ty: &str,
        mut read: impl FnMut(&str, &mut T, &str) -> Result<(), String>,
    ) -> Result<(), String> {
        if s == "null" {
            *self = Self::default();
            return Ok(());
        }
        let Some(mut s) = s.strip_prefix('[').map(str::trim_start) else {
            return Err(type_error(s, &format!("Go struct field {path}"), ty));
        };
        self.len = 0;
        if s.starts_with(']') {
            self.items.clear();
        }
        let mut first_error = None;
        while !s.starts_with(']') {
            let n = self.len;
            if n == self.items.len() {
                self.items.push(T::default());
            }
            if let Err(e) = read(take(&mut s), &mut self.items[n], &format!("{path}.{n}")) {
                first_error.get_or_insert(e);
            }
            self.len += 1;
            let Some(next) = s.strip_prefix(',') else {
                break;
            };
            s = next;
        }
        first_error.map_or(Ok(()), Err)
    }
}

fn repaired_args(input: &str, field: &str) -> Option<String> {
    let mut raw = serde_json::from_str::<serde_json::Map<String, Value>>(input).ok()?;
    let value = raw.remove(field)?;
    if field == "description" {
        if !value.is_null() {
            return None;
        }
    } else {
        raw.insert(field.into(), repair_slice(value, field == "descriptions")?);
    }
    serde_json::to_string(&raw).ok()
}

fn repair_slice(val: Value, strings: bool) -> Option<Value> {
    match val {
        Value::String(s) => normalize_json(s.trim())
            .ok()
            .and_then(|s| serde_json::from_str::<Vec<Value>>(&s).ok())
            .map(Value::Array)
            .or_else(|| strings.then(|| Value::Array(vec![Value::String(s)]))),
        Value::Object(m) if strings && m.len() == 1 => {
            let (_, v) = m.into_iter().next()?;
            v.is_string().then(|| Value::Array(vec![v]))
        }
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::go_json::MAX_JSON_DEPTH;
    use serde_json::json;

    // Expected bytes and error flags from todo.New().Tools(...)[i].Handler in Go.
    #[test]
    fn go_batch_transcript() {
        let mut t = Todos::default();
        for (name, args, result) in [
            ("list_todos", "ignored", Ok(r#"{"todos":[]}"#)),
            ("create_todos", "{}", Ok(r#"{"created":[],"all_todos":[]}"#)),
            ("update_todos", "null", Ok(r#"{"all_todos":[]}"#)),
            (
                "create_todos",
                r#"{"descriptions":["A","B"]}"#,
                Ok(
                    r#"{"created":[{"id":"todo_1","description":"A","status":"pending"},{"id":"todo_2","description":"B","status":"pending"}],"all_todos":[{"id":"todo_1","description":"A","status":"pending"},{"id":"todo_2","description":"B","status":"pending"}],"reminder":"The following todos are still incomplete and MUST be completed: (pending) [todo_1] A (pending) [todo_2] B"}"#,
                ),
            ),
            (
                "update_todos",
                r#"{"updates":[{"id":"todo_2","status":"in-progress"},{"id":"missing","status":"completed"}]}"#,
                Ok(
                    r#"{"updated":[{"id":"todo_2","status":"in-progress"}],"not_found":["missing"],"all_todos":[{"id":"todo_1","description":"A","status":"pending"},{"id":"todo_2","description":"B","status":"in-progress"}],"reminder":"The following todos are still incomplete and MUST be completed: (in-progress) [todo_2] B (pending) [todo_1] A"}"#,
                ),
            ),
            (
                "update_todos",
                r#"{"updates":[{"id":"todo_1","status":"completed"},{"id":"todo_2","status":"pending"},{"id":"todo_2","status":"other"}]}"#,
                Ok(
                    r#"{"updated":[{"id":"todo_1","status":"completed"},{"id":"todo_2","status":"pending"},{"id":"todo_2","status":"other"}],"all_todos":[{"id":"todo_1","description":"A","status":"completed"},{"id":"todo_2","description":"B","status":"other"}]}"#,
                ),
            ),
            (
                "update_todos",
                r#"{"updates":[{"id":"todo_02"},{"id":"todo_02"}]}"#,
                Err(r#"{"not_found":["todo_02","todo_02"],"all_todos":null}"#),
            ),
            (
                "list_todos",
                "",
                Ok(
                    r#"{"todos":[{"id":"todo_1","description":"A","status":"completed"},{"id":"todo_2","description":"B","status":"other"}]}"#,
                ),
            ),
        ] {
            assert_eq!(
                t.call(name, args).as_deref().map_err(String::as_str),
                result
            );
        }
        assert_eq!(
            Todos::default().call("list_todos", "").unwrap(),
            r#"{"todos":[]}"#
        );
    }

    #[test]
    fn go_create_escaping() {
        assert_eq!(
            Todos::default()
                .call(
                    "create_todo",
                    r#"{"description":"A<&>+\u2028\u2029\b\f\u000b\u001f\"\\\né🌍"}"#
                )
                .unwrap(),
            r#"{"created":{"id":"todo_1","description":"A\u003c\u0026\u003e+\u2028\u2029\b\f\u000b\u001f\"\\\né🌍","status":"pending"},"all_todos":[{"id":"todo_1","description":"A\u003c\u0026\u003e+\u2028\u2029\b\f\u000b\u001f\"\\\né🌍","status":"pending"}],"reminder":"The following todos are still incomplete and MUST be completed: (pending) [todo_1] A\u003c\u0026\u003e+\u2028\u2029\b\f\u000b\u001f\"\\\né🌍"}"#
        );
    }

    #[test]
    fn typed_arguments_match_go_defaults_and_repairs() {
        for (args, description) in [
            ("", ""),
            ("null", ""),
            (r#"{"description":1,"description":null}"#, ""),
            (r#"{"description":null}"#, ""),
            (
                r#"{"unrelated":[1e999],"DESCRIPTION":"x","description":null}"#,
                "x",
            ),
            (
                r#"{"deſcription":"\udfff\uD800\uD83C\uDF0D\\udfff"}"#,
                "��🌍\\udfff",
            ),
        ] {
            let result = Todos::default().call("create_todo", args).unwrap();
            assert_eq!(
                serde_json::from_str::<Value>(&result).unwrap()["created"]["description"],
                description
            );
        }
        for (args, values) in [
            (r#"{"descriptions":"X"}"#, vec!["X"]),
            (r#"{"descriptions":"[\"A\",null]"}"#, vec!["A", ""]),
            (r#"{"descriptions":{"any":"X"}}"#, vec!["X"]),
            (
                r#"{"descriptions":["a","b"],"Descriptions":[null]}"#,
                vec!["a"],
            ),
            (
                r#"{"descriptions":["a","b"],"descriptions":["z"],"descriptions":[null,null]}"#,
                vec!["z", "b"],
            ),
            (
                r#"{"descriptions":[7,"z"],"descriptions":"[null,null]"}"#,
                vec!["", "z"],
            ),
            (r#"{"descriptions":null}"#, vec![]),
        ] {
            let mut t = Todos::default();
            t.call("create_todos", args).unwrap();
            assert_eq!(
                t.items.iter().map(|t| &t.description).collect::<Vec<_>>(),
                values
            );
        }
        let mut t = Todos::default();
        t.call("create_todo", "").unwrap();
        t.call(
            "update_todos",
            r#"{"updates":"[{\"ID\":\"todo_1\",\"Status\":null}]"}"#,
        )
        .unwrap();
        assert_eq!(t.items[0].status, "");
    }

    #[test]
    fn type_errors_do_not_mutate_a_prefix_of_the_batch() {
        let mut t = Todos::default();
        t.call("create_todo", "{}").unwrap();
        for (name, args, error) in [
            ("create_todo", "[", "unexpected end of JSON input"),
            ("create_todo", "3", "json: cannot unmarshal number into Go value of type todo.CreateTodoArgs"),
            ("create_todo", r#"{"description":true,"description":"overridden"}"#, "json: cannot unmarshal bool into Go struct field CreateTodoArgs.description of type string"),
            ("create_todos", r#"{"descriptions":["ok",4]}"#, "json: cannot unmarshal number into CreateTodosArgs.descriptions.1 of type string"),
            ("create_todos", r#"{"descriptions":"[5]"}"#, "json: cannot unmarshal string into Go struct field CreateTodosArgs.descriptions of type []string"),
            ("update_todos", r#"{"updates":[{"id":"todo_1","status":"completed"},{"id":"todo_1","status":2}]}"#, "json: cannot unmarshal number into Go struct field UpdateTodosArgs.updates.1.status of type string"),
            ("update_todos", r#"{"updates":[["todo_1","completed"]]}"#, "json: cannot unmarshal array into UpdateTodosArgs.updates.0 of type todo.Update"),
        ] {
            assert_eq!(
                t.call(name, args).unwrap_err(),
                format!("Error calling tool: {error}")
            );
        }
        assert_eq!(t.items.len(), 1);
        assert_eq!(t.items[0].status, "pending");
    }

    #[test]
    fn count_and_byte_limits_do_not_consume_ids_or_change_statuses() {
        let mut t = Todos::default();
        t.call("create_todo", "{}").unwrap();
        let depth = "[".repeat(MAX_JSON_DEPTH) + &"]".repeat(MAX_JSON_DEPTH);
        assert_eq!(
            t.call("create_todo", &format!(r#"{{"ignored":{depth}}}"#))
                .unwrap_err(),
            "Error calling tool: exceeded max depth"
        );
        let batch = json!({"descriptions": vec![""; MAX_TODOS]}).to_string();
        assert!(t
            .call("create_todos", &batch)
            .unwrap_err()
            .contains("count limit"));
        let large = "x".repeat(MAX_TEXT_BYTES / 2);
        t.call("create_todo", &json!({"description":large}).to_string())
            .unwrap();
        assert_eq!(t.items[1].id, "todo_2");
        assert!(t
            .call("create_todo", &json!({"description":large}).to_string())
            .unwrap_err()
            .contains("text size limit"));
        let updates = json!({"updates":[{"id":"todo_1","status":"completed"},{"id":"todo_2","status":large}]});
        assert!(t
            .call("update_todos", &updates.to_string())
            .unwrap_err()
            .contains("text size limit"));
        assert_eq!(t.items.len(), 2);
        assert!(t.items.iter().all(|t| t.status == "pending"));
        assert!(t
            .call("create_todos", &" ".repeat(MAX_ARGUMENT_BYTES + 1))
            .unwrap_err()
            .contains("argument size limit"));
    }
}
