//go:build js

package bedrock

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// The browser has no shared config files, IMDS or STS to fall back to: a
// bearer token from the session env is the only credential.
const bearerTokenRequired = true

// buildAWSConfig returns a static config: no default credential chain, so
// nothing reads host config files or probes the instance metadata service.
func buildAWSConfig(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider) (aws.Config, error) {
	for _, key := range []string{"profile", "role_arn"} {
		if getProviderOpt[string](cfg.ProviderOpts, key) != "" {
			return aws.Config{}, fmt.Errorf("provider_opts.%s is not supported in the browser: only bearer token authentication is available", key)
		}
	}
	return aws.Config{
		Region:      resolveRegion(ctx, cfg, env),
		Credentials: aws.AnonymousCredentials{},
	}, nil
}
