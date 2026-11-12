package evaluator

import "errors"

// TerminalError marks a failure that must end the run. Callers with a fallback
// for ordinary assessment failures must not apply it to a terminal one, such
// as an exhausted budget or untrustworthy usage accounting.
type TerminalError struct{ Err error }

func (e *TerminalError) Error() string { return e.Err.Error() }
func (e *TerminalError) Unwrap() error { return e.Err }

// IsTerminal reports whether err, or any error it wraps, is terminal.
func IsTerminal(err error) bool {
	var terminal *TerminalError
	return errors.As(err, &terminal)
}
