// MCP Events Extension push demo (experimental SEP-3415).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"
)

const (
	extension        = "io.modelcontextprotocol/events"
	subscriptionMeta = "io.modelcontextprotocol/subscriptionId"
)

// A small wire-level server keeps this demo dependency-free. The official Go
// SDK does not yet expose custom server notifications for the Events draft.
type server struct {
	mu      sync.Mutex
	writer  io.Writer
	streams map[string]context.CancelFunc
	workers sync.WaitGroup
}

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func main() {
	listen := flag.String("listen", "", "Serve Streamable HTTP at this address instead of stdio (e.g. 127.0.0.1:8080)")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	var err error
	if *listen != "" {
		err = runHTTP(ctx, *listen)
	} else {
		err = run(ctx, os.Stdin, os.Stdout)
	}
	stop()
	if err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, reader io.Reader, writer io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	s := &server{writer: writer, streams: make(map[string]context.CancelFunc)}
	defer func() { cancel(); s.workers.Wait() }()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			return err
		}
		if err := s.handle(ctx, req); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (s *server) write(message any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.NewEncoder(s.writer).Encode(message)
}

func (s *server) result(id json.RawMessage, result any) error {
	return s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *server) failure(id json.RawMessage, code int, message string) error {
	return s.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func (s *server) notify(method string, id json.RawMessage, fields map[string]any) error {
	fields["_meta"] = map[string]any{subscriptionMeta: id}
	return s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": fields})
}

func (s *server) handle(ctx context.Context, req request) error {
	switch req.Method {
	case "server/discover":
		return s.result(req.ID, map[string]any{
			"resultType": "complete", "supportedVersions": []string{"2026-07-28"},
			"capabilities": map[string]any{"extensions": map[string]any{extension: map[string]any{}}},
			"instructions": "Discover and subscribe to demo.message using the MCP event tools. The server sends three messages by default, one every five seconds.",
			"ttlMs":        0, "cacheScope": "public",
		})
	case "tools/list":
		return s.result(req.ID, map[string]any{"resultType": "complete", "tools": []any{}, "ttlMs": 0, "cacheScope": "public"})
	case "prompts/list":
		return s.result(req.ID, map[string]any{"resultType": "complete", "prompts": []any{}, "ttlMs": 0, "cacheScope": "public"})
	case "events/list":
		return s.result(req.ID, map[string]any{"resultType": "complete", "events": []any{map[string]any{
			"name": "demo.message", "description": "A simulated external notification. Emits a finite number of messages, then closes the stream.",
			"delivery": []string{"push"},
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"interval_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 60, "default": 5},
				"count":            map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 3},
			}, "additionalProperties": false},
			"payloadSchema": map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string"}, "sequence": map[string]any{"type": "integer"}}, "required": []string{"message", "sequence"}},
		}}})
	case "events/stream":
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				Interval int `json:"interval_seconds"`
				Count    int `json:"count"`
			} `json:"arguments"`
		}
		p.Arguments.Interval, p.Arguments.Count = 5, 3
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name != "demo.message" || p.Arguments.Interval < 1 || p.Arguments.Interval > 60 || p.Arguments.Count < 1 || p.Arguments.Count > 100 {
			return s.failure(req.ID, -32602, "Expected demo.message, interval_seconds 1..60, count 1..100")
		}
		lifetime, cancel := context.WithCancel(ctx)
		key := string(req.ID)
		s.mu.Lock()
		if len(s.streams) >= 16 {
			s.mu.Unlock()
			cancel()
			return s.failure(req.ID, -32023, "Subscription limit reached")
		}
		if _, exists := s.streams[key]; exists {
			s.mu.Unlock()
			cancel()
			return s.failure(req.ID, -32600, "Duplicate request ID")
		}
		s.streams[key] = cancel
		s.mu.Unlock()
		s.workers.Go(func() {
			defer cancel()
			defer func() { s.mu.Lock(); delete(s.streams, key); s.mu.Unlock() }()
			if err := s.stream(lifetime, req.ID, p.Arguments.Interval, p.Arguments.Count); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("event stream: %v", err)
			}
		})
		return nil
	case "notifications/cancelled":
		var p struct {
			ID json.RawMessage `json:"requestId"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return err
		}
		s.mu.Lock()
		if cancel := s.streams[string(p.ID)]; cancel != nil {
			cancel()
		}
		s.mu.Unlock()
		return nil
	case "ping":
		return s.result(req.ID, map[string]any{"resultType": "complete"})
	default:
		if len(req.ID) == 0 {
			return nil
		}
		return s.failure(req.ID, -32601, "Method not found")
	}
}

func (s *server) stream(ctx context.Context, id json.RawMessage, interval, count int) error {
	// This emit-only source has no history: cursors are always null.
	if err := s.notify("notifications/events/active", id, map[string]any{"cursor": nil, "truncated": false}); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	for sequence := 1; sequence <= count; {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-heartbeat.C:
			if err := s.notify("notifications/events/heartbeat", id, map[string]any{"cursor": nil}); err != nil {
				return err
			}
		case now := <-ticker.C:
			if err := s.notify("notifications/events/event", id, map[string]any{
				"eventId": fmt.Sprintf("%s-%d-%d", id, now.UnixNano(), sequence),
				"name":    "demo.message", "timestamp": now.UTC().Format(time.RFC3339Nano), "cursor": nil,
				"data": map[string]any{"message": fmt.Sprintf("External update %d: the demo job made progress.", sequence), "sequence": sequence},
			}); err != nil {
				return err
			}
			sequence++
		}
	}
	return s.result(id, map[string]any{"resultType": "complete", "_meta": map[string]any{subscriptionMeta: id}})
}
