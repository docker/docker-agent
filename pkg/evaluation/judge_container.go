package evaluation

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/atomicfile"
	"github.com/docker/docker-agent/pkg/config/latest"
)

// Stage only the connection defaults needed to load a host-side named judge.
func (r *Runner) stageJudgeProvider() (string, error) {
	if r.JudgeType != JudgeTypeEvaluator || r.agentConfig == nil {
		return "", nil
	}
	def, ok := r.agentConfig.Evaluators[r.JudgeModel]
	if !ok {
		return "", nil
	}
	if _, local := r.agentConfig.Providers[def.Provider]; local {
		return "", nil
	}
	global, ok := r.runConfig.Providers[def.Provider]
	if !ok {
		return "", nil
	}
	resolved, err := def.Resolve(map[string]latest.ProviderConfig{def.Provider: global})
	if err != nil {
		return "", fmt.Errorf("resolving container judge provider: %w", err)
	}
	data, err := yaml.Marshal(struct {
		Providers map[string]latest.ProviderConfig `yaml:"providers"`
	}{Providers: map[string]latest.ProviderConfig{
		def.Provider: {Provider: resolved.Provider, BaseURL: resolved.BaseURL, TokenKey: resolved.TokenKey},
	}})
	if err != nil {
		return "", fmt.Errorf("encoding container judge provider: %w", err)
	}
	dir, err := os.MkdirTemp("", "docker-agent-eval-judge-")
	if err != nil {
		return "", fmt.Errorf("creating container judge config directory: %w", err)
	}
	if err := atomicfile.Write(filepath.Join(dir, "config.yaml"), bytes.NewReader(data), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("writing container judge provider: %w", err)
	}
	return dir, nil
}
