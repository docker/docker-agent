// Package toolexec hosts utilities used by the runtime to execute tool
// calls. It is intentionally free of runtime-private state so its
// primitives can be reused, tested in isolation, and incrementally
// grown into a fully fledged tool dispatcher.
//
// The package currently provides:
//
//   - LoopDetector: detects consecutive identical tool-call batches so the
//     runtime can break degenerate loops where the model is not making
//     progress.
//   - ResolveModelOverride: extracts the per-toolset model override that
//     should apply to the next LLM turn from a batch of tool calls.
//
// Future extractions (approval flow, hooks dispatch, tool handler
// registry) belong here so that the runtime keeps shrinking toward
// pure orchestration.
package toolexec
