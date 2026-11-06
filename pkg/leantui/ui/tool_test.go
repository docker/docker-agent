package ui

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tool/defaulttool"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	tuitypes "github.com/docker/docker-agent/pkg/tui/types"
)

func TestRunningToolFooter(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"shell", "write_file", "custom_tool", "transfer_task"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			view := NewToolView("root", tools.ToolCall{Function: tools.FunctionCall{
				Name: name, Arguments: `{"cmd":"pwd","path":"/tmp/file","content":"preview","agent":"helper","task":"help"}`,
			}}, tools.Tool{Name: name}, tuitypes.ToolStatusRunning)
			started := time.Now().Add(-5100 * time.Millisecond)
			view.message.StartedAt = &started
			render := func() string {
				return ansi.Strip(strings.Join(RenderToolWithState(view, 80, 0, nil), "\n"))
			}
			text := render()
			assert.Equal(t, 1, strings.Count(text, "Running for 5s"))
			assert.True(t, strings.HasSuffix(strings.TrimSpace(text), "Running for 5s"))
			if name == "write_file" {
				assert.Contains(t, text, "preview")
			}
			started = time.Now().Add(-90100 * time.Millisecond)
			assert.Contains(t, render(), "Running for 1m30s")
			for _, status := range []tuitypes.ToolStatus{tuitypes.ToolStatusPending, tuitypes.ToolStatusConfirmation, tuitypes.ToolStatusCompleted, tuitypes.ToolStatusError} {
				view.message.ToolStatus = status
				assert.NotContains(t, render(), "Running for")
			}
		})
	}
}

func TestFinishedToolRetainsDuration(t *testing.T) {
	t.Parallel()

	for _, failed := range []bool{false, true} {
		for _, finalize := range []bool{false, true} {
			tracker := NewToolTracker()
			call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"pwd"}`}}
			tracker.Upsert("root", call, tools.Tool{Name: "shell"}, tuitypes.ToolStatusRunning)
			started := time.Now().Add(-5100 * time.Millisecond)
			tracker.Get(call.ID).message.StartedAt = &started
			var snapshot *ToolView
			label := "Completed in 5s"
			status := tuitypes.ToolStatusCompleted
			if failed {
				label = "Failed after 5s"
				status = tuitypes.ToolStatusError
			}
			if finalize {
				snapshot = tracker.FinalizeAll(status)[0]
			} else {
				snapshot = tracker.Finish(call.ID, ToolResult{Result: &tools.ToolCallResult{IsError: failed}})
			}
			require.NotNil(t, snapshot.message.FinishedAt)
			first := RenderToolWithState(snapshot, 80, 0, nil)
			assert.Contains(t, ansi.Strip(strings.Join(first, "\n")), label)
			assert.Equal(t, first, RenderToolWithState(snapshot, 80, 100, nil))
			assert.Contains(t, ansi.Strip(strings.Join(RenderToolWithState(snapshot, 40, 0, nil), "\n")), label)
		}
	}
}

func TestToolViewReusesInstanceRegistry(t *testing.T) {
	t.Parallel()

	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"pwd"}`}}
	view := NewToolView("root", call, tools.Tool{Name: "shell"}, tuitypes.ToolStatusCompleted)
	other := NewToolView("root", call, tools.Tool{Name: "shell"}, tuitypes.ToolStatusCompleted)
	registry := view.renderers
	require.NotNil(t, registry)
	require.NotSame(t, registry, other.renderers)

	calls := 0
	registry.Register("shell", func(ar *animation.Runtime, msg *tuitypes.Message, state service.SessionStateReader) layout.Model {
		calls++
		return defaulttool.New(ar, msg, state)
	})
	assert.Zero(t, calls)
	for frame := range 3 {
		assert.NotEmpty(t, RenderToolWithState(view, 80, frame, nil))
		assert.Same(t, registry, view.renderers)
		assert.Equal(t, frame+1, calls, "reuse the registry, not the rendered model")
	}
	RenderTool(*view, 80)
	assert.Equal(t, 4, calls, "value copies must retain the instance registry")
	RenderTool(*other, 80)
	assert.Equal(t, 4, calls, "other views must not inherit registrations")
}

