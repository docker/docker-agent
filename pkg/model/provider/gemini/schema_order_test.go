package gemini

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
)

func TestWriteFileSchemaPropertyOrdering(t *testing.T) {
	t.Parallel()
	tool := tools.AddDescriptionParameter([]tools.Tool{{Name: "write_file", Parameters: tools.MustSchemaFor[filesystem.WriteFileArgs](), AddDescriptionParameter: true}})[0]
	schema, err := ConvertParametersToSchema(tool.Parameters)
	require.NoError(t, err)
	assert.Equal(t, []string{"path", "content", "description"}, schema.PropertyOrdering)
}

func TestNestedOptionalArgumentsKeepDeclarationOrder(t *testing.T) {
	t.Parallel()
	type child struct {
		Zulu  string `json:"zulu,omitempty"`
		Alpha string `json:"alpha"`
	}
	type args struct {
		Zulu  string  `json:"zulu,omitempty"`
		Alpha []child `json:"alpha"`
	}
	schema, err := ConvertParametersToSchema(tools.MustSchemaFor[args]())
	require.NoError(t, err)
	assert.Equal(t, []string{"zulu", "alpha"}, schema.PropertyOrdering)
	assert.Equal(t, []string{"zulu", "alpha"}, schema.Properties["alpha"].Items.PropertyOrdering)
}
