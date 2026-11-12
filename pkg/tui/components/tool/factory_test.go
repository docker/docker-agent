package tool

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	fetch "github.com/docker/docker-agent/pkg/tools/builtin/fetch/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tool/api"
	"github.com/docker/docker-agent/pkg/tui/components/tool/defaulttool"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type registryTestView struct {
	label string
	inits int
}

func (v *registryTestView) Init() tea.Cmd {
	v.inits++
	return nil
}

func (v *registryTestView) Update(tea.Msg) (layout.Model, tea.Cmd) { return v, nil }
func (v *registryTestView) SetSize(int, int) tea.Cmd               { return nil }
func (v *registryTestView) View() string                           { return v.label }

func TestRegistryDispatch(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name            string
		customExact     bool
		builtinExact    bool
		customCategory  bool
		builtinCategory bool
		nilResult       bool
		want            string
	}{
		{name: "generic fallback"},
		{name: "builtin category", builtinCategory: true, want: "builtin category"},
		{name: "custom category", customCategory: true, want: "custom category"},
		{name: "custom overrides builtin category", customCategory: true, builtinCategory: true, want: "custom category"},
		{name: "builtin exact", builtinExact: true, want: "builtin exact"},
		{name: "custom exact", customExact: true, want: "custom exact"},
		{name: "builtin exact before custom category", builtinExact: true, customCategory: true, builtinCategory: true, want: "builtin exact"},
		{name: "custom exact before builtin category", customExact: true, builtinCategory: true, want: "custom exact"},
		{name: "custom overrides all tiers", customExact: true, builtinExact: true, customCategory: true, builtinCategory: true, want: "custom exact"},
		{name: "nil custom exact skips builtin and category", customExact: true, builtinExact: true, customCategory: true, builtinCategory: true, nilResult: true, want: "custom exact"},
		{name: "nil builtin exact skips category", builtinExact: true, customCategory: true, builtinCategory: true, nilResult: true, want: "builtin exact"},
		{name: "nil custom category skips builtin category", customCategory: true, builtinCategory: true, nilResult: true, want: "custom category"},
		{name: "nil builtin category falls back", builtinCategory: true, nilResult: true, want: "builtin category"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, images := range []bool{false, true} {
				name := "without images"
				if images {
					name = "with images"
				}
				t.Run(name, func(t *testing.T) {
					r := NewRegistry()
					ar := animation.NewRuntime()
					state := &service.StaticSessionState{}
					msg := types.ToolCallMessage("root", tools.ToolCall{
						ID: "call-1", Function: tools.FunctionCall{Name: "weather", Arguments: `{"city":"Paris"}`},
					}, tools.Tool{Name: "weather", Category: "external"}, types.ToolStatusCompleted)
					if images {
						msg.Images = []tuiimage.Inline{{
							Name: "screenshot.png", MIME: "image/png", PNGData: []byte("png"), Width: 1600, Height: 900,
						}}
					}
					var calls []string
					selected := &registryTestView{label: tc.want}
					builder := func(label string) Builder {
						return func(gotAR *animation.Runtime, gotMsg *types.Message, gotState service.SessionStateReader) layout.Model {
							assert.Same(t, ar, gotAR)
							assert.Same(t, msg, gotMsg)
							assert.Same(t, state, gotState)
							calls = append(calls, label)
							if tc.nilResult {
								return nil
							}
							return selected
						}
					}
					if tc.customExact {
						r.Register("weather", builder("custom exact"))
					}
					if tc.builtinExact {
						r.RegisterBuiltin("weather", builder("builtin exact"))
					}
					if tc.customCategory {
						r.Register("category:external", builder("custom category"))
					}
					if tc.builtinCategory {
						r.RegisterBuiltin("category:external", builder("builtin category"))
					}

					view := r.New(ar, msg, state)
					if tc.want == "" {
						assert.Empty(t, calls)
					} else {
						assert.Equal(t, []string{tc.want}, calls)
					}
					assert.Zero(t, selected.inits, "the factory must not initialize the view")
					inner := view
					if images {
						wrapped, ok := view.(*inlineImagesModel)
						require.True(t, ok)
						inner = wrapped.model
					}
					if tc.nilResult || tc.want == "" {
						expected := defaulttool.New(ar, msg, state)
						expected.SetSize(40, 0)
						inner.SetSize(40, 0)
						assert.Equal(t, expected.View(), inner.View())
					} else {
						assert.Same(t, selected, inner)
					}
					view.SetSize(40, 0)
					rendered := view.View()
					if images {
						assert.Contains(t, rendered, "cagent-image")
						assert.Contains(t, rendered, ";36;", "images must stay inside the tool width")
						assert.LessOrEqual(t, len(strings.Split(rendered, "\n")), 22)
					} else {
						assert.NotContains(t, rendered, "cagent-image")
					}
				})
			}
		})
	}
}

func TestRegistrySkipsEmptyCategory(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register("category:", func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
		t.Fatal("an empty category must not be resolved")
		return nil
	})
	msg := types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{Name: "unknown"}}, tools.Tool{Name: "unknown"}, types.ToolStatusCompleted)
	assert.NotNil(t, r.New(animation.NewRuntime(), msg, service.StaticSessionState{}))
}

