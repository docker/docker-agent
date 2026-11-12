package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
)

// cachePreservingUpdatesOpt is the provider_opts key that opts a model into
// mid-conversation system messages, tool changes and per-message effort.
//
// The first request of a conversation fixes a baseline: the top-level
// `system`, `tools` and `output_config.effort`. Later requests resend that
// baseline byte for byte and express what changed since as `role: system`
// messages appended after the last user turn, so the cached prefix keeps
// matching. Each assistant reply records the update that preceded it in
// ProviderState.RequestContext; the next request replays every recorded
// update at its original position. A change that has no cache-preserving
// form (a rewritten system block, an edited tool definition, an effort
// change on a model without per-message effort or back to the model
// default) resets the baseline to the current request instead, which is
// exactly the request sent without this option: semantics are never
// sacrificed for the cache.
const cachePreservingUpdatesOpt = "cache_preserving_updates"

func cachePreservingUpdatesEnabled(opts map[string]any) bool {
	enabled, _ := providerutil.GetProviderOptBool(opts, cachePreservingUpdatesOpt)
	return enabled
}

// validateCachePreservingUpdates rejects the option on models that do not
// accept mid-conversation system messages, so a bad config fails at client
// construction rather than with a 400 on the second turn. Server-side
// fallback models receive the same request shape and are validated too.
func validateCachePreservingUpdates(cfg *latest.ModelConfig) error {
	if !cachePreservingUpdatesEnabled(cfg.ProviderOpts) {
		return nil
	}
	fallbacks := fallbackTargets(cfg.ProviderOpts)
	for _, model := range append([]string{cfg.Model}, fallbacks...) {
		if !supportsConversationUpdates(model) {
			return fmt.Errorf("anthropic: model %q does not support %s (requires Claude Opus 4.8+, Fable 5+ or Mythos 5+)", model, cachePreservingUpdatesOpt)
		}
	}
	for _, model := range fallbacks {
		if supportsPerMessageEffort(cfg.Model) && !supportsPerMessageEffort(model) {
			return fmt.Errorf("anthropic: cache_preserving_updates requires fallback %q to support per-message effort", model)
		}
	}
	return nil
}

// supportsConversationUpdates reports whether the model accepts
// mid-conversation system messages and tool changes: Claude Opus 4.8 and
// 5, Fable 5 and 5.1, Mythos 5 and 5.1. Sonnet 5 and Haiku do not.
func supportsConversationUpdates(model string) bool {
	m, ok := parseClaudeModel(model)
	return ok && (m.atLeast("opus", 4, 8) || m.atLeast("fable", 5, 0) || m.atLeast("mythos", 5, 0))
}

// supportsPerMessageEffort reports whether the model accepts
// `output_config.effort` on a system message: Claude Opus 5, Fable 5.1 and
// Mythos 5.1. Other models get a top-level effort change (a baseline reset).
// An unset effort is the model's own default, which differs between models
// and is never assumed to equal a named level.
func supportsPerMessageEffort(model string) bool {
	m, ok := parseClaudeModel(model)
	return ok && (m.atLeast("opus", 5, 0) || m.atLeast("fable", 5, 1) || m.atLeast("mythos", 5, 1))
}

// conversationContextVersion is bumped when the persisted layout changes;
// contexts of another version are ignored, which just costs one cache miss.
const conversationContextVersion = 1

// conversationContext is the per-turn record persisted on an assistant
// message (ProviderState.RequestContext) or a compaction summary
// (CompactionResult.RequestContext). Baseline is set on the turn that fixed
// or reset the request prefix; Update is the system message inserted right
// before this assistant turn. The history is the sum of these records: no
// message snapshots, so storage grows with the number of changes, not with
// the square of the conversation length.
type conversationContext struct {
	Version  int              `json:"v"`
	Baseline *requestBaseline `json:"baseline,omitempty"`
	Update   *updateRecord    `json:"update,omitempty"`
}

// requestBaseline is the request prefix replayed verbatim on every later
// turn: the top-level system blocks and tool definitions as raw JSON
// (cache_control and defer_loading stripped from tools: the breakpoint
// budget stays with the system blocks and the message tail, and baseline
// tools are always loaded) and the top-level effort.
type requestBaseline struct {
	System []json.RawMessage `json:"system,omitempty"`
	Tools  []json.RawMessage `json:"tools,omitempty"`
	Effort string            `json:"effort,omitempty"`
}

