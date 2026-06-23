package rulebased

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want []string
	}{
		{"lowercases", "Hello WORLD", []string{"hello", "world"}},
		{"splits on punctuation", "debug this, please!", []string{"debug"}},
		{"drops stop words", "what is the weather", []string{"weather"}},
		{"keeps numbers", "gpt 4o model", []string{"gpt", "4o", "model"}},
		{"empty", "", nil},
		{"only stop words", "the and is", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tokenize(tt.text)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDedupe(t *testing.T) {
	t.Parallel()

	got := dedupe([]string{"b", "a", "b", "c", "a"})
	assert.Equal(t, []string{"a", "b", "c"}, got)

	assert.Empty(t, dedupe(nil))
}

func TestMatcher_BestRoute(t *testing.T) {
	t.Parallel()

	m := newMatcher()
	m.add(0, "hello how are you")
	m.add(0, "hi there friend")
	m.add(0, "good morning")
	m.add(1, "explain the algorithm in detail")
	m.add(1, "debug this code")

	tests := []struct {
		name      string
		query     string
		wantRoute int
		wantOK    bool
	}{
		{"greeting", "hello there", 0, true},
		{"explain", "can you explain this algorithm to me", 1, true},
		{"debug", "debug this code please", 1, true},
		{"unrelated falls through", "what is the weather forecast for tomorrow", 0, false},
		{"empty query", "", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			route, ok := m.bestRoute(tt.query)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantRoute, route)
			}
		})
	}
}

func TestMatcher_Empty(t *testing.T) {
	t.Parallel()

	m := newMatcher()
	_, ok := m.bestRoute("anything")
	assert.False(t, ok)
}

func TestMatcher_RareTermWins(t *testing.T) {
	t.Parallel()

	// "kubernetes" appears in a single document, so it should outweigh a
	// common term shared across many documents.
	m := newMatcher()
	m.add(0, "deploy code")
	m.add(0, "deploy service")
	m.add(0, "deploy application")
	m.add(1, "kubernetes cluster")

	route, ok := m.bestRoute("deploy kubernetes")
	require.True(t, ok)
	assert.Equal(t, 1, route)
}

func TestMatcher_SkipsEmptyExamples(t *testing.T) {
	t.Parallel()

	// Examples that are empty or made entirely of stop words carry no routing
	// signal. They must not be indexed, otherwise they skew avgLen and, if every
	// example were empty, produce NaN scores that silently break routing.
	m := newMatcher()
	m.add(0, "")
	m.add(0, "the and is")
	assert.Empty(t, m.docs)
	assert.Zero(t, m.avgLen)

	// A real example after the empty ones must still match and keep avgLen sane.
	m.add(1, "deploy kubernetes cluster")
	require.Len(t, m.docs, 1)
	assert.InDelta(t, 3.0, m.avgLen, 1e-9)

	route, ok := m.bestRoute("deploy kubernetes")
	require.True(t, ok)
	assert.Equal(t, 1, route)
}

func TestMatcher_AllEmptyExamples(t *testing.T) {
	t.Parallel()

	// When every example is empty, the matcher stays empty and any query falls
	// through to the fallback rather than returning a NaN-driven false match.
	m := newMatcher()
	m.add(0, "")
	m.add(1, "the of and")

	_, ok := m.bestRoute("deploy kubernetes")
	assert.False(t, ok)
}
