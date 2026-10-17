package oauthflow

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRegisterClientHonorsServerAuthMethod(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"client_id":"cid","client_secret":"sec","token_endpoint_auth_method":"client_secret_basic"}`))
	}))
	t.Cleanup(srv.Close)

	id, secret, method, err := RegisterClientWithClient(t.Context(), srv.Client(), &AuthorizationServerMetadata{
		RegistrationEndpoint:              srv.URL,
		TokenEndpointAuthMethodsSupported: []string{"client_secret_basic", "client_secret_post"},
	}, "http://127.0.0.1/cb", nil)
	if err != nil {
		t.Fatal(err)
	}
	if id != "cid" || secret != "sec" || method != "client_secret_basic" {
		t.Fatalf("got id=%q secret=%q method=%q", id, secret, method)
	}
	if got["token_endpoint_auth_method"] != "client_secret_post" {
		t.Fatalf("registration asked for %v, want client_secret_post", got["token_endpoint_auth_method"])
	}
}

func TestRegisterClientAsksForBasicWhenThatIsAll(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"client_id":"cid","client_secret":"sec","token_endpoint_auth_method":"client_secret_basic"}`))
	}))
	t.Cleanup(srv.Close)

	_, _, method, err := RegisterClientWithClient(t.Context(), srv.Client(), &AuthorizationServerMetadata{
		RegistrationEndpoint:              srv.URL,
		TokenEndpointAuthMethodsSupported: []string{"client_secret_basic"},
	}, "http://127.0.0.1/cb", nil)
	if err != nil {
		t.Fatal(err)
	}
	if method != "client_secret_basic" {
		t.Fatalf("method = %q", method)
	}
	if got["token_endpoint_auth_method"] != "client_secret_basic" {
		t.Fatalf("registration asked for %v", got["token_endpoint_auth_method"])
	}
}

func TestRegisterClientOmittedMetadataUsesBasic(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusCreated)
		// Basic-only server that omits the method on the registration response.
		_, _ = w.Write([]byte(`{"client_id":"cid","client_secret":"sec"}`))
	}))
	t.Cleanup(srv.Close)

	_, _, method, err := RegisterClientWithClient(t.Context(), srv.Client(), &AuthorizationServerMetadata{
		RegistrationEndpoint: srv.URL,
	}, "http://127.0.0.1/cb", nil)
	if err != nil {
		t.Fatal(err)
	}
	if method != "client_secret_basic" {
		t.Fatalf("method = %q, want client_secret_basic", method)
	}
	if got["token_endpoint_auth_method"] != "client_secret_basic" {
		t.Fatalf("registration asked for %v", got["token_endpoint_auth_method"])
	}
}

func TestRegisterClientOmittedResponseIsBasic(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"client_id":"cid","client_secret":"sec"}`))
	}))
	t.Cleanup(srv.Close)

	_, _, method, err := RegisterClientWithClient(t.Context(), srv.Client(), &AuthorizationServerMetadata{
		RegistrationEndpoint:              srv.URL,
		TokenEndpointAuthMethodsSupported: []string{"client_secret_basic", "client_secret_post"},
	}, "http://127.0.0.1/cb", nil)
	if err != nil {
		t.Fatal(err)
	}
	if method != "client_secret_basic" {
		t.Fatalf("method = %q, want client_secret_basic", method)
	}
	if got["token_endpoint_auth_method"] != "client_secret_post" {
		t.Fatalf("registration asked for %v, want client_secret_post", got["token_endpoint_auth_method"])
	}
}

func TestExchangeUsesBasicWhenRegisteredThatWay(t *testing.T) {
	var form url.Values
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer"}`))
	}))
	t.Cleanup(srv.Close)

	tok, err := ExchangeCodeForTokenWithClient(t.Context(), srv.Client(), srv.URL, "code", "ver", "cid", "sec", "http://127.0.0.1/cb", "", "client_secret_basic")
	if err != nil {
		t.Fatal(err)
	}
	if tok.TokenEndpointAuthMethod != "client_secret_basic" {
		t.Fatalf("stored method %q", tok.TokenEndpointAuthMethod)
	}
	if form.Get("client_secret") != "" || form.Get("client_id") != "" {
		t.Fatalf("basic auth still put credentials in the form: %v", form)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("cid:sec"))
	if auth != want {
		t.Fatalf("Authorization = %q, want %q", auth, want)
	}
}

func TestExchangePostsSecretByDefault(t *testing.T) {
	var form url.Values
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer"}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := ExchangeCodeForTokenWithClient(t.Context(), srv.Client(), srv.URL, "code", "ver", "cid", "sec", "http://127.0.0.1/cb", "", ""); err != nil {
		t.Fatal(err)
	}
	if form.Get("client_secret") != "sec" || form.Get("client_id") != "cid" {
		t.Fatalf("form = %v", form)
	}
	if auth != "" {
		t.Fatalf("unexpected Authorization %q", auth)
	}
}

