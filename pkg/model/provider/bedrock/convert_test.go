package bedrock

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestToolSchemaPreservesDeclarationOrder(t *testing.T) {
	t.Parallel()
	type child struct {
		Zulu  string `json:"zulu,omitempty"`
		Alpha string `json:"alpha"`
	}
	type args struct {
		Zulu  string  `json:"zulu,omitempty"`
		Alpha []child `json:"alpha"`
	}
	schema := convertToolSchema(tools.MustSchemaFor[args]())
	data, err := schema.MarshalSmithyDocument()
	require.NoError(t, err)
	assert.NotContains(t, string(data), "x-docker-agent-property-order")
	assert.Contains(t, string(data), `"properties":{"zulu":`)
}
