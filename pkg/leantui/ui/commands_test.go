package ui

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFilterCommandsStableOrdering(t *testing.T) {
	t.Parallel()
	all := []Command{
		{Name: "alpha", Kind: CmdAgent, Value: "agent-first"},
		{Name: "zulu", Kind: CmdBuiltin, Value: "builtin-zulu"},
		{Name: "alpha", Kind: CmdBuiltin, Value: "builtin-first"},
		{Name: "alpha", Kind: CmdAgent, Value: "agent-second"},
		{Name: "alpha", Kind: CmdBuiltin, Value: "builtin-second"},
	}
	assert.Equal(t, []Command{all[2], all[4], all[1], all[0], all[3]}, FilterCommands(all, ""))
	assert.Equal(t, []Command{all[2], all[4], all[0], all[3]}, FilterCommands(all, "AL"))
	assert.Nil(t, FilterCommands(all, "missing"))
	assert.Equal(t, "agent-first", all[0].Value, "filtering must not reorder the input")
}

func TestFilterScopedCommandsStableRanking(t *testing.T) {
	t.Parallel()
	var calls []string
	match := func(name string, score int, ok bool) func(string) (int, bool) {
		return func(query string) (int, bool) {
			assert.Equal(t, "query", query)
			calls = append(calls, name)
			return score, ok
		}
	}
	all := []Command{
		{Name: "low", MatchScore: match("low", math.MinInt, true)},
		{Name: "first", MatchScore: match("first", math.MaxInt, true)},
		{Name: "filtered", MatchScore: match("filtered", math.MaxInt, false)},
		{Name: "middle", MatchScore: match("middle", 0, true)},
		{Name: "second", MatchScore: match("second", math.MaxInt, true)},
		{Name: "no matcher"},
	}
	matches := FilterScopedCommands(all, " query ")
	var names []string
	for _, command := range matches {
		names = append(names, command.Name)
	}
	assert.Equal(t, []string{"first", "second", "middle", "low"}, names)
	assert.Equal(t, []string{"low", "first", "filtered", "middle", "second"}, calls)
	assert.Equal(t, "low", all[0].Name, "filtering must not reorder the input")
}

func TestFilterScopedCommandsEmptyQueryPreservesOrder(t *testing.T) {
	t.Parallel()
	all := []Command{{Name: "second"}, {Name: "first"}}
	assert.Equal(t, all, FilterScopedCommands(all, " "))
	assert.Empty(t, FilterScopedCommands(nil, "query"))
}