// updateRecord is one mid-conversation update. DeclareTools are definitions
// appended to the top-level tools array with defer_loading so they stay
// withheld until the matching AddTools entry surfaces them. Transient texts
// are turn-scoped (clear_at: next_user_message) and resent verbatim forever.
type updateRecord struct {
	Texts        []string          `json:"texts,omitempty"`
	DeclareTools []json.RawMessage `json:"declare_tools,omitempty"`
	AddTools     []string          `json:"add_tools,omitempty"`
	RemoveTools  []string          `json:"remove_tools,omitempty"`
	Effort       string            `json:"effort,omitempty"`
	Transient    []string          `json:"transient,omitempty"`
}

func (u *updateRecord) empty() bool {
	return u == nil || len(u.Texts) == 0 && len(u.AddTools) == 0 && len(u.RemoveTools) == 0 && u.Effort == "" && len(u.Transient) == 0
}

// hasContent reports whether the update carries content blocks, which are
// bound by the placement rule (after a user turn, before an assistant turn
// or at the end). An effort-only update is accepted anywhere.
func (u *updateRecord) hasContent() bool {
	return u != nil && (len(u.Texts) > 0 || len(u.AddTools) > 0 || len(u.RemoveTools) > 0 || len(u.Transient) > 0)
}

// messages renders the update as wire messages: one persistent system
// message with text, tool_addition and tool_removal blocks plus the effort
// change, then one turn-scoped message for the transient texts.
func (u *updateRecord) messages() []anthropic.BetaMessageParam {
	if u.empty() {
		return nil
	}
	var out []anthropic.BetaMessageParam
	var blocks []anthropic.BetaContentBlockParamUnion
	for _, text := range u.Texts {
		blocks = append(blocks, anthropic.NewBetaTextBlock(text))
	}
	for _, name := range u.AddTools {
		blocks = append(blocks, anthropic.NewBetaToolAdditionBlock(anthropic.BetaToolChangeToolReferenceParam{Name: name}))
	}
	for _, name := range u.RemoveTools {
		blocks = append(blocks, anthropic.NewBetaToolRemovalBlock(anthropic.BetaToolChangeToolReferenceParam{Name: name}))
	}
	if len(blocks) > 0 || u.Effort != "" {
		out = append(out, anthropic.NewBetaSystemMessage(
			anthropic.BetaSystemMessageOutputConfigParam{Effort: anthropic.BetaSystemMessageOutputConfigEffort(u.Effort)},
			blocks...))
	}
	if len(u.Transient) > 0 {
		var transient []anthropic.BetaContentBlockParamUnion
		for _, text := range u.Transient {
			transient = append(transient, anthropic.NewBetaTextBlock(text))
		}
		out = append(out, anthropic.BetaMessageParam{
			Role:    anthropic.BetaMessageParamRoleSystem,
			ClearAt: anthropic.BetaMessageParamClearAtNextUserMessage,
			Content: transient,
		})
	}
	return out
}

// declaredTool is a tool definition as sent in the top-level tools array.
// key is the definition without cache_control and defer_loading, so a tool
// keeps matching itself when the runtime later marks it deferred or moves
// the tool-list cache breakpoint.
type declaredTool struct {
	name, key string
	raw       json.RawMessage
}

// normalizeTool canonicalizes one tool definition. The chain alone decides
// deferral: baseline tools are sent loaded, tools declared after the
// baseline are deferred until their tool_addition. The runtime's own
// defer_loading flag is dropped, since the tool_reference that would load
// it can vanish from the history (restart, edit, compaction) while the
// recorded definition stays.
func normalizeTool(raw json.RawMessage, deferred bool) (declaredTool, error) {
	var def map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&def); err != nil {
		return declaredTool{}, fmt.Errorf("decoding tool definition: %w", err)
	}
	name, _ := def["name"].(string)
	if name == "" {
		return declaredTool{}, errors.New("tool definition has no name")
	}
	delete(def, "cache_control")
	delete(def, "defer_loading")
	key, err := json.Marshal(def)
	if err != nil {
		return declaredTool{}, err
	}
	if deferred {
		def["defer_loading"] = true
	}
	stored, err := json.Marshal(def)
	if err != nil {
		return declaredTool{}, err
	}
	return declaredTool{name: name, key: string(key), raw: stored}, nil
}

