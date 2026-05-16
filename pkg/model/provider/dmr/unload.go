package dmr

import (
	"fmt"
	"net/url"
	"strings"
)

// ProviderType is the canonical [latest.ModelConfig.Provider] value
// for Docker Model Runner. Exported so callers outside the package
// (e.g. the `unload` hook builtin) can dispatch on provider type
// without hard-coding the literal.
const ProviderType = "dmr"

// UnloadURL resolves the URL of the per-model unload endpoint for a
// DMR-served model, given the resolved provider base URL and the
// per-model `unload_api` override (both as they appear on
// [hooks.ModelEndpoint]).
//
// Resolution order:
//
//  1. unloadAPI is an absolute URL — used verbatim (lets users point
//     at a different host than baseURL);
//  2. unloadAPI is set but relative — rebased onto baseURL's
//     scheme + host (the model's path is dropped);
//  3. unloadAPI is unset — the default `_unload` URL is derived from
//     baseURL by replacing its trailing `/v1` segment, mirroring the
//     `/v1` → `/_configure` convention the configure path uses.
//
// Returns ("", nil) when neither baseURL nor unloadAPI is set, so the
// caller can skip without erroring (in-process / test providers).
func UnloadURL(baseURL, unloadAPI string) (string, error) {
	if strings.HasPrefix(unloadAPI, "http://") || strings.HasPrefix(unloadAPI, "https://") {
		return unloadAPI, nil
	}
	if baseURL == "" && unloadAPI == "" {
		return "", nil
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("base_url %q is not absolute; cannot resolve unload endpoint", baseURL)
	}
	switch {
	case unloadAPI == "":
		u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/v1") + "/_unload"
	case strings.HasPrefix(unloadAPI, "/"):
		u.Path = unloadAPI
	default:
		u.Path = "/" + unloadAPI
	}
	return u.String(), nil
}
