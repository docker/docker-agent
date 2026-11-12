package tools_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
)

type authorizationProbeError struct {
	match error
	calls []string
}

func (*authorizationProbeError) Error() string { return "authorization probe" }

func (e *authorizationProbeError) As(target any) bool {
	e.calls = append(e.calls, fmt.Sprintf("%T", target))
	switch target := target.(type) {
	case **tools.PartialStartError:
		match, ok := errors.AsType[*tools.PartialStartError](e.match)
		*target = match
		return ok
	case **tools.TotalStartError:
		match, ok := errors.AsType[*tools.TotalStartError](e.match)
		*target = match
		return ok
	case **tools.AuthorizationRequiredError:
		match, ok := errors.AsType[*tools.AuthorizationRequiredError](e.match)
		*target = match
		return ok
	default:
		return false
	}
}

func TestIsAuthorizationRequiredProbeOrder(t *testing.T) {
	t.Parallel()

	probes := []string{"**tools.PartialStartError", "**tools.TotalStartError", "**tools.AuthorizationRequiredError"}
	for _, tc := range []struct {
		name  string
		match error
		want  bool
		calls int
	}{
		{"partial auth", &tools.PartialStartError{AuthOnly: true}, true, 1},
		{"partial mixed", &tools.PartialStartError{}, false, 1},
		{"total auth", &tools.TotalStartError{AuthOnly: true}, true, 2},
		{"total mixed", &tools.TotalStartError{}, false, 2},
		{"authorization", &tools.AuthorizationRequiredError{}, true, 3},
		{"unrelated", errors.New("unrelated"), false, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe := &authorizationProbeError{match: tc.match}
			err := fmt.Errorf("starting: %w", probe)

			assert.Equal(t, tc.want, tools.IsAuthorizationRequired(err))
			assert.Equal(t, probes[:tc.calls], probe.calls)
		})
	}
}
