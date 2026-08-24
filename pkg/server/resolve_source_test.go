package server

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

func gordonKey(tag string) string {
	return url.QueryEscape("http://localhost:7777/gordon-agent?gordonTag=" + tag + "&desktopVersion=4.81.0&origin=desktop")
}

// TestResolveSource_ExactMatchPreferred verifies that an exact key match is
// always used, even when other variants exist that would also normalise to the
// same stable identity.
func TestResolveSource_ExactMatchPreferred(t *testing.T) {
	t.Parallel()

	light := config.NewBytesSource("light", []byte("light"))
	dev := config.NewBytesSource("dev", []byte("dev"))
	sm := &SessionManager{
		Sources: config.Sources{
			gordonKey("v9-light"): light,
			gordonKey("v9-dev"):   dev,
		},
	}

	got, err := sm.resolveSource(gordonKey("v9-dev"))
	require.NoError(t, err)
	assert.Equal(t, "dev", got.Name())
}

// TestResolveSource_FallbackAcrossTag is the resume-after-upgrade case: the
// session recorded the v9-light key, but the server was relaunched with only
// v9-dev. The fallback resolves it because the two share a stable identity.
func TestResolveSource_FallbackAcrossTag(t *testing.T) {
	t.Parallel()

	dev := config.NewBytesSource("dev", []byte("dev"))
	sm := &SessionManager{
		Sources: config.Sources{
			gordonKey("v9-dev"): dev,
		},
	}

	got, err := sm.resolveSource(gordonKey("v9-light"))
	require.NoError(t, err)
	assert.Equal(t, "dev", got.Name(), "an old session's tagged ref should resolve to the live source")
}

// TestResolveSource_AmbiguousFallbackFails verifies that the fallback refuses
// to guess: when several live sources share the requested stable identity and
// none matches exactly, it returns not-found rather than picking one.
func TestResolveSource_AmbiguousFallbackFails(t *testing.T) {
	t.Parallel()

	sm := &SessionManager{
		Sources: config.Sources{
			gordonKey("v9-light"): config.NewBytesSource("light", []byte("light")),
			gordonKey("v9-dev"):   config.NewBytesSource("dev", []byte("dev")),
		},
	}

	// A third tag not present exactly; two candidates share its identity.
	_, err := sm.resolveSource(gordonKey("v9-canary"))
	require.ErrorIs(t, err, ErrAgentNotFound)
	assert.Contains(t, err.Error(), "agent not found")
}

// TestResolveSource_NoMatchFails verifies the plain not-found path is preserved
// for references that share no stable identity with any live source.
func TestResolveSource_NoMatchFails(t *testing.T) {
	t.Parallel()

	sm := &SessionManager{
		Sources: config.Sources{
			gordonKey("v9-dev"): config.NewBytesSource("dev", []byte("dev")),
		},
	}

	_, err := sm.resolveSource(url.QueryEscape("http://localhost:7777/other-agent?gordonTag=v9-dev"))
	require.ErrorIs(t, err, ErrAgentNotFound)
	assert.Contains(t, err.Error(), "agent not found")
}

// TestResolveSource_LocalFileKeyExactOnly verifies that non-URL keys (local
// files, OCI refs) still resolve only by exact match, since their stable
// identity is the key itself.
func TestResolveSource_LocalFileKeyExactOnly(t *testing.T) {
	t.Parallel()

	sm := &SessionManager{
		Sources: config.Sources{
			"my-agent": config.NewBytesSource("my-agent", []byte("x")),
		},
	}

	got, err := sm.resolveSource("my-agent")
	require.NoError(t, err)
	assert.Equal(t, "my-agent", got.Name())

	_, err = sm.resolveSource("other-agent")
	require.Error(t, err)
}
