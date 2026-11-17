// Package markdowntransform rewrites assistant Markdown for terminal display.
// Results are display-only: they are never persisted or sent to a model.
package markdowntransform

import (
	"context"
	"log/slog"
	"strings"
)

// Config starts one long-lived transformer process that speaks
// newline-delimited JSON-RPC 2.0 on stdin and stdout.
type Config struct {
	Command    string            `yaml:"command" json:"command"`
	TimeoutMS  int               `yaml:"timeout_ms,omitempty" json:"timeout_ms,omitempty"`
	WorkingDir string            `yaml:"working_dir,omitempty" json:"working_dir,omitempty"`
	Env        map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
}

// Input is the params object of a markdown/transform request.
type Input struct {
	Markdown       string `json:"markdown"`
	MessageType    string `json:"message_type"`
	IsStreaming    bool   `json:"is_streaming"`
	AvailableWidth int    `json:"available_width"`
	SessionID      string `json:"session_id,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
}

// Transformer rewrites one complete Markdown snapshot.
type Transformer func(context.Context, Input) (string, error)

// Start returns a display backed by the configured processes, or nil when none
// are configured. Processes start on first use and stop when ctx is done.
func Start(ctx context.Context, configs []Config) *Display {
	transformers := make([]Transformer, 0, len(configs))
	for _, config := range configs {
		if strings.TrimSpace(config.Command) == "" {
			slog.WarnContext(ctx, "Ignoring Markdown transformer without a command")
			continue
		}
		transformers = append(transformers, newCommandTransformer(config).transform)
	}
	if len(transformers) == 0 {
		return nil
	}
	display := NewDisplay(transformers...)
	display.Start(ctx)
	return display
}

// apply runs transformers in order. A failing transformer leaves the Markdown
// produced so far unchanged and the chain continues.
func apply(ctx context.Context, transformers []Transformer, in Input) string {
	for _, transform := range transformers {
		if out, err := transform(ctx, in); err == nil {
			in.Markdown = out
		}
	}
	return in.Markdown
}