func TestToolViewSnapshotsRetainRegistry(t *testing.T) {
	t.Parallel()

	for _, finalize := range []bool{false, true} {
		name := "finish"
		if finalize {
			name = "finalize all"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tracker := NewToolTracker()
			call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"pwd"}`}}
			def := tools.Tool{Name: "shell"}
			tracker.Upsert("root", call, def, tuitypes.ToolStatusRunning)
			original := tracker.Get(call.ID)
			require.NotNil(t, original)
			var snapshot *ToolView
			if finalize {
				views := tracker.FinalizeAll(tuitypes.ToolStatusCompleted)
				require.Len(t, views, 1)
				snapshot = views[0]
			} else {
				snapshot = tracker.Finish(call.ID, ToolResult{ToolDefinition: def})
			}
			require.NotNil(t, snapshot)
			assert.True(t, tracker.Empty())
			assert.NotSame(t, original.message, snapshot.message)
			assert.Same(t, original.renderers, snapshot.renderers)
			assert.Equal(t, RenderTool(*original, 80), RenderTool(*snapshot, 80))
			assert.Contains(t, strings.Join(RenderTool(*snapshot, 80), "\n"), "pwd")
		})
	}
}

func TestRenderToolWithoutRegistry(t *testing.T) {
	t.Parallel()

	assert.Nil(t, RenderToolWithState(nil, 0, 0, nil))
	var empty ToolView
	assert.Nil(t, RenderToolWithState(&empty, 0, 0, nil))
	assert.Nil(t, empty.renderers)

	view := ToolView{message: tuitypes.ToolCallMessage("root", tools.ToolCall{
		Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"pwd"}`},
	}, tools.Tool{Name: "shell"}, tuitypes.ToolStatusCompleted)}
	first := RenderToolWithState(&view, 80, 0, nil)
	require.NotNil(t, view.renderers)
	registry := view.renderers
	assert.Equal(t, first, RenderToolWithState(&view, 80, 0, nil))
	assert.Same(t, registry, view.renderers)
	assert.Contains(t, strings.Join(first, "\n"), "pwd")
}

func TestWriteFileStreamingPreview(t *testing.T) {
	t.Parallel()
	tracker := NewToolTracker()
	definition := tools.Tool{Name: "write_file", Annotations: tools.ToolAnnotations{Title: "Write"}}
	call := tools.ToolCall{ID: "write", Function: tools.FunctionCall{Name: "write_file", Arguments: `{"path":"/tmp/file","content":"first\nsec`}}
	tracker.Upsert("root", call, definition, tuitypes.ToolStatusPending)
	view := tracker.Get(call.ID)
	render := func() string { return strings.Join(RenderToolWithState(view, 80, 0, nil), "\n") }
	first := render()
	assert.Contains(t, first, "Write")
	assert.NotContains(t, first, "write_file")
	assert.Contains(t, first, "first")
	assert.Contains(t, first, "sec")
	for _, fragment := range []string{`ond\`, `nthird\u26`, `03"}`} {
		tracker.Upsert("root", tools.ToolCall{ID: call.ID, Function: tools.FunctionCall{Name: "write_file", Arguments: fragment}}, tools.Tool{Name: "write_file"}, tuitypes.ToolStatusPending)
		assert.Contains(t, render(), "Write")
		assert.Contains(t, render(), "second")
	}
	assert.Contains(t, render(), "third☃")
	snapshot := tracker.Finish(call.ID, ToolResult{})
	final := strings.Join(RenderTool(*snapshot, 80), "\n")
	assert.Contains(t, final, "Write")
	assert.Contains(t, final, "third☃")
}

