package hooks

import (
	"context"
	"fmt"
	"maps"

	"github.com/docker/docker-agent/pkg/hooks/events"
)

// runPipeline passes each accepted rewrite to the next hook without changing
// the caller's input. Verdicts still aggregate across every matching hook.
func (e *Executor) runPipeline(ctx context.Context, event EventType, hooks []Hook, input Input, inputJSON []byte) *Result {
	results := make([]hookResult, 0, len(hooks))
	for _, hook := range hooks {
		results = append(results, e.runHook(ctx, event, hook, inputJSON))
		r := &results[len(results)-1]
		if r.err != nil || r.ExitCode != 0 || r.Output == nil || r.Output.HookSpecificOutput == nil {
			continue
		}

		next, changed := rewrittenInput(input, event, r.Output.HookSpecificOutput)
		if !changed {
			continue
		}
		nextJSON, err := next.ToJSON()
		if err != nil {
			// Discard this failed hook's output, but retain all earlier verdicts and rewrites.
			r.err = fmt.Errorf("hook %q: serialize rewritten input: %w", hook.DisplayName(), err)
			r.ExitCode = -1
			r.Output = nil
			continue
		}
		input, inputJSON = next, nextJSON
	}

	final := aggregate(results, event)
	if final.ModifiedInput != nil {
		// The runtime replaces tool arguments with this complete, patched input.
		final.ModifiedInput = input.ToolInput
	}
	return final
}

func rewrittenInput(input Input, event EventType, out *HookSpecificOutput) (Input, bool) {
	switch EventContract(event).Rewrite {
	case events.RewriteToolInput:
		if out.UpdatedInput == nil {
			return input, false
		}
		input.ToolInput = maps.Clone(input.ToolInput)
		if input.ToolInput == nil {
			input.ToolInput = make(map[string]any)
		}
		maps.Copy(input.ToolInput, out.UpdatedInput)
	case events.RewriteMessages:
		if len(out.UpdatedMessages) == 0 {
			return input, false
		}
		input.Messages = out.UpdatedMessages
	case events.RewriteToolResponse:
		if out.UpdatedToolResponse == nil {
			return input, false
		}
		input.ToolResponse = *out.UpdatedToolResponse
	default:
		return input, false
	}
	return input, true
}
