---
title: "Contributing"
description: "Docker Agent is open source. Here's how to set up your development environment and contribute."
keywords: docker agent, ai agents, community, contributing
weight: 10
canonical: https://docker.github.io/docker-agent/community/contributing/
---

_Docker Agent is open source. Here's how to set up your development environment and contribute._

## Development Setup

### Prerequisites

- [Go 1.27](https://go.dev/dl/) or higher
- API key(s) for your chosen AI provider
- [Task](https://taskfile.dev/installation/)
- [golangci-lint 2.14.0](https://golangci-lint.run/docs/welcome/install/local/) or higher

> [!NOTE]
> **Platform Support**
>
> macOS and Linux are fully supported for development. On Windows, use `task build-local` to build via Docker.

### Build from Source

```bash
# Clone and build
git clone https://github.com/docker/docker-agent.git
cd docker-agent
task build

# Set API keys
export OPENAI_API_KEY=your_key_here
export ANTHROPIC_API_KEY=your_key_here

# Run an example
./bin/docker-agent run examples/code.yaml
```

### Development Commands

| Command            | Description                                     |
| ------------------ | ----------------------------------------------- |
| `task build`       | Build the binary to `./bin/docker-agent`        |
| `task test`        | Run all tests (clears API keys for determinism) |
| `task lint`        | Run golangci-lint, custom checks, and module tidiness checks |
| `task format`      | Format code                                     |
| `task dev`         | Run lint, test, and build in parallel           |
| `task build-local` | Build for local platform via Docker             |
| `task cross`       | Cross-platform builds (all architectures)       |
| `task generate-ts` | Regenerate the TypeScript SDK from Go types     |
| `task check-ts`    | Check the generated TypeScript SDK (needs Node) |

## Adding a model provider

First decide whether the provider can use an existing API implementation or needs
its own backend. A user can also configure an OpenAI-compatible endpoint without
adding a built-in provider; see [provider definitions](../../providers/custom/index.md).

### Compatible providers

For a built-in provider using an existing API, start with the embedded
[provider catalog](https://github.com/docker/docker-agent/blob/main/pkg/model/provider/catalog/providers.json).
Choose a canonical provider ID (the models.dev ID where available), API type,
endpoint, credentials and default model as appropriate. The
[catalog type](https://github.com/docker/docker-agent/blob/main/pkg/model/provider/catalog/catalog.go)
documents the metadata and selection rules; use behavior flags only when they
apply to the provider. Catalog order affects automatic provider selection. Check
the [models.dev ID tests](https://github.com/docker/docker-agent/blob/main/pkg/model/provider/catalog_test.go)
if the provider is absent from the embedded snapshot.

A typical OpenAI-compatible provider uses the existing OpenAI implementation;
it does not need a separate client or factory.

### Native backends

If the protocol or authentication cannot be handled by an existing backend,
implement the [provider contract](https://github.com/docker/docker-agent/blob/main/pkg/model/provider/contracts/contracts.go),
register its factory in the [default registry](https://github.com/docker/docker-agent/blob/main/pkg/model/provider/providers/providers.go),
and add catalog metadata as appropriate. Check the optional provider interfaces
in the contract if the backend supports additional capabilities.

### Implementation checklist

For either path, inspect the affected integrations rather than assuming a catalog
entry covers them all:

- **Configuration and dispatch:** Follow [defaults and overrides](https://github.com/docker/docker-agent/blob/main/pkg/model/provider/defaults.go)
  through the [factory](https://github.com/docker/docker-agent/blob/main/pkg/model/provider/factory.go).
  For new backends, consider whether other registries, such as [WASM](https://github.com/docker/docker-agent/blob/main/cmd/wasm/providers.go),
  should support them. If the public config changes, update [latest](https://github.com/docker/docker-agent/blob/main/pkg/config/latest/types.go),
  `agent-schema.json`, docs and an example; leave frozen versions unchanged.
- **Credentials and discovery:** Check [credential preflight](https://github.com/docker/docker-agent/blob/main/pkg/config/gather.go),
  [auto-selection](https://github.com/docker/docker-agent/blob/main/pkg/config/auto.go) and
  [discovery](https://github.com/docker/docker-agent/blob/main/pkg/config/discovery.go),
  particularly for native authentication or templated endpoints.
- **Model listing:** Inspect [models.dev lookup](https://github.com/docker/docker-agent/blob/main/pkg/modelsdev/store.go),
  the [runtime model picker](https://github.com/docker/docker-agent/blob/main/pkg/runtime/modelcatalog/catalog.go)
  and [CLI listing](https://github.com/docker/docker-agent/blob/main/cmd/root/models.go).
  Auto-selection, the picker and the CLI have different eligibility rules; a
  catalog entry does not by itself enable live endpoint discovery.
- **Tests and docs:** Start with [catalog tests](https://github.com/docker/docker-agent/blob/main/pkg/model/provider/catalog/catalog_test.go),
  [alias request tests](https://github.com/docker/docker-agent/blob/main/pkg/model/provider/openai_alias_providers_test.go)
  and [factory tests](https://github.com/docker/docker-agent/blob/main/pkg/model/provider/factory_test.go).
  Cover affected defaults, overrides, credentials, requests, streaming, errors,
  selection and discovery as applicable; see tests alongside those integrations.
  Update the [provider credentials overview](../../providers/overview/index.md#provider-credentials)
  and add user-facing setup docs or examples as needed.

Run the relevant tests and the [development checks](#development-commands); live
API tests can make external requests, so use them only when intentionally
testing with credentials.

## Message-merging compatibility probes

Consecutive system and user message merging is controlled by catalog policy, not
by endpoint evidence. The [message-merging compatibility probes](../message-merging/index.md)
compare preserved boundaries with the existing merge under explicit opt-in and a
fixed request budget. Use them before changing a provider's
`merge_consecutive_messages` value, and record the scoped result instead of
treating a skipped run as a pass.

## Debugging TUI Tests

The headless TUI harness can save each captured frame for inspection:

```bash
mkdir -p .cache/tui-test
go test -count=1 -v -artifacts -outputdir="$PWD/.cache/tui-test" ./e2e/tui -tuitest.frames
```

`-tuitest.frames` writes numbered text files in a separate `frames-*` directory
for each driver, under the test's `testing.TB.ArtifactDir()`. The test log prints
the exact path. `-artifacts` retains these directories after the tests finish;
without it, dumps are temporary and removed during test cleanup. Use `-count=1`
to capture fresh frames rather than reuse a cached test result.

With the command above, retained dumps are under `.cache/tui-test/_artifacts/`.
For CI jobs that enable frame dumping, upload this directory even when tests
fail. Dumps are no longer written alongside golden files in `testdata/frames`;
`-tuitest.update` still updates golden files in `testdata/`.

For an approximate live view instead, use `go test -v ./e2e/tui -tuitest.live`.

## Dogfooding

Use Docker Agent to work on Docker Agent! The project includes a specialized developer agent:

```bash
cd docker-agent
docker agent run ./golang_developer.yaml
```

This agent is an expert Go developer that understands the Docker Agent codebase. Ask it questions, request fixes, or have it implement features.

## Core Concepts

- **Root Agent** — Main entry point that coordinates the system
- **Sub-Agents** — Specialized agents for specific domains
- **Tools** — External capabilities via MCP
- **Models** — AI provider configurations

## Code Style

The project uses `golangci-lint` with strict rules. As long as `task lint` passes, the code is stylistically acceptable.

Key conventions:

- Use `fmt.Errorf("context: %w", err)` for error wrapping
- Always pass `context.Context` as the first parameter
- Use `slog` for structured logging
- Use functional options pattern for constructors
- In tests: use `t.Context()`, `t.TempDir()`, `t.Setenv()`, and `t.Parallel()`

## Profiling the lint runner

Benchmark the full command with a warm Go build cache:

```bash
hyperfine --warmup 1 --runs 5 'go run ./lint .'
```

For CPU and allocation profiles of the in-process runner:

```bash
mkdir -p .cache/lint-profile
go test ./lint -run '^$' -bench '^BenchmarkLintRepository$' -benchtime=1x \
  -cpuprofile="$PWD/.cache/lint-profile/cpu.pprof" \
  -memprofile="$PWD/.cache/lint-profile/mem.pprof" \
  -o "$PWD/.cache/lint-profile/lint.test"
go tool pprof -top .cache/lint-profile/cpu.pprof
go tool pprof -alloc_space -top .cache/lint-profile/mem.pprof
```

`BenchmarkChannelDirection` isolates channel analysis on already-loaded packages.
Go benchmark timings exclude compilation and may benefit from in-process caches;
use the full command to confirm end-to-end improvements. Allocation totals are
cumulative, not peak memory, and profiles do not include `go/packages` subprocesses.

## Lint rules

`task lint` runs the shared and project-specific cops selected in `lint/main.go`.
Reusable checks come from [rubocop-go](https://github.com/dgageot/rubocop-go/blob/0b7e76d9373ad390ac84652375150459bde13ebe/docs/shared-cops.md)
(pinned in `go.mod`); project-specific checks and frozen-config exclusions stay
in `lint/`. Cop IDs and `//rubocop:disable` annotations are unchanged. Add shared checks by their
constructors, not by enabling the entire upstream catalog. 40 of the 42 shared
opt-in cops are currently selected. `Lint/ContextFirstParameter` and
`Lint/NoContextField` remain disabled: existing public constructor signatures and
intentionally owned lifecycle or telemetry contexts would require suppressions or
unrelated refactoring.

Execution adapters and candidate prefilters also come from rubocop-go.
`runner.WithDirectoryTypes` retains inactive and partially typed constructor
checks; `prog.FromFileWithTests` shares the test-package cache for slice checks.
The runner opts into `WithSharedProgramLoad` to share production/test type-checking.
Program cops must keep source files, package data, and the build environment
unchanged during analysis; file-cop coverage still includes inactive files.

`CancellationLoop`, `ClosedChannelLoop`, `ChannelStateCheck`,
`CancellationSafeResultSend`, `NonblockingChannelReplacement`, and `SlicesConcat`
use the upstream implementations and tests. Shared-cop behavior and version
compatibility are tested in rubocop-go; only project-owned rules, exclusions,
and registration checks are tested here.

`Lint/ChannelDirection` suggests directional raw-channel parameters and results.
It follows requirements through locals, fields, direct calls, returns, and closure
captures, including internal/external tests. Closing requires send capability;
receiving and closing requires both. Connected declarations may need coordinated
changes, so there is no autofix. Generated and frozen declarations retain their
original contracts. Unknown calls, interface escapes, reflective type arguments,
function values, and unsupported structural conversions conservatively retain
capabilities. Tuple flows preserve both channel operations and aggregate escapes.
The check assumes the loaded program is the consumer: review exported API changes
for downstream users and validate other build targets. It covers the active build,
not every build-tag or platform variant; it does not prove channel ownership or
send/close race safety.

`Lint/CancellationLoop` warns when a `context.Context.Done()` case can return to
the same select in an unconditional loop, including an unlabelled `break` that
only exits the select. `Lint/ClosedChannelLoop` follows explicit comma-ok closure
checks and warns when the closed channel can be polled again without exiting or
replacing/disabling it. These checks use local control flow and stable channel
expressions; they do not prove aliases, helper side effects, conditional loops,
or arbitrary cancellation protocols.

`Lint/ChannelStateCheck` warns about potentially blocking channel operations
guarded by a `len`/`cap` check on the same channel. The observation does not reserve
capacity or a value; use `select` or explicit synchronization. Nonblocking select
communications are excluded. Matching covers local guards and resolved storage
expressions, not channel aliases or indexed containers; labels discard inferred
guards rather than assuming jump paths are safe. All three checks include
tests, skip generated files and frozen configs, and provide no automatic fixes.

`Lint/HTTPRequestWithContext` enforces contextual HTTP request construction outside
frozen config versions. It inspects production files only; matching is syntactic,
so aliases and shadowing require care. `Lint/NoFatalOutsideMain` reserves
`log.Fatal*` for package main; tests are exempt.

The following modernization cops inspect resolved production, internal/external
tests, and test-only packages, excluding generated files and frozen configs:

- `Lint/MapsCopy` and `Lint/MapsClone` suggest plain entry-copy loops and nil-safe
  shallow-copy helpers. Preserve merge order, destination initialization, named
  types, and nil results; the two cops can report overlapping suggestions.
- `Lint/SlicesContains` and `Lint/SlicesEqual` suggest simple comparable-element
  membership and equality helpers. Preserve nil-sensitive guards and comparison
  behavior, including NaNs and interface-comparison panics.
- `Lint/SplitSeq` suggests lazy `strings.SplitSeq` or `SplitAfterSeq` for direct
  value-only ranges. Preserve input/separator evaluation and empty/trailing fields.
- `Lint/SortedMapKeys` suggests `slices.Sorted(maps.Keys(m))` for adjacent ascending
  integer/string key collection. Review nil/empty results and downstream capacity
  contracts; this is not an allocation-performance guarantee.
- `Lint/WaitGroupGo` suggests reviewing adjacent `Add(1)`/`go`/`defer Done()`.
  The callback must not let a panic escape; preserve captures and completion timing.
- `Lint/ErrorsAsType` suggests fresh targets consumed only on successful matches.
  Custom `As` methods can retain target pointers; do not mechanically replace them.
- `Lint/HTTPTestRequestWithContext` suggests immediate test-request context
  attachment with constant method/target and nil body. Keep non-nil contexts and
  preserve context evaluation versus request validation order.

These checks follow active build constraints, skip ill-typed candidate packages,
and gate suggestions on the target module/file's Go and stdlib versions, not the
linter toolchain. Suggestions require behavior review, not automatic rewriting.
Rubocop exclusions and suppressions do not configure golangci-lint's overlapping
`modernize` checks.

`Lint/FieldsSeq` flags
`strings.Fields` slices used only for one value-only range, including loops
with an empty-input fallback. Use `strings.FieldsSeq`, evaluate its input at
the original location, and track whether any word was yielded when preserving
that fallback. Indexing, repeated traversal, capacity/count uses, mutable byte
inputs, and `FieldsFunc` callbacks are intentionally excluded.

`Lint/FieldsSeqLookup` complements it by flagging guarded first-field
reads and `slices.Contains(strings.Fields(...), ...)`. Stop iteration once the
first field or match is found. Preserve empty-input fallbacks, and evaluate the
input and membership search value once, in their original order, before iterating.
Unguarded indexing, other slice uses, and exact field-count checks are excluded.

`Lint/ConstructorCommandExec` flags command setup and resolved `exec.Cmd`
execution methods in constructors, including aliased imports and typed test
packages. Unrelated methods
named `Run` or `Start` are excluded. Move process work to an explicit operation.

`Lint/NoStdoutInLibraries` checks non-main production packages under `pkg/` for
`fmt.Print*` and `fmt.Fprint*` calls targeting `os.Stdout`; use a caller-provided
writer instead. CLI packages and tests remain exempt.

`Lint/SortStableFunc` recommends `slices.SortStableFunc` with `cmp.Compare`
for reflection-based `sort.SliceStable` calls comparing integer or string keys,
including short-circuiting tie-breaks. Reverse comparison arguments for descending
keys and preserve stable ties. Floating-point keys (NaN ordering), side-effecting
comparators, generated code, tests, and frozen configs are excluded. The cop
reports suggestions, not automatic rewrites.

`Lint/CutPrefix` flags `strings.HasPrefix` paired with `TrimPrefix` or equivalent
prefix slicing, including early-exit guards and switch cases. It matches resolved
stdlib calls and stable arguments, stopping at intervening calls or mutations.
Preserve short-circuit evaluation and assignments to existing variables when
using `strings.CutPrefix`; a redundant check may only need `TrimPrefix`.
Generated files and frozen config versions are excluded.

`Lint/CutSuffix` flags an adjacent `strings.HasSuffix` check and matching
`TrimSuffix` or suffix-only slice, including early-return/continue guards.
Only constant or local identifier inputs are matched; compound conditions,
intervening work, and effectful expressions are excluded. Preserve original
values, assignment scope, and evaluation order when introducing the cut result.
Both cut cops also cover immediate `bytes.HasPrefix`/`HasSuffix` plus matching
trimming or slicing on local unnamed byte slices. Preserve nilness, capacity, and
backing-array aliasing.

`Lint/NewExpr` flags a fresh local declared only to return its address, recommending
`new(expr)` instead. It requires the declaration and `return &x` to be adjacent, the
variable to have no other uses, and skips composite literals (`&T{...}` stays clearer
than `new(T{...})`) and any file that shadows the builtin `new`.

`Lint/SplitTrimJoin` flags `strings.Split` followed by suffix-only trimming and a
`strings.Join` that needlessly materializes and rejoins the string; use
`strings.CutLast` instead. Only adjacent, local patterns with a single-byte
separator are matched, and predicates that mutate or retain a pointer into the
slice are excluded.

`Lint/SlicesConcat` flags nested append chains and adjacent local append sequences
that combine slices into fresh storage (nil, empty literals, `make`, or
`slices.Clone`). It skips in-place appends, single-slice copies, mixed named
slice types, scalar appends, and inputs with calls or other side effects.
Review nil versus non-nil empty results, capacity assumptions, and allocation
panics before replacing a match; `Concat` returns nil for an empty result.
Keep a reasoned `//rubocop:disable Lint/SlicesConcat` where those semantics matter.
The cop uses resolved types, inspects production packages, excludes generated
and frozen config files, and never rewrites code automatically.

`Lint/PointerHelper` recommends native `new` expressions for AWS scalar pointer
helpers. Preserve explicit numeric conversions; slice/map and dereference helpers
are not replacements for `new`.

`Lint/ReflectFields` covers paired `reflect.Value.Field(i)` and
`Value.Type().Field(i)` loops that the upstream iterator analyzer misses. It
excludes receiver mutation/escape, unrelated index uses, and unsafe callbacks.

`Lint/StdlibUUID` recommends stdlib UUIDs for random strings and canonical
literal-to-string conversions. It preserves UUIDv5 and compatibility parsers,
and skips recommendations when production or test code configures Google UUID
randomness. General parsers and values exposing the Google UUID type are excluded.

`Lint/URLClone` recommends `url.URL.Clone` for equivalent nil-safe deep copies
or copies guarded by `User == nil`. General shallow copies are excluded because
`Clone` also copies userinfo.

`Lint/JSONMarshalWrite` flags a single buffered encoding followed by newline
trimming. Review the suggested `jsonv2.MarshalWrite` migration with v1 defaults,
explicit HTML escaping, unchanged evaluation order, and discarded partial output
on error. Streaming encoders and indentation are excluded.

`Lint/BenchmarkLoop` covers simple `ResetTimer`/`b.N` loops, including
sub-benchmarks and test-only packages. Review measurement and compiler effects
before adopting `b.Loop`; parallel, timer-sensitive, and escaping benchmark
handles are excluded. These cops report suggestions, never automatic rewrites.
The modernization cops above inspect production packages; the UUID randomness
guard additionally reads tests.

`Lint/SlicesClone` checks production and test code for append-to-empty slice
copies and adjacent fresh-local `make(len(src))` / `copy` pairs. It resolves
builtin calls and slice types, skips generated files and frozen configs, and
excludes buffer reuse, explicit capacities, and effectful repeated sources.
Suggestions require review: `slices.Clone` preserves source nilness and named
slice types, whereas the old idioms may normalize empty results or change the
type. Preserve those behaviors and any observable capacity contract when migrating.

Test output/artifact lifetimes and in-memory HTTP compatibility require review;
there are no blanket rules for `t.Output`, `ArtifactDir`, or `NewTestServer`.

## Correctness cops

`Lint/CancellationSafeResultSend` detects unbuffered, local result sends whose
sole receiver can return on context cancellation. It checks inline goroutines
and simple aliases, excluding escaped channels and extra receivers. Buffered
channels and cancellation-aware send selects are not reported. Every returning
cancellation arm must be covered by the producer. Straight-line context copies
and `WithValue` preserve identity; escaped contexts and unresolved context
constructors are excluded. Buffering alone is only sufficient for a one-shot
result, not an arbitrary stream.

`Lint/NonblockingChannelReplacement` flags a failed nonblocking send followed
by an adjacent blocking drain/resend on the same local or stable field channel.
Another consumer can empty the queue after the failed send. Nonblocking drains,
indexed receivers, and effectful send expressions are excluded. The check does
not prove ownership of a queue; exclusive-access protocols need review.

`Lint/BoundedCommandPipeLifetime` flags synchronous shell stdout/stderr pipe
reads between `Start` and `Wait` without recognized cancellation cleanup.
`CommandContext` killing the shell does not close pipes inherited by descendants;
`WaitDelay` alone cannot bound a caller-owned read before `Wait`. The check
recognizes literal `-c` shells and `shellpath.DetectShell`, local aliases, and
explicit cancellation-driven pipe close or `SIGKILL` of a configured process
group. Replaced cancellation callbacks and stopped `AfterFunc` registrations no
longer provide protection. Conditional calls do not establish unconditional
cleanup or completion. It does not prove helper, cross-block, or asynchronous
read lifetimes.

`Lint/SilentLimitedRead` follows local `io.ReadAll(io.LimitReader(...))`
results into complete-file writes. It checks that overflow
cannot reach those sinks; a typical guard reads `limit+1` and rejects lengths
greater than `limit`. Preview-only uses, transformed or escaped buffers, and
conditionally evaluated sinks are excluded. Mutable or escaped limits are
outside the stable-bound analysis. File-size snapshots and HTTP Content-Length
metadata alone do not establish a bound on a later read. The analysis is local,
not a proof of stream completeness. JSON decoding alone is not a complete-file sink: a
truncated valid JSON object either fails decoding or loses only trailing
whitespace, so it is outside this cop's scope.

All four correctness cops inspect resolved production code, exclude generated
files and frozen configs, and support ordinary rubocop suppressions. They report
existing issues without automatically changing application behavior.

## Opening Issues

File issues on the [GitHub issue tracker](https://github.com/docker/docker-agent/issues). Please:

> [!NOTE]
> **See also**
>
> [Troubleshooting](../../guides/troubleshooting/index.md) — Common issues and debug mode. [Telemetry](../../configuration/telemetry/index.md) — What data is collected and how to opt out.

- Use the included issue template
- Search for existing issues before creating new ones
- Only use issues for bugs and feature requests (not support)

## Submitting Pull Requests

1. **Fork** the repository and create a branch for your changes
2. **Write** your code following the style and testing guidelines above
3. **Test** your changes: run `task lint` and `task test`
4. **Sign off** your commits with `git commit -s` (DCO required)
5. **Open a pull request** against the `main` branch

> [!TIP]
> Use the dogfooding agent (`docker agent run ./golang_developer.yaml`) to help write and review your changes before submitting.

## Sign Your Work

All contributions require a Developer Certificate of Origin (DCO) sign-off:

```bash
# Set the identity used for the sign-off
$ git config user.name "Your Name"
$ git config user.email "your.email@example.com"

# Add a DCO sign-off to this commit
$ git commit -s -m "Your commit message"
```

`-s` adds a `Signed-off-by` trailer; it does not cryptographically sign the commit. To also sign with a GPG or SSH key, configure Git signing and use `git commit -S -s`.

## Community

Find us on [Slack](https://dockercommunity.slack.com/archives/C09DASHHRU4) for questions and discussions.

## Code of Conduct

We want to keep the Docker Agent community welcoming, inclusive, and collaborative. Key guidelines:

- **Be nice** — Be courteous, respectful, and polite. No abuse of any kind will be tolerated.
- **Encourage diversity** — Make everyone feel welcome regardless of background.
- **Keep it legal** — Share only content you own and don't break the law.
- **Stay on topic** — Post to the correct channel and avoid off-topic discussions.

The governance for this repository is handled by Docker Inc.

## Reviewer-only Gemini integration fixtures

Gemini HTTP/SSE fixtures, recorded CLI flows, and real SDK/session composition
checks are **off by default**, including fixtures that use only loopback or an
in-memory transport. Isolated adapter, configuration, policy, and metadata units
remain enabled, as do non-Gemini subtests.

Only reviewers on their own non-CI workstation may enable these fixtures with
`DOCKER_AGENT_RUN_GEMINI_INTEGRATION_TESTS=1` (exactly `1`). Any nonempty
`GITHUB_ACTIONS` value, even `false`, disables them regardless of opt-in. Do not
unset that variable to bypass the guard in GitHub Actions. No workflow or test
selection changes are needed: the guard applies to PRs (including drafts), main
and tag pushes, merge groups, manual dispatches, and the WASM test lane.

Before opting in, **deny external network egress while allowing loopback**, keep
all checked-in cassettes, and do not supply live API credentials. The E2E proxy
uses `ModeRecordOnce`: a missing cassette can attempt a real upstream request.
The OpenCode fixture also relies on a transport redirect to stay local. Opt-in
permits existing offline fixtures only, not cassette recording or paid calls.

The following commands are for reviewer execution only, not for implementers or
project CI. Run them from the repository root after checking out the PR head:

```bash
# Confirm GITHUB_ACTIONS is empty on your own workstation; never bypass it in CI.
printf 'GITHUB_ACTIONS=%s\n' "$GITHUB_ACTIONS"
export DOCKER_AGENT_RUN_GEMINI_INTEGRATION_TESTS=1

go test -count=1 ./pkg/model/provider/gemini
go test -count=1 ./pkg/model/provider -run '^TestGatewayProviderRequests$/^google$'
go test -count=1 ./pkg/runtime -run '^TestRunStream_ImageOutputGuard_RejectsBeforeDispatch$'
go test -count=1 ./pkg/tui/page/chat -run '^TestImageOutputGuard_RuntimeToTUI_RejectsBeforeDispatchAndClearsSpinner$'
go test -count=1 ./e2e -run '^TestExec_Gemini($|_)'
go test -count=1 ./e2e -run '^TestDebug_Title$/^Google_'

# Requires Node for the Go WASM test runner.
GOOS=js GOARCH=wasm go test -count=1 -tags docker_agent_wasm_cloud \
  -exec="$(go env GOROOT)/lib/wasm/go_js_wasm_exec" ./cmd/wasm \
  -run '^TestCloud(SessionsUseSessionEnvOnly|TokensReachRequests)$/.*gemini'
GOOS=js GOARCH=wasm go test -count=1 \
  -exec="$(go env GOROOT)/lib/wasm/go_js_wasm_exec" ./cmd/wasm \
  -run '^TestGoogleFactory(RejectsVertexWithoutCloudTag|KeepsGeminiAPIWithoutCloudTag)$'
```

Expected: selected fixtures run rather than skip and their existing assertions
pass. They cover streaming keepalives/tool replay, request wire shape, auth and
transport wrapping, embeddings, image-output guards, runtime/TUI error propagation,
CLI tools/title, and WASM session/token flows. These are existing
regressions, not live-backend entitlement or new model acceptance tests. Record
head SHA, OS/Go/Node versions, command, pass/fail/skip and sanitized evidence.
A skip is not a pass, and pure unit success is not integration evidence.

For the preparation change, no Gemini integration fixture was executed by the
implementer, even to verify the skip path. Validation is limited to pure gate
units, source placement review, compile-only checks, static analysis and builds.
Future fixture additions must be guarded before server, proxy, transport, SDK
flow or browser-session setup, with pure cases and non-Gemini rows kept separate.

## Toolset worker lifetimes

Toolsets with a background loop and no startup handshake can implement
`tools.Runnable.Run(ctx)` instead of `tools.Startable`. `Run` blocks until
cancellation and joins its work before returning; timers and other run resources
stay local. Callers must not run the same toolset concurrently.

Agent, additional-toolset, Code Mode, and fork-skill paths use the canonical
`tools.NewStartable` wrapper. It owns one run, preserves startup context values
without inheriting startup cancellation, and cancels and joins on shutdown—even
with an already-canceled shutdown context. `Startable` takes precedence over
`Runnable` so existing lifecycle wrappers retain ownership. Fork-skill toolsets
are started lazily and owned until team shutdown, not one skill invocation.

For direct scheduler users, replace `Start`/`Stop` with a caller-owned `Run(ctx)`
and cancellation/join, or wrap it once with `tools.NewStartable`. Two independent
wrappers around the same raw worker are not a shared lifecycle. The wrapper is
an intentional bridge to legacy host `Start`/`Stop`; it does not make those host
lifetimes structured.

Raw runnable session extras supplied programmatically are instead adopted once
by their runtime and joined by `Close`; existing lifecycle wrappers remain
borrowed. Keep a session's extras fixed during execution. Share one explicit
wrapper when sharing a worker across runtimes or using a noncomparable runnable
value across sessions. Runtime adoption deduplicates comparable worker providers
across decorated references; noncomparable workers are scoped to a session slot.
Runnable-only workers do not support the separate `Restartable` capability;
stop and start their canonical wrapper to replace a run.
