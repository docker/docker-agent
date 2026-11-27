package openai

import (
	"log/slog"

	"github.com/docker/docker-agent/pkg/model/provider/oaistream"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
)

// applyChatProviderOpts combines sampling options and explicit extra_body overrides.
func applyChatProviderOpts(params *oaistream.ChatCompletionRequest, opts map[string]any) error {
	if len(opts) == 0 {
		return nil
	}

	extras := make(map[string]any)

	for _, key := range providerutil.SamplingProviderOptsKeys() {
		if key == "seed" {
			if v, ok := providerutil.GetProviderOptInt64(opts, key); ok {
				params.Seed = new(v)
				slog.Debug("OpenAI provider_opts: set seed", "value", v)
			}
			continue
		}

		if v, ok := providerutil.GetProviderOptFloat64(opts, key); ok {
			extras[key] = v
			slog.Debug("OpenAI provider_opts: forwarding sampling param", "key", key, "value", v)
		} else if vi, ok := providerutil.GetProviderOptInt64(opts, key); ok {
			extras[key] = vi
			slog.Debug("OpenAI provider_opts: forwarding sampling param", "key", key, "value", vi)
		}
	}

	if err := providerutil.MergeExtraBody(extras, opts); err != nil {
		return err
	}
	if len(extras) > 0 {
		params.ExtraBody = extras
	}
	return nil
}
