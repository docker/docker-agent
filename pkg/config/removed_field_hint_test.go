package config

import (
	"errors"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// TestRemovedFieldHint_FieldRemovedInOlderVersion pins the main case this
// hint exists for: `safer` was part of the shell toolset through config
// version 14 and has not existed since (it was dropped for v15, see
// PR #4169). Declaring the latest version and hitting an unknown-field
// error for `safer` should point at version 14, not offer to lower
// `version` as if that were the fix.
func TestRemovedFieldHint_FieldRemovedInOlderVersion(t *testing.T) {
	t.Parallel()

	err := &yaml.UnknownFieldError{Message: `unknown field "safer"`}
	hint := removedFieldHint(latest.Version, err)

	assert.Contains(t, hint, "'safer'")
	assert.Contains(t, hint, "config version 14")
	assert.Contains(t, hint, "delete it from your config")
	assert.NotContains(t, hint, "update the top-level 'version' field", "should not read like newerVersionHint's bump-the-version advice")
}

// TestRemovedFieldHint_FieldNeverExisted ensures a key that was never valid
// in any registered config version produces no hint: the whole point is to
// only fire for fields we can prove were once part of the schema.
func TestRemovedFieldHint_FieldNeverExisted(t *testing.T) {
	t.Parallel()

	err := &yaml.UnknownFieldError{Message: `unknown field "not_a_real_key_ever"`}
	hint := removedFieldHint(latest.Version, err)

	assert.Empty(t, hint)
}

// TestRemovedFieldHint_FieldStillValid guards against a false positive when
// the "unknown field" error is really about a field being nested in the
// wrong place rather than removed: if the declared version's own schema
// still contains the field name somewhere, we stay silent instead of
// claiming it was removed.
func TestRemovedFieldHint_FieldStillValid(t *testing.T) {
	t.Parallel()

	err := &yaml.UnknownFieldError{Message: `unknown field "model"`}
	hint := removedFieldHint(latest.Version, err)

	assert.Empty(t, hint)
}

// TestRemovedFieldHint_NonUnknownFieldError ensures the hint only reacts to
// UnknownFieldError, leaving other parse failures (type mismatches, syntax
// errors) alone.
func TestRemovedFieldHint_NonUnknownFieldError(t *testing.T) {
	t.Parallel()

	hint := removedFieldHint(latest.Version, errors.New("some other parse error"))

	assert.Empty(t, hint)
}

// TestRemovedFieldHint_NonNumericVersion guards the strconv.Atoi bailout: a
// malformed declared version must not panic or produce a bogus hint.
func TestRemovedFieldHint_NonNumericVersion(t *testing.T) {
	t.Parallel()

	err := &yaml.UnknownFieldError{Message: `unknown field "safer"`}
	hint := removedFieldHint("not-a-version", err)

	assert.Empty(t, hint)
}

// TestSchemaFieldNames_SaferOnlyInV14 spot-checks the reflection helper
// backing removedFieldHint against the known safer/instruction_file history:
// safer is reachable from v14's Config type and not from v15 or latest;
// instruction_file (introduced in v11) is reachable from every version at
// or after that.
func TestSchemaFieldNames_SaferOnlyInV14(t *testing.T) {
	t.Parallel()

	parsers, _ := versions()

	assert.True(t, schemaFieldNames(parsers, "14")["safer"], "safer should still be reachable from v14's schema")
	assert.False(t, schemaFieldNames(parsers, "15")["safer"], "safer was removed as of v15")
	assert.False(t, schemaFieldNames(parsers, latest.Version)["safer"], "safer must not be reachable from latest")

	assert.True(t, schemaFieldNames(parsers, "11")["instruction_file"])
	assert.True(t, schemaFieldNames(parsers, latest.Version)["instruction_file"])
}

// TestSchemaFieldNames_UntaggedFieldFallsBackToLowercaseName guards the gap
// found in review: AgentConfig.Name carries no json or yaml tag at all, so
// go-yaml matches it against the lowercased Go field name ("name"). Without
// replicating that fallback, schemaFieldNames would never see "name" as
// part of the schema, and any other untagged field would be silently
// invisible to removedFieldHint.
func TestSchemaFieldNames_UntaggedFieldFallsBackToLowercaseName(t *testing.T) {
	t.Parallel()

	parsers, _ := versions()
	assert.True(t, schemaFieldNames(parsers, latest.Version)["name"], "AgentConfig.Name has no json/yaml tag and must fall back to its lowercased Go name")
}
