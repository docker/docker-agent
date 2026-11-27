package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

func runHTTP(ctx context.Context, address string) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "MCP Events demo listening", "url", fmt.Sprintf("http://%s/mcp", listener.Addr()))
	srv := &http.Server{Handler: http.HandlerFunc(serveHTTP), ReadHeaderTimeout: 5 * time.Second}
	stopped := context.AfterFunc(ctx, func() { _ = srv.Close() })
	defer stopped()
	err = srv.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func serveHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.URL.Path != "/mcp" {
		slog.Info("HTTP request rejected", "method", r.Method, "status", http.StatusNotFound)
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		slog.Info("HTTP request rejected", "method", r.Method, "status", http.StatusMethodNotAllowed)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024)).Decode(&req); err != nil {
		slog.Info("MCP request rejected: invalid JSON-RPC", "status", http.StatusBadRequest)
		http.Error(w, "Invalid JSON-RPC request", http.StatusBadRequest)
		return
	}
	slog.Info("MCP request received", "method", req.Method, "id", requestLogID(req.ID))
	defer func() {
		slog.Info("MCP request finished", "method", req.Method, "id", requestLogID(req.ID), "duration", time.Since(started).Round(time.Millisecond), "cancelled", r.Context().Err() != nil)
	}()
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	s := &server{writer: w, streams: make(map[string]context.CancelFunc)}
	if req.Method == "events/stream" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		slog.Info("MCP event stream opened", "id", requestLogID(req.ID))
		defer func() {
			slog.Info("MCP event stream closed", "id", requestLogID(req.ID), "cancelled", r.Context().Err() != nil)
		}()
		s.writer = &sseWriter{response: w, requestID: requestLogID(req.ID)}
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	if err := s.handle(r.Context(), req); err != nil {
		slog.Info("MCP request failed", "method", req.Method, "id", requestLogID(req.ID), "error", err)
	}
	// Holding the request open binds stream cancellation to the HTTP connection.
	s.workers.Wait()
}

func requestLogID(id json.RawMessage) string {
	if len(id) == 0 {
		return "none"
	}
	return fmt.Sprintf("%q", id)
}

type sseWriter struct {
	response  http.ResponseWriter
	requestID string
}

func (w *sseWriter) Write(p []byte) (int, error) {
	if _, err := fmt.Fprintf(w.response, "event: message\ndata: %s\n\n", p[:len(p)-1]); err != nil {
		slog.Info("MCP event stream write failed", "id", w.requestID, "error", err)
		return 0, err
	}
	if err := http.NewResponseController(w.response).Flush(); err != nil {
		slog.Info("MCP event stream flush failed", "id", w.requestID, "error", err)
		return 0, err
	}
	var message struct {
		Method string `json:"method"`
		Params struct {
			EventID string `json:"eventId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(p, &message); err != nil {
		slog.Info("MCP event stream frame sent", "id", w.requestID, "bytes", len(p))
	} else if message.Method == "" {
		slog.Info("MCP event stream completed", "id", w.requestID)
	} else {
		slog.Info("MCP event stream notification sent", "id", w.requestID, "method", message.Method, "event_id", message.Params.EventID)
	}
	return len(p), nil
}
