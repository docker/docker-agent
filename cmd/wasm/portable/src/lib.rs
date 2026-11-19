mod calculator;
mod config;
mod fallback;
mod providers;
mod responses;
mod todo;
mod turn;
mod wire;

use serde::Deserialize;
use serde_json::{json, Value};
use std::{cell::RefCell, collections::BTreeMap};
use turn::Conversation;

#[derive(Deserialize)]
#[serde(tag = "op", rename_all = "camelCase")]
enum Command {
    Parse {
        yaml: String,
    },
    List {
        yaml: String,
    },
    Open {
        yaml: String,
        agent: String,
        env: BTreeMap<String, String>,
        history: Vec<Value>,
    },
    Begin {
        session: u32,
        now: f64,
        prompt: String,
        id: String,
    },
    Chunk {
        session: u32,
        chunk: Value,
    },
    Failure {
        session: u32,
        reason: String,
        status: Option<u16>,
    },
    End {
        session: u32,
        now: f64,
    },
    AnswerElicitation {
        session: u32,
        id: String,
        action: String,
        content: Option<serde_json::Map<String, Value>>,
    },
    Cancel {
        session: u32,
    },
    Restart {
        session: u32,
    },
    Close {
        session: u32,
    },
}

#[derive(Default)]
struct Core {
    sessions: BTreeMap<u32, Session>,
    next_id: u32,
}

struct Session {
    conversation: Conversation,
    models: Vec<config::Target>,
    policy: fallback::Policy,
    responses: Option<responses::Stream>,
    replayable: bool,
}

impl Session {
    fn request(&mut self, mut body: Value) -> Result<Value, String> {
        let index = self.policy.current;
        let target = &self.models[index];
        body.as_object_mut()
            .expect("model request")
            .extend(target.model_params.clone());
        if target.merge_messages {
            wire::merge_text_messages(&mut body);
        }
        self.responses = target.responses.then(responses::Stream::default);
        if target.responses {
            if self.models.len() > 1 {
                fallback::check_responses_history(&body)?;
            }
            body = responses::request(body)?;
        }
        Ok(json!({"target":index, "body":body}))
    }

    fn next(&mut self, mut next: Value) -> Result<Value, String> {
        if let Some(body) = next.get_mut("request") {
            *body = self.request(body.take())?;
        }
        Ok(next)
    }

    fn chunk(&mut self, chunk: Value) -> Result<Value, String> {
        let failure = if self.responses.is_some() {
            responses::failure(&chunk)
        } else {
            chunk.get("error").filter(|e| !e.is_null()).map(|e| {
                (
                    e,
                    format!("error receiving from stream: received error while streaming: {e}"),
                )
            })
        };
        if let Some((error, reason)) = failure {
            self.replayable &= chunk["response"]["output"]
                .as_array()
                .is_none_or(Vec::is_empty)
                && chunk["choices"].as_array().is_none_or(Vec::is_empty);
            let transient = fallback::retryable(fallback::status(error), &reason);
            return self.failure(reason, transient);
        }
        self.replayable &= fallback::prelude(&chunk, self.responses.is_some());
        let mut events = Vec::new();
        if let Some(stream) = &mut self.responses {
            for c in stream.push(chunk)? {
                events.extend(self.conversation.chunk(c)?);
            }
        } else {
            events = self.conversation.chunk(chunk)?;
        }
        Ok(json!({"events":events}))
    }

    fn failure(&mut self, reason: String, transient: bool) -> Result<Value, String> {
        let previous = self.policy.current;
        let decision = if self.replayable {
            self.policy.fail(transient, self.models.len())
        } else {
            fallback::Decision::Stop
        };
        let mut next = match decision {
            fallback::Decision::Retry(delay) => json!({"delay":delay}),
            fallback::Decision::Switch => json!({"event":{"type":"fallback", "attempt":1,
                "from":self.models[previous].id, "to":self.models[self.policy.current].id, "reason":reason}}),
            fallback::Decision::Stop => {
                self.conversation.fail();
                let prefix = if self.models.len() > 1 && self.replayable {
                    "all models failed"
                } else {
                    "model failed"
                };
                return Err(format!("{prefix}: {reason}"));
            }
        };
        next["request"] = self.request(self.conversation.request())?;
        Ok(next)
    }
}

