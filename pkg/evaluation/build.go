package evaluation

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
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

// imageKey uniquely identifies a Docker image build configuration.
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
		r.imageCache[key] = imageID
		r.imageCacheMu.Unlock()

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

// buildEvalImage builds a Docker image for an evaluation.
func (r *Runner) buildEvalImage(ctx context.Context, evals *session.EvalCriteria) (string, error) {
	var buildContext string
	var data struct {
		CopyWorkingDir bool
		BaseImage      string
	}

	if evals.WorkingDir == "" {
		buildContext = r.EvalsDir
		data.CopyWorkingDir = false
	} else {
		buildContext = filepath.Join(r.EvalsDir, "working_dirs", evals.WorkingDir)
		if _, err := os.Stat(buildContext); os.IsNotExist(err) {
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

	cmd := exec.CommandContext(ctx, "docker", "build", "-q", "-f-", ".")
	cmd.Dir = buildContext
	cmd.Stdin = &dockerfile

	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", fmt.Errorf("docker build failed: %s", string(exitErr.Stderr))
		}
		return "", fmt.Errorf("docker build failed: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}
