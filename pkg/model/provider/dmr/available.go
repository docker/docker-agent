package dmr

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/model/provider/dmr/dmrmodels"
)

// ModelNotAvailableError is returned at client-creation time when the
// requested model is not pulled in Docker Model Runner and the `docker model`
// CLI could not be used to offer a pull. Failing here replaces the raw HTTP
// 404 the chat endpoint would otherwise return at message time.
type ModelNotAvailableError struct {
	Model string
}

func (e *ModelNotAvailableError) Error() string {
	return fmt.Sprintf("model %s is not pulled in Docker Model Runner\n\nTo resolve this, you can:\n  - Pull it first: docker model pull %s\n  - Or choose a model that is already available (see `docker model ls`).", e.Model, e.Model)
}

// ModelPullErrorSummary is the concise one-liner used when this error is
// nested as the cause of another error (e.g. config.AutoModelFallbackError),
// so the full multi-line guidance is not duplicated.
func (e *ModelNotAvailableError) ModelPullErrorSummary() string {
	return fmt.Sprintf("model %s is not pulled in Docker Model Runner", e.Model)
}

// checkModelAvailable verifies through the DMR HTTP API that a model is
// pulled locally. It is the fallback used when `docker model status` fails
// (missing or broken CLI plugin) and the CLI-based inspect/pull flow is
// therefore unusable.
func checkModelAvailable(ctx context.Context, httpClient *http.Client, baseURL, model string) error {
	available, err := dmrmodels.ListModelsAt(ctx, httpClient, baseURL)
	if err != nil {
		return fmt.Errorf("cannot query Docker Model Runner at %s (is it installed and running? https://docs.docker.com/ai/model-runner/get-started/): %w", baseURL, err)
	}
	if !modelAvailable(available, model) {
		return &ModelNotAvailableError{Model: model}
	}
	return nil
}

// modelAvailable reports whether model matches one of the locally-available
// IDs. An untagged model (e.g. "ai/qwen3") matches any tag of the same
// repository, mirroring how `docker model` resolves references; a model with
// an explicit tag requires an exact match.
func modelAvailable(available []string, model string) bool {
	if slices.Contains(available, model) {
		return true
	}
	if modelRepo(model) != model {
		return false
	}
	for _, id := range available {
		if modelRepo(id) == model {
			return true
		}
	}
	return false
}

// modelRepo returns the repository portion of a DMR model ID, dropping a
// trailing ":<tag>" suffix. A trailing colon is only treated as a tag
// separator when the suffix has no slash, so a registry host:port like
// "registry:5000/ai/x" is preserved.
func modelRepo(id string) string {
	if before, after, ok := strings.CutLast(id, ":"); ok && !strings.Contains(after, "/") {
		return before
	}
	return id
}
