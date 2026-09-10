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
	"path/filepath"
	"regexp"
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

	return pullWithRecovery(ctx, model)
}

// Pull pulls a Docker Model Runner model via `docker model pull`, streaming
// progress to the terminal. Unlike the run-path pull it never asks for
// confirmation, so callers that already obtained consent (e.g. the setup
// wizard) can invoke it directly. It skips the pull when the model is already
// available locally and returns a *PullFailedError on failure.
func Pull(ctx context.Context, model string) error {
	if modelExists(ctx, model) {
		slog.DebugContext(ctx, "Model already exists, skipping pull", "model", model)
		return nil
	}

	return pullWithRecovery(ctx, model)
}

// pullWithRecovery runs the pull and recovers once from a corrupted partial
// download.
func pullWithRecovery(ctx context.Context, model string) error {
	err := runModelPull(ctx, model)
	if err == nil {
		return nil
	}

	// A 416 (Requested Range Not Satisfiable) means Docker Model Runner resumed
	// from a corrupted partial: a leftover ".incomplete" blob larger than the
	// remote object, so the resume Range is unsatisfiable and the pull loops on
	// the same error forever. Offer to remove the exact file (parsed from the
	// failure) and retry once.
	var pfe *PullFailedError
	if !errors.As(err, &pfe) {
		return err
	}
	if path, size, ok := corruptPartial(pfe.Detail); ok {
		pfe.CorruptPartial = path
		if confirmRemoveCorruptPartial(ctx, path, size) {
			if rmErr := os.Remove(path); rmErr != nil {
				slog.WarnContext(ctx, "Failed to remove corrupted partial download", "path", path, "error", rmErr)
			} else {
				slog.InfoContext(ctx, "Removed corrupted partial download, retrying pull", "path", path, "model", model)
				fmt.Printf("Removed corrupted partial download. Retrying...\n")
				return runModelPull(ctx, model)
			}
		}
	}
	return pfe
}

