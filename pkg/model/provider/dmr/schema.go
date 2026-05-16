package dmr

import (
	"github.com/docker/docker-agent/pkg/tools"
)

// ConvertParametersToSchema converts parameters to DMR Schema format
func ConvertParametersToSchema(params any) (any, error) {
	m, err := tools.SchemaToMap(params)
	if err != nil {
		return nil, err
	}

	// DMR models tend to dislike `additionalProperties` in the schema
	// e.g. ai/qwen3 and ai/gpt-oss
	delete(m, "additionalProperties")

	return m, nil
}
