//go:build !js

package bedrock

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// Native builds fall back to the AWS credential chain (SigV4) without a bearer token.
const bearerTokenRequired = false

func buildAWSConfig(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider) (aws.Config, error) {
	var configOpts []func(*config.LoadOptions) error

	configOpts = append(configOpts, config.WithRegion(resolveRegion(ctx, cfg, env)))

	// Profile from provider_opts
	if profile := getProviderOpt[string](cfg.ProviderOpts, "profile"); profile != "" {
		configOpts = append(configOpts, config.WithSharedConfigProfile(profile))
	}

	// Load base config with default credential chain
	awsCfg, err := config.LoadDefaultConfig(ctx, configOpts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Handle assume role if specified
	if roleARN := getProviderOpt[string](cfg.ProviderOpts, "role_arn"); roleARN != "" {
		stsClient := sts.NewFromConfig(awsCfg)
		creds := stscreds.NewAssumeRoleProvider(stsClient, roleARN, func(o *stscreds.AssumeRoleOptions) {
			if sessionName := getProviderOpt[string](cfg.ProviderOpts, "role_session_name"); sessionName != "" {
				o.RoleSessionName = sessionName
			} else {
				o.RoleSessionName = "docker-agent-bedrock-session"
			}
			if externalID := getProviderOpt[string](cfg.ProviderOpts, "external_id"); externalID != "" {
				o.ExternalID = aws.String(externalID)
			}
		})
		awsCfg.Credentials = aws.NewCredentialsCache(creds)
		slog.DebugContext(ctx, "Bedrock using assumed role", "role_arn", roleARN)
	}

	return awsCfg, nil
}
