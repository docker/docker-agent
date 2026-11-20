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

#[derive(Default)]
pub struct Transfer {
    agent: String,
    task: String,
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

pub struct Handoff {
    agent: String,
}

pub enum Switch {
    Transfer(Transfer),
    Handoff(Handoff),
    Forced(String),
}

impl Switch {
    /// Decodes like the Go handlers; `Err` is their model-facing complaint.
    pub fn parse(name: &str, args: &str) -> Result<Self, String> {
        use crate::go_json::{
            field_matches, field_path, fields, normalize_json, object, read_string,
        };
        let handoff = name == "handoff";
        // transfer_task decodes into an anonymous struct, which Go prints in full.
        let ty = if handoff {
            "types.Args"
        } else {
            r#"struct { Agent string "json:\"agent\""; Task string "json:\"task\""; ExpectedOutput string "json:\"expected_output\"" }"#
        };
        let mut to = Transfer::default();
        let mut decode = || -> Result<(), String> {
            let args = normalize_json(args)?;
            let Some(obj) = object(&args, ty)? else {
                return Ok(());
            };
            fields(obj, |key, val| {
                let field = if field_matches(&key, "agent") {
                    &mut to.agent
                } else if !handoff && field_matches(&key, "task") {
                    &mut to.task
                } else if !handoff && field_matches(&key, "expected_output") {
                    &mut to.expected_output
                } else {
                    return Ok(());
                };
                read_string(val, field, &field_path(ty, &key))
            })
        };
        decode().map_err(|e| format!("Error calling tool: invalid arguments: {e}"))?;
        Ok(if handoff {
            Self::Handoff(Handoff { agent: to.agent })
        } else {
            Self::Transfer(to)
        })
    }
}

pub struct Session {
    pub conversation: Conversation,
    agents: BTreeMap<String, Rc<Profile>>,
    team: bool,
    /// A suspended caller always has exactly one unresolved transfer tool message.
    parents: Vec<Conversation>,
    responses: Option<responses::Stream>,
    replayable: bool,
    summary_attempt: fallback::Attempt,
    switches: usize,
    turn_id: String,
    /// A handoff that stopped at the cap is announced when the next run starts.
    unannounced: Option<Value>,
}

impl Session {
    pub fn open(
        yaml: &str,
        agent: &str,
        env: &BTreeMap<String, String>,
        history: Option<Vec<Value>>,
        stamp: session_context::Stamp,
        auto_approve: bool,
    ) -> Result<Self, String> {
        // Only one-shot chat supplies seed history (which may be empty).
        let (entry, specs, plain_history) =
            crate::config::team(yaml, agent, env, history.is_some())?;
        let history = history.unwrap_or_default();
        let multipart = history
            .iter()
            .any(|m| m["multi_content"].as_array().is_some_and(|p| !p.is_empty()));
        // Projections can erase names and transform attachment text before per-agent redaction.
        if multipart || history.iter().any(|m| !m["name"].is_null()) {
            if specs.iter().any(|s| s.has_schema_instruction()) {
                return Err("Claude schema instructions require unnamed text-only history".into());
            }
            if plain_history {
                return Err(
                    "team output tools and redaction opt-outs require unnamed text-only history"
                        .into(),
                );
            }
        }
        let shared = Rc::new(RefCell::new(crate::todo::Todos::default()));
        let mut agents = BTreeMap::new();
        for mut spec in specs {
            spec.tool_policy = spec.tool_policy.with_auto_approve(auto_approve);
            agents.insert(
                spec.name.clone(),
                Rc::new(Profile::new(spec, Rc::clone(&shared))?),
            );
        }
        let profile = agents[&entry].clone();
        let context = profile
            .spec
            .toolsets
            .iter()
            .any(|ts| ts.kind == "session_context");
        let primary = &profile.spec.models[0];
        let team = !profile.spec.team_prompts.is_empty() || !profile.spec.force_handoff.is_empty();
        let routing = team || profile.spec.routed_summary();
        let routed_media = routing && multipart;
        let seed_estimates_lost = multipart
            || history.iter().any(|m| {
                !m["usage"].is_null()
                    || !m["reasoning_content"].is_null()
                    || !m["tool_definitions"].is_null()
                    || !m["name"].is_null()
                    || (m["role"] != "tool"
                        && m["tool_calls"].is_null()
                        && m["content"].as_str().is_none_or(|s| s.trim().is_empty()))
            });
        if routed_media && primary.compaction.enabled() {
            return Err("multipart history with routed compaction is not supported".into());
        }
        let targets = || agents.values().flat_map(|p| p.spec.targets());
        let reproject =
            routed_media && targets().any(|m| m.media_projection() != primary.media_projection());
        // A handoff can apply any agent's history limits to the seed.
        let limits = agents
            .values()
            .fold(profile.spec.limits, |l, p| l.covering(&p.spec.limits));
        let limited = limits.limits_history();
        let annotated = history
            .iter()
            .any(|m| !m["name"].is_null() || !m["reasoning_content"].is_null());
        if profile.spec.selected_fallback_chain && annotated {
            return Err(
                "model selection with fallback requires unnamed seed history without reasoning"
                    .into(),
            );
        }
        if team && limited && annotated {
            return Err(
                "history limits in a local team require unnamed history without reasoning".into(),
            );
        }
        let checks_responses = primary.responses
            || profile.spec.summary_target().1.responses
            || ((routed_media || limited) && targets().any(|m| m.responses));
        if routing && checks_responses && annotated {
            return Err("cannot replay named or reasoning history with Responses".into());
        }
        limits.check_seed(&history, !profile.spec.tool_models.is_empty())?;
        let source = reproject.then(|| history.clone());
        // Go's chat.Message has no name field.
        let preserve_names = routing && !limited;
        let history = media::history(history, primary.media_projection(), preserve_names)?;
        let mut conversation = Conversation::open(profile, history);
        conversation.set_seed_estimates_lost(seed_estimates_lost);
        if let Some(source) = source {
            conversation.set_routed_seed(source);
        }
        conversation.archive = context.then(|| session_context::Archive::new(stamp));
        Ok(Self {
            conversation,
            agents,
            team,
            parents: vec![],
            responses: None,
            replayable: false,
            summary_attempt: Default::default(),
            switches: 0,
            turn_id: String::new(),
            unannounced: None,
        })
    }

