package tui

import (
	"context"
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/defaults"
	"github.com/docker/docker-agent/pkg/tui/components/transcript"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/dialog/toolconfirmation"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	chatpage "github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type rendererProbe struct {
	registry      *tool.Registry
	label         string
	builds        int
	invalidations int
}

func newRendererProbe(label string) *rendererProbe {
	p := &rendererProbe{registry: tool.NewRegistry(), label: label}
	p.registry.Register("weather", p.build)
	p.registry.RegisterCacheInvalidator(func() { p.invalidations++ })
	return p
}

func (p *rendererProbe) build(_ *animation.Runtime, msg *types.Message, _ service.SessionStateReader) layout.Model {
	p.builds++
	return &rendererProbeView{label: p.label, msg: msg}
}

type rendererProbeView struct {
	label  string
	msg    *types.Message
	themes int
}

func (v *rendererProbeView) Init() tea.Cmd            { return nil }
func (v *rendererProbeView) SetSize(int, int) tea.Cmd { return nil }
func (v *rendererProbeView) View() string {
	return fmt.Sprintf("%s status=%d %s %s theme=%d", v.label, v.msg.ToolStatus, v.msg.ToolCall.Function.Arguments, v.msg.Content, v.themes)
}

func (v *rendererProbeView) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if _, ok := msg.(tuimessages.ThemeChangedMsg); ok {
		v.themes++
	}
	return v, nil
}

func registryToolCall() (tools.ToolCall, tools.Tool) {
	return tools.ToolCall{ID: "call-1", Function: tools.FunctionCall{Name: "weather", Arguments: `{"city":"Paris"}`}}, tools.Tool{Name: "weather"}
}

func TestToolRendererOptionsAreAppScoped(t *testing.T) {
	t.Parallel()

	first, second := newRendererProbe("first-renderer"), newRendererProbe("second-renderer")
	apps := []*appModel{{toolRenderers: defaults.NewRegistry()}, {toolRenderers: defaults.NewRegistry()}}
	WithToolRenderers(map[string]tool.Builder{"weather": first.build})(apps[0])
	WithToolRenderers(map[string]tool.Builder{"weather": second.build})(apps[1])
	WithToolRenderers(nil)(apps[0])
	call, def := registryToolCall()
	msg := types.ToolCallMessage("root", call, def, types.ToolStatusCompleted)
	ar := animation.NewRuntime()
	state := service.StaticSessionState{}
	assert.Contains(t, apps[0].toolRenderers.New(ar, msg, state).View(), first.label)
	assert.Contains(t, apps[1].toolRenderers.New(ar, msg, state).View(), second.label)
	assert.NotContains(t, defaults.NewRegistry().New(ar, msg, state).View(), "-renderer")

	var pages []chatpage.Page
	for _, m := range apps {
		m.ctx = t.Context
		m.buildCommandCategories = func(context.Context, tea.Model) []commands.Category { return nil }
		sess := session.New()
		page := chatpage.New(ar, t.Context(), app.New(t.Context(), stubRuntime{}, sess), &service.SessionState{}, m.chatPageOpts()...)
		t.Cleanup(func() { chatpage.Cleanup(page) })
		page.SetSize(120, 40)
		page.Update(&runtime.ToolCallConfirmationEvent{ToolCall: call, ToolDefinition: def})
		pages = append(pages, page)
	}
	assert.Contains(t, ansi.Strip(pages[0].View()), first.label)
	assert.NotContains(t, ansi.Strip(pages[0].View()), second.label)
	assert.Contains(t, ansi.Strip(pages[1].View()), second.label)
	assert.NotContains(t, ansi.Strip(pages[1].View()), first.label)
}

