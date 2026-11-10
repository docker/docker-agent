---
title: "Message-merging compatibility probes"
description: "How to collect endpoint-scoped evidence for consecutive system and user message merging without changing production defaults."
keywords: docker agent, contributing, providers, message merging, compatibility
weight: 20
canonical: https://docker.github.io/docker-agent/community/message-merging/
---

The provider catalog records whether consecutive system or user messages must
be merged before a Chat Completions request. A catalog flag is a policy
assumption, not evidence about a real endpoint. This page describes the opt-in
probe that compares preserved message boundaries with the existing merge.

The probe is an experiment harness. It does not change production defaults, and
its output is not a compatibility guarantee.

## Scope

The first probe wave is deliberately narrow:

- Chat Completions only (`/chat/completions`). Targets that route to the
  Responses API are reported as `not_applicable` before a request is sent.
- Text-only fixtures. Multimodal parts, tool transcripts, structured output and
  explicit prompt-cache breakpoints are covered by hermetic tests instead.
- Built-in catalog providers only. Custom provider definitions are out of scope
  because their endpoint and template behaviour is deployment-specific. DMR is
  included and needs a local Model Runner with the model already pulled; the
  probe never downloads models. Targets that cannot be constructed are reported
  as `setup_error`, not as a merge result.

The OpenAI API classification redesign tracked in
[#4756](https://github.com/docker/docker-agent/issues/4756) is intentionally
separate. A provider's protocol flavour does not prove a template requirement.

## Safety guards

The probe requires both an explicit opt-in and an explicit target list:

```text
DOCKER_AGENT_RUN_MESSAGE_MERGE_LIVE_TESTS=1
DOCKER_AGENT_MESSAGE_MERGE_TARGETS=provider/model[,provider/model...]
```

- Credentials alone never start an inference request. The opt-in and target
  validation run before any provider or credential is constructed.
- Wildcards are rejected, so a typo cannot scan the catalog.
- Each variant sends one inference request. SDK retries are counted through the
  probe transport and then blocked by the budget.
- DMR availability is checked through the read-only models API. The probe never
  runs `docker model pull`, and a missing model is reported as `setup_error`.
- Probe requests set a small output budget (`max_tokens: 256`) and temperature
  `0`, so scenarios only spend enough tokens to return their markers and
  sampling noise does not decide the comparison.
- Requests run sequentially, not in parallel, to avoid provider rate limits
  contaminating the comparison.
- The report contains provider, model, normalized API path, request count, HTTP
  status, outcome category, stream end, marker result and token usage. It does
  not contain API keys, authorization headers, account ids or raw response
  bodies.

## Running the probe

Use an explicit target list. A model id may contain slashes; the provider is
everything before the first slash.

```bash
DOCKER_AGENT_RUN_MESSAGE_MERGE_LIVE_TESTS=1 \
DOCKER_AGENT_MESSAGE_MERGE_TARGETS='deepseek/deepseek-v4-pro,openrouter/qwen/qwen3.6-35b' \
DOCKER_AGENT_MESSAGE_MERGE_REPORT=/tmp/message-merge.jsonl \
go test -count=1 -buildvcs=true -v -run '^TestMessageMergeLive$' ./pkg/model/provider
```

PowerShell:

```powershell
$env:DOCKER_AGENT_RUN_MESSAGE_MERGE_LIVE_TESTS = "1"
$env:DOCKER_AGENT_MESSAGE_MERGE_TARGETS = "deepseek/deepseek-v4-pro,openrouter/qwen/qwen3.6-35b"
$env:DOCKER_AGENT_MESSAGE_MERGE_REPORT = "$env:TEMP\message-merge.jsonl"
go test -count=1 -buildvcs=true -v -run '^TestMessageMergeLive$' ./pkg/model/provider
```

The test skips when the opt-in is absent. It fails when the test binary has no
`vcs.revision`; `-buildvcs=true` is required so the JSONL report records the
commit. A target that cannot be constructed fails the run and records
`setup_error`; a missing credential should be treated as a setup failure, not as
a compatibility result.

## Scenarios and variants

Each scenario is sent twice against the same provider/model:

| Variant | Wire shape |
| --- | --- |
| `preserve` | Converted messages keep their original boundaries. |
| `merge_system_user` | The existing `MergeConsecutiveMessages` helper is applied. |

The variants differ only in the `messages` field. Model, tools, temperatures and
all other request fields stay unchanged.

| Scenario | Preserved shape | Merge effect | Markers |
| --- | --- | --- | --- |
| `control` | system, user | none | `PROBE-CONTROL-OK` |
| `multi_system` | system, system, user | two systems become one | `PROBE-SYSTEM-A-OK`, `PROBE-SYSTEM-B-OK` |
| `consecutive_user` | system, user, user | two users become one | `PROBE-USER-A-OK`, `PROBE-USER-B-OK` |
| `late_system` | system, user, system, user | none; adjacent merging cannot fix it | `PROBE-LEADING-OK`, `PROBE-LATE-OK` |

`late_system` is diagnostic. If a target rejects it, that is not evidence that
adjacent merging is sufficient: the template or server may reject non-leading
system messages entirely.

## Reading the result

| Outcome | Meaning |
| --- | --- |
| `accepted` | The endpoint returned a stream and the request was not rejected. |
| `role_rejected` | The endpoint rejected the message shape with a role/template error. |
| `auth_rejected` | Credentials or permissions failed; the comparison is invalid. |
| `rate_limited` | The provider throttled the request; retry later. |
| `empty` | A stream completed without visible content or a finish reason. Investigate usage and wire data; do not label it a merge requirement by itself. |
| `timeout` | The request exceeded the per-request deadline. |
| `endpoint_error` | Another HTTP or transport failure; the comparison is invalid. |
| `not_applicable` | The target did not route to `/chat/completions`; no request was sent. |
| `setup_error` | Provider construction failed before the probe could run. |

A `preserve` rejection followed by a `merge_system_user` acceptance is strong
evidence for that endpoint/model/scenario. Two acceptances only show that both
shapes were accepted; they do not prove semantic equivalence. A merge rejection
is evidence against a global merge.

## Recording evidence

Use one row per provider, model, endpoint and run. Keep the date and commit so a
later template change does not silently inherit an old conclusion.

| Date | Commit | Provider/model | Endpoint | Scenario | Preserve | Merge | Notes |
| --- | --- | --- | --- | --- | --- | --- | --- |
| not established | | | | | | | No live run recorded in this change. |

Only after enough scoped evidence exists should the catalog policy be
simplified. Until then, the existing defaults stay in place.

## Hermetic tests

The production request path and the merge helper are covered without network
access:

```bash
go test ./pkg/model/provider/oaistream ./pkg/model/provider
```

These tests pin:

- exact content and delimiter behaviour for string and mixed content;
- assistant and tool turns are never merged;
- late system messages are not hoisted;
- singleton messages and messages carrying metadata are passed through
  unchanged;
- metadata on any member of a same-role run is a barrier, not silently dropped;
- the merge is idempotent and does not mutate its input;
- the probe transport replaces only `messages` and preserves every other field;
- a non-Chat-Completions target is rejected before the base transport is called;
- the request budget blocks a retry.
