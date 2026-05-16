package api

const maxOutputSize = 30000

func limitOutput(output string) string {
	if len(output) > maxOutputSize {
		return output[:maxOutputSize] + "\n\n[Output truncated: exceeded 30,000 character limit]"
	}
	return output
}
