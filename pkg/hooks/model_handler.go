package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"text/template"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// ModelClient is the runtime-provided seam between [HookTypeModel]
// hooks and the LLM provider stack. It receives a model spec
// (provider/model), a system+user prompt pair, and the structured-output
// schema (or nil for free-form text), and returns the model's text
// reply.
//
// Implementations are responsible for credential lookup, provider
// construction, streaming, and assembling the final string. The hook
// machinery only consumes the returned text plus any error.
//
// A non-nil error is treated as a hook failure and routed through the
// executor's fail-closed semantics (deny on PreToolUse). Returning an
// empty string with no error is treated as an unparseable response by
// downstream [ResponseShape] interpreters.
type ModelClient interface {
	Ask(ctx context.Context, modelSpec, system, user string, schema *latest.StructuredOutput) (string, error)
}

// ResponseShape interprets a raw model reply as a hook [Output] for a
// well-known schema name (registered via [RegisterResponseShape]). It
// is paired with an optional [latest.StructuredOutput] (registered via
// [RegisterResponseSchema]) that the [ModelClient] requests from the
// provider — turning best-effort JSON parsing into a strict contract on
// providers that honor it.
//
// The empty schema name uses [defaultShape], which surfaces the reply
// verbatim as additional_context.
type ResponseShape func(raw string, in *Input) (*Output, error)

// modelRegistry holds the response-shape and structured-output
// registries. They are package-level by design so the runtime can
// register a shape once and every Executor that uses [DefaultRegistry]
// (or any other registry sharing the same package state) sees it. The
// process-wide default is harmless because shapes are pure functions
// of (raw, input).
var modelRegistry = struct {
	mu      sync.RWMutex
	shapes  map[string]ResponseShape
	schemas map[string]*latest.StructuredOutput
}{
	shapes:  map[string]ResponseShape{},
	schemas: map[string]*latest.StructuredOutput{},
}

// RegisterResponseShape registers a [ResponseShape] under name. The
// empty name is reserved for the default "additional_context" shape
// shipped in this package and cannot be re-registered. Registering
// the same name twice replaces the previous shape.
func RegisterResponseShape(name string, shape ResponseShape) error {
	if name == "" {
		return errors.New("response shape name must not be empty")
	}
	if shape == nil {
		return errors.New("response shape must not be nil")
	}
	modelRegistry.mu.Lock()
	defer modelRegistry.mu.Unlock()
	modelRegistry.shapes[name] = shape
	return nil
}

// RegisterResponseSchema associates a [latest.StructuredOutput] with a
// shape name so the [ModelClient] can request strict JSON output from
// providers that honor it.
func RegisterResponseSchema(name string, schema *latest.StructuredOutput) error {
	if name == "" {
		return errors.New("response schema name must not be empty")
	}
	modelRegistry.mu.Lock()
	defer modelRegistry.mu.Unlock()
	modelRegistry.schemas[name] = schema
	return nil
}

// lookupShape returns the [ResponseShape] for name, falling back to
// [defaultShape] for the empty name.
func lookupShape(name string) (ResponseShape, bool) {
	if name == "" {
		return defaultShape, true
	}
	modelRegistry.mu.RLock()
	defer modelRegistry.mu.RUnlock()
	s, ok := modelRegistry.shapes[name]
	return s, ok
}

// lookupSchema returns the structured-output schema for name, or nil
// when the shape is free-form (e.g. the default additional_context).
func lookupSchema(name string) *latest.StructuredOutput {
	if name == "" {
		return nil
	}
	modelRegistry.mu.RLock()
	defer modelRegistry.mu.RUnlock()
	return modelRegistry.schemas[name]
}

// defaultShape passes the model's reply through as additional_context.
// Useful for turn_start summarizers, post_tool_use commentary, etc. —
// any event where the runtime consumes AdditionalContext.
func defaultShape(raw string, in *Input) (*Output, error) {
	if in == nil {
		return nil, nil
	}
	return NewAdditionalContextOutput(in.HookEventName, strings.TrimSpace(raw)), nil
}

