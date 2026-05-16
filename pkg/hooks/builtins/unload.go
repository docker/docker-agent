package builtins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider/dmr"
)

// Unload is the registered name of the on_agent_switch builtin that
// asks the previous agent's local inference engines (today: Docker
// Model Runner) to release the resources they hold.
//
// Wire it into a config with:
//
//	hooks:
//	  on_agent_switch:
//	    - type: builtin
//	      command: unload
//
// The hook is pure: it depends only on the [hooks.Input.FromAgentModels]
// snapshot the runtime ships on every on_agent_switch dispatch, plus
// net/http. It carries no runtime-side coupling and silently skips any
// model whose endpoint isn't reachable as plain HTTP (e.g. cloud
// providers that don't expose [hooks.ModelEndpoint.BaseURL]).
//
// Provider dispatch and URL resolution are owned by
// [pkg/model/provider/dmr] (see [dmr.ProviderType] and [dmr.UnloadURL]),
// so this builtin stays a dumb dispatcher and DMR keeps full control
// of its conventions.
const Unload = "unload"

// unloadTimeout caps each per-model Unload call so a stalled engine
// cannot stall agent switching.
const unloadTimeout = 10 * time.Second

// unload iterates the [hooks.Input.FromAgentModels] snapshot the
// runtime captured at dispatch time and POSTs `{"model": "<id>"}` to
// the resolved unload endpoint of each DMR model. Errors are logged
// but never propagated — agent switching must never block on a slow
// or unreachable engine.
func unload(ctx context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
	if in == nil || in.FromAgent == "" || in.FromAgent == in.ToAgent {
		return nil, nil
	}
	for _, m := range in.FromAgentModels {
		if m.Provider != dmr.ProviderType {
			continue
		}
		if err := unloadOne(ctx, m); err != nil {
			slog.WarnContext(ctx, "unload: failed",
				"agent", in.FromAgent, "model", m.Model, "error", err)
		}
	}
	return nil, nil
}

// unloadOne resolves the unload URL for m and POSTs the model id to
// it, bounded by [unloadTimeout]. A model with no resolvable endpoint
// (no base_url and no unload_api) is a silent no-op so the hook stays
// harmless on test / in-process providers.
func unloadOne(parent context.Context, m hooks.ModelEndpoint) error {
	endpoint, err := dmr.UnloadURL(m.BaseURL, m.UnloadAPI)
	if err != nil || endpoint == "" {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, unloadTimeout)
	defer cancel()

	body, _ := json.Marshal(map[string]string{"model": m.Model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building unload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	slog.DebugContext(ctx, "Unloading model", "url", endpoint, "model", m.Model)

	// Unlike the http_post builtin, the unload target is the
	// operator-configured DMR base URL — typically a loopback engine
	// (Docker Desktop socket, 127.0.0.1:12434, …). The SSRF-safe
	// dialer used by http_post would refuse those addresses by
	// design, so we use the default client here.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling unload endpoint %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return fmt.Errorf("unload endpoint returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	// Drain the success-path body so the underlying transport can reuse
	// the connection (Go's http.Client only re-pools a connection whose
	// body has been read to EOF and closed).
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
