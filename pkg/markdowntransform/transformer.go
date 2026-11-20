package markdowntransform

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	transformMethod = "markdown/transform"
	defaultTimeout  = time.Second
	restartDelay    = time.Second
)

// commandTransformer serves markdown/transform from a lazily started command.
// Display calls it from one worker goroutine whose context bounds the
// command lifetime, so it needs no locking.
type commandTransformer struct {
	config  Config
	timeout time.Duration

	cmd     *command
	client  *rpcClient
	retryAt time.Time
	failing bool
}

func newCommandTransformer(config Config) *commandTransformer {
	timeout := time.Duration(config.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &commandTransformer{config: config, timeout: timeout}
}

func (t *commandTransformer) transform(ctx context.Context, in Input) (string, error) {
	out, err := t.request(ctx, in)
	if err != nil {
		if _, reply := errors.AsType[*rpcError](err); !reply {
			// Timeouts and broken streams may leave a late reply; restart instead.
			t.restart()
		}
		if !t.failing {
			slog.WarnContext(ctx, "Markdown transformer failed", "command", t.config.Command, "error", err)
		}
		t.failing = true
		return "", err
	}
	t.failing = false
	return out, nil
}

func (t *commandTransformer) request(ctx context.Context, in Input) (string, error) {
	if t.client == nil {
		if time.Now().Before(t.retryAt) {
			return "", errors.New("transformer is restarting")
		}
		cmd, err := startCommand(ctx, t.config)
		if err != nil {
			t.retryAt = time.Now().Add(restartDelay)
			return "", err
		}
		t.cmd, t.client = cmd, &rpcClient{w: cmd.stdin, lines: cmd.lines}
	}
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	var result struct {
		Markdown *string `json:"markdown"`
	}
	if err := t.client.call(ctx, transformMethod, in, &result); err != nil {
		return "", err
	}
	if result.Markdown == nil {
		return "", &rpcError{Code: -32603, Message: "result requires a markdown string"}
	}
	return *result.Markdown, nil
}

func (t *commandTransformer) restart() {
	if t.cmd == nil {
		return
	}
	t.cmd.Close()
	t.cmd, t.client = nil, nil
	t.retryAt = time.Now().Add(restartDelay)
}
