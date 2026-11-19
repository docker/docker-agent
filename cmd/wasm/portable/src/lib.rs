mod calculator;
mod compaction;
mod config;
mod discovery;
mod go_json;
mod hooks;
mod media;
mod model_picker;
mod permission;
mod providers;
mod responses;
mod session_context;
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
        #[serde(default)]
        auto_approve: bool,
        history: Vec<Value>,
        stamp: session_context::Stamp,
    },
    Begin {
        session: u32,
        prompt: String,
        id: String,
    },
    Prepare {
        session: u32,
        environment: Option<String>,
        date: Option<String>,
    },
    Chunk {
        session: u32,
        chunk: Value,
    },
    End {
        session: u32,
    },
    RegisterTools {
        session: u32,
        sets: Vec<turn::HostSet>,
    },
    Confirm {
        session: u32,
        token: String,
        approved: bool,
        #[serde(default)]
        remember: bool,
        reason: String,
    },
    HostResult {
        session: u32,
        token: String,
        output: String,
        is_error: bool,
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
        stamp: session_context::Stamp,
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
    chatgpt: bool,
    responses: Option<responses::Stream>,
}

impl Session {
    fn request(&mut self, body: Value) -> Result<Value, String> {
        if body.is_null() {
            return Ok(body);
        }
        if let Some(stream) = &mut self.responses {
            *stream = responses::Stream::default();
            let mut request = responses::request(body)?;
            if self.chatgpt {
                providers::apply_chatgpt_policy(&mut request);
            }
            Ok(request)
        } else {
            Ok(body)
        }
    }

    fn next(&mut self, mut next: Value) -> Result<Value, String> {
        if let Some(body) = next.get_mut("request") {
            *body = self.request(body.take())?;
        }
        Ok(next)
    }

    fn chunk(&mut self, chunk: Value) -> Result<Value, String> {
        let mut events = Vec::new();
        if let Some(stream) = &mut self.responses {
            for c in stream.push(chunk)? {
                events.extend(self.conversation.chunk(c)?);
            }
        } else {
            events = self.conversation.chunk(chunk)?;
        }
        Ok(Value::Array(events))
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
                auto_approve,
                history,
                stamp,
            } => {
                let spec = config::select(&yaml, &agent, &env)?;
                let history = media::history(
                    history,
                    spec.capabilities,
                    spec.model_params["model"].as_str().expect("selected model"),
                    spec.responses,
                )?;
                let toolsets = json!(spec.toolsets);
                let archive = spec
                    .toolsets
                    .iter()
                    .any(|ts| ts.kind == "session_context")
                    .then(|| session_context::Archive::new(stamp));
                let mut conv = Conversation::configured(
                    spec.model_params,
                    spec.instruction,
                    turn::ToolProfile {
                        specs: spec.toolsets,
                        policy: spec.policy.with_auto_approve(auto_approve),
                    },
                    spec.merge_messages,
                    history,
                    spec.picker,
                    archive,
                )?;
                conv.compaction.policy = spec.compaction;
                let dynamic = json!({"addDate":spec.hooks.add_date,"addEnvironmentInfo":spec.hooks.add_environment_info});
                conv.hooks = spec.hooks;
                self.next_id = self
                    .next_id
                    .checked_add(1)
                    .ok_or("session ID space exhausted")?;
                self.sessions.insert(
                    self.next_id,
                    Session {
                        conversation: conv,
                        chatgpt: spec.transport.chatgpt,
                        responses: spec.responses.then(responses::Stream::default),
                    },
                );
                Ok(
                    json!({"session": self.next_id, "dynamic":dynamic,"transport": spec.transport, "responses":spec.responses,"toolsets":toolsets}),
                )
            }
            Command::Begin {
                session,
                prompt,
                id,
            } => {
                let s = self.session(session)?;
                let request = s.conversation.begin(&prompt, &id)?;
                s.request(request)
            }
            Command::Prepare {
                session,
                environment,
                date,
            } => {
                let s = self.session(session)?;
                let body = s.conversation.prepare(environment, date)?;
                s.request(body)
            }
            Command::Chunk { session, chunk } => self.session(session)?.chunk(chunk),
            Command::End { session } => {
                let s = self.session(session)?;
                let next = s.conversation.end()?;
                s.next(next)
            }
            Command::RegisterTools { session, sets } => {
                self.session(session)?.conversation.configure_tools(&sets)?;
                Ok(Value::Null)
            }
            Command::Confirm {
                session,
                token,
                approved,
                remember,
                reason,
            } => {
                let s = self.session(session)?;
                let next = s
                    .conversation
                    .confirm(&token, approved, remember, &reason)?;
                s.next(next)
            }
            Command::HostResult {
                session,
                token,
                output,
                is_error,
            } => {
                let s = self.session(session)?;
                let n = s.conversation.host_result(&token, output, is_error)?;
                s.next(n)
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
            Command::Restart { session, stamp } => {
                self.session(session)?.conversation.restart(stamp)?;
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
