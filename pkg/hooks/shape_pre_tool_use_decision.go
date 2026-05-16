package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// ShapePreToolUseDecision is the well-known schema name for an
// "LLM as a judge" pre_tool_use hook. The model is asked to reply with
//
//	{"decision":"allow|ask|deny","reason":"<short explanation>"}
//
// and the shape produces a [HookSpecificOutput.PermissionDecision]
// verdict the executor's pre_tool_use aggregator honors.
const ShapePreToolUseDecision = "pre_tool_use_decision"

// registerPreToolUseDecisionShape installs the shape and its strict
// JSON schema on the package-level registry. It is called via a
// package-level var initializer so the shape is available before any
// caller looks it up; using `init()` would trip the gochecknoinits
// linter, while `var _ = registerXxx()` keeps the side effect
// explicit at the declaration site.
//
// Returns true on success; panics on registry error since the inputs
// are static.
func registerPreToolUseDecisionShape() bool {
	if err := errors.Join(
		RegisterResponseShape(ShapePreToolUseDecision, preToolUseDecisionShape),
		RegisterResponseSchema(ShapePreToolUseDecision, preToolUseDecisionSchema),
	); err != nil {
		panic(fmt.Errorf("hooks: register %s: %w", ShapePreToolUseDecision, err))
	}
	return true
}

var _ = registerPreToolUseDecisionShape()

// preToolUseDecisionSchema is the strict JSON schema we ask compatible
// providers (OpenAI's structured-output, etc.) to honor. Providers that
// silently ignore it still work — the shape's parser is lenient enough
// to pull JSON out of fenced or surrounded text.
var preToolUseDecisionSchema = &latest.StructuredOutput{
	Name:        "pre_tool_use_decision",
	Description: "Verdict for a single pre_tool_use call",
	Strict:      true,
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"decision": map[string]any{
				"type": "string",
				"enum": []string{"allow", "ask", "deny"},
			},
			"reason": map[string]any{"type": "string"},
		},
		"required":             []string{"decision", "reason"},
		"additionalProperties": false,
	},
}

// preToolUseDecisionShape parses the model's reply and produces the
// nested [HookSpecificOutput] the executor honors as a verdict.
//
// It is deliberately tolerant of providers that wrap JSON in markdown
// fences or surround it with prose, but rejects unparseable text by
// returning an error — which the executor maps to fail-closed (deny)
// per the documented PreToolUse contract.
func preToolUseDecisionShape(raw string, in *Input) (*Output, error) {
	body, err := extractJSONObject(raw)
	if err != nil {
		return nil, err
	}
	var v struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("parse decision json: %w", err)
	}
	d, err := normalizeDecision(v.Decision)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(v.Reason)
	if reason == "" {
		reason = "no reason provided"
	}
	event := EventPreToolUse
	if in != nil && in.HookEventName != "" {
		event = in.HookEventName
	}
	return &Output{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:            event,
			PermissionDecision:       d,
			PermissionDecisionReason: reason,
		},
		// Surface the verdict in the UI — auto-approvals shouldn't be
		// silent. Mirrors the convention used by the legacy llm_judge
		// builtin so existing prompts/transcripts don't change.
		SystemMessage: "🤖 model judge: " + string(d) + " — " + reason,
	}, nil
}

// extractJSONObject returns the bytes of the first top-level JSON
// object inside raw. It strips a leading ```json (or plain ```) fence
// and trims surrounding prose so the standard library parser sees
// clean input. Returns an error for empty input or no `{`.
func extractJSONObject(raw string) ([]byte, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errors.New("empty model reply")
	}
	s = stripCodeFence(s)
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return nil, errors.New("no JSON object in reply")
	}
	return []byte(s[start : end+1]), nil
}

// stripCodeFence removes a leading ```json (or plain ```) fence and
// the matching trailing fence so the JSON parser sees clean input.
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	}
	s = strings.TrimSuffix(strings.TrimRight(s, " \n\t"), "```")
	return strings.TrimSpace(s)
}

// normalizeDecision validates and lower-cases a decision string.
func normalizeDecision(s string) (Decision, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(DecisionAllow):
		return DecisionAllow, nil
	case string(DecisionAsk):
		return DecisionAsk, nil
	case string(DecisionDeny):
		return DecisionDeny, nil
	default:
		return "", fmt.Errorf("invalid decision %q (want allow|ask|deny)", s)
	}
}
