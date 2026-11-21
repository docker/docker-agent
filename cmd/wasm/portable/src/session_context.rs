//! Previous conversations of this handle, not the module's registry of active handles.
use crate::go_json::{encode, quote, read_string, scalar, type_error};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::fmt::Write as _;

const TRANSCRIPT_BUDGET: usize = 24_000;
const MAX_SESSIONS: usize = 256;
const MAX_ARCHIVE_BYTES: usize = 8 * 1024 * 1024;

#[derive(Deserialize)]
pub struct Stamp {
    pub id: String,
    pub created_at: String,
}

#[derive(Serialize)]
struct Summary {
    id: String,
    title: &'static str,
    created_at: String,
    num_messages: usize,
}

struct Previous {
    summary: Summary,
    transcript: String,
}

pub struct Archive {
    current: Stamp,
    past: Vec<Previous>,
    bytes: usize,
}

impl Archive {
    pub fn new(current: Stamp) -> Self {
        Self {
            current,
            past: Vec::new(),
            bytes: 0,
        }
    }

    pub fn restart(&mut self, next: Stamp, messages: &[Value]) -> Result<(), String> {
        if !messages.is_empty() {
            if self.past.len() >= MAX_SESSIONS {
                return Err("session archive count limit reached".into());
            }
            let transcript = render(&self.current, messages);
            if self.bytes + transcript.len() > MAX_ARCHIVE_BYTES {
                return Err("session archive size limit reached".into());
            }
            self.bytes += transcript.len();
            self.past.push(Previous {
                summary: Summary {
                    id: std::mem::take(&mut self.current.id),
                    title: "",
                    created_at: std::mem::take(&mut self.current.created_at),
                    num_messages: messages.len(),
                },
                transcript,
            });
        }
        self.current = next;
        Ok(())
    }

    pub fn list(&self, args: &str) -> Result<String, String> {
        let input = if args.trim().is_empty() { "" } else { args };
        let limit: i64 = scalar(
            input,
            "sessioncontext.ListSessionsArgs",
            "limit",
            |val, n, path| {
                if val != "null" {
                    *n = val.parse().map_err(|_| {
                        if val.starts_with(|ch: char| ch.is_ascii_digit() || ch == '-') {
                            format!("json: cannot unmarshal number {val} into {path} of type int")
                        } else {
                            type_error(val, path, "int")
                        }
                    })?;
                }
                Ok(())
            },
            "invalid arguments: ",
        )?;
        let limit = if limit <= 0 {
            20
        } else {
            limit.min(100) as usize
        };
        let summaries: Vec<_> = self
            .past
            .iter()
            .rev()
            .take(limit)
            .map(|p| &p.summary)
            .collect();
        Ok(encode(summaries))
    }

    pub fn read(&self, args: &str) -> Result<String, String> {
        let id = scalar(
            args,
            "sessioncontext.ReadSessionArgs",
            "session_id",
            read_string,
            "invalid arguments: ",
        )?;
        let id = id.trim();
        if id.is_empty() {
            return Err("session_id must not be empty".into());
        }
        // Go resolves offsets against the full list (including the current session), then excludes self.
        let offset = id
            .parse::<i64>()
            .ok()
            .filter(|n| *n < 0)
            .map(i64::wrapping_neg);
        if id == self.current.id || offset == Some(1) {
            return Err("cannot read the current session; reference a different one".into());
        }
        let session = if let Some(n) = offset {
            let count = 1 + self.past.len();
            // Go int negation wraps for MinInt64; its index (offset - 1) wraps too.
            if n.wrapping_sub(1) >= count as i64 {
                return Err(format!("cannot resolve session {}: session offset {n} out of range (have {count} sessions)", quote(id)));
            }
            &self.past[count - n as usize]
        } else {
            self.past
                .iter()
                .find(|p| p.summary.id == id)
                .ok_or_else(|| format!("session {} not found", quote(id)))?
        };
        Ok(session.transcript.clone())
    }
}

