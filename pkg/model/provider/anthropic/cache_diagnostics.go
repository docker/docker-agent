package anthropic

import (
	"log/slog"
	"slices"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
)

func cacheDiagnosticsEnabled(opts map[string]any) bool {
	enabled, _ := providerutil.GetProviderOptBool(opts, "cache_diagnostics")
	return enabled
}

func configureCacheDiagnostics(params *MessageRequest, messages []chat.Message, opts map[string]any) {
	if !cacheDiagnosticsEnabled(opts) {
		return
	}
	params.Betas = append(params.Betas, AnthropicBetaCacheDiagnosis2026_04_07)
	params.Diagnostics = &DiagnosticsParam{
		PreviousMessageID: NullableString{Set: true, Null: true},
	}
	for _, message := range slices.Backward(messages) {
		state := message.ProviderState
		if message.Role == chat.MessageRoleAssistant && state != nil && state.Provider == providerStateName && state.MessageID != "" {
			params.Diagnostics.PreviousMessageID = NullableString{Set: true, Value: state.MessageID}
			return
		}
	}
}

func cacheDiagnostics(diagnostics Diagnostics) *chat.CacheDiagnostics {
	reason := diagnostics.CacheMissReason
	if reason.Type == "" {
		return nil
	}
	slog.Debug("Anthropic prompt cache diagnosis", "reason", reason.Type, "missed_input_tokens", reason.CacheMissedInputTokens)
	return &chat.CacheDiagnostics{Reason: reason.Type, MissedInputTokens: reason.CacheMissedInputTokens}
}
