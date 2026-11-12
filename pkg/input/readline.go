package input

import (
	"bufio"
	"context"
	"io"
)

// ReadLine waits for a line or cancellation; cancellation cannot interrupt rd itself.
func ReadLine(ctx context.Context, rd io.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	results := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(rd).ReadString('\n')
		results <- result{line: line, err: err}
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result := <-results:
		if result.err != nil {
			return "", result.err
		}
		return result.line, nil
	}
}
