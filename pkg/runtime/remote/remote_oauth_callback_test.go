package remote

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/mcp/oauthflow"
)

func TestRemoteOAuthCallbackFlow(t *testing.T) {
	for _, failBrowser := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "browser failure"}[failBrowser], func(t *testing.T) {
			// OAuth dependency overrides are process-wide; do not parallelize.
			var registeredRedirect, exchangedRedirect string
			var exchanged bool
			mux := http.NewServeMux()
			server := httptest.NewServer(mux)
			defer server.Close()
			mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"client_id":"registered"}`)
			})
			mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				exchangedRedirect = r.FormValue("redirect_uri")
				exchanged = true
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"access_token":"token","token_type":"Bearer"}`)
			})
			restoreClient := oauthflow.SetHTTPClientForTesting(server.Client())
			defer restoreClient()
			failure := errors.New("cannot open browser")
			restoreBrowser := oauthflow.SetBrowserOpenerForTesting(func(ctx context.Context, authURL string) error {
				if failBrowser {
					return failure
				}
				parsed, err := url.Parse(authURL)
				if err != nil {
					return err
				}
				registeredRedirect = parsed.Query().Get("redirect_uri")
				callback, err := url.Parse(registeredRedirect)
				if err != nil {
					return err
				}
				query := callback.Query()
				query.Set("code", "code")
				query.Set("state", parsed.Query().Get("state"))
				callback.RawQuery = query.Encode()
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, callback.String(), http.NoBody)
				if err != nil {
					return err
				}
				response, err := server.Client().Do(request)
				if err != nil {
					return err
				}
				defer response.Body.Close()
				_, err = io.Copy(io.Discard, response.Body)
				return err
			})
			defer restoreBrowser()
			client := &elicitationRecordingClient{}
			rt, err := NewRuntime(client)
			require.NoError(t, err)
			err = rt.handleOAuthElicitation(t.Context(), "session", &agentruntime.ElicitationRequestEvent{
				ElicitationID: "oauth-request",
				Meta: map[string]any{
					"docker-agent/server_url": server.URL,
					"auth_server_metadata": map[string]any{
						"issuer":                 server.URL,
						"authorization_endpoint": server.URL + "/authorize",
						"registration_endpoint":  server.URL + "/register",
						"token_endpoint":         server.URL + "/token",
					},
				},
			})
			if failBrowser {
				require.ErrorIs(t, err, failure)
				assert.False(t, exchanged)
				assert.Equal(t, []tools.ElicitationAction{tools.ElicitationActionDecline}, client.actions)
			} else {
				require.NoError(t, err)
				assert.True(t, exchanged, "exchange must use the still-live parent context")
				assert.Equal(t, registeredRedirect, exchangedRedirect)
				assert.Equal(t, []tools.ElicitationAction{tools.ElicitationActionAccept}, client.actions)
			}
			assert.Equal(t, []string{"oauth-request"}, client.ids)
		})
	}
}