func TestMessagesToolRendererOptions(t *testing.T) {
	t.Parallel()

	for _, scrollable := range []bool{false, true} {
		for _, reasoning := range []bool{false, true} {
			for _, restored := range []bool{false, true} {
				t.Run(fmt.Sprintf("scrollable=%t/reasoning=%t/restored=%t", scrollable, reasoning, restored), func(t *testing.T) {
					t.Parallel()
					first, second := newRendererProbe("first-renderer"), newRendererProbe("second-renderer")
					var models []messages.Model
					call, def := registryToolCall()
					for _, probe := range []*rendererProbe{first, second} {
						state := &service.SessionState{}
						state.SetExpandThinking(true)
						ar := animation.NewRuntime()
						opts := []messages.Option{messages.WithToolRenderers(probe.registry), messages.WithToolRenderers(nil)}
						var m messages.Model
						if scrollable {
							m = messages.NewScrollableView(ar, 120, 30, state, opts...)
						} else {
							m = messages.New(ar, state, opts...)
							m.SetSize(120, 30)
						}
						t.Cleanup(m.StopAnimations)
						if restored {
							assistant := chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{def}}
							if reasoning {
								assistant.ReasoningContent = "Checking the forecast."
							}
							m.LoadFromSession(&session.Session{Messages: []session.Item{
								session.NewMessageItem(&session.Message{AgentName: "root", Message: assistant}),
								session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: call.ID, Content: "restored-result"}}),
							}}, nil)
							assert.Contains(t, ansi.Strip(m.View()), "restored-result")
						} else {
							if reasoning {
								m.AppendReasoning("root", "Checking the forecast.")
							}
							m.AddOrUpdateToolCall("root", call, def, types.ToolStatusPending)
						}
						if reasoning {
							assert.Equal(t, 1, m.MessageTypeCount(types.MessageTypeAssistantReasoningBlock))
							assert.Zero(t, m.MessageTypeCount(types.MessageTypeToolCall))
						} else {
							assert.Equal(t, 1, m.MessageTypeCount(types.MessageTypeToolCall))
						}
						assert.Contains(t, ansi.Strip(m.View()), probe.label)
						models = append(models, m)
					}

					call.Function.Arguments = `{"city":"London"}`
					models[0].AddOrUpdateToolCall("root", call, def, types.ToolStatusRunning)
					assert.Contains(t, ansi.Strip(models[0].View()), "London")
					assert.Contains(t, ansi.Strip(models[1].View()), "Paris")
					before := first.builds
					models[0].AddToolResult(&runtime.ToolCallResponseEvent{
						ToolCallID: call.ID, Response: "sunny-result", Result: &tools.ToolCallResult{Output: "sunny-result"},
					}, types.ToolStatusCompleted)
					assert.Greater(t, first.builds, before, "result updates must rebuild through the supplied registry")
					out := ansi.Strip(models[0].View())
					assert.Contains(t, out, first.label)
					assert.Contains(t, out, "sunny-result")
					assert.NotContains(t, out, second.label)
					assert.NotContains(t, ansi.Strip(models[1].View()), "sunny-result")

					models[0].Update(tuimessages.ThemeChangedMsg{})
					assert.Equal(t, 1, first.invalidations)
					assert.Zero(t, second.invalidations)
					assert.Contains(t, ansi.Strip(models[0].View()), "theme=1")
					assert.Contains(t, ansi.Strip(models[1].View()), "theme=0")
				})
			}
		}
	}
}

func TestReasoningBlockToolRendererOptions(t *testing.T) {
	t.Parallel()

	first, second := newRendererProbe("first-renderer"), newRendererProbe("second-renderer")
	var blocks []*reasoningblock.Model
	call, def := registryToolCall()
	for _, probe := range []*rendererProbe{first, second} {
		block := reasoningblock.New(animation.NewRuntime(), "block", "root", service.StaticSessionState{},
			reasoningblock.WithToolRenderers(probe.registry), reasoningblock.WithToolRenderers(nil))
		block.SetSize(120, 30)
		block.SetExpanded(true)
		block.AddToolCall(types.ToolCallMessage("root", call, def, types.ToolStatusPending))
		t.Cleanup(block.StopAnimation)
		blocks = append(blocks, block)
	}
	call.Function.Arguments = `{"city":"London"}`
	blocks[0].AddToolCall(types.ToolCallMessage("root", call, def, types.ToolStatusRunning))
	assert.Equal(t, 2, first.builds, "replacing an existing call must retain the registry")
	assert.Equal(t, 1, blocks[0].ToolCount())
	blocks[0].UpdateToolResult(call.ID, "sunny-result", types.ToolStatusCompleted, &tools.ToolCallResult{Output: "sunny-result"})
	assert.Equal(t, 3, first.builds)
	blocks[0].Update(tuimessages.ThemeChangedMsg{})
	out := ansi.Strip(blocks[0].View())
	assert.Contains(t, out, first.label)
	assert.Contains(t, out, "London")
	assert.Contains(t, out, "sunny-result")
	assert.Contains(t, out, "theme=1")
	assert.NotContains(t, out, second.label)
	assert.Contains(t, ansi.Strip(blocks[1].View()), second.label)
	assert.Contains(t, ansi.Strip(blocks[1].View()), "Paris")
	assert.Contains(t, ansi.Strip(blocks[1].View()), "theme=0")
	assert.Equal(t, 1, second.builds)
}

