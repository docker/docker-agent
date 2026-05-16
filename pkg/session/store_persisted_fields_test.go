package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionPersistedFieldsOf_Defaults(t *testing.T) {
	got, err := sessionPersistedFieldsOf(&Session{ID: "x"})
	require.NoError(t, err)

	assert.Empty(t, got.PermissionsJSON, "nil permissions should serialise to empty string")
	assert.Equal(t, "{}", got.AgentModelOverridesJSON, "nil overrides should default to {}")
	assert.Equal(t, "[]", got.CustomModelsUsedJSON, "nil models should default to []")
	assert.Nil(t, got.ParentID, "empty parent_id must encode as SQL NULL")
}

func TestSessionPersistedFieldsOf_PopulatedValues(t *testing.T) {
	session := &Session{
		ID:       "x",
		ParentID: "parent-1",
		Permissions: &PermissionsConfig{
			Allow: []string{"shell"},
		},
		AgentModelOverrides: map[string]string{"root": "openai/gpt-5"},
		CustomModelsUsed:    []string{"openai/gpt-5"},
	}

	got, err := sessionPersistedFieldsOf(session)
	require.NoError(t, err)

	assert.JSONEq(t, `{"allow":["shell"]}`, got.PermissionsJSON)
	assert.JSONEq(t, `{"root":"openai/gpt-5"}`, got.AgentModelOverridesJSON)
	assert.JSONEq(t, `["openai/gpt-5"]`, got.CustomModelsUsedJSON)
	assert.Equal(t, "parent-1", got.ParentID)
}

func TestSessionPersistedFieldsOf_EmptyMapsAndSlicesUseDefaults(t *testing.T) {
	// len() == 0 for non-nil empty values must take the default branch
	// because JSON encoding "{}" / "[]" matches what the schema expects.
	got, err := sessionPersistedFieldsOf(&Session{
		AgentModelOverrides: map[string]string{},
		CustomModelsUsed:    []string{},
	})
	require.NoError(t, err)
	assert.Equal(t, "{}", got.AgentModelOverridesJSON)
	assert.Equal(t, "[]", got.CustomModelsUsedJSON)
}
