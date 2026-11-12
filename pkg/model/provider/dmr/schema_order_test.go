package dmr

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	filesystem "github.com/docker/docker-agent/pkg/tools/builtin/filesystem/types"
)

func TestWriteFileWireSchemaPathFirst(t *testing.T) {
	t.Parallel()
	schema, err := ConvertParametersToSchema(tools.MustSchemaFor[filesystem.WriteFileArgs]())
	require.NoError(t, err)
	wire, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.Less(t, bytes.Index(wire, []byte(`"path":`)), bytes.Index(wire, []byte(`"content":`)))
}