func systemBlockText(raw json.RawMessage) (string, error) {
	var block struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return "", fmt.Errorf("decoding system block: %w", err)
	}
	return block.Text, nil
}

// requestSnapshot is the current request as the caller built it, in the
// form the planner compares and replays.
type requestSnapshot struct {
	system []json.RawMessage
	texts  []string
	tools  []declaredTool
	effort string
}

func snapshotRequest(params *anthropic.BetaMessageNewParams) (*requestSnapshot, error) {
	snap := &requestSnapshot{effort: string(params.OutputConfig.Effort)}
	for i := range params.System {
		raw, err := params.System[i].MarshalJSON()
		if err != nil {
			return nil, err
		}
		text, err := systemBlockText(raw)
		if err != nil {
			return nil, err
		}
		snap.system = append(snap.system, raw)
		snap.texts = append(snap.texts, text)
	}
	for i := range params.Tools {
		raw, err := params.Tools[i].MarshalJSON()
		if err != nil {
			return nil, err
		}
		tool, err := normalizeTool(raw, false)
		if err != nil {
			return nil, err
		}
		snap.tools = append(snap.tools, tool)
	}
	return snap, nil
}

func (s *requestSnapshot) baseline() *requestBaseline {
	b := &requestBaseline{System: s.system, Effort: s.effort}
	for _, t := range s.tools {
		b.Tools = append(b.Tools, t.raw)
	}
	return b
}

// effectiveState is the request prefix the model currently sees: the
// baseline with every recorded update applied.
type effectiveState struct {
	baseline *requestBaseline
	texts    []string
	declared []declaredTool
	offered  map[string]bool
	effort   string
}

func newEffectiveState(b *requestBaseline) (*effectiveState, error) {
	eff := &effectiveState{baseline: b, offered: make(map[string]bool, len(b.Tools)), effort: b.Effort}
	for _, raw := range b.System {
		text, err := systemBlockText(raw)
		if err != nil {
			return nil, err
		}
		eff.texts = append(eff.texts, text)
	}
	for _, raw := range b.Tools {
		if err := eff.declare(raw, false); err != nil {
			return nil, err
		}
	}
	return eff, nil
}

func (e *effectiveState) declare(raw json.RawMessage, deferred bool) error {
	tool, err := normalizeTool(raw, deferred)
	if err != nil {
		return err
	}
	if e.isDeclared(tool.name) {
		return fmt.Errorf("tool %q declared twice", tool.name)
	}
	e.declared = append(e.declared, tool)
	e.offered[tool.name] = true
	return nil
}

func (e *effectiveState) isDeclared(name string) bool {
	return slices.ContainsFunc(e.declared, func(t declaredTool) bool { return t.name == name })
}

// apply folds one update into the state. It rejects an update that does
// not fit the state it is applied to (a record made against an older
// baseline): replaying it would send a tool change the API rejects.
func (e *effectiveState) apply(u *updateRecord) error {
	e.texts = append(e.texts, u.Texts...)
	for _, raw := range u.DeclareTools {
		if err := e.declare(raw, true); err != nil {
			return err
		}
	}
	for _, name := range u.AddTools {
		if !e.isDeclared(name) {
			return fmt.Errorf("tool %q added without a declaration", name)
		}
		e.offered[name] = true
	}
	for _, name := range u.RemoveTools {
		if !e.offered[name] {
			return fmt.Errorf("tool %q removed while not offered", name)
		}
		delete(e.offered, name)
	}
	if u.Effort != "" {
		e.effort = u.Effort
	}
	return nil
}

