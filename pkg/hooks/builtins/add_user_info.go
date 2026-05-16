package builtins

import (
	"context"
	"log/slog"
	"os"
	"os/user"
	"strings"

	"github.com/docker/docker-agent/pkg/hooks"
)

// AddUserInfo is the registered name of the add_user_info builtin.
const AddUserInfo = "add_user_info"

// addUserInfo emits the current OS user (username and full name) and
// hostname as session_start additional context.
//
// Lookup failures from [user.Current] / [os.Hostname] are logged at
// debug and skipped individually; the builtin still emits whichever
// pieces it could resolve. Returns a nil Output only when nothing
// could be discovered at all (e.g. an unusual sandbox where both
// lookups fail) rather than producing an empty stanza.
func addUserInfo(_ context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
	var lines []string

	if u, err := user.Current(); err != nil {
		slog.Debug("add_user_info: user.Current failed; skipping", "error", err)
	} else {
		if u.Username != "" {
			lines = append(lines, "User: "+u.Username)
		}
		// Name is often empty on minimal containers; only include it
		// when present so the section stays useful.
		if name := strings.TrimSpace(u.Name); name != "" {
			lines = append(lines, "Full name: "+name)
		}
	}

	if host, err := os.Hostname(); err != nil {
		slog.Debug("add_user_info: os.Hostname failed; skipping", "error", err)
	} else if host != "" {
		lines = append(lines, "Hostname: "+host)
	}

	if len(lines) == 0 {
		return nil, nil
	}
	return hooks.NewAdditionalContextOutput(hooks.EventSessionStart, "Current user:\n\n"+strings.Join(lines, "\n")), nil
}
