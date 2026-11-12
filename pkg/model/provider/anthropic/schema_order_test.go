package anthropic

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
)

func TestWriteFileWireSchemaPathFirst(t *testing.T) {
	t.Parallel()
	schema, err := ConvertParametersToSchema(tools.MustSchemaFor[filesystem.WriteFileArgs]())
	require.NoError(t, err)
	wire, err := json.Marshal(anthropic.ToolParam{Name: "write_file", InputSchema: schema})
	require.NoError(t, err)
	assert.Less(t, bytes.Index(wire, []byte(`"path":`)), bytes.Index(wire, []byte(`"content":`)))
}

func TestBetaToolsRetainNestedDeclarationOrder(t *testing.T) {
	t.Parallel()
	type child struct {
		Zulu  string `json:"zulu,omitempty"`
		Alpha string `json:"alpha"`
	}
	type args struct {
		Zulu  string  `json:"zulu,omitempty"`
		Alpha []child `json:"alpha"`
	}
	converted, err := convertBetaTools([]tools.Tool{{Name: "ordered", Parameters: tools.MustSchemaFor[args]()}})
	require.NoError(t, err)
	data, err := json.Marshal(converted)
	require.NoError(t, err)
	assert.Equal(t, 2, bytes.Count(data, []byte(`"properties":{"zulu":`)))
	assert.NotContains(t, string(data), "x-docker-agent-property-order")
}