    pub fn profiles(&self) -> Value {
        Value::Object(
            self.agents
                .iter()
                .map(|(name, p)| {
                    let targets: Vec<_> = p
                        .spec
                        .targets()
                        .map(|t| json!({"transport":t.transport,"keyBase":t.key_base}))
                        .collect();
                    (
                        name.clone(),
                        json!({"targets":targets,"toolsets":p.spec.toolsets}),
                    )
                })
                .collect(),
        )
    }

    pub fn active(&mut self, agent: &str) -> Result<&mut Conversation, String> {
        if self.conversation.profile.spec.name != agent {
            return Err("tool operation does not match active agent".into());
        }
        Ok(&mut self.conversation)
    }

    pub fn begin(&mut self, prompt: &str, id: &str, now: f64) -> Result<Value, String> {
        self.conversation.attempt = self.conversation.profile.spec.policy.begin(now);
        let body = self.conversation.begin(prompt, id)?;
        self.replayable = !self.conversation.summarizing()
            && (self.conversation.profile.spec.hooks.max_calls.is_none()
                || self.conversation.profile.spec.separate_model_attempts);
        self.switches = 0;
        self.turn_id = id.into();
        let mut request = self.request(body)?;
        if let Some(event) = self.unannounced.take() {
            request["event"] = event;
        }
        Ok(request)
    }

