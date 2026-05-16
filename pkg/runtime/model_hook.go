package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// providerModelClient is the runtime's [hooks.ModelClient]. It builds a
// fresh [provider.Provider] per call from the model spec and a default
// environment, then streams the assistant's reply.
//
// Provider construction per call (rather than caching per model spec)
// keeps the wiring simple — model hooks fire at most a handful of
// times per turn and the marginal cost of construction is negligible
// compared to the LLM round-trip itself. If profiling later shows
// otherwise, a sync.Map keyed on modelSpec would be a drop-in.
type providerModelClient struct{}

// Ask implements [hooks.ModelClient].
//
// Errors from any stage (model spec parsing, provider construction,
// stream open, stream recv) are returned unwrapped so the executor's
// fail-closed semantics deny PreToolUse calls cleanly. The caller
// (the model handler) is responsible for surfacing the error to the
// hook's [HandlerResult].
func (providerModelClient) Ask(
	ctx context.Context,
	modelSpec, system, user string,
	schema *latest.StructuredOutput,
) (string, error) {
	cfg, err := latest.ParseModelRef(modelSpec)
	if err != nil {
		return "", fmt.Errorf("invalid model spec: %w", err)
	}

	var opts []options.Opt
	if schema != nil {
		opts = append(opts, options.WithStructuredOutput(schema))
	}
	p, err := provider.New(ctx, &cfg, environment.NewDefaultProvider(), opts...)
	if err != nil {
		return "", fmt.Errorf("create provider: %w", err)
	}

	stream, err := p.CreateChatCompletionStream(ctx, []chat.Message{
		{Role: chat.MessageRoleSystem, Content: system},
		{Role: chat.MessageRoleUser, Content: user},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("start stream: %w", err)
	}
	defer stream.Close()

	var sb strings.Builder
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("stream recv: %w", err)
		}
		for _, c := range resp.Choices {
			sb.WriteString(c.Delta.Content)
		}
	}
	return sb.String(), nil
}

// registerModelHook installs the [hooks.HookTypeModel] factory on r
// using the runtime's default [hooks.ModelClient]. It is called once
// from [NewLocalRuntime] alongside the builtins.
func registerModelHook(r *hooks.Registry) {
	r.Register(hooks.HookTypeModel, hooks.NewModelFactory(providerModelClient{}))
}
