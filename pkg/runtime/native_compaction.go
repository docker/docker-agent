package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime/compactor"
	"github.com/docker/docker-agent/pkg/runtime/pricing"
	"github.com/docker/docker-agent/pkg/session"
)

// nativeCompactionOpt is the provider_opts key that opts a model into
// provider-native compaction instead of the LLM summarization strategy.
const nativeCompactionOpt = "native_compaction"

// nativeCompactor returns the agent's model as a [contracts.ConversationCompactor]
// when provider_opts.native_compaction is set. It returns (nil, nil) when the
// opt is unset so the agent uses the LLM strategy.
//
// The opt is explicit, so a model that cannot honour it is a configuration
// error rather than a silent reroute to the LLM strategy. Likewise native
// compaction runs on the conversation model: the block it returns is only
// meaningful to that provider, so a dedicated compaction_model pointing
// elsewhere is rejected.
func nativeCompactor(ctx context.Context, a *agent.Agent) (contracts.ConversationCompactor, error) {
	model := a.Model(ctx)
	if model == nil {
		return nil, nil
	}
	if enabled, _ := providerutil.GetProviderOptBool(model.BaseConfig().ModelConfig.ProviderOpts, nativeCompactionOpt); !enabled {
		return nil, nil
	}
	native, ok := unwrapProvider(model).(contracts.ConversationCompactor)
	if !ok {
		return nil, fmt.Errorf("native_compaction is set on %s but the model does not support provider-native compaction", model.ID())
	}
	if cm := a.CompactionModel(); cm != nil && cm.ID() != model.ID() {
		return nil, fmt.Errorf("native_compaction on %s is incompatible with compaction_model %s: native compaction must run on the conversation model", model.ID(), cm.ID())
	}
	return native, nil
}

// unwrapProvider returns the leaf provider beneath instrumentation wrappers.
func unwrapProvider(p provider.Provider) provider.Provider {
	for {
		u, ok := p.(interface{ Unwrap() provider.Provider })
		if !ok {
			return p
		}
		p = u.Unwrap()
	}
}

// compactNatively asks the conversation model to compact the exact prompt it
// would receive next: the fully assembled messages (system prompts included)
// and the current request tools, with no canonical compaction prompt. The
// prompt goes through the same outbound pipeline as a model call, so
// before_llm_call hooks (e.g. redact_secrets) and runtime message transforms
// see it first. The whole prompt is compacted; FirstKeptEntry is the
// snapshot's item count so only items appended after the snapshot survive
// verbatim.
//
// Returns (nil, nil) when there is nothing to compact or the provider
// produced an empty summary; the session is left untouched either way.
func (r *LocalRuntime) compactNatively(ctx context.Context, sess *session.Session, a *agent.Agent, native contracts.ConversationCompactor, additionalPrompt string, events EventSink) (*compactor.Result, error) {
	if hasPendingCompactionTools(sess) {
		return nil, errors.New("native compaction must wait for pending tool results")
	}
	agentTools, err := r.getTools(ctx, sess, a, trace.SpanFromContext(ctx), events, false)
	if err != nil {
		return nil, fmt.Errorf("native compaction: %w", err)
	}
	instructions := sess.InstructionContextSnapshot()
	if err := r.checkStoredPromptFiles(ctx, sess, a, instructions); err != nil {
		return nil, err
	}
	messages, itemCount := sess.GetMessagesWithInstructionContext(a, instructions)
	if !slices.ContainsFunc(messages, func(m chat.Message) bool { return m.Role != chat.MessageRoleSystem }) {
		slog.WarnContext(ctx, "Compaction skipped: no conversation messages to compact", "session_id", sess.ID)
		return nil, nil
	}

	modelID := native.ID()
	// Iteration 0: this is not a run-loop iteration, so iteration-counting hooks stay out.
	stop, msg, rewritten := r.executeBeforeLLMCallHooks(ctx, sess, a, modelID.String(), 0, messages)
	if stop {
		return nil, fmt.Errorf("native compaction blocked by before_llm_call hook: %s", msg)
	}
	if rewritten != nil {
		messages = rewritten
	}
	messages = r.prepareMessagesForModel(ctx, sess, a, native, messages)

	agentTools = toolsForProvider(ctx, native, agentTools)
	r.ensureBudget()
	if breach := r.currentBudget().exceededFor(a.Name()); breach != nil {
		events.Emit(Warning(fmt.Sprintf("Compaction skipped: %s limit reached (used %s of %s).", breach.configPath(), breach.Used, breach.Max), a.Name()))
		return nil, errCompactionBudgetExceeded
	}
	started := r.now()
	res, err := native.CompactConversation(ctx, messages, agentTools, additionalPrompt)
	if err != nil {
		return nil, fmt.Errorf("native compaction: %w", err)
	}
	if res == nil {
		r.recordBudget(sess, a, nil, nil, r.now().Sub(started), events)
		return nil, nil
	}

	m, err := r.modelsStore.GetModel(ctx, modelID)
	if err != nil {
		slog.DebugContext(ctx, "Failed to get model definition for native compaction cost", "model_id", modelID.String(), "error", err)
		m = nil
	}
	messageCost := compactionCost(res, m, modelID, native.BaseConfig())
	r.recordBudget(sess, a, &res.Usage, messageCost, r.now().Sub(started), events)
	if strings.TrimSpace(res.Summary) == "" {
		slog.WarnContext(ctx, "Compaction skipped: native compaction produced no summary",
			"session_id", sess.ID, "model", modelID.String())
		return nil, nil
	}
	var cost float64
	if messageCost != nil {
		cost = *messageCost
	}

	summary := strings.TrimSpace(res.Summary)
	nativeResult := res.Clone()
	nativeResult.Summary = summary
	return &compactor.Result{
		Summary:        summary,
		FirstKeptEntry: itemCount,
		Cost:           cost,
		Model:          modelID.String(),
		Usage:          res.Usage,
		InputTokens: compaction.EstimateMessageTokens(&chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: summary,
		}),
		Compaction: nativeResult,
	}, nil
}

// compactionCost prices each compaction iteration as its own request, so a
// context tier is chosen per request and never on the summed usage. Nil when
// any iteration cannot be priced.
func compactionCost(res *chat.CompactionResult, m *modelsdev.Model, modelID modelsdev.ID, cfg base.Config) *float64 {
	iterations := res.Iterations
	if len(iterations) == 0 {
		iterations = []chat.Usage{res.Usage}
	}
	var total float64
	for i := range iterations {
		usage := iterations[i]
		cost := pricing.ComputeMessageCost(&usage, pricing.ApplyModelCost(m, modelID, &usage, cfg))
		if cost == nil {
			return nil
		}
		total += *cost
	}
	return &total
}

func hasPendingCompactionTools(sess *session.Session) bool {
	results := make(map[string]bool)
	for _, item := range slices.Backward(sess.MessagesSnapshot()) {
		if item.Summary != "" {
			break
		}
		if !item.IsMessage() {
			continue
		}
		message := item.Message.Message
		if message.Role == chat.MessageRoleTool {
			results[message.ToolCallID] = true
		}
		if message.Role != chat.MessageRoleAssistant {
			continue
		}
		for _, call := range message.ToolCalls {
			if !results[call.ID] {
				return true
			}
		}
		return false
	}
	return false
}
