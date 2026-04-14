package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"

	"github.com/docker/docker-agent/pkg/chat"
)

// StreamValue represents a single value yielded during streaming.
type StreamValue[Out, Stream any] struct {
	Done     bool
	Chunk    Stream         // valid if Done is false
	Value    Out            // valid if Done is true
	Response *ModelResponse // valid if Done is true
}

// ModelStreamValue is a stream value for a model response.
// Out is never set because the value is already available in the Response field.
type ModelStreamValue = StreamValue[struct{}, chat.MessageStreamResponse]

// GenerateStream generates a model response and streams the output.
// It returns an iterator that yields streaming results.
func GenerateStream(ctx context.Context, opts ...Option) iter.Seq2[*ModelStreamValue, error] {
	return func(yield func(*ModelStreamValue, error) bool) {
		c := &completion{
			yield: func(resp chat.MessageStreamResponse) bool {
				return yield(&ModelStreamValue{
					Done:  false,
					Chunk: resp,
				}, nil)
			},
		}

		c = c.applyOptions(opts...)

		res, err := c.generate(ctx)
		if errors.Is(err, io.EOF) {
			return
		}

		if err != nil {
			yield(nil, err)
			return
		}

		yield(&ModelStreamValue{
			Done:     true,
			Response: res,
		}, nil)
	}
}

// Generate runs a completion and returns the final model response.
// It handles retry, fallback, tool execution, and streaming internally.
func Generate(ctx context.Context, opts ...Option) (*ModelResponse, error) {
	return new(completion).applyOptions(opts...).generate(ctx)
}

// GenerateText is a convenience wrapper around Generate that returns
// only the text content from the model response.
func GenerateText(ctx context.Context, opts ...Option) (string, error) {
	res, err := Generate(ctx, opts...)
	if err != nil {
		return "", err
	}

	return res.Content, nil
}

// GenerateValue runs a completion and unmarshals the model's response
// content into the provided type. Use with structured output to get
// typed responses from the model.
func GenerateValue[Out any](ctx context.Context, opts ...Option) (*Out, error) {
	res, err := Generate(ctx, opts...)
	if err != nil {
		return nil, err
	}

	var out Out
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		return nil, err
	}

	return &out, nil
}
