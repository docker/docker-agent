package anthropic

import (
	"log/slog"
	"slices"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
)

func cacheDiagnosticsEnabled(opts map[string]any) bool {
	enabled, _ := providerutil.GetProviderOptBool(opts, "cache_diagnostics")
	return enabled
}

func configureCacheDiagnostics(params *anthropic.BetaMessageNewParams, messages []chat.Message, opts map[string]any) {
	if !cacheDiagnosticsEnabled(opts) {
		return
	}
	params.Betas = append(params.Betas, anthropic.AnthropicBetaCacheDiagnosis2026_04_07)
	params.Diagnostics.PreviousMessageID = param.Null[string]()
	for _, message := range slices.Backward(messages) {
		state := message.ProviderState
		if message.Role == chat.MessageRoleAssistant && state != nil && state.Provider == providerStateName && state.MessageID != "" {
			params.Diagnostics.PreviousMessageID = anthropic.String(state.MessageID)
			return
		}
	}
}

func cacheDiagnostics(diagnostics anthropic.BetaDiagnostics) *chat.CacheDiagnostics {
	reason := diagnostics.CacheMissReason
	if reason.Type == "" {
		return nil
	}
	slog.Debug("Anthropic prompt cache diagnosis", "reason", reason.Type, "missed_input_tokens", reason.CacheMissedInputTokens)
	return &chat.CacheDiagnostics{Reason: reason.Type, MissedInputTokens: reason.CacheMissedInputTokens}
}
