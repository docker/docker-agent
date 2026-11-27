package chat

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// parseDataURI parses a data URI of the form "data:<mime>;base64,<payload>".
// Returns the MIME type and decoded bytes.
func parseDataURI(uri string) (mimeType string, data []byte, err error) {
	rest, ok := strings.CutPrefix(uri, "data:")
	if !ok {
		return "", nil, errors.New("not a data URI")
	}

	header, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return "", nil, errors.New("data URI missing comma separator")
	}

	// Header is "<mime>[;charset=…];base64" or "<mime>" (plain text, unsupported here).
	mimeType, ok = strings.CutSuffix(header, ";base64")
	if !ok {
		return "", nil, errors.New("data URI is not base64-encoded (only base64 data URIs are supported)")
	}

	// Strip any charset parameter (e.g. "image/png;charset=utf-8;base64" → "image/png").
	if idx := strings.Index(mimeType, ";"); idx >= 0 {
		mimeType = mimeType[:idx]
	}
	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	data, err = base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, fmt.Errorf("base64 decode: %w", err)
	}
	return mimeType, data, nil
}
