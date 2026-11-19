mod config;
mod turn;

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
        prompt: String,
        id: String,
    },
    Chunk {
        session: u32,
        chunk: Value,
    },
    End {
        session: u32,
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
    Close {
        session: u32,
    },
}

#[derive(Default)]
struct Core {
    sessions: BTreeMap<u32, Conversation>,
    next_id: u32,
}

impl Core {
    fn session(&mut self, id: u32) -> Result<&mut Conversation, String> {
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
                let conv = Conversation::new(
                    spec.model,
                    spec.instruction,
                    &spec.toolsets,
                    spec.merge_instructions,
                    history,
                )?;
                self.next_id = self
                    .next_id
                    .checked_add(1)
                    .ok_or("session ID space exhausted")?;
                self.sessions.insert(self.next_id, conv);
                Ok(json!({"session": self.next_id, "endpoint": spec.endpoint, "token": spec.token}))
            }
            Command::Begin {
                session,
                prompt,
                id,
            } => self.session(session)?.begin(&prompt, &id),
            Command::Chunk { session, chunk } => {
                Ok(Value::Array(self.session(session)?.chunk(chunk)?))
            }
            Command::End { session } => self.session(session)?.end(),
            Command::AnswerElicitation {
                session,
                id,
                action,
                content,
            } => self.session(session)?.answer(&id, &action, content),
            Command::Cancel { session } => {
                self.session(session)?.cancel();
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
