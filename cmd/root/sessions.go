package root

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/replay"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

type sessionsDiffFlags struct {
	sessionDB  string
	asJSON     bool
	failOnDiff bool
}

// newSessionsCmd groups session-inspection subcommands.
//
// Deliberately not called "replay": pkg/recording already owns that word for
// recording and replaying API interactions, and `--record` writes cassettes.
// This command replays nothing — it diffs two recordings — and naming it replay
// would also take the word from the re-run-against-another-model feature that
// actually is a replay.
func newSessionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sessions",
		Short:   "Inspect recorded sessions",
		GroupID: "advanced",
	}
	cmd.AddCommand(newSessionsDiffCmd())
	return cmd
}

func newSessionsDiffCmd() *cobra.Command {
	var flags sessionsDiffFlags

	cmd := &cobra.Command{
		Use:   "diff <session-a> <session-b>",
		Short: "Compare the behaviour of two recorded sessions",
		Args:  cobra.ExactArgs(2),
		Long: `Compare two recorded sessions and report the first point where the agent
behaved differently.

Comparison is over the sequence of tool calls, not over the assistant's prose.
Model output is nondeterministic: two runs of the same task almost always word
things differently while doing exactly the same work, so diffing text would report
a difference on every comparison. The tool calls are what changed the world, so
they are what is compared.

Reporting stops at the first divergence: everything after it is downstream of that
difference and comparing it produces noise rather than information.`,
		Example: `  docker agent sessions diff <session-a> <session-b>
  docker agent sessions diff -1 -2
  docker agent sessions diff <a> <b> --json | jq '.divergence.turn_index'
  docker agent sessions diff <a> <b> --fail-on-divergence`,
		RunE: flags.run,
	}

	cmd.Flags().StringVarP(&flags.sessionDB, "session-db", "s", "", "Path to the session database (default: <data-dir>/session.db)")
	cmd.Flags().BoolVar(&flags.asJSON, "json", false, "Emit the comparison as JSON")
	cmd.Flags().BoolVar(&flags.failOnDiff, "fail-on-divergence", false, "Exit non-zero when the two sessions diverge")

	return cmd
}

func (f *sessionsDiffFlags) run(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	dbPath, err := pathx.ExpandHomeDir(sessionDBPath(f.sessionDB))
	if err != nil {
		return err
	}

	store, err := sqlitestore.New(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("opening session store: %w", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			slog.ErrorContext(ctx, "Failed to close session store", "error", err)
		}
	}()

	sessA, err := loadSessionRef(ctx, store, args[0])
	if err != nil {
		return err
	}
	sessB, err := loadSessionRef(ctx, store, args[1])
	if err != nil {
		return err
	}

	result := replay.CompareSessions(sessA, sessB)
	if err := renderReplay(cmd.OutOrStdout(), result, args[0], args[1], f.asJSON); err != nil {
		return err
	}

	if f.failOnDiff && !result.Identical() {
		return errors.New("sessions diverged")
	}
	return nil
}

// loadSessionRef resolves a user-supplied reference and loads the session.
//
// References go through session.ResolveSessionID like every other
// session-consuming command, so relative forms work — "compare my last two
// runs" is `sessions diff -1 -2`. An unambiguous ID prefix is accepted too,
// since full UUIDs are the hardest thing for a user to produce by hand.
func loadSessionRef(ctx context.Context, store session.Store, ref string) (*session.Session, error) {
	id, err := session.ResolveSessionID(ctx, store, ref)
	if err != nil {
		return nil, err
	}
	if sess, err := store.GetSession(ctx, id); err == nil {
		return sess, nil
	}

	summaries, err := store.GetSessionSummaries(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	var matches []string
	for _, summary := range summaries {
		if strings.HasPrefix(summary.ID, id) {
			matches = append(matches, summary.ID)
		}
	}
	switch len(matches) {
	case 1:
		sess, err := store.GetSession(ctx, matches[0])
		if err != nil {
			return nil, fmt.Errorf("reading session %q: %w", ref, err)
		}
		return sess, nil
	case 0:
		return nil, fmt.Errorf("no session matches %q", ref)
	default:
		return nil, fmt.Errorf("%q matches %d sessions; use more characters", ref, len(matches))
	}
}

// renderReplay writes the comparison as JSON or as text.
func renderReplay(w io.Writer, result replay.Result, nameA, nameB string, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	replay.PrintResult(w, result, nameA, nameB)
	return nil
}
