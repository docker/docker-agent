package harness

import "context"

// EventSink receives canonical harness events emitted by adapters.
type EventSink interface {
	Emit(Event)
}

// RawEventSink is an optional interface consumers implement to receive
// unstructured harness-native events for debugging and logging.
// Adapters check: if sink, ok := req.Events.(RawEventSink); ok { sink.OnHarnessRaw(...) }
type RawEventSink interface {
	OnHarnessRaw(source, kind string, data []byte)
}

// ToolExecutor executes host-side tools on behalf of ACP adapters.
// The method name matches the ACP wire method (e.g. "fs/read_text_file").
type ToolExecutor interface {
	Execute(ctx context.Context, method string, params []byte) ([]byte, error)
}

// PermissionRequester handles synchronous permission decisions for ACP adapters.
// Returns allowed=true if the decision permits the tool call, plus the source
// of the decision ("user", "policy", "remembered", "timeout").
type PermissionRequester interface {
	Request(ctx context.Context, toolCallID, toolName, description string, options []string) (allowed bool, source string, err error)
}
