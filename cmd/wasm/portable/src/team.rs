//! Local agent switches. Transfer frames own independent histories; handoffs keep the current frame.
use crate::{
    fallback, media, responses, session_context,
    turn::{Conversation, Profile},
};
use serde::Deserialize;
use serde_json::{json, Value};
use std::{cell::RefCell, collections::BTreeMap, rc::Rc};

pub fn prompt(tool: &str, roster: &str, ids: &str) -> String {
    if tool == "transfer_task" {
        format!("You are a multi-agent system, make sure to answer the user query in the most helpful way possible. You have access to these sub-agents:\n{roster}\nIMPORTANT: You can ONLY transfer tasks to the agents listed above using their ID. The valid agent names are: {ids}. You MUST NOT attempt to transfer to any other agent IDs - doing so will cause system errors.\n\nIf you are the best to answer the question according to your description, you can answer it.\n\nIf another agent is better for answering the question according to its description, call `transfer_task` function to transfer the question to that agent using the agent's ID. When transferring, do not generate any text other than the function call.\n\nWhen the task involves files, always include their absolute paths in the `task` description (never just bare filenames). Sub-agents start in a fresh session and do not see the conversation history or files attached by the user, so a non-absolute path may resolve to the wrong file or force the sub-agent to scan the filesystem.\n\n")
    } else {
        format!("You are part of a multi-agent team. Your goal is to answer the user query in the most helpful way possible.\n\nAvailable agents in your team:\n{roster}\nYou can hand off the conversation to any of these agents at any time by using the `handoff` function with their ID. The valid agent IDs are: {ids}.\n\nWhen to hand off:\n- If another agent's description indicates they are better suited for the current task or question\n- If the user explicitly asks for a specific agent\n- If you need specialized capabilities that another agent provides\n\nIf you are the best agent to handle the current request based on your capabilities, respond directly. When handing off to another agent, only handoff without talking about the handoff.")
    }
}

fn handoff_note(from: &str) -> String {
    format!("The agent {from} handed off the conversation to you. Your available handoff agents and tools are specified in the system messages that follow. Only use those capabilities - do not attempt to use tools or hand off to agents that you see in the conversation history from previous agents, as those were available to different agents with different capabilities. Look at the conversation history for context, but only use the handoff agents and tools that are listed in your system messages below. Complete your part of the task and hand off to the next appropriate agent in your workflow (if any are available to you), or respond directly to the user if you are the final agent.")
}

const MAX_DEPTH: usize = 10;
const MAX_SWITCHES: usize = 32;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Transfer {
    agent: String,
    task: String,
    #[serde(default)]
    expected_output: String,
}

