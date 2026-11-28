package programresume

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSpecClearVector(t *testing.T) {
	assert.Equal(t, "\x1b]7577;pid=74877:time=1795543200:clear=1:mac=c30603369f98fef581a4098be889175b0c74f0641e49f8b25f4326cb38f97c93\x1b\\", Clear("dummy-token-replace-me", 74877, time.Unix(1795543200, 0)))
}

func TestReportValidation(t *testing.T) {
	token := "dummy-token-replace-me"
	now := time.Unix(1795543200, 0)
	assert.Contains(t, Report(token, 1, now, []string{"/bin/docker-agent", "run", "a;$(touch x)"}, "/tmp"), "argv=L2Jpbi9kb2NrZXItYWdlbnQ=,cnVu,YTskKHRvdWNoIHgp:cwd=L3RtcA==")
	for _, tc := range []struct {
		token string
		pid   int
		argv  []string
		cwd   string
	}{
		{"bad", 1, []string{"/bin/x"}, "/tmp"}, {token, 0, []string{"/bin/x"}, "/tmp"}, {token, 1, []string{"./x"}, "/tmp"}, {token, 1, []string{"/bin/x", "bad\nargument"}, "/tmp"}, {token, 1, []string{"/bin/x"}, "relative"}, {token, 1, []string{"/bin/x", strings.Repeat("a", 2100)}, "/tmp"}, {token, 1, []string{"/bin/x", string([]byte{0xff})}, "/tmp"},
	} {
		assert.Empty(t, Report(tc.token, tc.pid, now, tc.argv, tc.cwd))
	}
	assert.False(t, ValidToken(strings.Repeat("!", 22)))
	assert.False(t, ValidToken(strings.Repeat("a", 65)))
}
