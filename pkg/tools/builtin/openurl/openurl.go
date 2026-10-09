// Package openurl implements a built-in toolset that exposes a single tool
// which opens a fixed URL in the user's default browser. The URL is baked
// into the toolset definition (the YAML `url` field), so the model only has
// to call the tool by name — it never supplies the URL itself.
//
// Opening the URL is delegated to pkg/browser, which picks the right platform
// helper (open / xdg-open / rundll32), so the tool works across macOS, Linux
// and Windows.
package openurl

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/docker/docker-agent/pkg/browser"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/tools"
)

type urlExpander interface {
	Expand(ctx context.Context, text string, values map[string]string) string
}

const ToolNameOpenURL = "open_url"

type ToolSet struct {
	url      string
	name     string
	expander urlExpander
	open     func(context.Context, string) error
}

// Verify interface compliance.
var (
	_ tools.ToolSet      = (*ToolSet)(nil)
	_ tools.Instructable = (*ToolSet)(nil)
)

// CreateToolSet is used by the tools registry.
func CreateToolSet(toolset latest.Toolset, runConfig *config.RuntimeConfig) (tools.ToolSet, error) {
	if toolset.URL == "" {
		return nil, errors.New("open_url toolset requires a url to be set")
	}

	expander := js.NewJsExpander(runConfig.EnvProvider())
	return New(toolset.URL, WithName(toolset.Name), WithExpander(expander)), nil
}

func New(url string, opts ...Option) *ToolSet {
	t := &ToolSet{
		url:  url,
		open: browser.Open,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

type Option func(*ToolSet)

// WithName overrides the tool name. An empty name keeps the default.
func WithName(name string) Option {
	return func(t *ToolSet) { t.name = name }
}

// WithExpander enables ${env.X} interpolation of the configured URL.
func WithExpander(expander urlExpander) Option {
	if concrete, ok := expander.(*js.Expander); ok && concrete == nil {
		expander = nil
	}
	return func(t *ToolSet) { t.expander = expander }
}

// WithOpener overrides the function used to open the URL. Tests use it to
// avoid spawning a real browser.
func WithOpener(open func(context.Context, string) error) Option {
	return func(t *ToolSet) { t.open = open }
}

func (t *ToolSet) toolName() string {
	return cmp.Or(t.name, ToolNameOpenURL)
}

func (t *ToolSet) Instructions() string {
	return fmt.Sprintf("## Open URL Tool\n\nUse the %q tool to open %s in the user's default browser.", t.toolName(), t.url)
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:         t.toolName(),
			Category:     "open_url",
			Description:  fmt.Sprintf("Open the URL %q in the user's default browser. Takes no arguments — the URL is fixed by the tool's configuration.", t.url),
			Parameters:   tools.MustSchemaFor[struct{}](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.callTool),
			Annotations: tools.ToolAnnotations{
				Title: cmp.Or(t.name, "Open URL"),
			},
		},
	}, nil
}

func (t *ToolSet) callTool(ctx context.Context, _ struct{}) (*tools.ToolCallResult, error) {
	url := t.url
	if t.expander != nil {
		url = t.expander.Expand(ctx, url, nil)
	}

	// browser.Open validates the (post-expansion) URL before launching: it
	// requires a non-empty scheme and rejects flag-like values (leading "-"),
	// which would otherwise be parsed as options by the OS launcher
	// (open / xdg-open / rundll32) — argument injection. Both static and
	// env-expanded URLs go through this guard. See pkg/browser.validate.
	//
	// The launcher hands the URL to the OS and exits immediately; the OS then
	// opens the browser asynchronously. exec.CommandContext kills the launcher
	// when its context is canceled, so we strip cancellation (while keeping
	// trace/values) to prevent a finishing or interrupted agent turn from
	// killing the launcher mid-handoff.
	if err := t.open(context.WithoutCancel(ctx), url); err != nil {
		return tools.ResultError(fmt.Sprintf("failed to open %s: %v", url, err)), nil
	}

	return tools.ResultSuccess("Opened " + url), nil
}