func TestWriteFilePreviewModes(t *testing.T) {
	t.Parallel()
	var content strings.Builder
	for i := range 15 {
		fmt.Fprintf(&content, "line-%02d\n", i)
	}
	raw, err := json.Marshal(map[string]string{"path": "/tmp/file", "content": content.String()})
	require.NoError(t, err)
	for _, full := range []bool{false, true} {
		t.Run(strconv.FormatBool(full), func(t *testing.T) {
			transcript := NewTranscript()
			transcript.SetFullFilePreview(full)
			transcript.UpsertTool("root", tools.ToolCall{ID: "write", Function: tools.FunctionCall{Name: "write_file", Arguments: string(raw)}}, tools.Tool{Name: "write_file", Annotations: tools.ToolAnnotations{Title: "Write"}}, tuitypes.ToolStatusPending)
			rendered := strings.Join(transcript.Lines(80, 0, true, nil, nil), "\n")
			assert.Contains(t, rendered, "line-14")
			assert.Contains(t, rendered, "line-05")
			assert.Equal(t, full, strings.Contains(rendered, "line-04"))
			transcript.FinishTool("write", ToolResult{}, nil)
			rendered = strings.Join(transcript.Lines(80, 0, false, nil, nil), "\n")
			assert.Equal(t, full, strings.Contains(rendered, "line-04"))
			transcript.SetFullFilePreview(!full)
			rendered = strings.Join(transcript.Lines(80, 0, false, nil, nil), "\n")
			assert.Equal(t, !full, strings.Contains(rendered, "line-04"))
		})
	}
}

func TestWriteFilePreviewHideAndError(t *testing.T) {
	t.Parallel()
	view := NewToolView("root", tools.ToolCall{Function: tools.FunctionCall{Name: "write_file", Arguments: `{"path":"/tmp/file","content":"secret"}`}}, tools.Tool{Name: "write_file"}, tuitypes.ToolStatusError)
	view.Message().Content = "permission denied"
	rendered := strings.Join(RenderTool(*view, 20), "\n")
	assert.NotContains(t, rendered, "secret")
	assert.Contains(t, rendered, "permission denied")
	state := service.NewSessionState(nil)
	state.SetHideToolResults(true)
	rendered = strings.Join(RenderToolWithState(view, 80, 0, state), "\n")
	assert.NotContains(t, rendered, "secret")
	assert.NotContains(t, rendered, "permission denied")
}

func TestToolTitlesPersistAcrossLifecycle(t *testing.T) {
	t.Parallel()
	for _, tool := range []struct {
		name  string
		title string
		args  string
	}{
		{"shell", "Shell", `{"cmd":"pwd"}`},
		{"read_file", "Read", `{"path":"/tmp/file"}`},
		{"custom_tool", "Custom Tool", `{"value":"test"}`},
		{"untitled_tool", "", `{"value":"test"}`},
	} {
		t.Run(tool.name, func(t *testing.T) {
			t.Parallel()
			tracker := NewToolTracker()
			call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: tool.name, Arguments: tool.args}}
			definition := tools.Tool{Name: tool.name, Annotations: tools.ToolAnnotations{Title: tool.title}}
			assertTitle := func(view *ToolView) {
				t.Helper()
				rendered := strings.Join(RenderToolWithState(view, 80, 0, nil), "\n")
				if tool.title == "" {
					assert.Contains(t, rendered, tool.name)
				} else {
					assert.Contains(t, rendered, tool.title)
					assert.NotContains(t, rendered, tool.name)
				}
			}
			tracker.Upsert("root", call, definition, tuitypes.ToolStatusPending)
			assertTitle(tracker.Get(call.ID))
			tracker.Upsert("root", tools.ToolCall{ID: call.ID, Function: tools.FunctionCall{Name: tool.name}}, tools.Tool{Name: tool.name}, tuitypes.ToolStatusPending)
			assertTitle(tracker.Get(call.ID))
			tracker.Upsert("root", call, tools.Tool{Name: tool.name}, tuitypes.ToolStatusRunning)
			assertTitle(tracker.Get(call.ID))
			assertTitle(tracker.Finish(call.ID, ToolResult{ToolDefinition: tools.Tool{Name: tool.name}}))
		})
	}
}