impl Transfer {
    fn system_message(&self) -> Value {
        let mut text = format!("You are a member of a team of agents. Your goal is to complete the following task:\n\n<task>\n{}\n</task>", self.task);
        if !self.expected_output.is_empty() {
            text.push_str(&format!(
                "\n\n<expected_output>\n{}\n</expected_output>",
                self.expected_output
            ));
        }
        text.push_str("\n\nIf the task references files, treat any absolute paths in <task> as authoritative and use them as-is. If a referenced file is given by name only (e.g. \"foo.go\"), do not guess: search the workspace or ask the calling agent for the absolute path before reading or modifying the file.");
        json!({"role":"system", "content":text})
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Handoff {
    agent: String,
}

pub enum Switch {
    Transfer(Transfer),
    Handoff(Handoff),
}

impl Switch {
    pub fn parse(name: &str, args: &str) -> Result<Self, String> {
        if args.len() > 64 * 1024 || !args.trim_start().starts_with('{') {
            return Err(format!("{name} expects a JSON object of at most 64 KiB"));
        }
        match name {
            "handoff" => serde_json::from_str(args).map(Self::Handoff),
            "transfer_task" => serde_json::from_str(args).map(Self::Transfer),
            _ => return Err(format!("unknown team tool {name:?}")),
        }
        .map_err(|e| format!("invalid {name} arguments: {e}"))
    }
}

pub struct Session {
    pub conversation: Conversation,
    agents: BTreeMap<String, Rc<Profile>>,
    /// A suspended caller always has exactly one unresolved transfer tool message.
    parents: Vec<Conversation>,
    responses: Option<responses::Stream>,
    replayable: bool,
    switches: usize,
    turn_id: String,
}

impl Session {
    pub fn open(
        yaml: &str,
        agent: &str,
        env: &BTreeMap<String, String>,
        history: Vec<Value>,
        stamp: session_context::Stamp,
    ) -> Result<Self, String> {
        let (entry, specs) = crate::config::team(yaml, agent, env)?;
        let shared = Rc::new(RefCell::new(crate::todo::Todos::default()));
        let mut agents = BTreeMap::new();
        for spec in specs {
            let todos = if spec.shared_todo {
                Rc::clone(&shared)
            } else {
                Default::default()
            };
            agents.insert(spec.name.clone(), Rc::new(Profile::new(spec, todos)?));
        }
        let profile = if !agents[&entry].spec.team_prompts.is_empty() {
            agents[&entry].clone()
        } else {
            agents.remove(&entry).unwrap()
        };
        let context = profile
            .spec
            .toolsets
            .iter()
            .any(|ts| ts.kind == "session_context");
        let primary = &profile.spec.models[0];
        let routing = !agents.is_empty() || profile.spec.models.len() > 1;
        if routing
            && history
                .iter()
                .any(|m| m["multi_content"].as_array().is_some_and(|p| !p.is_empty()))
        {
            return Err("cannot replay multipart history across model targets".into());
        }
        if routing
            && primary.responses
            && history
                .iter()
                .any(|m| !m["name"].is_null() || !m["reasoning_content"].is_null())
        {
            return Err("cannot replay named or reasoning history with Responses".into());
        }
        let history = media::history(
            history,
            primary.capabilities,
            primary.model(),
            primary.responses,
            routing,
        )?;
        let mut conversation = Conversation::open(profile, history);
        conversation.archive = context.then(|| session_context::Archive::new(stamp));
        Ok(Self {
            conversation,
            agents,
            parents: vec![],
            responses: None,
            replayable: false,
            switches: 0,
            turn_id: String::new(),
        })
    }

    pub fn targets(&self) -> Value {
        let profiles = self
            .agents
            .values()
            .chain(std::iter::once(&self.conversation.profile).filter(|_| self.agents.is_empty()));
        Value::Object(
            profiles
                .map(|p| {
                    let targets = p
                        .spec
                        .models
                        .iter()
                        .map(|t| json!({"transport": t.transport, "keyBase": t.key_base}))
                        .collect();
                    (p.spec.name.clone(), Value::Array(targets))
                })
                .collect(),
        )
    }

    pub fn register(&mut self, sets: &[crate::turn::HostSet]) -> Result<(), String> {
        if !self.agents.is_empty() {
            return Err("host toolsets are not supported in local teams".into());
        }
        self.conversation.register(sets)
    }

    pub fn begin(&mut self, prompt: &str, id: &str, now: f64) -> Result<Value, String> {
        self.conversation.attempt = self.conversation.profile.spec.policy.begin(now);
        let body = self.conversation.begin(prompt, id)?;
        self.replayable = !self.conversation.summarizing()
            && self.conversation.profile.spec.hooks.max_calls.is_none();
        self.switches = 0;
        self.turn_id = id.into();
        self.request(body)
    }

    pub fn request(&mut self, mut body: Value) -> Result<Value, String> {
        let spec = &self.conversation.profile.spec;
        if body.is_null() {
            return Ok(
                json!({"dynamic": {"addEnvironmentInfo":spec.hooks.add_environment_info,"addDate":spec.hooks.add_date}}),
            );
        }
        let index = self.conversation.attempt.current;
        let target = &spec.models[index];
        self.responses = target.responses.then(responses::Stream::default);
        if target.responses {
            if spec.models.len() > 1 || !self.agents.is_empty() {
                fallback::check_responses_history(&body)?;
            }
            body = responses::request(body)?;
            if target.transport.chatgpt {
                crate::providers::apply_chatgpt_policy(&mut body);
            }
        }
        Ok(json!({"agent":spec.name, "target": index, "responses":target.responses, "body":body}))
    }

    pub fn chunk(&mut self, chunk: Value) -> Result<Value, String> {
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
        let mut events;
        if let Some(stream) = &mut self.responses {
            events = Vec::new();
            for c in stream.push(chunk)? {
                events.extend(self.conversation.chunk(c)?);
            }
        } else {
            events = self.conversation.chunk(chunk)?;
        }
        Ok(json!({"events":events}))
    }

    pub fn failure(&mut self, reason: String, transient: bool) -> Result<Value, String> {
        let reason = self
            .conversation
            .target()
            .transport
            .safe_diagnostic(reason)?;
        let spec = &self.conversation.profile.spec;
        let attempt = &mut self.conversation.attempt;
        let previous = attempt.current;
        let decision = if self.replayable {
            attempt.fail(&spec.policy, transient, spec.models.len())
        } else {
            fallback::Decision::Stop
        };
        let mut next = match decision {
            fallback::Decision::Retry(delay) => json!({"delay":delay}),
            fallback::Decision::Switch => json!({"event":{"type":"fallback", "attempt":1,
                "from":spec.models[previous].id, "to":spec.models[attempt.current].id, "reason":reason}}),
            fallback::Decision::Stop => {
                let prefix = if spec.models.len() > 1 && self.replayable {
                    "all models failed"
                } else {
                    "model failed"
                };
                // Close suspended transfer calls without copying a child's provider diagnostics
                // into its caller's history or rolling back already-completed caller tools.
                while let Some(mut parent) = self.parents.pop() {
                    parent.complete_switch(
                        "transfer_task",
                        "Delegated task failed".into(),
                        true,
                    )?;
                    self.conversation = parent;
                }
                self.conversation.fail();
                return Err(format!("{prefix}: {reason}"));
            }
        };
        next["request"] = self.request(self.conversation.request()?)?;
        Ok(next)
    }

    pub fn end(&mut self, now: f64) -> Result<Value, String> {
        let mut next = self.conversation.end()?;
        self.replayable = false; // Follow-ups must not replay a completed tool or parent transfer.
        self.conversation
            .attempt
            .success(&self.conversation.profile.spec.policy, now);
        if let Some(switch) = self.conversation.switch.take() {
            let events = next["events"].as_array_mut().unwrap();
            next["request"] = self.route(switch, events, now)?;
        } else if next.get("result").is_some() && !self.parents.is_empty() {
            let output = next["result"]["message"]["content"]
                .as_str()
                .expect("assistant text")
                .trim()
                .to_owned();
            let usage = &next["result"]["usage"];
            let child = std::mem::replace(&mut self.conversation, self.parents.pop().unwrap());
            self.conversation.include_usage(usage);
            let events = next["events"].as_array_mut().unwrap();
            events.retain(|e| e["type"] != "finish");
            events.push(handoff(
                &child.profile.spec.name,
                &self.conversation.profile.spec.name,
            ));
            events.push(
                self.conversation
                    .complete_switch("transfer_task", output, false)?,
            );
            next.as_object_mut().unwrap().remove("result");
            next["request"] = self.conversation.request()?;
        }
        self.next(next)
    }

    fn route(
        &mut self,
        switch: Switch,
        events: &mut Vec<Value>,
        now: f64,
    ) -> Result<Value, String> {
        let caller = &self.conversation.profile.spec;
        let from = &caller.name;
        let (target, allowed, name, action, list) = match &switch {
            Switch::Handoff(Handoff { agent }) => (
                agent,
                &caller.handoffs,
                "handoff",
                "hand off to",
                "handoffs list",
            ),
            Switch::Transfer(Transfer { agent, .. }) => (
                agent,
                &caller.sub_agents,
                "transfer_task",
                "transfer task to",
                "sub-agents list",
            ),
        };
        let denial = if !allowed.contains(target) {
            Some(format!("Agent {from} cannot {action} {target}: target agent not in {list}. Available agent IDs are: {}", allowed.join(", ")))
        } else if name == "transfer_task"
            && (target == from || self.parents.iter().any(|p| &p.profile.spec.name == target))
        {
            let path = self
                .parents
                .iter()
                .map(|p| p.profile.spec.name.as_str())
                .chain([from.as_str(), target])
                .collect::<Vec<_>>()
                .join(" -> ");
            Some(format!("delegation cycle detected: {path}. Agent {target} is already part of the active delegation chain; complete the task directly or delegate to a different agent."))
        } else if name == "transfer_task" && self.parents.len() >= MAX_DEPTH {
            Some(format!(
                "delegation depth limit exceeded: maximum depth is {MAX_DEPTH}"
            ))
        } else {
            None
        };
        if let Some(reason) = denial {
            events.push(self.conversation.complete_switch(name, reason, true)?);
            return self.conversation.request();
        }
        if self.switches == MAX_SWITCHES {
            return Err("team switch limit reached".into());
        }
        self.switches += 1;
        let profile = self.agents[target].clone();
        let switched = handoff(from, target);
        match switch {
            Switch::Handoff(Handoff { agent }) => {
                let note = handoff_note(from);
                let changed = &agent != from;
                events.push(self.conversation.complete_switch(name, note, false)?);
                if changed {
                    events.push(switched);
                    self.conversation.attempt = profile.spec.policy.begin(now);
                }
                self.conversation.profile = profile;
                self.conversation.request()
            }
            Switch::Transfer(task) => {
                let mut child = Conversation::open(profile, vec![task.system_message()]);
                child.attempt = child.profile.spec.policy.begin(now);
                self.replayable = true; // Only the new child request can be retried.
                let body = child.begin(
                    "Please proceed.",
                    &format!("{}:child:{}", self.turn_id, self.switches),
                )?;
                self.parents
                    .push(std::mem::replace(&mut self.conversation, child));
                events.push(switched);
                Ok(body)
            }
        }
    }

    pub fn next(&mut self, mut next: Value) -> Result<Value, String> {
        if let Some(body) = next.get_mut("request") {
            *body = self.request(body.take())?;
        }
        Ok(next)
    }

    pub fn cancel(&mut self) {
        if let Some(root) = self.parents.drain(..).next() {
            self.conversation = root;
        }
        self.conversation.cancel();
        self.responses = None;
    }
}

fn handoff(from: &str, to: &str) -> Value {
    json!({"type":"handoff", "from":from, "to":to})
}
