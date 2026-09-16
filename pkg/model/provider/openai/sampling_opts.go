package openai

import (
	"log/slog"
	"maps"
	"slices"

	oai "github.com/openai/openai-go/v3"

	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
)

// applyProviderOptsExtraFields forwards provider_opts that travel as extra
// JSON body fields on the OpenAI ChatCompletionNewParams: the sampling
// allowlist (top_k, repetition_penalty, min_p, ...) that custom
// OpenAI-compatible providers (vLLM, Ollama, llama.cpp, etc.) accept, then
// provider_opts.extra_body verbatim, so an explicit user field wins over
// anything derived. extras carries fields the caller derived itself (e.g.
// chat_template_kwargs); SetExtraFields replaces the map wholesale, so every
// contributor is merged here before the single Set call.
func applyProviderOptsExtraFields(params *oai.ChatCompletionNewParams, opts, extras map[string]any) {
	if extras == nil {
		extras = make(map[string]any)
	}

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

	if body := providerutil.ExtraBody(opts); len(body) > 0 {
		maps.Copy(extras, body)
		slog.Debug("OpenAI provider_opts: forwarding extra_body", "keys", slices.Sorted(maps.Keys(body)))
	}

	if len(extras) > 0 {
		params.SetExtraFields(extras)
	}
}