func TestRegistryDefaults(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		category string
		builder  Builder
	}{
		{name: fetch.ToolNameFetch, builder: api.New},
		{name: "remote", category: "api", builder: api.New},
		{name: "shell", builder: defaulttool.New},
		{name: "read_file", builder: defaulttool.New},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ar := animation.NewRuntime()
			state := service.StaticSessionState{}
			msg := types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{
				Name: tc.name, Arguments: `{"url":"https://example.com","cmd":"pwd","path":"test.txt"}`,
			}}, tools.Tool{Name: tc.name, Category: tc.category}, types.ToolStatusCompleted)
			msg.Content = "result body"
			expected := tc.builder(ar, msg, state)
			expected.SetSize(100, 0)
			for _, view := range []layout.Model{NewRegistry().New(ar, msg, state), New(ar, msg, state)} {
				view.SetSize(100, 0)
				assert.Equal(t, expected.View(), view.View())
			}
		})
	}
}

func TestRegistryRegistrationIsIndependent(t *testing.T) {
	t.Parallel()

	var first Registry // The zero value also supports registration.
	second := NewRegistry()
	msg := types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{Name: "weather"}}, tools.Tool{Name: "weather"}, types.ToolStatusCompleted)
	builder := func(label string) Builder {
		return func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
			return &registryTestView{label: label}
		}
	}
	first.RegisterBuiltin("weather", builder("first builtin"))
	first.RegisterBuiltin("weather", builder("replacement builtin"))
	assert.Equal(t, "replacement builtin", first.New(animation.NewRuntime(), msg, service.StaticSessionState{}).View())
	first.Register("weather", builder("first custom"))
	first.Register("weather", builder("replacement custom"))
	second.RegisterBuiltin("weather", builder("second builtin"))
	assert.Equal(t, "replacement custom", first.New(animation.NewRuntime(), msg, service.StaticSessionState{}).View())
	assert.Equal(t, "second builtin", second.New(animation.NewRuntime(), msg, service.StaticSessionState{}).View())
	second.Register("weather", builder("second custom"))
	assert.Equal(t, "replacement custom", first.New(animation.NewRuntime(), msg, service.StaticSessionState{}).View())
	assert.Equal(t, "second custom", second.New(animation.NewRuntime(), msg, service.StaticSessionState{}).View())
	_, ok := NewRegistry().resolve("weather")
	assert.False(t, ok)
}

func TestRegistryConcurrentRegistrationAndLookup(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	builder := func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model {
		return &registryTestView{label: "custom"}
	}
	r.Register("weather", builder)
	r.Register("category:external", builder)
	r.RegisterBuiltin("builtin-weather", builder)
	r.RegisterBuiltin("category:internal", builder)
	var invalidations atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			ar := animation.NewRuntime()
			msgs := []*types.Message{
				types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{Name: "weather"}}, tools.Tool{Name: "weather"}, types.ToolStatusCompleted),
				types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{Name: "builtin-weather"}}, tools.Tool{Name: "builtin-weather"}, types.ToolStatusCompleted),
				types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{Name: "external-weather"}}, tools.Tool{Name: "external-weather", Category: "external"}, types.ToolStatusCompleted),
				types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{Name: "internal-weather"}}, tools.Tool{Name: "internal-weather", Category: "internal"}, types.ToolStatusCompleted),
			}
			for range 50 {
				r.Register("weather", builder)
				r.Register("category:external", builder)
				r.RegisterBuiltin("builtin-weather", builder)
				r.RegisterBuiltin("category:internal", builder)
				r.RegisterCacheInvalidator(func() { invalidations.Add(1) })
				for _, msg := range msgs {
					assert.Equal(t, "custom", r.New(ar, msg, service.StaticSessionState{}).View())
				}
				r.InvalidateCaches()
			}
		})
	}
	wg.Wait()
	assert.Positive(t, invalidations.Load())
}

func TestRegistryCacheInvalidatorsAreIndependentAndReentrant(t *testing.T) {
	t.Parallel()

	first, second := NewRegistry(), NewRegistry()
	var calls []string
	first.RegisterCacheInvalidator(func() {
		calls = append(calls, "first")
		first.Register("weather", func(*animation.Runtime, *types.Message, service.SessionStateReader) layout.Model { return nil })
		first.RegisterCacheInvalidator(func() { calls = append(calls, "later") })
	})
	first.RegisterCacheInvalidator(func() { calls = append(calls, "next") })
	second.RegisterCacheInvalidator(func() { calls = append(calls, "second") })
	first.InvalidateCaches()
	assert.Equal(t, []string{"first", "next"}, calls, "hooks added during invalidation run on the next pass")
	calls = nil
	first.InvalidateCaches()
	assert.Equal(t, []string{"first", "next", "later"}, calls)
	calls = nil
	second.InvalidateCaches()
	assert.Equal(t, []string{"second"}, calls)
}