fn render(stamp: &Stamp, messages: &[Value]) -> String {
    let msgs = messages.iter().filter(|m| m["role"] != "system");
    let mut out = format!(
        "# Session {} — (untitled)\nCreated: {}\nMessages: {}\n",
        stamp.id,
        stamp.created_at,
        msgs.clone().count()
    );
    let mut used = out.len();
    let mut kept = Vec::new();
    let mut omitted = 0;
    for m in msgs.rev() {
        let text = m["content"].as_str().unwrap_or_default().trim();
        let calls = m["tool_calls"].as_array().map(Vec::as_slice).unwrap_or(&[]);
        if text.is_empty() && calls.is_empty() {
            continue;
        }
        if omitted != 0 {
            omitted += 1;
            continue;
        }
        let role = match m["role"].as_str().unwrap_or_default() {
            "tool" => "tool result",
            other => other,
        };
        let mut block = format!("## {role}");
        if !text.is_empty() {
            write!(block, "\n{text}").unwrap();
        }
        for call in calls {
            let f = &call["function"];
            let name = f["name"]
                .as_str()
                .filter(|s| !s.is_empty())
                .unwrap_or("(unknown)");
            write!(block, "\n→ called tool `{name}`").unwrap();
            if let Some(args) = f["arguments"]
                .as_str()
                .map(str::trim)
                .filter(|s| !s.is_empty())
            {
                write!(block, " with {args}").unwrap();
            }
        }
        // RenderTranscript always keeps the newest block, even if it alone exceeds the budget.
        used += block.len() + 1;
        if !kept.is_empty() && used > TRANSCRIPT_BUDGET {
            omitted = 1;
        } else {
            kept.push(block);
        }
    }
    if omitted != 0 {
        writeln!(out, "\n[{omitted} earlier message(s) omitted to fit the context budget; showing the most recent {}]", kept.len()).unwrap();
    }
    for block in kept.into_iter().rev() {
        writeln!(out, "\n{block}").unwrap();
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn stamp(id: &str) -> Stamp {
        Stamp {
            id: id.into(),
            created_at: "2026-11-19T12:00:00Z".into(),
        }
    }

    #[test]
    fn renders_only_retained_conversation_text_and_tool_calls() {
        let mut a = Archive::new(stamp("one"));
        let history = [
            json!({"role":"system", "content":"private instruction"}),
            json!({"role":"user","content":" drop ".repeat(5000)}),
            json!({"role":"assistant","reasoning_content":"private reasoning", "tool_calls":[{"function":{"name":"calculate","arguments":" {} "}}]}),
            json!({"role":"tool","content":" 42 "}),
        ];
        a.restart(stamp("two"), &history).unwrap();
        assert_eq!(
            a.list("{}").unwrap(),
            r#"[{"id":"one","title":"","created_at":"2026-11-19T12:00:00Z","num_messages":4}]"#
        );
        let expected = "# Session one — (untitled)\nCreated: 2026-11-19T12:00:00Z\nMessages: 3\n\n[1 earlier message(s) omitted to fit the context budget; showing the most recent 2]\n\n## assistant\n→ called tool `calculate` with {}\n\n## tool result\n42\n";
        for id in ["one", " -2 "] {
            assert_eq!(
                a.read(&json!({"session_id":id}).to_string()).unwrap(),
                expected
            );
        }
        for id in ["-1", "two"] {
            assert!(a
                .read(&json!({"session_id":id}).to_string())
                .unwrap_err()
                .contains("current session"));
        }
    }

    #[test]
    fn newest_first_limits_and_empty_restart() {
        let mut a = Archive::new(stamp("unused"));
        a.restart(stamp("0"), &[]).unwrap();
        for i in 1..=102 {
            a.restart(
                stamp(&i.to_string()),
                &[json!({"role":"user", "content":"hello"})],
            )
            .unwrap();
        }
        for (args, count) in [
            ("", 20),
            ("{\"limit\":null}", 20),
            ("{\"LIMIT\":1}", 1),
            ("{\"limit\":-9}", 20),
            ("{\"limit\":900}", 100),
        ] {
            let result: Vec<Value> = serde_json::from_str(&a.list(args).unwrap()).unwrap();
            assert_eq!(result.len(), count);
            assert_eq!(result[0]["id"], "101");
        }
        assert_eq!(
            a.read(r#"{"session_id":"-104"}"#).unwrap_err(),
            "cannot resolve session \"-104\": session offset 104 out of range (have 103 sessions)"
        );
    }
}
