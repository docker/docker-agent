// Package toolexec hosts utilities used by the runtime to execute tool
// calls. It is intentionally free of runtime-private state so its
// primitives can be reused and tested in isolation.
//
// The package provides, among others:
//
//   - Dispatcher: runs tool-call batches, including approval flow, hook
//     dispatch, tracing, telemetry, and event emission.
//   - LoopDetector: detects consecutive identical tool-call batches so the
//     runtime can break degenerate loops where the model is not making
//     progress.
//   - ResolveModelOverride: extracts the per-toolset model override that
//     should apply to the next LLM turn from a batch of tool calls.
package toolexec