func TestTranscriptToolRendererOptions(t *testing.T) {
	t.Parallel()

	first, second := newRendererProbe("first-renderer"), newRendererProbe("second-renderer")
	var transcripts []*transcript.Transcript
	call, def := registryToolCall()
	for _, probe := range []*rendererProbe{first, second} {
		tr := transcript.New(animation.NewRuntime(), service.StaticSessionState{},
			transcript.WithToolRenderers(probe.registry), transcript.WithToolRenderers(nil))
		tr.Append(types.ToolCallMessage("root", call, def, types.ToolStatusPending))
		t.Cleanup(tr.StopAnimations)
		transcripts = append(transcripts, tr)
	}
	call.Function.Arguments = `{"city":"London"}`
	transcripts[0].AddOrUpdateToolCall("root", call, def, types.ToolStatusRunning)
	assert.Equal(t, 2, first.builds)
	assert.Contains(t, transcripts[0].Render(100), "London")
	_, found := transcripts[0].SetToolStatus(call.ID, types.ToolStatusConfirmation)
	require.True(t, found)
	assert.Equal(t, 3, first.builds)
	transcripts[0].FinalizeToolCalls(types.ToolStatusError)
	assert.Equal(t, 4, first.builds)
	assert.Contains(t, transcripts[0].Render(100), fmt.Sprintf("status=%d", types.ToolStatusError))

	transcripts[0].Update(tuimessages.ThemeChangedMsg{})
	assert.Contains(t, transcripts[0].Render(100), "theme=1")
	first.label = "rebuilt-renderer"
	transcripts[0].Rebuild()
	assert.Equal(t, 5, first.builds)
	out := transcripts[0].Render(60)
	assert.Contains(t, out, first.label)
	assert.Contains(t, out, "London")
	assert.NotContains(t, out, second.label)
	assert.Contains(t, transcripts[1].Render(60), second.label)
	assert.Contains(t, transcripts[1].Render(60), "Paris")
	assert.Contains(t, transcripts[1].Render(60), "theme=0")
	assert.Equal(t, 1, second.builds)
}

func TestHostedConfirmationUsesAppToolRenderers(t *testing.T) {
	t.Parallel()

	first, second := newRendererProbe("first-renderer"), newRendererProbe("second-renderer")
	var confirmations []dialog.Dialog
	call, def := registryToolCall()
	for _, probe := range []*rendererProbe{first, second} {
		m := newTabLifecycleModel(t)
		m.toolRenderers = probe.registry
		id := m.supervisor.ActiveID()
		event := runtime.ToolCallConfirmation(call, def, "root", nil)
		m.Update(queuedRuntimeDelivery(m, id, event))
		opened := takeAttention(m)
		require.Len(t, opened, 1)
		require.Same(t, event, opened[0].OriginatingEvent)
		opened[0].Model.SetSize(160, 40)
		confirmations = append(confirmations, opened[0].Model)
		assert.Equal(t, 1, probe.builds)
	}
	assert.Contains(t, ansi.Strip(confirmations[0].View()), first.label)
	assert.NotContains(t, ansi.Strip(confirmations[0].View()), second.label)
	assert.Contains(t, ansi.Strip(confirmations[1].View()), second.label)
	assert.NotContains(t, ansi.Strip(confirmations[1].View()), first.label)
}

func TestConfirmationToolRendererOptions(t *testing.T) {
	t.Parallel()

	for _, entryPoint := range []string{"component", "dialog", "attention"} {
		t.Run(entryPoint, func(t *testing.T) {
			t.Parallel()
			first, second := newRendererProbe("first-renderer"), newRendererProbe("second-renderer")
			var dialogs []dialog.Dialog
			call, def := registryToolCall()
			for _, probe := range []*rendererProbe{first, second} {
				ar := animation.NewRuntime()
				state := &service.SessionState{}
				event := &runtime.ToolCallConfirmationEvent{ToolCall: call, ToolDefinition: def}
				var d dialog.Dialog
				switch entryPoint {
				case "component":
					d = toolconfirmation.NewToolConfirmationDialog(ar, event, state, messages.WithToolRenderers(probe.registry))
				case "dialog":
					d = dialog.NewToolConfirmationDialog(ar, event, state, probe.registry)
				case "attention":
					d = dialog.NewAttentionDialog(t.Context(), ar, nil, state, event, probe.registry)
				}
				require.NotNil(t, d)
				d.SetSize(160, 40)
				dialogs = append(dialogs, d)
				assert.Equal(t, 1, probe.builds)
			}
			assert.Contains(t, ansi.Strip(dialogs[0].View()), first.label)
			assert.NotContains(t, ansi.Strip(dialogs[0].View()), second.label)
			assert.Contains(t, ansi.Strip(dialogs[1].View()), second.label)
			assert.NotContains(t, ansi.Strip(dialogs[1].View()), first.label)
		})
	}
}
