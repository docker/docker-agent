package openapi

import (
	"fmt"
	"unicode/utf8"
)

const maxOutputSize = 30000

func limitOutput(output string, limit int) string {
	if limit <= 0 || len(output) <= limit {
		return output
	}
	end := limit
	for end > 0 && !utf8.RuneStart(output[end]) {
		end--
	}
	if limit == maxOutputSize {
		return output[:end] + "\n\n[Output truncated: exceeded 30,000 character limit]"
	}
	return output[:end] + fmt.Sprintf("\n\n[Output truncated: exceeded %d byte limit]", limit)
}