// NewModelFactory returns a [HandlerFactory] for [HookTypeModel] backed
// by client. Register it with [Registry.Register] in the runtime to
// enable `type: model` hooks.
//
// The returned factory pre-parses the prompt template at factory time
// so syntax errors surface at registry-lookup, not on the first hook
// invocation. The shape/schema lookup happens at handler-construction
// time so adding new shapes after factory registration still works.
func NewModelFactory(client ModelClient) HandlerFactory {
	if client == nil {
		// The empty client always errors; this lets a runtime register
		// the factory without a credentialed client and fail at the
		// first use rather than at construction.
		client = nilClient{}
	}
	return func(_ HandlerEnv, hook Hook) (Handler, error) {
		if hook.Model == "" {
			return nil, errors.New("model hook requires a non-empty model")
		}
		if hook.Prompt == "" {
			return nil, errors.New("model hook requires a non-empty prompt")
		}
		shape, ok := lookupShape(hook.Schema)
		if !ok {
			return nil, fmt.Errorf("model hook: unknown schema %q (register one with hooks.RegisterResponseShape)", hook.Schema)
		}
		tpl, err := template.New("hook-prompt").Funcs(promptFuncs).Parse(hook.Prompt)
		if err != nil {
			return nil, fmt.Errorf("model hook: parse prompt template: %w", err)
		}
		return &modelHandler{
			client: client,
			model:  hook.Model,
			tpl:    tpl,
			schema: lookupSchema(hook.Schema),
			shape:  shape,
		}, nil
	}
}

// promptFuncs is the [text/template] FuncMap exposed to model-hook
// prompts. Kept small on purpose: anything more elaborate belongs in
// the prompt itself.
var promptFuncs = template.FuncMap{
	// toJSON renders any value as compact JSON. Useful for embedding
	// .ToolInput / .ToolResponse in the user prompt without writing
	// custom Go-template formatting.
	"toJSON": func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	},
	// truncate caps a string at n bytes, appending "…" when cut. A
	// safety net for prompts that interpolate large tool inputs.
	"truncate": func(n int, s string) string {
		if n <= 0 || len(s) <= n {
			return s
		}
		return s[:n] + "…"
	},
}

// modelSystemPrompt is the fixed system message paired with every
// rendered user prompt. It documents the JSON-only contract that
// [ResponseShape]s expect.
const modelSystemPrompt = `You are a hook running inside an autonomous agent.
Follow the user's instructions exactly and reply ONLY with content the
hook contract expects: when a JSON schema is supplied, return a single
JSON object matching it with no surrounding prose or markdown fences;
otherwise return concise free-form text suitable for direct injection
as additional context.`

type modelHandler struct {
	client ModelClient
	model  string
	tpl    *template.Template
	schema *latest.StructuredOutput
	shape  ResponseShape
}

// Run implements [Handler]. It renders the prompt, asks the model, and
// applies the configured [ResponseShape]. The executor's timeout and
// fail-closed semantics are unchanged from any other handler.
func (h *modelHandler) Run(ctx context.Context, input []byte) (HandlerResult, error) {
	var in Input
	if err := json.Unmarshal(input, &in); err != nil {
		return HandlerResult{ExitCode: -1}, fmt.Errorf("decode hook input: %w", err)
	}
	var buf bytes.Buffer
	if err := h.tpl.Execute(&buf, &in); err != nil {
		return HandlerResult{ExitCode: -1}, fmt.Errorf("render prompt: %w", err)
	}
	raw, err := h.client.Ask(ctx, h.model, modelSystemPrompt, buf.String(), h.schema)
	if err != nil {
		return HandlerResult{ExitCode: -1}, fmt.Errorf("model %s: %w", h.model, err)
	}
	out, err := h.shape(raw, &in)
	if err != nil {
		return HandlerResult{ExitCode: -1}, fmt.Errorf("interpret reply: %w", err)
	}
	return HandlerResult{Output: out}, nil
}

// nilClient is the placeholder used when [NewModelFactory] is called
// without a real client. It keeps the factory constructible but errors
// at the first dispatch, surfacing the misconfiguration through the
// normal fail-closed path.
type nilClient struct{}

func (nilClient) Ask(context.Context, string, string, string, *latest.StructuredOutput) (string, error) {
	return "", errors.New("model hook: no ModelClient configured (runtime did not register one)")
}
