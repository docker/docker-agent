package teamloader

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/alpkeskin/gotoon"

	"github.com/docker/docker-agent/pkg/tools"
)

type toonTools struct {
	tools.ToolSet
	toolRegexps []*regexp.Regexp
}

// Verify interface compliance
var _ tools.Unwrapper = (*toonTools)(nil)

// Start forwards the Start call to the inner toolset if it implements Startable.
func (f *toonTools) Start(ctx context.Context) error {
	if startable, ok := f.ToolSet.(tools.Startable); ok {
		return startable.Start(ctx)
	}
	return nil
}

// Stop forwards the Stop call to the inner toolset if it implements Startable.
func (f *toonTools) Stop(ctx context.Context) error {
	if startable, ok := f.ToolSet.(tools.Startable); ok {
		return startable.Stop(ctx)
	}
	return nil
}

func (f *toonTools) Tools(ctx context.Context) ([]tools.Tool, error) {
	allTools, err := f.ToolSet.Tools(ctx)
	if err != nil {
		return nil, err
	}

	for i, tool := range allTools {
		for _, regex := range f.toolRegexps {
			if !regex.MatchString(tool.Name) {
				continue
			}

			handler := tool.Handler
			tool.Handler = func(ctx context.Context, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
				res, err := handler(ctx, toolCall)
				if err != nil {
					return res, err
				}

				var o map[string]any
				err = json.Unmarshal([]byte(res.Output), &o)
				if err != nil {
					return res, nil
				}

				tooned, err := gotoon.Encode(o)
				if err != nil {
					return res, err
				}

				res.Output = tooned
				return res, nil
			}
			allTools[i] = tool
		}
	}

	return allTools, nil
}

// Unwrap implements tools.Unwrapper.
func (f *toonTools) Unwrap() tools.ToolSet {
	return f.ToolSet
}

func WithToon(inner tools.ToolSet, toon string) tools.ToolSet {
	if toon == "" {
		return inner
	}

	var toolRegexps []*regexp.Regexp

	for toolName := range strings.SplitSeq(toon, ",") {
		toolRegexps = append(toolRegexps, regexp.MustCompile(strings.TrimSpace(toolName)))
	}
	return &toonTools{
		ToolSet:     inner,
		toolRegexps: toolRegexps,
	}
}
