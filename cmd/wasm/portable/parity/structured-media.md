# Structured output and multipart seed history

The `structuredFeatures*` replays compare the unmodified native schema on both
OpenAI APIs. `outputReject*` and `outputExclusive*` also compare the tool
result sent to the host, the retry request, the final JSON, and the history
on the next send. The normalized tool parameters are **not** the schema used
to validate the model's arguments.

- Item failures precede array length failures; declared properties are checked
  before additional and missing properties. Go's validator can choose different
  first errors for multiple invalid properties, or list extra names in a
  different order (map iteration). Portable does not randomize diagnostics.
- Go validates a `float64`-decoded instance but returns the compacted **original**
  JSON. Portable rejects duplicate keys and requires safe integer literals for
  integer properties rather than accepting rounded or ambiguous answers:
  `1.00000000000000001` is not an integer. Lone UTF-16 surrogates are rejected
  rather than validating a replacement character and returning the original escape.
  Out-of-range floats can have a different decoder diagnostic.
- Native mode delegates validation to the provider; it does not parse the
  streamed text a second time.
- Use ordinary decimal bounds and quote string enum values. The config parsers
  can interpret unquoted YAML scalars differently (e.g. `012`, `1_000`,
  `1e+3` or `2:30` in a flow sequence). Claude's additional native instruction
  has stricter scalar checks. A Responses refusal is still an error in portable;
  it is not a successful empty answer.

`mediaSeed*` pins the distinction between the provider's multipart view and the
host result. When the model replies with no content, Go looks up the most recent
stored assistant message, skipping usage-only records. It reads `content`, not
the text or images projected into the request. An empty stored caption counts:
returning an earlier answer would identify the wrong turn.

`mediaRepeatedParts*` compares the projected history again after a tool call.
Document delimiters are defused even inside unrelated malformed markup. Unlike
Go, portable does not let an earlier unrelated tag mask a nested delimiter. The
browser does not dereference attachment URLs. File IDs, inconsistent byte counts
and history exceeding portable's size or part limits remain errors.

For exact replay of multipart history, use `session_compaction: false`. With
single-model compaction, the projection can change both a seed's estimated cost
and its message boundaries; this can change what Go keeps outside the summary.
Multipart history with history limits or routed compaction is rejected.
