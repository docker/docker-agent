package openai

import (
	"log/slog"
	"maps"
	"slices"

	oai "github.com/openai/openai-go/v3"

	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
)

// applyProviderOptsExtraFields merges extras, the sampling allowlist and extra_body (last, so it wins) into one SetExtraFields call.
func applyProviderOptsExtraFields(params *oai.ChatCompletionNewParams, opts, extras map[string]any) {
	set := func(key string, value any) {
		if extras == nil {
			extras = make(map[string]any)
		}
		extras[key] = value
	}

	if len(opts) > 0 {
		for _, key := range providerutil.SamplingProviderOptsKeys() {
			if key == "seed" {
				// seed is a native ChatCompletionNewParams field (int64).
				if v, ok := providerutil.GetProviderOptInt64(opts, key); ok {
					params.Seed = oai.Int(v)
					slog.Debug("OpenAI provider_opts: set seed", "value", v)
				}
				continue
			}

			if v, ok := providerutil.GetProviderOptFloat64(opts, key); ok {
				set(key, v)
				slog.Debug("OpenAI provider_opts: forwarding sampling param", "key", key, "value", v)
			} else if vi, ok := providerutil.GetProviderOptInt64(opts, key); ok {
				set(key, vi)
				slog.Debug("OpenAI provider_opts: forwarding sampling param", "key", key, "value", vi)
			}
		}

		if body := providerutil.ExtraBody(opts); len(body) > 0 {
			if extras == nil {
				extras = make(map[string]any, len(body))
			}
			maps.Copy(extras, body)
			slog.Debug("OpenAI provider_opts: forwarding extra_body", "keys", slices.Sorted(maps.Keys(body)))
		}
	}

	if len(extras) > 0 {
		params.SetExtraFields(extras)
	}
}
