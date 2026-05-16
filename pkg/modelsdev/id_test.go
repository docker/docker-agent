package modelsdev

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewID(t *testing.T) {
	t.Parallel()

	id := NewID("openai", "gpt-4o")
	assert.Equal(t, "openai", id.Provider)
	assert.Equal(t, "gpt-4o", id.Model)
	assert.Equal(t, "openai/gpt-4o", id.String())
	assert.True(t, id.IsValid())
	assert.False(t, id.IsZero())
}

func TestParseID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		ref         string
		wantID      ID
		wantErr     bool
		wantStringR bool // round-trips via String()
	}{
		{"valid", "openai/gpt-4o", ID{Provider: "openai", Model: "gpt-4o"}, false, true},
		{"valid with slash in model", "openai/foo/bar", ID{Provider: "openai", Model: "foo/bar"}, false, false},
		{"missing separator", "openai-gpt-4o", ID{}, true, false},
		{"missing provider", "/gpt-4o", ID{}, true, false},
		{"missing model", "openai/", ID{}, true, false},
		{"empty", "", ID{}, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseID(tt.ref)
			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, ID{}, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantID, got)
			if tt.wantStringR {
				assert.Equal(t, tt.ref, got.String())
			}
		})
	}
}

func TestParseIDOrZero(t *testing.T) {
	t.Parallel()

	id := ParseIDOrZero("openai/gpt-4o")
	assert.Equal(t, ID{Provider: "openai", Model: "gpt-4o"}, id)

	id = ParseIDOrZero("not-a-ref")
	assert.True(t, id.IsZero())
	assert.Empty(t, id.String())
}

func TestIDZero(t *testing.T) {
	t.Parallel()

	var id ID
	assert.True(t, id.IsZero())
	assert.False(t, id.IsValid())
	assert.Empty(t, id.String())
}

func TestIDIsValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id   ID
		want bool
	}{
		{ID{Provider: "openai", Model: "gpt-4o"}, true},
		{ID{Provider: "openai"}, false},
		{ID{Model: "gpt-4o"}, false},
		{ID{}, false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.id.IsValid(), "id=%+v", tt.id)
	}
}