// plan computes the update that turns the effective state into the current
// request. A non-empty reason means the change has no cache-preserving form.
func (e *effectiveState) plan(model string, cur *requestSnapshot) (update *updateRecord, reason string) {
	u := &updateRecord{}

	n := len(e.texts)
	if len(cur.texts) < n || !slices.Equal(cur.texts[:n], e.texts) {
		return nil, "system prompt changed beyond appending blocks"
	}
	u.Texts = cur.texts[n:]

	declared := make(map[string]declaredTool, len(e.declared))
	for _, t := range e.declared {
		declared[t.name] = t
	}
	current := make(map[string]bool, len(cur.tools))
	for _, t := range cur.tools {
		current[t.name] = true
		d, ok := declared[t.name]
		switch {
		case !ok:
			deferred, err := normalizeTool(t.raw, true)
			if err != nil {
				return nil, err.Error()
			}
			u.DeclareTools = append(u.DeclareTools, deferred.raw)
			u.AddTools = append(u.AddTools, t.name)
		case d.key != t.key:
			return nil, fmt.Sprintf("definition of tool %q changed", t.name)
		case !e.offered[t.name]:
			u.AddTools = append(u.AddTools, t.name)
		}
	}
	for _, t := range e.declared {
		if e.offered[t.name] && !current[t.name] {
			u.RemoveTools = append(u.RemoveTools, t.name)
		}
	}

	if cur.effort != e.effort {
		switch {
		case cur.effort == "":
			// The model default is not a level a system message can name.
			return nil, fmt.Sprintf("effort %q unset, back to the model default", e.effort)
		case !supportsPerMessageEffort(model):
			return nil, fmt.Sprintf("effort changed from %q to %q on a model without per-message effort", e.effort, cur.effort)
		}
		u.Effort = cur.effort
	}
	return u, ""
}

// chainRecord is an update recorded on the assistant message at index.
type chainRecord struct {
	index  int
	update *updateRecord
}

// conversationChain is the state recovered from the persisted history: the
// latest baseline and the updates recorded after it, in order. Records
// before the latest baseline were superseded by it and are dropped.
type conversationChain struct {
	baseline *requestBaseline
	records  []chainRecord
}

func loadConversationChain(ctx context.Context, messages []chat.Message) conversationChain {
	var chain conversationChain
	for i := range messages {
		msg := &messages[i]
		var raw json.RawMessage
		switch {
		case msg.Role == chat.MessageRoleAssistant && msg.ProviderState != nil && msg.ProviderState.Provider == providerStateName:
			raw = msg.ProviderState.RequestContext
		case msg.Compaction != nil && msg.Compaction.Provider == providerStateName:
			raw = msg.Compaction.RequestContext
		}
		if len(raw) == 0 {
			continue
		}
		var cc conversationContext
		if err := json.Unmarshal(raw, &cc); err != nil {
			slog.DebugContext(ctx, "Anthropic: ignoring malformed conversation context", "index", i, "error", err)
			continue
		}
		if cc.Version != conversationContextVersion {
			slog.DebugContext(ctx, "Anthropic: ignoring conversation context of another version", "index", i, "version", cc.Version)
			continue
		}
		if cc.Baseline != nil {
			chain.baseline = cc.Baseline
			chain.records = nil
		}
		if !cc.Update.empty() && msg.Role == chat.MessageRoleAssistant {
			chain.records = append(chain.records, chainRecord{index: i, update: cc.Update})
		}
	}
	return chain
}

// conversationUpdateResult is what the caller persists after the request.
type conversationUpdateResult struct {
	// RequestContext goes on the assistant reply's ProviderState (see
	// withRequestContext). Nil when this turn recorded nothing.
	RequestContext json.RawMessage
	// CompactionContext goes on the CompactionResult when this request is a
	// compaction. It is a fresh baseline: the request prefix as the model
	// saw it at compaction time, with every recorded update folded into the
	// top-level system blocks, tools and effort. The continuation from the
	// summary starts its cached prefix over (the compaction block replaces
	// the history, so there is no prefix left to preserve) and replays none
	// of the updates recorded before the compaction.
	CompactionContext json.RawMessage
	// Reset reports that the change could not be expressed as an update and
	// the baseline was restarted from this request (one cache miss).
	Reset bool
}

