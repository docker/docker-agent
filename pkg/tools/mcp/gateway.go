package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/gateway"
	"github.com/docker/docker-agent/pkg/tools"
)

type GatewayToolset struct {
	*Toolset

	cleanUp func() error
}

var _ tools.ToolSet = (*GatewayToolset)(nil)

func NewGatewayToolset(ctx context.Context, name, mcpServerName string, secrets []gateway.Secret, config any, envProvider environment.Provider, cwd string) (*GatewayToolset, error) {
	slog.DebugContext(ctx, "Creating MCP Gateway toolset", "name", mcpServerName)

	// Make sure all the required secrets are available in the environment.
	// TODO(dga): Ideally, the MCP gateway would use the same provider that we have.
	fileSecrets, err := writeSecretsToFile(ctx, mcpServerName, secrets, envProvider)
	if err != nil {
		return nil, fmt.Errorf("writing secrets to file: %w", err)
	}

	fileConfig, err := writeConfigToFile(ctx, mcpServerName, config)
	if err != nil {
		os.Remove(fileSecrets)
		return nil, fmt.Errorf("writing config to file: %w", err)
	}

	// Isolate ourselves from the MCP Toolkit config by always using the Docker MCP catalog and custom config and secrets.
	// This improves shareability of agents.
	args := []string{
		"mcp", "gateway", "run",
		"--servers", mcpServerName,
		"--catalog", gateway.DockerCatalogURL,
		"--secrets", fileSecrets,
		"--config", fileConfig,
	}

	inner := NewToolsetCommand(name, "docker", args, nil, cwd)
	inner.description = "mcp(ref=" + mcpServerName + ")"

	return &GatewayToolset{
		Toolset: inner,
		cleanUp: func() error {
			return errors.Join(os.Remove(fileSecrets), os.Remove(fileConfig))
		},
	}, nil
}

func (t *GatewayToolset) Stop(ctx context.Context) error {
	stopErr := t.Toolset.Stop(ctx)

	cleanUpErr := t.cleanUp()
	if cleanUpErr != nil {
		slog.WarnContext(ctx, "Failed to clean up MCP Gateway temp files", "error", cleanUpErr)
	}

	return errors.Join(stopErr, cleanUpErr)
}

func writeSecretsToFile(ctx context.Context, mcpServerName string, secrets []gateway.Secret, envProvider environment.Provider) (string, error) {
	var secretValues []string
	for _, secret := range secrets {
		v, found := envProvider.Get(ctx, secret.Env)
		if !found || v == "" {
			return "", errors.New("missing environment variable " + secret.Env + " required by MCP server " + mcpServerName)
		}

		if strings.ContainsAny(v, "\n\r") {
			return "", fmt.Errorf("secret %s contains newline characters", secret.Env)
		}

		secretValues = append(secretValues, fmt.Sprintf("%s=%s", secret.Name, v))
	}

	// We have all the secrets, let's create a file with all of them for the MCP Gateway
	return writeTempFile("mcp-secrets-*", []byte(strings.Join(secretValues, "\n")))
}

func writeConfigToFile(_ context.Context, mcpServerName string, config any) (string, error) {
	buf, err := yaml.Marshal(map[string]any{
		mcpServerName: config,
	})
	if err != nil {
		return "", err
	}

	return writeTempFile("mcp-config-*", buf)
}

func writeTempFile(nameTemplate string, content []byte) (string, error) {
	f, err := os.CreateTemp("", nameTemplate)
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}

	if _, err := f.Write(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}

	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}

	return f.Name(), nil
}