func TestBasicAuthEncodesReservedCharacters(t *testing.T) {
	const id, secret = "ci d+1", "s@ec:ret"
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(id)+":"+url.QueryEscape(secret)))
	var exchangeAuth, refreshAuth string
	var exchangeForm, refreshForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		switch form.Get("grant_type") {
		case "authorization_code":
			exchangeAuth = r.Header.Get("Authorization")
			exchangeForm = form
		case "refresh_token":
			refreshAuth = r.Header.Get("Authorization")
			refreshForm = form
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":60,"refresh_token":"rt"}`))
	}))
	t.Cleanup(srv.Close)

	tok, err := ExchangeCodeForTokenWithClient(t.Context(), srv.Client(), srv.URL, "code", "ver", id, secret, "http://127.0.0.1/cb", "", "client_secret_basic")
	if err != nil {
		t.Fatal(err)
	}
	if exchangeAuth != wantAuth {
		t.Fatalf("exchange Authorization = %q, want %q", exchangeAuth, wantAuth)
	}
	if exchangeForm.Get("client_id") != "" || exchangeForm.Get("client_secret") != "" {
		t.Fatalf("exchange form still has credentials: %v", exchangeForm)
	}

	raw, err := json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	var loaded OAuthToken
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.TokenEndpointAuthMethod != "client_secret_basic" {
		t.Fatalf("reloaded method %q", loaded.TokenEndpointAuthMethod)
	}
	refreshed, err := RefreshAccessTokenWithClient(t.Context(), srv.Client(), srv.URL, loaded.RefreshToken, loaded.ClientID, loaded.ClientSecret, loaded.TokenEndpointAuthMethod)
	if err != nil {
		t.Fatal(err)
	}
	if refreshAuth != wantAuth {
		t.Fatalf("refresh Authorization = %q, want %q", refreshAuth, wantAuth)
	}
	if refreshForm.Get("client_id") != "" || refreshForm.Get("client_secret") != "" {
		t.Fatalf("refresh form still has credentials: %v", refreshForm)
	}
	if refreshed.TokenEndpointAuthMethod != "client_secret_basic" {
		t.Fatalf("method after refresh %q", refreshed.TokenEndpointAuthMethod)
	}
}

func TestNoneOmitsClientSecret(t *testing.T) {
	var sawExchange, sawRefresh bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected Authorization %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		if form.Get("client_secret") != "" || form.Get("client_id") != "cid" {
			t.Errorf("form = %v", form)
		}
		switch form.Get("grant_type") {
		case "authorization_code":
			sawExchange = true
		case "refresh_token":
			sawRefresh = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","refresh_token":"rt"}`))
	}))
	t.Cleanup(srv.Close)

	tok, err := ExchangeCodeForTokenWithClient(t.Context(), srv.Client(), srv.URL, "code", "ver", "cid", "sec", "http://127.0.0.1/cb", "", "none")
	if err != nil {
		t.Fatal(err)
	}
	if tok.TokenEndpointAuthMethod != "none" {
		t.Fatalf("method %q", tok.TokenEndpointAuthMethod)
	}
	refreshed, err := RefreshAccessTokenWithClient(t.Context(), srv.Client(), srv.URL, tok.RefreshToken, tok.ClientID, tok.ClientSecret, tok.TokenEndpointAuthMethod)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.TokenEndpointAuthMethod != "none" {
		t.Fatalf("method after refresh %q", refreshed.TokenEndpointAuthMethod)
	}
	if !sawExchange || !sawRefresh {
		t.Fatalf("saw exchange=%v refresh=%v", sawExchange, sawRefresh)
	}
}

func TestLegacyEmptyMethodStillPostsSecret(t *testing.T) {
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","refresh_token":"rt2"}`))
	}))
	t.Cleanup(srv.Close)

	// A token stored before token_endpoint_auth_method existed.
	raw := []byte(`{"access_token":"old","refresh_token":"rt","client_id":"cid","client_secret":"sec"}`)
	var loaded OAuthToken
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.TokenEndpointAuthMethod != "" {
		t.Fatalf("legacy method %q", loaded.TokenEndpointAuthMethod)
	}
	refreshed, err := RefreshAccessTokenWithClient(t.Context(), srv.Client(), srv.URL, loaded.RefreshToken, loaded.ClientID, loaded.ClientSecret, loaded.TokenEndpointAuthMethod)
	if err != nil {
		t.Fatal(err)
	}
	if form.Get("client_id") != "cid" || form.Get("client_secret") != "sec" {
		t.Fatalf("form = %v", form)
	}
	if refreshed.TokenEndpointAuthMethod != "" {
		t.Fatalf("legacy method changed to %q", refreshed.TokenEndpointAuthMethod)
	}
}

func TestUnsupportedAuthMethodIsRejected(t *testing.T) {
	id, secret, method, err := RegisterClientWithClient(t.Context(), http.DefaultClient, &AuthorizationServerMetadata{
		RegistrationEndpoint:              "http://127.0.0.1/register",
		TokenEndpointAuthMethodsSupported: []string{"client_secret_jwt", "private_key_jwt"},
	}, "http://127.0.0.1/cb", nil)
	if err == nil || !strings.Contains(err.Error(), "no usable token endpoint auth method") {
		t.Fatalf("register err = %v", err)
	}
	if id != "" || secret != "" || method != "" {
		t.Fatalf("register returned id=%q secret=%q method=%q", id, secret, method)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"client_id":"cid","client_secret":"sec","token_endpoint_auth_method":"private_key_jwt"}`))
	}))
	t.Cleanup(srv.Close)
	id, secret, method, err = RegisterClientWithClient(t.Context(), srv.Client(), &AuthorizationServerMetadata{
		RegistrationEndpoint:              srv.URL,
		TokenEndpointAuthMethodsSupported: []string{"client_secret_post"},
	}, "http://127.0.0.1/cb", nil)
	if err == nil || !strings.Contains(err.Error(), "private_key_jwt") {
		t.Fatalf("negotiated err = %v", err)
	}
	if id != "" || secret != "" || method != "" {
		t.Fatalf("negotiated returned id=%q secret=%q method=%q", id, secret, method)
	}

	if _, err := ExchangeCodeForTokenWithClient(t.Context(), srv.Client(), srv.URL, "code", "ver", "cid", "sec", "http://127.0.0.1/cb", "", "client_secret_jwt"); err == nil {
		t.Fatal("exchange accepted client_secret_jwt")
	}
}
