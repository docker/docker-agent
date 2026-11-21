// Package tool provides instance-scoped tool renderer selection and generic/API views.
package tool

import (
	"slices"
	"sync"

	fetch "github.com/docker/docker-agent/pkg/tools/builtin/fetch/types"
	handoff "github.com/docker/docker-agent/pkg/tools/builtin/handoff/types"
	transfertask "github.com/docker/docker-agent/pkg/tools/builtin/transfertask/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tool/api"
	"github.com/docker/docker-agent/pkg/tui/components/tool/defaulttool"
	handoffview "github.com/docker/docker-agent/pkg/tui/components/tool/handoff"
	transfertaskview "github.com/docker/docker-agent/pkg/tui/components/tool/transfertask"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Builder constructs a view without starting its Init command.
type Builder = func(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader) layout.Model

// Registry resolves exact names, then category:<name>, then the generic fallback.
// At each tier custom renderers take precedence over builtins. Registries are
// independent and safe for concurrent registration and lookup; do not copy them.
type Registry struct {
	mu           sync.RWMutex
	builtins     map[string]Builder
	custom       map[string]Builder
	invalidators []func()
}

// NewRegistry includes API and agent transition renderers. Import tool/defaults for the full bundle.
func NewRegistry() *Registry {
	return &Registry{builtins: map[string]Builder{
		fetch.ToolNameFetch:               api.New,
		"category:api":                    api.New,
		handoff.ToolNameHandoff:           handoffview.New,
		transfertask.ToolNameTransferTask: transfertaskview.New,
	}}
}

func (r *Registry) Register(key string, b Builder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.custom == nil {
		r.custom = make(map[string]Builder)
	}
	r.custom[key] = b
}

func (r *Registry) RegisterBuiltin(key string, b Builder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.builtins == nil {
		r.builtins = make(map[string]Builder)
	}
	r.builtins[key] = b
}

// RegisterCacheInvalidator adds a theme-change hook for a renderer's shared caches.
func (r *Registry) RegisterCacheInvalidator(invalidate func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invalidators = append(r.invalidators, invalidate)
}

func (r *Registry) InvalidateCaches() {
	r.mu.RLock()
	invalidators := slices.Clone(r.invalidators)
	r.mu.RUnlock()
	for _, invalidate := range invalidators {
		invalidate()
	}
}

func (r *Registry) resolve(key string) (Builder, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if b, ok := r.custom[key]; ok {
		return b, true
	}
	b, ok := r.builtins[key]
	return b, ok
}

// New returns a view without the optional builtin bundle.
func New(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return NewRegistry().New(ar, msg, sessionState)
}

func (r *Registry) New(ar *animation.Runtime, msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	var view layout.Model
	if b, ok := r.resolve(msg.ToolCall.Function.Name); ok {
		view = b(ar, msg, sessionState)
	} else if cat := msg.ToolDefinition.Category; cat != "" {
		if b, ok := r.resolve("category:" + cat); ok {
			view = b(ar, msg, sessionState)
		}
	}
	if view == nil {
		view = defaulttool.New(ar, msg, sessionState)
	}
	return withInlineImages(view, msg.Images, sessionState)
}
