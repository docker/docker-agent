mod calculator;
mod compaction;
mod config;
mod discovery;
mod fallback;
mod go_json;
mod hooks;
mod limits;
mod media;
mod model_picker;
mod permission;
mod providers;
mod responses;
mod session_context;
mod team;
mod todo;
mod turn;
mod wire;

use serde::Deserialize;
use serde_json::{json, Value};
use std::{cell::RefCell, collections::BTreeMap};
use team::Session;

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
        now: f64,
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
    Failure {
        session: u32,
        reason: String,
        status: Option<u16>,
    },
    End {
        session: u32,
        now: f64,
    },
    RegisterTools {
        session: u32,
        sets: Vec<turn::HostSet>,
    },
    Confirm {
        now: f64,
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
                let session = Session::open(&yaml, &agent, &env, history, stamp, auto_approve)?;
                self.next_id = self
                    .next_id
                    .checked_add(1)
                    .ok_or("session ID space exhausted")?;
                let handle = json!({
                    "session": self.next_id,
                    "targets": session.targets(),
                    "toolsets": session.conversation.profile.spec.toolsets,
                });
                self.sessions.insert(self.next_id, session);
                Ok(handle)
            }
            Command::Begin {
                session,
                prompt,
                id,
                now,
            } => self.session(session)?.begin(&prompt, &id, now),
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
            Command::Failure {
                session,
                reason,
                status,
            } => {
                let transient = status.is_none() || fallback::retryable(status, &reason);
                self.session(session)?.failure(reason, transient)
            }
            Command::End { session, now } => self.session(session)?.end(now),
            Command::RegisterTools { session, sets } => {
                self.session(session)?.register(&sets)?;
                Ok(Value::Null)
            }
            Command::Confirm {
                now,
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
                s.confirmed(next, now)
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
                self.session(session)?.cancel();
                Ok(Value::Null)
            }
            Command::Restart { session, stamp } => {
                let s = self.session(session)?;
                s.cancel();
                s.conversation.restart(stamp)?;
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
