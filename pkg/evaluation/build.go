package evaluation

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/docker/docker-agent/pkg/session"
)

var (
	//go:embed Dockerfile.template
	dockerfileTmpl string

	//go:embed Dockerfile.custom.template
	dockerfileCustomTmpl string

	dockerfileTemplate       = template.Must(template.New("Dockerfile").Parse(dockerfileTmpl))
	dockerfileCustomTemplate = template.Must(template.New("DockerfileCustom").Parse(dockerfileCustomTmpl))
)

// imageKey uniquely identifies a container image build configuration.
type imageKey struct {
	workingDir string
	image      string
}

// String returns a stable string representation for use as a singleflight key.
func (k imageKey) String() string {
	return k.workingDir + "\x00" + k.image
}

// getOrBuildImage returns a cached image ID or builds a new one.
// Concurrent calls for the same (workingDir, image) pair are deduplicated
// using singleflight so that only one build runs at a time per key.
func (r *Runner) getOrBuildImage(ctx context.Context, evals *session.EvalCriteria) (string, error) {
	key := imageKey{workingDir: evals.WorkingDir, image: evals.Image}

	r.imageCacheMu.Lock()
	if imageID, ok := r.imageCache[key]; ok {
		r.imageCacheMu.Unlock()
		return imageID, nil
	}
	r.imageCacheMu.Unlock()

	v, err, _ := r.imageBuildGroup.Do(key.String(), func() (any, error) {
		imageID, err := r.buildEvalImage(ctx, evals)
		if err != nil {
			return "", err
		}

		r.imageCacheMu.Lock()
		defer r.imageCacheMu.Unlock()
		r.imageCache[key] = imageID

		return imageID, nil
	})
	if err != nil {
		return "", err
	}

	return v.(string), nil
}

// resolveBaseImage returns the effective base image for an eval.
// The per-eval image takes priority over the global --base-image flag.
func (r *Runner) resolveBaseImage(evals *session.EvalCriteria) string {
	if evals.Image != "" {
		return evals.Image
	}
	return r.BaseImage
}

// buildEvalImage builds a container image for an evaluation using the
// configured container runtime.
func (r *Runner) buildEvalImage(ctx context.Context, evals *session.EvalCriteria) (string, error) {
	var buildContext string
	var data struct {
		CopyWorkingDir bool
		BaseImage      string
		AgentImage     string
	}
	data.AgentImage = ResolvedAgentImage(r.Config)

	if evals.WorkingDir == "" {
		buildContext = r.EvalsDir
		data.CopyWorkingDir = false
	} else {
		buildContext = filepath.Join(r.EvalsDir, "working_dirs", evals.WorkingDir)
		if _, err := os.Stat(buildContext); errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("working directory not found: %s", buildContext)
		}
		data.CopyWorkingDir = true
	}

	// Choose template based on whether a custom base image is provided
	tmpl := dockerfileTemplate
	if baseImage := r.resolveBaseImage(evals); baseImage != "" {
		tmpl = dockerfileCustomTemplate
		data.BaseImage = baseImage
	}

	var dockerfile bytes.Buffer
	if err := tmpl.Execute(&dockerfile, data); err != nil {
		return "", fmt.Errorf("executing dockerfile template: %w", err)
	}

	containerRuntime := r.containerRuntimeOrDefault()
	cmd := exec.CommandContext(ctx, containerRuntime, "build", "-q", "-f-", ".")
	cmd.Dir = buildContext
	cmd.Stdin = &dockerfile

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("%s build failed: %s", containerRuntime, string(exitErr.Stderr))
		}
		return "", fmt.Errorf("%s build failed: %w", containerRuntime, err)
	}

	return strings.TrimSpace(string(output)), nil
}
