package openai

import (
	"bytes"
	"encoding/json"
	"testing"

	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
)

func TestWriteFileWireSchemaPathFirst(t *testing.T) {
	t.Parallel()
	tool := tools.AddDescriptionParameter([]tools.Tool{{Name: "write_file", Parameters: tools.MustSchemaFor[filesystem.WriteFileArgs](), AddDescriptionParameter: true}})[0]
	parameters, strict, err := ConvertParametersToSchema(tool.Parameters)
	require.NoError(t, err)
	require.True(t, strict)
	tools.OrderedSchemaProperties(parameters)
	for _, request := range []any{
		sdk.ChatCompletionNewParams{Tools: []sdk.ChatCompletionToolUnionParam{sdk.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{Name: tool.Name, Parameters: parameters})}},
		responses.ResponseNewParams{Tools: []responses.ToolUnionParam{{OfFunction: &responses.FunctionToolParam{Name: tool.Name, Parameters: parameters}}}},
	} {
		wire, err := json.Marshal(request)
		require.NoError(t, err)
		assert.Less(t, bytes.Index(wire, []byte(`"path":`)), bytes.Index(wire, []byte(`"content":`)))
	}
}
