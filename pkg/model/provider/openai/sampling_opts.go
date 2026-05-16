package openai

import (
	"log/slog"

	oai "github.com/openai/openai-go/v3"

	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
)

// applySamplingProviderOpts forwards sampling-related provider_opts as extra
// body fields on the OpenAI ChatCompletionNewParams. This enables custom
// OpenAI-compatible providers (vLLM, Ollama, llama.cpp, etc.) to receive
// parameters like top_k, repetition_penalty, min_p, etc. that the native
// OpenAI API does not support but these backends do.
func applySamplingProviderOpts(params *oai.ChatCompletionNewParams, opts map[string]any) {
	if len(opts) == 0 {
		return
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

	if len(extras) > 0 {
		params.SetExtraFields(extras)
	}
}
