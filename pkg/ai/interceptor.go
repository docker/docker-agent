package ai

import (
	"context"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/tools"
)

// StreamRequest holds the parameters for a single model stream call.
// It is passed through the interceptor chain and can be inspected or
// modified by interceptors before reaching the actual model call.
type StreamRequest struct {
	Model    provider.Provider
	Messages []chat.Message
	Tools    []tools.Tool
}

// StreamInterceptor wraps a stream call, allowing callers to observe,
// modify, or short-circuit the request before and after it reaches the
// model. The interceptor receives the request and a handler to call the
// next step in the chain — either another interceptor or the actual
// model call. Returning without calling the handler skips the model call.
//
// Example:
//
//	func logInterceptor(ctx context.Context, r *StreamRequest, h StreamHandler) (*ModelResponse, error) {
//		// before: inspect or modify request
//		res, err := h(ctx, r)
//		// after: inspect response, record telemetry, etc.
//		return res, err
//	}
type StreamInterceptor func(context.Context, *StreamRequest, StreamHandler) (*ModelResponse, error)

// StreamHandler is the function signature for the next step in the
// interceptor chain. Call it to proceed with the stream request.
type StreamHandler func(context.Context, *StreamRequest) (*ModelResponse, error)

// ToolCallInterceptor wraps an individual tool call execution.
// The interceptor is responsible for calling tool.Handler and can
// add behavior before and after (permissions, logging, telemetry).
type ToolCallInterceptor func(context.Context, *ModelResponse, tools.ToolCall, tools.Tool) (*tools.ToolCallResult, error)
