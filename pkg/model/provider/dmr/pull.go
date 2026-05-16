package dmr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"

	"github.com/docker/docker-agent/pkg/input"
)

func pullDockerModelIfNeeded(ctx context.Context, model string) error {
	if modelExists(ctx, model) {
		slog.DebugContext(ctx, "Model already exists, skipping pull", "model", model)
		return nil
	}

	if err := confirmModelPull(ctx, model); err != nil {
		return err
	}

	slog.InfoContext(ctx, "Pulling DMR model", "model", model)
	fmt.Printf("Pulling model %s...\n", model)

	cmd := exec.CommandContext(ctx, "docker", "model", "pull", model)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to pull model %s: %w", model, err)
	}

	slog.InfoContext(ctx, "Model pulled successfully", "model", model)
	fmt.Printf("Model %s pulled successfully.\n", model)

	return nil
}

// confirmModelPull asks for user confirmation in interactive mode.
// In non-interactive mode (e.g. devcontainers, CI), it proceeds automatically.
func confirmModelPull(ctx context.Context, model string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		slog.InfoContext(ctx, "Model not found locally, pulling automatically (non-interactive mode)", "model", model)
		return nil
	}

	fmt.Printf("\nModel %s not found locally.\n", model)
	fmt.Printf("Do you want to pull it now? ([y]es/[n]o): ")

	response, err := input.ReadLine(ctx, os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to read user input: %w", err)
	}

	response = strings.TrimSpace(strings.ToLower(response))
	if response != "y" && response != "yes" {
		return errors.New("model pull declined by user")
	}

	return nil
}

func modelExists(ctx context.Context, model string) bool {
	cmd := exec.CommandContext(ctx, "docker", "model", "inspect", model)
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		slog.DebugContext(ctx, "Model does not exist", "model", model, "error", strings.TrimSpace(stderr.String()))
		return false
	}
	return true
}
