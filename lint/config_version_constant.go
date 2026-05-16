package main

import (
	"strconv"

	"github.com/dgageot/rubocop-go/cop"
)

// ConfigVersionConstant enforces that a file under pkg/config/vN/ declaring a
// `const Version = "<value>"` uses "<N>" as its value.
//
// This guards against the common mistake of bumping the directory name
// without bumping the constant (or vice versa) when freezing the
// work-in-progress config and creating a new "latest". A mismatch would
// silently break the parser dispatch in pkg/config/versions.go, which
// registers parsers keyed by `Version`.
//
// Files under pkg/config/latest/ are intentionally exempt: their `Version`
// is the next, work-in-progress value (one greater than the highest vN).
var ConfigVersionConstant = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/ConfigVersionConstant",
		Description: `Version constant in pkg/config/vN/ must equal "N"`,
		Severity:    cop.Error,
	},
	Scope: cop.InPathSegment("pkg/config", func(seg string) bool {
		_, ok := versionFromDir(seg)
		return ok
	}),
	Run: func(p *cop.Pass) {
		dir, _ := p.PathSegment("pkg/config")
		dirVersion, _ := versionFromDir(dir)
		expected := strconv.Itoa(dirVersion)

		lit, ok := p.StringConstNodes()["Version"]
		if !ok {
			return
		}
		got, err := strconv.Unquote(lit.Value)
		if err != nil || got == expected {
			return
		}
		p.Reportf(lit, "Version in pkg/config/v%s/ must be %q, got %q", expected, expected, got)
	},
}
