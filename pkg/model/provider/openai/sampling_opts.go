package openai

import (
	"log/slog"

	oai "github.com/openai/openai-go/v3"

	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
)

// applyChatProviderOpts combines sampling options and explicit extra_body overrides.
func applyChatProviderOpts(params *oai.ChatCompletionNewParams, opts map[string]any) error {
	if len(opts) == 0 {
		return nil
	}

	extras := make(map[string]any)

	for _, key := range providerutil.SamplingProviderOptsKeys() {
		if key == "seed" {
			// seed is a native ChatCompletionNewParams field (int64),
			// so set it directly rather than as an extra field.
			if v, ok := providerutil.GetProviderOptInt64(opts, key); ok {
				params.Seed = oai.Int(v)
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
		params.SetExtraFields(extras)
	}
	return nil
}