impl Core {
    fn session(&mut self, id: u32) -> Result<&mut Session, String> {
        self.sessions
            .get_mut(&id)
            .ok_or_else(|| "session is closed".into())
    }

    fn run(&mut self, cmd: Command) -> Result<Value, String> {
        match cmd {
            Command::Parse { yaml } => config::parse(&yaml),
            Command::List { yaml } => config::list(&yaml),
            Command::Open {
                yaml,
                agent,
                env,
                history,
            } => {
                let spec = config::select(&yaml, &agent, &env)?;
                let conv = Conversation::new(spec.instruction, &spec.toolsets, history)?;
                self.next_id = self
                    .next_id
                    .checked_add(1)
                    .ok_or("session ID space exhausted")?;
                let targets: Vec<_> = spec.models.iter().map(|t| json!({"endpoint":t.endpoint, "token":t.token, "responses":t.responses, "keyBase":t.key_base})).collect();
                self.sessions.insert(
                    self.next_id,
                    Session {
                        conversation: conv,
                        models: spec.models,
                        policy: spec.policy,
                        responses: None,
                        replayable: false,
                    },
                );
                Ok(json!({"session": self.next_id, "targets":targets}))
            }
            Command::Begin {
                session,
                prompt,
                id,
                now,
            } => {
                let s = self.session(session)?;
                let request = s.conversation.begin(&prompt, &id)?;
                s.policy.begin(now);
                s.replayable = true;
                s.request(request)
            }
            Command::Chunk { session, chunk } => self.session(session)?.chunk(chunk),
            Command::Failure {
                session,
                reason,
                status,
            } => {
                let transient = status.is_none() || fallback::retryable(status, &reason);
                self.session(session)?.failure(reason, transient)
            }
            Command::End { session, now } => {
                let s = self.session(session)?;
                let next = s.conversation.end()?;
                s.replayable = false; // Never restart a turn after executing its tools.
                s.policy.success(now);
                s.next(next)
            }
            Command::AnswerElicitation {
                session,
                id,
                action,
                content,
            } => {
                let s = self.session(session)?;
                let next = s.conversation.answer(&id, &action, content)?;
                s.next(next)
            }
            Command::Cancel { session } => {
                self.session(session)?.conversation.cancel();
                Ok(Value::Null)
            }
            Command::Restart { session } => {
                self.session(session)?.conversation.restart();
                Ok(Value::Null)
            }
            Command::Close { session } => {
                self.sessions.remove(&session);
                Ok(Value::Null)
            }
        }
    }
}

// The host copies one UTF-8 JSON command in and its reply out synchronously.
// No borrowed pointers survive dispatch or callback/re-entry into the module.
thread_local! {
    static CORE: RefCell<Core> = RefCell::new(Core::default());
    static BUFFER: RefCell<Vec<u8>> = const { RefCell::new(Vec::new()) };
}

#[no_mangle]
pub extern "C" fn input(len: u32) -> *mut u8 {
    BUFFER.with_borrow_mut(|buf| {
        buf.resize(len as usize, 0);
        buf.as_mut_ptr()
    })
}

#[no_mangle]
pub extern "C" fn dispatch() -> u32 {
    BUFFER.with_borrow_mut(|buf| {
        let result = serde_json::from_slice(buf)
            .map_err(|e| e.to_string())
            .and_then(|cmd| CORE.with_borrow_mut(|core| core.run(cmd)));
        let reply = match result {
            Ok(value) => json!({"ok": value}),
            Err(error) => json!({"error": error}),
        };
        buf.clear();
        serde_json::to_writer(&mut *buf, &reply).expect("writing JSON to Vec");
        buf.len() as u32
    })
}

#[no_mangle]
pub extern "C" fn output() -> *const u8 {
    BUFFER.with_borrow(|buf| buf.as_ptr())
}
