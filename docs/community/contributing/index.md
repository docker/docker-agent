---
title: "Contributing"
description: "Docker Agent is open source. Here's how to set up your development environment and contribute."
keywords: docker agent, ai agents, community, contributing
weight: 10
canonical: https://docs.docker.com/ai/docker-agent/community/contributing/
---

_Docker Agent is open source. Here's how to set up your development environment and contribute._

## Development Setup

### Prerequisites

- [Go 1.27](https://go.dev/dl/) or higher
- API key(s) for your chosen AI provider
- [Task](https://taskfile.dev/installation/)
- [golangci-lint](https://golangci-lint.run/docs/welcome/install/local/)

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

## Lint rules

`task lint` runs the shared and project-specific cops selected in `lint/main.go`.
Reusable checks come from [rubocop-go v1.0.0](https://github.com/dgageot/rubocop-go/blob/v1.0.0/docs/shared-cops.md);
project-specific checks and frozen-config exclusions stay in `lint/`. Cop IDs
and `//rubocop:disable` annotations are unchanged. Add shared checks by their
constructors, not by enabling the entire upstream catalog.

`Lint/FieldsSeq` flags
`strings.Fields` slices used only for one value-only range, including loops
with an empty-input fallback. Use `strings.FieldsSeq`, evaluate its input at
the original location, and track whether any word was yielded when preserving
that fallback. Indexing, repeated traversal, capacity/count uses, mutable byte
inputs, and `FieldsFunc` callbacks are intentionally excluded.

The custom `Lint/FieldsSeqLookup` complements it by flagging guarded first-field
reads and `slices.Contains(strings.Fields(...), ...)`. Stop iteration once the
first field or match is found. Preserve empty-input fallbacks, and evaluate the
input and membership search value once, in their original order, before iterating.
Unguarded indexing, other slice uses, and exact field-count checks are excluded.

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

## Opening Issues

File issues on the [GitHub issue tracker](https://github.com/docker/docker-agent/issues). Please:

> [!NOTE]
> **See also**
>
> [Troubleshooting](../troubleshooting/index.md) — Common issues and debug mode. [Telemetry](../telemetry/index.md) — What data is collected and how to opt out.

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
