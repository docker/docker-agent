package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/docker/docker-agent/pkg/hooks"
)

const HandleLargeToolOutput = "handle_large_tool_output"

func handleLargeToolOutput(ctx context.Context, in *hooks.Input, args []string) (*hooks.Output, error) {
	if in == nil {
		return nil, nil
	}

	if in.HookEventName != hooks.EventToolResponseTransform {
		return nil, nil
	}

	response, ok := in.ToolResponse.(string)
	if !ok || response == "" {
		return nil, nil
	}

	cfg := parseArgs(args)
	threshold := cfg.Threshold
	if threshold == 0 {
		threshold = 30000
	}

	if len(response) <= threshold {
		return nil, nil
	}

	outputDir := cfg.OutputDir
	if outputDir == "" {
		outputDir = os.TempDir()
	}

	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}

	filename := fmt.Sprintf("%s_%s.txt", in.SessionID, in.ToolUseID)
	path := filepath.Join(outputDir, filename)

	if err := os.WriteFile(path, []byte(response), 0o600); err != nil {
		return nil, fmt.Errorf("write output file: %w", err)
	}

	previewSize := cfg.PreviewSize
	if previewSize == 0 {
		previewSize = 3000
	}
	preview := response
	if len(preview) > previewSize {
		preview = response[:previewSize]
	}

	pointer := fmt.Sprintf("[%s response: %d chars, full output saved to %s]\n\nFirst %d chars:\n%s\n\n[Use shell tool to read: cat %s]",
		in.ToolName, len(response), path, previewSize, preview, path)

	return &hooks.Output{
		HookSpecificOutput: &hooks.HookSpecificOutput{
			HookEventName:       hooks.EventToolResponseTransform,
			UpdatedToolResponse: &pointer,
		},
	}, nil
}

type toolOutputConfig struct {
	Threshold   int
	OutputDir   string
	PreviewSize int
}

func parseArgs(args []string) toolOutputConfig {
	if len(args) == 0 {
		return toolOutputConfig{}
	}

	var cfg toolOutputConfig
	if err := json.Unmarshal([]byte(args[0]), &cfg); err != nil {
		return toolOutputConfig{}
	}
	return cfg
}
