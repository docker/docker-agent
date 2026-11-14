package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

func runHTTP(ctx context.Context, address string) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if err != nil {
		return err
	}
	log.Printf("MCP Events demo listening on http://%s/mcp", listener.Addr())
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
		log.Printf("HTTP request rejected: method=%s status=%d", r.Method, http.StatusNotFound)
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		log.Printf("HTTP request rejected: method=%s status=%d", r.Method, http.StatusMethodNotAllowed)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024)).Decode(&req); err != nil {
		log.Printf("MCP request rejected: invalid JSON-RPC status=%d", http.StatusBadRequest)
		http.Error(w, "Invalid JSON-RPC request", http.StatusBadRequest)
		return
	}
	log.Printf("MCP request received: method=%q id=%s", req.Method, requestLogID(req.ID))
	defer func() {
		log.Printf("MCP request finished: method=%q id=%s duration=%s cancelled=%t", req.Method, requestLogID(req.ID), time.Since(started).Round(time.Millisecond), r.Context().Err() != nil)
	}()
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	s := &server{writer: w, streams: make(map[string]context.CancelFunc)}
	if req.Method == "events/stream" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		log.Printf("MCP event stream opened: id=%s", requestLogID(req.ID))
		defer func() {
			log.Printf("MCP event stream closed: id=%s cancelled=%t", requestLogID(req.ID), r.Context().Err() != nil)
		}()
		s.writer = &sseWriter{response: w, requestID: requestLogID(req.ID)}
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	if err := s.handle(r.Context(), req); err != nil {
		log.Printf("MCP request failed: method=%q id=%s error=%v", req.Method, requestLogID(req.ID), err)
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
		log.Printf("MCP event stream write failed: id=%s error=%v", w.requestID, err)
		return 0, err
	}
	if err := http.NewResponseController(w.response).Flush(); err != nil {
		log.Printf("MCP event stream flush failed: id=%s error=%v", w.requestID, err)
		return 0, err
	}
	var message struct {
		Method string `json:"method"`
		Params struct {
			EventID string `json:"eventId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(p, &message); err != nil {
		log.Printf("MCP event stream frame sent: id=%s bytes=%d", w.requestID, len(p))
	} else if message.Method == "" {
		log.Printf("MCP event stream completed: id=%s", w.requestID)
	} else {
		log.Printf("MCP event stream notification sent: id=%s method=%q event_id=%q", w.requestID, message.Method, message.Params.EventID)
	}
	return len(p), nil
}
