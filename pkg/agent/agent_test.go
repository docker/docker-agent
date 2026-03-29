package agent

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/tools"
)

type stubToolSet struct {
	startErr error
	tools    []tools.Tool
	listErr  error
}

// Verify interface compliance
var (
	_ tools.ToolSet   = (*stubToolSet)(nil)
	_ tools.Startable = (*stubToolSet)(nil)
)

func newStubToolSet(startErr error, toolsList []tools.Tool, listErr error) tools.ToolSet {
	return &stubToolSet{
		startErr: startErr,
		tools:    toolsList,
		listErr:  listErr,
	}
}

func (s *stubToolSet) Start(context.Context) error { return s.startErr }
func (s *stubToolSet) Stop(context.Context) error  { return nil }
func (s *stubToolSet) Tools(context.Context) ([]tools.Tool, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.tools, nil
}

func TestAgentTools(t *testing.T) {
	tests := []struct {
		name          string
		toolsets      []tools.ToolSet
		wantToolCount int
		wantWarnings  int
	}{
		{
			name:          "partial success",
			toolsets:      []tools.ToolSet{newStubToolSet(nil, []tools.Tool{{Name: "good", Parameters: map[string]any{}}}, nil), newStubToolSet(errors.New("boom"), nil, nil)},
			wantToolCount: 1,
			wantWarnings:  1,
		},
		{
			name:          "all fail on start",
			toolsets:      []tools.ToolSet{newStubToolSet(errors.New("fail1"), nil, nil), newStubToolSet(errors.New("fail2"), nil, nil)},
			wantToolCount: 0,
			wantWarnings:  2,
		},
		{
			name:          "list failure becomes warning",
			toolsets:      []tools.ToolSet{newStubToolSet(nil, nil, errors.New("list boom"))},
			wantToolCount: 0,
			wantWarnings:  1,
		},
		{
			name:          "no toolsets",
			toolsets:      nil,
			wantToolCount: 0,
			wantWarnings:  0,
		},
		{
			name: "duplicate tool names are deduplicated",
			toolsets: []tools.ToolSet{
				newStubToolSet(nil, []tools.Tool{
					{Name: "read_file", Parameters: map[string]any{}},
					{Name: "write_file", Parameters: map[string]any{}},
				}, nil),
				newStubToolSet(nil, []tools.Tool{
					{Name: "read_file", Parameters: map[string]any{}},
					{Name: "shell", Parameters: map[string]any{}},
				}, nil),
			},
			wantToolCount: 3,
			wantWarnings:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New("root", "test", WithToolSets(tt.toolsets...))
			got, err := a.Tools(t.Context())

			require.NoError(t, err)
			require.Len(t, got, tt.wantToolCount)

			warnings := a.DrainWarnings()
			if tt.wantWarnings == 0 {
				require.Nil(t, warnings)
			} else {
				require.Len(t, warnings, tt.wantWarnings)
			}
		})
	}
}

// mockProvider implements provider.Provider for testing
type mockProvider struct {
	id string
}

func (m *mockProvider) ID() string { return m.id }
func (m *mockProvider) CreateChatCompletionStream(_ context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	return nil, nil
}
func (m *mockProvider) BaseConfig() base.Config { return base.Config{} }

func TestModelOverride(t *testing.T) {
	t.Parallel()

	defaultModel := &mockProvider{id: "openai/gpt-4o"}
	overrideModel := &mockProvider{id: "anthropic/claude-sonnet-4-0"}

	a := New("root", "test", WithModel(defaultModel))

	// Initially should return the default model
	model := a.Model()
	assert.Equal(t, "openai/gpt-4o", model.ID())
	assert.False(t, a.HasModelOverride())

	// Set an override
	a.SetModelOverride(overrideModel)
	assert.True(t, a.HasModelOverride())

	// Now Model() should return the override
	model = a.Model()
	assert.Equal(t, "anthropic/claude-sonnet-4-0", model.ID())

	// ConfiguredModels should still return the original models
	configuredModels := a.ConfiguredModels()
	require.Len(t, configuredModels, 1)
	assert.Equal(t, "openai/gpt-4o", configuredModels[0].ID())

	// Clear the override
	a.SetModelOverride(nil)
	assert.False(t, a.HasModelOverride())

	// Model() should return the default again
	model = a.Model()
	assert.Equal(t, "openai/gpt-4o", model.ID())
}

func TestModel_LogsSelection(t *testing.T) {
	t.Parallel()

	var buf concurrent.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	model1 := &mockProvider{id: "anthropic/claude-sonnet-4-0"}
	model2 := &mockProvider{id: "openai/gpt-4o"}

	a := New("scanner", "test", WithModel(model1), WithModel(model2))

	// Verify basic selection logging
	selected := a.Model()
	logOutput := buf.String()

	assert.Contains(t, logOutput, "Model selected")
	assert.Contains(t, logOutput, "agent=scanner")
	assert.Contains(t, logOutput, selected.ID())
	assert.Contains(t, logOutput, "pool_size=2")

	// Verify override scenario logs correct pool_size
	buf.Reset()
	override := &mockProvider{id: "google/gemini-2.0-flash"}
	a.SetModelOverride(override)

	selected = a.Model()
	logOutput = buf.String()

	assert.Equal(t, "google/gemini-2.0-flash", selected.ID())
	assert.Contains(t, logOutput, "google/gemini-2.0-flash")
	assert.Contains(t, logOutput, "pool_size=1")
}

func TestModelOverride_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	defaultModel := &mockProvider{id: "default"}
	overrideModel := &mockProvider{id: "override"}

	a := New("root", "test", WithModel(defaultModel))

	// Run concurrent reads and writes
	done := make(chan bool)

	// Writer goroutine
	go func() {
		for range 100 {
			a.SetModelOverride(overrideModel)
			a.SetModelOverride(nil)
		}
		done <- true
	}()

	// Reader goroutine
	go func() {
		for range 100 {
			_ = a.Model()
			_ = a.HasModelOverride()
		}
		done <- true
	}()

	<-done
	<-done
	// If we got here without a race condition panic, the test passes
}
