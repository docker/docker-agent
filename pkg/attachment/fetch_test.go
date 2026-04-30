package attachment_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker-agent/pkg/attachment"
)

func TestFetchURL_Success(t *testing.T) {
	t.Parallel()

	const body = "hello from test server"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	got, err := attachment.FetchURL(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("FetchURL returned unexpected error: %v", err)
	}
	if string(got) != body {
		t.Errorf("FetchURL body = %q, want %q", string(got), body)
	}
}

func TestFetchURL_Non2xx(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := attachment.FetchURL(t.Context(), srv.URL)
	if err == nil {
		t.Fatal("FetchURL expected an error for 404 response, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status code 404, got: %v", err)
	}
}

func TestFetchURL_ServerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := attachment.FetchURL(t.Context(), srv.URL)
	if err == nil {
		t.Fatal("FetchURL expected an error for 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status code 500, got: %v", err)
	}
}

func TestFetchURL_CancelledContext(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never respond — let the context cancel first.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	_, err := attachment.FetchURL(ctx, srv.URL)
	if err == nil {
		t.Fatal("FetchURL expected an error for cancelled context, got nil")
	}
}

func TestFetchURL_BinaryContent(t *testing.T) {
	t.Parallel()

	content := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a} // PNG magic bytes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	got, err := attachment.FetchURL(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("FetchURL returned unexpected error: %v", err)
	}
	if len(got) != len(content) {
		t.Errorf("FetchURL returned %d bytes, want %d", len(got), len(content))
	}
	for i, b := range content {
		if got[i] != b {
			t.Errorf("byte %d: got %02x, want %02x", i, got[i], b)
		}
	}
}
