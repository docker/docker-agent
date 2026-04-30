package vertexai

// Package vertexai wraps either an anthropic.Client or openai.Client.
// Attachment support (SupportedMIMETypes, convertDocument) is provided by
// the underlying implementation and requires no additional code here.
//
// - Anthropic publisher: anthropic.Client — uses Anthropic's capability table.
// - Other publishers:    openai.Client   — uses the openai capability table
//                        (provider name "openai", or overridden by alias).
//
// The attachment.Advisor interface is implemented transitively via the
// embedded client, so callers that type-assert to attachment.Advisor will
// get the right implementation automatically.
