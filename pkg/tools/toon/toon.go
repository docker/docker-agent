// Package toon wraps tool outputs in TOON, a compact encoding of JSON that
// saves tokens on large structured results. Toolsets opt in with the `toon`
// field of their YAML declaration; teamloader applies the wrapper when
// enabled with teamloader.WithToon(toon.Wrap).
package toon

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
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

func (f *toonTools) Tools(ctx context.Context) ([]tools.Tool, error) {
	allTools, err := f.ToolSet.Tools(ctx)
	if err != nil {
		return nil, err
	}

	// Clone: inner toolsets (e.g. MCP) may hand out their cached slice, and
	// mutating it would re-wrap the same handlers on every listing.
	result := slices.Clone(allTools)
	for i := range result {
		if f.matches(result[i].Name) {
			result[i].Handler = toonHandler(result[i].Handler)
		}
	}

	return result, nil
}

func (f *toonTools) matches(name string) bool {
	for _, regex := range f.toolRegexps {
		if regex.MatchString(name) {
			return true
		}
	}
	return false
}

func toonHandler(handler tools.ToolHandler) tools.ToolHandler {
	return func(ctx context.Context, toolCall tools.ToolCall, rt tools.Runtime) (*tools.ToolCallResult, error) {
		res, err := handler(ctx, toolCall, rt)
		if err != nil {
			return res, err
		}

		var o map[string]any
		if err := json.Unmarshal([]byte(res.Output), &o); err != nil {
			return res, nil
		}

		tooned, err := gotoon.Encode(o)
		if err != nil {
			return res, err
		}

		res.Output = tooned
		return res, nil
	}
}

// Unwrap implements tools.Unwrapper.
func (f *toonTools) Unwrap() tools.ToolSet {
	return f.ToolSet
}

// Wrap TOON-encodes the JSON output of the tools whose name matches one of
// the comma-separated regular expressions in spec.
func Wrap(inner tools.ToolSet, spec string) tools.ToolSet {
	if spec == "" {
		return inner
	}

	var toolRegexps []*regexp.Regexp

	for toolName := range strings.SplitSeq(spec, ",") {
		toolRegexps = append(toolRegexps, regexp.MustCompile(strings.TrimSpace(toolName)))
	}
	return &toonTools{
		ToolSet:     inner,
		toolRegexps: toolRegexps,
	}
}
