# Builtin tool results in the browser replay

The builtin lifecycle cases compare the tool events **and the next provider
request**. A tool can report an error without rejecting `send`. Go's helpers
do not validate every call or answer against the schema offered to the model.

- Each private `todo` declaration owns a list and ID counter. `shared: true`
  uses the handle's shared list across agents and configured declarations;
  filtering or deferring a tool does not change which list it uses. Shared
  and private declarations offering the same tool name remain ambiguous and
  are rejected.
- `user_prompt` returns the host's action. Object content is included (even on
  decline or cancel); non-object content is omitted. Its JSON uses Go's HTML
  escaping, float64 number spelling and sorted keys.
- `get_datetime` uses the IANA database bundled with Go for named zones,
  including historical abbreviations and future DST rules. `Local` uses Intl
  to select the name, falling back to the browser's offset if necessary.
  Stored memory and session timestamps instead use the runtime's fixed local
  offset. The clock fixtures advance wall time between provider requests.
- `list_sessions` summaries and `read_session` transcripts contain generated
  IDs. The archive replay renames only the IDs from the first listing, so it
  still checks their identity, order, timestamps and message counts.

After a Go toolchain update, regenerate the zone and printable-character
lookup tables from the repository root:

```sh
python3 cmd/wasm/portable/gen/zoneinfo.py
node cmd/wasm/portable/gen/printable.mjs  # Node with the same Unicode version as Go
```

**Known differences:** tied deferred search results follow Go map iteration
order, which cannot be replayed deterministically. Aborting a send while an
elicitation is pending can leave completed calls, the cancelled tool result and
scratchpad in Go's conversation. Portable rolls back that active turn's
history and scratchpad; already performed todo and host mutations survive.
Restart and close remain available to abandon the conversation.
