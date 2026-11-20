# Model request parity

`requestProvider_*` runs a tool and a follow-up turn for every OpenAI entry in
Go's provider catalog and its four legacy names. The fetch shim checks the
origin, path and query before choosing a scripted reply. It compares all
browser-visible headers (including SDK retries), correlates generated session
IDs, and compares the parsed JSON bodies, events and results. No real provider
or credential is needed.

| Config | On the wire |
| --- | --- |
| `provider/model` or named model | Builtins use catalog endpoints and key names. `openai`, `github-copilot` and `opencode` auto-select Responses for GPT-4.1, GPT-5+, Codex and o-series; `chatgpt` always uses Responses. Other aliases default to Chat. |
| Custom provider | Model fields override provider fields. Model `provider_opts.api_type` precedes provider opts, then the provider's `api_type`. With no override, a custom provider chooses its API **before** model-name expansion. Merely using the factory name `openai_responses` as the model's `provider` does not force Responses. |
| Sampling | Chat sends temperature, top-p and penalties, including explicit zero. Responses omits penalties. A positive cap is `max_tokens` on older Chat models, `max_completion_tokens` on newer ones and `max_output_tokens` on Responses. Nonpositive caps suppress the inherited cap. `parallel_tool_calls` requires tools. Both protocols accept `provider_opts.service_tier`. |
| GPT-5.6+ cache markers | On a genuine OpenAI model, mark the last invariant instruction and the last transient prompt, not conversation history. Do not merge those instructions on Chat. `openai` with a nondefault base URL does not get these markers. |
| Reasoning effort | `thinking_budget` accepts `minimal`, `low`, `medium`, `high`, `xhigh`, `max`. Chat uses `reasoning_effort`; Responses uses `reasoning.effort` with `summary: detailed` (`auto` on ChatGPT). No budget seeds `medium` only for a configured o-series name. GPT-5-chat and non-reasoning models ignore the budget. |
| Disabled reasoning | `0` or `none` sends `low` unless the vendor and model have a real `none` switch (GPT-5.6 variants and GPT-6 Sol/Luna). Omit the Responses summary. Raise a configured cap below 256 to 256, but never introduce a cap when none was set. ChatGPT strips output caps and sampling. |
| Tools + effort | Forced Chat on GPT-5.4+ drops effort and skips the cap floor **when tools are actually sent**. GPT-6 Sol/Luna on a genuine vendor accept tools with explicit `none`. |
| Stream and replay | Both protocols emit plain reasoning deltas. Chat replays them in `reasoning_content` unless `provider_opts.replay_reasoning: false`. Stateless Responses sends function calls/results and visible assistant text, not reasoning or encrypted items. Effort alone never requests encryption. |

## Admission boundaries

- Numeric (nonzero) and adaptive budgets are accepted on non-reasoning models,
  which ignore them. On reasoning models they stay rejected: Go fails the request
  without tools even when forced Chat can drop the budget on a tool call. A
  summarizer or filtered tool catalog need not send any tools. Fractional token
  counts, oversized positive caps beyond JS's safe integer range and object
  budgets are not coerced.
- `preserve_reasoning: false` is supported. `true` is inert on Chat and on
  third-party Responses endpoints. First-party Responses and ChatGPT reject it:
  Go requests `reasoning.encrypted_content` and can replay ordered raw items,
  with IDs and phase, only on the matching provider/model and after checking
  for hook edits. Portable does not store or forward that opaque state.
- `track_usage: false` stays rejected on Chat: Go changes both `include_usage`
  and when the stream stops, not just the final usage event. Responses still
  reports terminal usage in Go, even with tracking disabled.
- Native deferred tool loads on supported Responses models require replaying
  `tool_search_call` and `tool_search_output`. Select
  `provider_opts.supports_deferred_tools: false` for portable discovery instead.
  ChatGPT model picker/tool overrides and OpenCode teams stay rejected rather
  than attributing a different client or delegated conversation to the same ID.
- Arbitrary headers, `extra_body`, backend-specific sampling opts, WebSocket
  transport and native auth schemes are not admitted. In particular Go small
  forwarded float-valued `top_k`, `min_p`, `typical_p`, `repetition_penalty`
  and integral float `seed`, but omitted the same YAML integer values.
- No SDK environment lookup, including for Ollama. A keyless Ollama config
  with `OPENAI_API_KEY` present is rejected rather than sending the OpenAI key
  to loopback. An inherited provider key cannot follow a cross-origin model
  override; set the model's `token_key` explicitly. Encoded (`%`) or backslash
  base URLs are rejected rather than resolving ambiguous path separators;
  provider-owned Azure query values are encoded separately.

See [session compaction](session-compaction.md) for summary requests, overflow
recovery, and the limits on unobserved continuations.