// runModelPull shells out to `docker model pull`, streaming output live while
// capturing stderr so a failure carries the real cause. It returns a
// *PullFailedError on failure so callers can inspect the captured output.
func runModelPull(ctx context.Context, model string) error {
	slog.InfoContext(ctx, "Pulling DMR model", "model", model)
	fmt.Printf("Pulling model %s...\n", model)

	cmd := exec.CommandContext(ctx, "docker", "model", "pull", model)
	cmd.Stdout = os.Stdout
	// Tee stderr so the live pull output still reaches the terminal while we
	// also capture it, otherwise the real cause (e.g. a registry error) is lost
	// and the returned error degrades to a bare "exit status 1".
	var stderr bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	if err := cmd.Run(); err != nil {
		return &PullFailedError{Model: model, Detail: cleanPullStderr(stderr.String()), Cause: err}
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

// PullFailedError is returned when `docker model pull` fails. It carries the
// model name and the captured pull output so callers (and the user) get an
// actionable message instead of a bare "exit status 1". teamloader surfaces it
// unwrapped, so its Error() is what the user sees.
type PullFailedError struct {
	Model  string
	Detail string // cleaned stderr from `docker model pull`
	Cause  error  // underlying *exec.ExitError, exposed via Unwrap
	// CorruptPartial is the path to a leftover ".incomplete" blob that caused a
	// 416 on resume, when one was identified. When set, the remediation names
	// the exact file to remove.
	CorruptPartial string
}

func (e *PullFailedError) Error() string {
	return buildPullErrorMessage(e.Model, e.Detail, e.CorruptPartial, e.Cause)
}

func (e *PullFailedError) Unwrap() error { return e.Cause }

// ModelPullErrorSummary is a concise one-liner used when this error is nested
// as the cause of another error (e.g. config.AutoModelFallbackError), so the
// full multi-line guidance is not duplicated.
func (e *PullFailedError) ModelPullErrorSummary() string {
	return "failed to pull model " + e.Model
}

// ansiEscape matches the CSI escape sequences (colors, cursor moves, line
// erase) emitted by `docker model pull` progress output. The final byte of
// these sequences is always an ASCII letter (e.g. m, K, A, H).
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// maxPullStderrLines bounds how many trailing stderr lines are embedded in the
// error so progress-bar spam doesn't bury the real message.
const maxPullStderrLines = 5

// cleanPullStderr normalizes captured `docker model pull` stderr for embedding
// in an error message: it strips ANSI escapes, collapses carriage-return
// progress rewrites to the final state of each line, drops blank lines, and
// keeps only the last few lines (where the actual failure reason lives).
func cleanPullStderr(raw string) string {
	raw = ansiEscape.ReplaceAllString(raw, "")

	var lines []string
	for line := range strings.SplitSeq(raw, "\n") {
		// Progress bars rewrite a line in place with '\r'; keep the last state.
		if _, after, ok := strings.CutLast(line, "\r"); ok {
			line = after
		}
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}

	if len(lines) > maxPullStderrLines {
		lines = lines[len(lines)-maxPullStderrLines:]
	}
	return strings.Join(lines, "\n")
}

// buildPullErrorMessage renders the user-facing message for a failed model
// pull. When a corrupted partial download is identified, it names the exact
// file to remove; otherwise it gives generic, actionable remediation.
func buildPullErrorMessage(model, detail, corruptPartial string, cause error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "failed to pull model %s", model)

	// Never produce a contentless message: fall back to the underlying cause
	// when no stderr was captured.
	if detail == "" && cause != nil {
		detail = strings.TrimSpace(cause.Error())
	}
	if detail != "" {
		b.WriteString("\n\ndocker model pull reported:\n")
		for line := range strings.SplitSeq(detail, "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	} else {
		b.WriteString("\n")
	}

	b.WriteString("\nTo resolve this, you can:\n")
	if corruptPartial != "" {
		// A leftover partial blob larger than the remote object makes the
		// resume Range unsatisfiable (HTTP 416); removing it forces a clean
		// re-download.
		fmt.Fprintf(&b, "  - Remove the corrupted partial download and pull again:\n      rm %s\n      docker model pull %s\n", corruptPartial, model)
		b.WriteString("  - Or choose a model that is already available (see `docker model ls`).")
		return b.String()
	}
	fmt.Fprintf(&b, "  - Check the model name is correct and pull it manually:\n      docker model pull %s\n", model)
	fmt.Fprintf(&b, "  - If a previous pull was interrupted or the copy is corrupted, remove it and retry:\n      docker model rm %s\n      docker model pull %s\n", model, model)
	b.WriteString("  - Or choose a model that is already available (see `docker model ls`).")
	return b.String()
}

// blobDigestRe captures the blob digest from a Model Runner registry URL of the
// form ".../blobs/sha256/<aa>/<digest>/data".
var blobDigestRe = regexp.MustCompile(`blobs/sha256/[0-9a-f]{2}/([0-9a-f]{64})`)

// corruptPartial reports the path and size of a leftover ".incomplete" blob in
// the Model Runner content store when a pull failed with HTTP 416 (Requested
// Range Not Satisfiable). That status means the puller resumed from a partial
// larger than the remote object, so the file is corrupt and must be removed.
// It returns ok=false when the failure is not a 416, the digest cannot be
// parsed, or no matching ".incomplete" file exists.
func corruptPartial(detail string) (path string, size int64, ok bool) {
	if !strings.Contains(detail, "416") &&
		!strings.Contains(strings.ToLower(detail), "requested range not satisfiable") {
		return "", 0, false
	}
	m := blobDigestRe.FindStringSubmatch(detail)
	if m == nil {
		return "", 0, false
	}
	dir := modelStoreDir()
	if dir == "" {
		return "", 0, false
	}
	p := filepath.Join(dir, "blobs", "sha256", m[1]+".incomplete")
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return "", 0, false
	}
	return p, fi.Size(), true
}

// modelStoreDir returns the Docker Model Runner content store directory,
// honoring DOCKER_CONFIG and otherwise defaulting to ~/.docker/models.
func modelStoreDir() string {
	if dc := os.Getenv("DOCKER_CONFIG"); dc != "" {
		return filepath.Join(dc, "models")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".docker", "models")
}

// confirmRemoveCorruptPartial asks, in interactive mode, whether to delete a
// corrupted partial download. In non-interactive mode it returns false so files
// are never removed without explicit consent.
func confirmRemoveCorruptPartial(ctx context.Context, path string, size int64) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false
	}
	fmt.Printf("\nThe pull failed because a partially downloaded copy is corrupted:\n  %s (%s)\n", path, humanizeBytes(size))
	fmt.Printf("Remove it and retry the pull? ([y]es/[n]o): ")
	response, err := input.ReadLine(ctx, os.Stdin)
	if err != nil {
		return false
	}
	response = strings.TrimSpace(strings.ToLower(response))
	return response == "y" || response == "yes"
}

// humanizeBytes formats a byte count using binary (IEC) units.
func humanizeBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
