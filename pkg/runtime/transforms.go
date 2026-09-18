package runtime

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
)

// MessageTransform is the in-process-only handler signature for a
// before_llm_call transform that rewrites the chat messages about to
// be sent to the model. It receives the full message slice in chain
// order and returns the (possibly-rewritten) replacement.
//
// Transforms are intentionally a runtime-private contract: the cost of
// JSON-roundtripping a full conversation through the cross-process
// hook protocol would be prohibitive, so command and model hooks
// cannot rewrite messages. Embedders register transforms via
// [WithMessageTransform]; the runtime ships
// [BuiltinStripUnsupportedModalities] out of the box.
//
// Transforms run AFTER the standard before_llm_call gate (see
// [LocalRuntime.executeBeforeLLMCallHooks]) — a hook that wants to
// abort the call should target the gate, not a transform.
//
// Returning a non-nil error logs a warning and falls through to the
// previous message slice; a transform failure must never break the
// run loop.
type MessageTransform func(ctx context.Context, in *hooks.Input, msgs []chat.Message) ([]chat.Message, error)

// registeredTransform pairs a [MessageTransform] with the name it was
// registered under. The name is purely diagnostic — it shows up in
// slog records when a transform errors out — so re-registering the
// same name simply appends another entry without any de-duplication.
type registeredTransform struct {
	name string
	fn   MessageTransform
}

// WithMessageTransform registers a [MessageTransform] under name so
// it is applied to every LLM call, in registration order, after the
// before_llm_call gate. Transforms are runtime-global: per-agent
// scoping (if needed) lives in the transform body, where
// [hooks.Input.AgentName] is available — the runtime-shipped strip
// transform is an example.
//
// Empty name or nil fn are silently ignored, matching the no-error
// shape of the other [Opt] helpers.
func WithMessageTransform(name string, fn MessageTransform) Opt {
	return func(r *LocalRuntime) {
		if name == "" || fn == nil {
			slog.Warn("Ignoring message transform with empty name or nil fn", "name", name)
			return
		}
		r.transforms = append(r.transforms, registeredTransform{name: name, fn: fn})
	}
}

// applyBeforeLLMCallTransforms runs every registered
// [MessageTransform] in chain order, just before the model call and
// AFTER [LocalRuntime.executeBeforeLLMCallHooks] has approved it.
// Errors from individual transforms are logged at warn level and the
// chain continues with the previous slice — a transform failure must
// never break the run loop.
//
// modelID is the canonical model identifier the loop has just
// resolved (after per-tool overrides and alloy-mode selection);
// transforms read it via [hooks.Input.ModelID]. Calling
// agent.Model() from a transform would re-randomize the alloy pick
// and miss the per-tool override.
//
// caps is the model's already-resolved attachment capability set
// (explicit `capabilities:` config override applied — see
// [modelinfo.ResolveCapsFromModel]); transforms read it via
// [hooks.Input.ModelCapabilities]. nil means the caller has no
// capability information (e.g. the coding-harness path) and
// capability-gated transforms must not act.
func (r *LocalRuntime) prepareMessagesForModel(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	model provider.Provider,
	msgs []chat.Message,
) []chat.Message {
	modelID := model.ID()
	catalogModel, err := r.modelsStore.GetModel(ctx, modelID)
	if err != nil {
		slog.DebugContext(ctx, "Failed to resolve model capabilities for message transforms", "model", modelID.String(), "error", err)
	}
	cfg := model.BaseConfig()
	caps := modelinfo.ResolveCapsFromModel(catalogModel, cfg.CapsOverride())
	if catalogModel == nil && cfg.CapsOverride() == nil {
		caps = providerFallbackCaps(ctx, cfg.ModelConfig, modelID)
	}
	return r.applyBeforeLLMCallTransforms(ctx, sess, a, modelID.String(), &caps, msgs)
}

func providerFallbackCaps(ctx context.Context, cfg latest.ModelConfig, id modelsdev.ID) modelinfo.ModelCapabilities {
	if cfg.Provider == "dmr" {
		return modelinfo.CapsWith(providerOptBool(cfg.ProviderOpts, "supports_images"), providerOptBool(cfg.ProviderOpts, "supports_pdf"), false, false)
	}
	if cfg.Provider == "anthropic" || modelinfo.IsClaude(ctx, nil, id) {
		return modelinfo.CapsWith(true, true, false, false)
	}
	return modelinfo.ModelCapabilities{}
}

func providerOptBool(opts map[string]any, key string) bool {
	switch value := opts[key].(type) {
	case bool:
		return value
	case string:
		parsed, err := strconv.ParseBool(value)
		return err == nil && parsed
	default:
		return false
	}
}

func (r *LocalRuntime) applyBeforeLLMCallTransforms(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	modelID string,
	caps *modelinfo.ModelCapabilities,
	msgs []chat.Message,
) []chat.Message {
	if len(r.transforms) == 0 {
		return msgs
	}
	in := &hooks.Input{
		SessionID:         sess.ID,
		AgentName:         a.Name(),
		ModelID:           modelID,
		ModelCapabilities: caps,
		HookEventName:     hooks.EventBeforeLLMCall,
		Cwd:               r.workingDir,
	}
	for _, t := range r.transforms {
		out, err := t.fn(ctx, in, msgs)
		if err != nil {
			slog.WarnContext(ctx, "Message transform failed; continuing with previous messages",
				"transform", t.name, "agent", a.Name(), "error", err)
			continue
		}
		msgs = out
	}
	return msgs
}
