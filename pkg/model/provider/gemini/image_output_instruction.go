package gemini

import (
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/model/provider/base"
)

func applyImageOutputMediaFileInstruction(config *genai.GenerateContentConfig) {
	if config.SystemInstruction == nil {
		config.SystemInstruction = &genai.Content{}
	}
	config.SystemInstruction.Parts = append(config.SystemInstruction.Parts,
		genai.NewPartFromText(base.MediaFileInstruction))
}
