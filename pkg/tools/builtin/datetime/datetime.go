// Package datetime provides the current date and time in a model-chosen format.
package datetime

import (
	"context"
	"fmt"
	"strings"
	"time"
	// Keep IANA zones available in minimal containers and browser builds.
	_ "time/tzdata"

	"github.com/docker/docker-agent/pkg/tools"
)

const ToolNameGetDatetime = "get_datetime"

// CreateToolSet is the registry entry point.
func CreateToolSet() (tools.ToolSet, error) {
	return New(), nil
}

type ToolSet struct {
	now func() time.Time
}

type Args struct {
	Format   string `json:"format" jsonschema:"Go time layout using the reference Mon Jan 2 15:04:05 MST 2006. Examples: 2006-01-02 for date, 15:04:05 for time, 2006-01-02T15:04:05Z07:00 for RFC3339 datetime, Monday, January 2, 2006 for a long date. Use Go layouts, not strftime or YYYY-MM-DD tokens."`
	Timezone string `json:"timezone,omitempty" jsonschema:"Optional IANA timezone, e.g. Europe/Paris, America/New_York, or UTC. Omit or use Local for the host's local timezone."`
}

func New() *ToolSet {
	return &ToolSet{now: time.Now}
}

func (t *ToolSet) get(_ context.Context, args Args) (*tools.ToolCallResult, error) {
	if strings.TrimSpace(args.Format) == "" {
		return tools.ResultError("Error: format is required; use a Go time layout such as 2006-01-02 or 15:04:05."), nil
	}

	var location *time.Location
	if args.Timezone != "" && args.Timezone != "Local" {
		var err error
		location, err = time.LoadLocation(args.Timezone)
		if err != nil {
			return tools.ResultError(fmt.Sprintf("Error: invalid timezone %q: %v", args.Timezone, err)), nil
		}
	}

	now := t.now()
	if location == nil {
		now = localTime(now)
	} else {
		now = now.In(location)
	}
	return tools.ResultSuccess(now.Format(args.Format)), nil
}

func (t *ToolSet) Instructions() string {
	return `## Date and Time

Call get_datetime to obtain the current date, time, or datetime rather than guessing.
Choose format using Go's reference time: Mon Jan 2 15:04:05 MST 2006.
Examples: "2006-01-02" (date), "15:04:05" (time), "2006-01-02T15:04:05Z07:00" (datetime).
Do not use strftime directives or YYYY-MM-DD tokens. The result is the formatted string only.
Timezone defaults to the host's local timezone; set timezone to an IANA name or UTC when needed.`
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:               ToolNameGetDatetime,
			Category:           "datetime",
			Description:        "Obtain the current date, time, or datetime in the chosen Go time layout, optionally in a specific timezone. Returns only the formatted string. Read-only, no side effects.",
			Parameters:         tools.MustSchemaFor[Args](),
			OutputSchema:       tools.MustSchemaFor[string](),
			Handler:            tools.NewHandler(t.get),
			AllowRepeatedCalls: true,
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "Date and Time",
			},
		},
	}, nil
}
