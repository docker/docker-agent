package builtins

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/hooks"
)

// AddDirectoryListing is the registered name of the add_directory_listing builtin.
const AddDirectoryListing = "add_directory_listing"

// maxDirectoryEntries caps how many entries from the cwd top level are
// included in the listing. A very large project root would otherwise
// dominate every prompt. Hidden entries (dot-files) are skipped before
// the cap is applied so the visible budget isn't burned on
// configuration noise.
const maxDirectoryEntries = 100

// addDirectoryListing emits a one-shot listing of the working
// directory's top-level entries as session_start additional context.
//
// Hidden entries (names starting with ".") are skipped; the rest are
// sorted alphabetically with a trailing "/" on directories. Output is
// capped at [maxDirectoryEntries] with a "... and N more" line when
// truncated. Read errors are logged at debug and produce a nil Output
// rather than aborting session start.
//
// No-op when Input.Cwd is empty or the listing has no visible entries.
func addDirectoryListing(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
	if in == nil || in.Cwd == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(in.Cwd)
	if err != nil {
		slog.Debug("add_directory_listing: read dir failed; skipping", "cwd", in.Cwd, "error", err)
		return nil, nil
	}

	var names []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, nil
	}
	slices.Sort(names)

	truncated := 0
	if len(names) > maxDirectoryEntries {
		truncated = len(names) - maxDirectoryEntries
		names = names[:maxDirectoryEntries]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Top-level entries in %s:\n\n", in.Cwd)
	for _, n := range names {
		b.WriteString("- ")
		b.WriteString(n)
		b.WriteByte('\n')
	}
	if truncated > 0 {
		fmt.Fprintf(&b, "... and %d more\n", truncated)
	}

	return hooks.NewAdditionalContextOutput(hooks.EventSessionStart, strings.TrimRight(b.String(), "\n")), nil
}
