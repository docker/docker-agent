# pkg/fuzzy

`fuzzy.Score` is the fuzzy matcher behind `search_tool` in
`pkg/tools/builtin/deferred`. It wraps a pure-Go extraction of fzf's
`FuzzyMatchV2`, so it builds and runs on every `GOOS/GOARCH`, including
`js/wasm` where `github.com/junegunn/fzf/src/util` does not compile
(`syscall.Exec`, `Setpgid`, `unix.Dup2`).

The upstream module stays in `go.mod`: the TUI completion popup and the model
picker still import it directly.

## Provenance

Extracted by hand from <https://github.com/junegunn/fzf> at **v0.74.4**
(module cache `github.com/junegunn/fzf@v0.74.4`), MIT licensed,
Copyright (c) 2013-2026 Junegunn Choi. The license is in `LICENSE.fzf`.
These files are not generated; there is no extraction script.

| Here                      | Upstream                                              | Notes |
|---------------------------|-------------------------------------------------------|-------|
| `fzfalgo/algo.go`         | `src/algo/algo.go`                                    | `FuzzyMatchV2`, its `FuzzyMatchV1` fallback and shared helpers only |
| `fzfalgo/normalize.go`    | `src/algo/normalize.go`                               | table verbatim; `NormalizeRunes` dropped |
| `fzfalgo/scan.go`         | `src/algo/indexbyte2_other.go`, `src/algo/runeindex_ref.go` | portable reference scanners, build tags removed |
| `fzfutil/chars.go`        | `src/util/chars.go`                                   | `Chars` rewritten without `unsafe`; matcher-facing API only |
| `fzfutil/slab.go`         | `src/util/slab.go`                                    | verbatim |

### Deviations from upstream

Behavior-preserving only; `fzfalgo` tests compare against the upstream package
on native platforms (`!js`).

- Import path `github.com/junegunn/fzf/src/util` → `pkg/fuzzy/fzfutil`.
- Dropped: `ExactMatchNaive`, `ExactMatchBoundary`, `PrefixMatch`,
  `SuffixMatch`, `EqualMatch`, `bonusAt`, the `Algo` type, `DEBUG`/`debugV2`,
  and the `disableSingle`/`disableTwo`/`disableRunePrefilter` test hooks.
- Dropped: amd64/arm64 assembly and the 386 byte-view rune scanners; the
  reference implementations are used everywhere.
- `fzfutil.Chars` stores `bytes`/`runes` in two fields instead of one
  `unsafe`-reinterpreted slice, and keeps only `ToChars`, `IsBytes`,
  `MayFoldToASCII`, `Bytes`, `Runes`, `Get`, `Length`, `CopyRunes`.
- Lint-driven cosmetics: `Ascii` → `ASCII` in identifiers, `if/else` chains
  → `switch`, combined parameter types, `posArray(len)` → `posArray(n)`,
  `indexAt(max)` → `indexAt(length)`, `slices.Backward` in
  `lastIndexByteTwo`, `//nolint:gosec` on guarded `rune → byte` conversions.

### `Init` is not called

Upstream fills its ASCII class and bonus tables in `algo.Init(scheme)`, which
fzf calls from option parsing. The callers in this repository never called it,
so `fuzzy.Score` runs `fzfalgo` uninitialized on purpose to keep scores
identical to what they were. `fzfalgo.Init` is kept so that turning bonuses on
is a deliberate one-line change, covered by the `Init("default")` half of the
parity tests.

## Updating

Diff `src/algo/algo.go`, `src/algo/normalize.go`, `src/algo/runeindex_ref.go`,
`src/algo/indexbyte2_other.go` and `src/util/chars.go` between the recorded
version and the new one, port the changes, bump the version here and in the
file headers, then run:

```sh
go test ./pkg/fuzzy/...
GOOS=js GOARCH=wasm go test -exec="$(go env GOROOT)/lib/wasm/go_js_wasm_exec" ./pkg/fuzzy/...
```