// applyConversationUpdates rewrites params for cache-preserving updates.
// Call it once params carry the converted messages, system blocks, tools
// and thinking/effort config. It replaces params.System, params.Tools,
// params.OutputConfig.Effort and params.Messages (a fresh slice; the
// caller's slices and messages are not mutated) and appends the beta
// headers the resulting messages need. transient texts are turn-scoped
// reminders for this turn only; they require the conversation to end with
// a user turn.
func applyConversationUpdates(ctx context.Context, model string, params *anthropic.BetaMessageNewParams, messages []chat.Message, transient []string) (*conversationUpdateResult, error) {
	cur, err := snapshotRequest(params)
	if err != nil {
		return nil, fmt.Errorf("anthropic %s: %w", cachePreservingUpdatesOpt, err)
	}
	tailAcceptsSystem := len(params.Messages) > 0 && params.Messages[len(params.Messages)-1].Role != anthropic.BetaMessageParamRoleAssistant
	if len(transient) > 0 && !tailAcceptsSystem {
		return nil, fmt.Errorf("anthropic %s: turn-scoped system messages require the conversation to end with a user turn", cachePreservingUpdatesOpt)
	}

	result := &conversationUpdateResult{}
	if result.CompactionContext, err = json.Marshal(conversationContext{Version: conversationContextVersion, Baseline: cur.baseline()}); err != nil {
		return nil, err
	}

	chain := loadConversationChain(ctx, messages)
	if chain.baseline == nil {
		return startBaseline(params, cur, transient, result)
	}

	eff, err := newEffectiveState(chain.baseline)
	if err != nil {
		return reset(ctx, params, cur, transient, result, err.Error())
	}
	inserts, ok := alignRecords(messages, params.Messages, chain.records)
	if !ok {
		return reset(ctx, params, cur, transient, result, "recorded updates do not align with the converted history")
	}
	for _, rec := range chain.records {
		// Records are model-agnostic except per-message effort: a session
		// switched to a model without it must not replay one.
		if rec.update.Effort != "" && !supportsPerMessageEffort(model) {
			return reset(ctx, params, cur, transient, result, fmt.Sprintf("recorded per-message effort %q on a model without per-message effort", rec.update.Effort))
		}
		if err := eff.apply(rec.update); err != nil {
			return reset(ctx, params, cur, transient, result, err.Error())
		}
	}
	update, reason := eff.plan(model, cur)
	if reason != "" {
		return reset(ctx, params, cur, transient, result, reason)
	}
	if update.hasContent() && !tailAcceptsSystem {
		return reset(ctx, params, cur, transient, result, "conversation does not end with a user turn")
	}
	update.Transient = transient
	if err := eff.apply(update); err != nil {
		return reset(ctx, params, cur, transient, result, err.Error())
	}

	out := make([]anthropic.BetaMessageParam, 0, len(params.Messages)+len(chain.records)*2+2)
	for i, msg := range params.Messages {
		out = append(out, inserts[i]...)
		out = append(out, msg)
	}
	params.Messages = append(out, update.messages()...)
	applyBaseline(params, eff.baseline, eff.declared)
	if !update.empty() {
		if result.RequestContext, err = json.Marshal(conversationContext{Version: conversationContextVersion, Update: update}); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// reset restarts the baseline from the current request after logging why
// the change had no cache-preserving form.
func reset(ctx context.Context, params *anthropic.BetaMessageNewParams, cur *requestSnapshot, transient []string, result *conversationUpdateResult, reason string) (*conversationUpdateResult, error) {
	slog.WarnContext(ctx, "Anthropic: restarting the cached request prefix", "reason", reason)
	result.Reset = true
	return startBaseline(params, cur, transient, result)
}

// startBaseline sends the current request as the new baseline: the same
// bytes the caller built, minus tool cache_control, plus this turn's
// transient reminders.
func startBaseline(params *anthropic.BetaMessageNewParams, cur *requestSnapshot, transient []string, result *conversationUpdateResult) (*conversationUpdateResult, error) {
	cc := conversationContext{Version: conversationContextVersion, Baseline: cur.baseline()}
	if len(transient) > 0 {
		cc.Update = &updateRecord{Transient: transient}
		params.Messages = slices.Concat(params.Messages, cc.Update.messages())
	}
	applyBaseline(params, cc.Baseline, cur.tools)
	var err error
	if result.RequestContext, err = json.Marshal(cc); err != nil {
		return nil, err
	}
	return result, nil
}

// applyBaseline installs the baseline prefix verbatim and the beta headers
// the messages need.
func applyBaseline(params *anthropic.BetaMessageNewParams, b *requestBaseline, declared []declaredTool) {
	params.System = nil
	for _, raw := range b.System {
		params.System = append(params.System, param.Override[anthropic.BetaTextBlockParam](raw))
	}
	params.Tools = nil
	for _, t := range declared {
		params.Tools = append(params.Tools, param.Override[anthropic.BetaToolUnionParam](t.raw))
	}
	params.OutputConfig.Effort = anthropic.BetaOutputConfigEffort(b.Effort)
	params.Betas = appendConversationBetas(params.Betas, params.Messages)
}

// appendConversationBetas adds the beta header for each feature the system
// messages actually use: tool changes, per-message effort, clear_at.
func appendConversationBetas(betas []anthropic.AnthropicBeta, messages []anthropic.BetaMessageParam) []anthropic.AnthropicBeta {
	var toolChanges, effort, clearAt bool
	for _, msg := range messages {
		if msg.Role != anthropic.BetaMessageParamRoleSystem {
			continue
		}
		effort = effort || msg.OutputConfig.Effort != ""
		clearAt = clearAt || msg.ClearAt != ""
		for _, block := range msg.Content {
			toolChanges = toolChanges || block.OfToolAddition != nil || block.OfToolRemoval != nil
		}
	}
	for _, b := range []struct {
		beta   anthropic.AnthropicBeta
		needed bool
	}{
		{anthropic.AnthropicBetaMidConversationToolChanges2026_07_01, toolChanges},
		{anthropic.AnthropicBetaMidConversationOutputConfig2026_07_01, effort},
		{anthropic.AnthropicBetaMidConversationSystemClearAt2026_08_21, clearAt},
	} {
		if b.needed && !slices.Contains(betas, b.beta) {
			betas = append(betas, b.beta)
		}
	}
	return betas
}

// alignRecords maps each recorded update to the converted message it must
// precede. Chat and converted assistant turns are paired in order by their
// visible content (text and tool_use ids), which both replay and the
// flattened fallback preserve. Turns with no visible content are left out
// on both sides, so a native compaction block (an assistant message with
// no chat assistant behind it) or a thinking-only reply the converter
// dropped cannot shift the pairing. It fails when the visible turns do not
// pair one-to-one, when a recorded turn has no counterpart, or when the
// insertion point does not follow a user turn.
func alignRecords(messages []chat.Message, converted []anthropic.BetaMessageParam, records []chainRecord) (map[int][]anthropic.BetaMessageParam, bool) {
	if len(records) == 0 {
		return nil, true
	}
	var chatTurns []int
	var chatFingerprints []string
	for i := range messages {
		if fp := chatFingerprint(&messages[i]); messages[i].Role == chat.MessageRoleAssistant && fp != "" {
			chatTurns = append(chatTurns, i)
			chatFingerprints = append(chatFingerprints, fp)
		}
	}
	var convTurns []int
	var convFingerprints []string
	for j := range converted {
		if fp := convertedFingerprint(converted[j].Content); converted[j].Role == anthropic.BetaMessageParamRoleAssistant && fp != "" {
			convTurns = append(convTurns, j)
			convFingerprints = append(convFingerprints, fp)
		}
	}
	if !slices.Equal(chatFingerprints, convFingerprints) {
		return nil, false
	}
	position := make(map[int]int, len(chatTurns))
	for k, i := range chatTurns {
		position[i] = convTurns[k]
	}

	inserts := make(map[int][]anthropic.BetaMessageParam, len(records))
	for _, rec := range records {
		j, ok := position[rec.index]
		if !ok || j == 0 || converted[j-1].Role == anthropic.BetaMessageParamRoleAssistant {
			return nil, false
		}
		inserts[j] = append(inserts[j], rec.update.messages()...)
	}
	return inserts, true
}

// chatFingerprint and convertedFingerprint render the visible content of an
// assistant turn identically on both sides: the text followed by one
// tool_use id per call, in order. Empty means no visible content.
func chatFingerprint(msg *chat.Message) string {
	var sb strings.Builder
	sb.WriteString(msg.Content)
	for _, call := range msg.ToolCalls {
		sb.WriteString("\x00")
		sb.WriteString(call.ID)
	}
	return sb.String()
}

func convertedFingerprint(content []anthropic.BetaContentBlockParamUnion) string {
	var text, ids strings.Builder
	for _, block := range content {
		switch {
		case block.OfText != nil:
			text.WriteString(block.OfText.Text)
		case block.OfToolUse != nil:
			ids.WriteString("\x00")
			ids.WriteString(block.OfToolUse.ID)
		}
	}
	return text.String() + ids.String()
}

// withRequestContext returns a copy of state carrying requestContext,
// allocating a content-less state when the response produced none (replay
// then falls back to the flattened fields). Nil in, nil out when there is
// nothing to record.
func withRequestContext(state *chat.ProviderState, requestContext json.RawMessage) *chat.ProviderState {
	if len(requestContext) == 0 {
		return state
	}
	sealed := chat.ProviderState{Provider: providerStateName, Content: json.RawMessage("[]")}
	if state != nil {
		sealed = *state
	}
	sealed.RequestContext = requestContext
	return &sealed
}