    pub fn request(&mut self, mut body: Value) -> Result<Value, String> {
        let spec = &self.conversation.profile.spec;
        if body.is_null() {
            let mut next = json!({"dynamic": {"addEnvironmentInfo":self.conversation.add_environment_info(),"addDate":spec.hooks.add_date}});
            if self.conversation.profile.needs_host() {
                next["openTools"] = json!(spec.name);
            }
            return Ok(next);
        }
        let summarizing = self.conversation.summarizing();
        if summarizing {
            self.summary_attempt = Default::default();
        }
        let (index, target) = self.conversation.request_target();
        self.responses = target.responses.then(responses::Stream::default);
        if target.responses {
            if spec.routed_summary() || self.team {
                fallback::check_responses_history(&body)?;
            }
            body = responses::request(body, spec.output_tool.as_ref())?;
            if target.transport.chatgpt {
                crate::providers::apply_chatgpt_policy(&mut body);
            }
        }
        Ok(
            json!({"agent":spec.name, "target": index, "responses":target.responses, "summary":summarizing, "body":body}),
        )
    }

    pub fn chunk(&mut self, mut chunk: Value, now: f64) -> Result<Value, String> {
        let raw_detail = if let Some(raw) = chunk.as_str() {
            #[derive(Deserialize)]
            struct Envelope<'a> {
                #[serde(borrow)]
                error: &'a serde_json::value::RawValue,
            }
            let envelope: Envelope =
                serde_json::from_str(raw).map_err(|_| "invalid stream error record")?;
            let parsed: Value =
                serde_json::from_str(raw).map_err(|_| "invalid stream error record")?;
            let error = &parsed["error"];
            let detail = if error.is_null() {
                ""
            } else {
                error.as_str().unwrap_or_else(|| envelope.error.get())
            };
            let decoded = error
                .as_str()
                .map_or_else(|| error.to_string(), str::to_owned);
            // Escapes in the wire diagnostic must not bypass the credential guard.
            if decoded != detail
                && self
                    .conversation
                    .request_target()
                    .1
                    .transport
                    .safe_diagnostic(decoded.clone())?
                    != decoded
            {
                return Err("provider diagnostic contained credentials".into());
            }
            let reason = format!("received error while streaming: {detail}");
            chunk = parsed;
            Some(reason)
        } else {
            None
        };
        let failure = raw_detail.or_else(|| {
            self.responses
                .as_ref()
                .and_then(|_| responses::failure(&chunk))
        });
        if let Some(detail) = failure {
            let reason = format!("error receiving from stream: {detail}");
            self.replayable &= chunk["response"]["output"]
                .as_array()
                .is_none_or(Vec::is_empty)
                && chunk["choices"].as_array().is_none_or(Vec::is_empty);
            // The Go SDK exposes SSE errors as text: only the rendered diagnostic is classified.
            let status = if self.conversation.summarizing()
                || self.conversation.profile.spec.selected_fallback_chain
                || self.responses.is_some()
            {
                None
            } else {
                fallback::status(&chunk["error"])
            };
            let transient = fallback::retryable(status, &reason);
            return self.failure(reason, None, transient, now);
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

    pub fn transport_failure(
        &mut self,
        status: Option<u16>,
        endpoint: Option<String>,
        reason: Option<String>,
        now: f64,
    ) -> Result<Value, String> {
        let Some(reason) = reason else {
            return self.stop("unsupported HTTP error body".into());
        };
        let (status, reason) = if let Some(code) = status {
            match fallback::http_error(
                code,
                endpoint.as_deref().ok_or("missing model endpoint")?,
                &reason,
            ) {
                Ok(failure) => failure,
                Err(message) => return self.stop(message),
            }
        } else {
            (None, reason)
        };
        // Read failures in an established stream retain their own retry policy.
        let transient = endpoint.is_none() || fallback::retryable(status, &reason);
        self.failure(reason, status, transient, now)
    }

    fn failure(
        &mut self,
        reason: String,
        http_status: Option<u16>,
        transient: bool,
        now: f64,
    ) -> Result<Value, String> {
        let (_, target) = self.conversation.request_target();
        let overflow = fallback::overflow(http_status, &reason)?;
        let reason = target.transport.safe_diagnostic(reason)?;
        let transient = overflow.is_none() && transient;
        if self.conversation.summarizing() {
            if overflow == Some(fallback::Overflow::Tokens) {
                if let Some(body) = self.conversation.summary_request()? {
                    return Ok(json!({"request":self.request(body)?}));
                }
            }
            // The child summarizer has its own default retries and no fallback chain.
            if let fallback::Decision::Retry(delay) =
                self.summary_attempt.fail(&Default::default(), transient, 1)
            {
                self.responses = self
                    .responses
                    .as_ref()
                    .map(|_| responses::Stream::default());
                self.conversation.discard_summary_output();
                return Ok(json!({"retry":true,"delay":delay}));
            }
            let message = format!(
                "run compaction agent: {}",
                fallback::model_error("model failed", overflow, &reason)
            );
            self.conversation
                .skip_summary(overflow.is_some())
                .map_err(|e| format!("{message} ({e})"))?;
            let request = self.next_iteration(now)?;
            return Ok(json!({"request":request, "event":{"type":"error", "message":message}}));
        }
        let spec = &self.conversation.profile.spec;
        let attempt = &mut self.conversation.attempt;
        let previous = attempt.current;
        let decision = if self.replayable {
            // Go loads fallback.retries only when it loads fallback.models.
            let policy = if http_status.is_some() && spec.models.len() == 1 {
                &Default::default()
            } else {
                &spec.policy
            };
            attempt.fail(policy, transient, spec.models.len())
        } else {
            fallback::Decision::Stop
        };
        let current = attempt.current;
        let mut next = match decision {
            fallback::Decision::Retry(delay) => json!({"delay":delay}),
            fallback::Decision::Switch => json!({"event":{"type":"fallback", "attempt":1,
                "from":self.conversation.attempt_model(previous).id, "to":self.conversation.attempt_model(current).id, "reason":reason}}),
            fallback::Decision::Stop => {
                let prefix = if spec.models.len() > 1 && self.replayable {
                    "all models failed"
                } else {
                    "model failed"
                };
                let message = if http_status.is_some() || spec.selected_fallback_chain {
                    fallback::model_error(prefix, overflow, &reason)
                } else {
                    format!("{prefix}: {reason}")
                };
                return self.stop(message);
            }
        };
        let body = self.conversation.request()?;
        let mut request = self.request(body)?;
        if let Some(dynamic) = request.get_mut("dynamic") {
            dynamic["reuse"] = true.into();
        }
        next["request"] = request;
        Ok(next)
    }

    fn stop(&mut self, message: String) -> Result<Value, String> {
        // Keep completed caller tools without copying a child's diagnostics into its history.
        while let Some(mut parent) = self.parents.pop() {
            parent.complete_switch("transfer_task", "Delegated task failed".into(), true)?;
            self.conversation = parent;
        }
        self.conversation.fail();
        Err(message)
    }

    fn begin_model_attempt(&mut self, now: f64) {
        // Neither tools nor the primary summarizer extend a fallback pin.
        self.conversation.attempt = self.conversation.profile.spec.policy.begin(now);
        self.replayable = true;
    }

    pub fn next_iteration(&mut self, now: f64) -> Result<Value, String> {
        self.begin_model_attempt(now);
        let body = self.conversation.request()?;
        self.request(body)
    }

    pub fn end(&mut self, now: f64) -> Result<Value, String> {
        if self.conversation.summarizing() {
            self.begin_model_attempt(now);
            let next = self.conversation.end()?;
            return self.next(next);
        }
        let next = self.conversation.end()?;
        self.replayable = false; // Only an explicit new model attempt may retry.
        self.conversation
            .attempt
            .success(&self.conversation.profile.spec.policy, now);
        self.advance(next, now)
    }

    fn route(&mut self, switch: Switch, mut events: Vec<Value>, now: f64) -> Result<Value, String> {
        let caller = &self.conversation.profile.spec;
        let from = &caller.name;
        if let Switch::Forced(target) = switch {
            if target != caller.force_handoff || self.switches >= MAX_SWITCHES {
                return Err("invalid or excessive forced handoff".into());
            }
            self.switches += 1;
            let profile = self.agents[&target].clone();
            let note=format!("The agent {from} finished its response and the conversation was automatically handed off to you. Your available handoff agents and tools are specified in the system messages that follow. Only use those capabilities - do not attempt to use tools or hand off to agents that you see in the conversation history from previous agents, as those were available to different agents with different capabilities. Look at the conversation history for context, continue the work from where the previous agent stopped, and complete your part of the task.");
            let switched = handoff(from, &target);
            self.conversation.attempt = profile.spec.policy.begin(now);
            self.conversation.forced(profile, note);
            self.announce(switched, &mut events);
            return Ok(self.conversation.resume(events));
        }
        let (target, allowed, name, action, list) = match &switch {
            Switch::Forced(_) => unreachable!(),
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
            return Ok(self.conversation.resume(events));
        }
        if self.switches == MAX_SWITCHES {
            return Err("team switch limit reached".into());
        }
        self.switches += 1;
        let profile = self.agents[target].clone();
        let switched = handoff(from, target);
        match switch {
            Switch::Forced(_) => unreachable!(),
            Switch::Handoff(Handoff { agent }) => {
                let note = handoff_note(from);
                let changed = &agent != from;
                events.push(self.conversation.complete_switch(name, note, false)?);
                if changed {
                    self.conversation.attempt = profile.spec.policy.begin(now);
                }
                self.conversation.hand_off(profile);
                if changed {
                    self.announce(switched, &mut events);
                }
                Ok(self.conversation.resume(events))
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
                Ok(json!({"events":events, "request":body}))
            }
        }
    }

    // Go names the acting agent after its cap check; a stopped root run does so on its next send.
    fn announce(&mut self, switched: Value, events: &mut Vec<Value>) {
        if !self.conversation.at_cap() {
            events.push(switched);
        } else if self.parents.is_empty() {
            self.unannounced = Some(switched);
        }
    }

    pub fn advance(&mut self, mut next: Value, now: f64) -> Result<Value, String> {
        if let Some(switch) = self.conversation.switch.take() {
            let events = std::mem::take(next["events"].as_array_mut().unwrap());
            next = self.route(switch, events, now)?;
        }
        self.next(next)
    }

    pub fn next(&mut self, mut next: Value) -> Result<Value, String> {
        // A child stopped by its cap returns that notice like any other final answer.
        while next.get("result").is_some() && !self.parents.is_empty() {
            let child = std::mem::replace(&mut self.conversation, self.parents.pop().unwrap());
            let reply = child.last_reply();
            self.conversation.include_usage(&next["result"]["usage"]);
            let mut events = std::mem::take(next["events"].as_array_mut().unwrap());
            events.retain(|e| e["type"] != "finish");
            // Go reports the delegate that was called, even if it handed off since.
            events.push(handoff(
                child.opener(),
                &self.conversation.profile.spec.name,
            ));
            events.push(self.conversation.complete_switch(
                "transfer_task",
                reply.clone().unwrap_or_default(),
                false,
            )?);
            self.conversation.delegated(reply);
            next = self.conversation.resume(events);
        }
        if next
            .as_object_mut()
            .unwrap()
            .remove("nextIteration")
            .is_some()
        {
            next["request"] = json!({"nextIteration":true});
        } else if let Some(body) = next.get_mut("request") {
            *body = self.request(body.take())?;
        }
        Ok(next)
    }

    pub fn restart(&mut self, stamp: session_context::Stamp) -> Result<(), String> {
        self.cancel();
        self.unannounced = None;
        self.conversation.restart(stamp)?;
        for profile in self.agents.values() {
            profile.clear_grants();
        }
        Ok(())
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
