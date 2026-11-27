package gemini

import (
	"fmt"
)

// Backend selects between the Gemini Developer API and Vertex AI.
type Backend int

const (
	BackendGeminiAPI Backend = iota + 1
	BackendVertexAI
)

// Role identifies the speaker of a Content turn.
type Role = string

const (
	RoleUser  Role = "user"
	RoleModel Role = "model"
)

// Modality represents an output modality for GenerateContent.
type Modality string

const (
	ModalityText  Modality = "TEXT"
	ModalityImage Modality = "IMAGE"
)

// ServiceTier selects Gemini's service tier.
type ServiceTier string

const (
	ServiceTierFlex     ServiceTier = "flex"
	ServiceTierStandard ServiceTier = "standard"
	ServiceTierPriority ServiceTier = "priority"
)

// ThinkingLevel selects Gemini 3+ thinking effort level.
type ThinkingLevel string

const (
	ThinkingLevelMinimal ThinkingLevel = "MINIMAL"
	ThinkingLevelLow     ThinkingLevel = "LOW"
	ThinkingLevelMedium  ThinkingLevel = "MEDIUM"
	ThinkingLevelHigh    ThinkingLevel = "HIGH"
	ThinkingLevelXhigh   ThinkingLevel = "XHIGH"
)

// FunctionCallingConfigMode controls function calling behavior.
type FunctionCallingConfigMode string

const (
	FunctionCallingConfigModeAuto FunctionCallingConfigMode = "AUTO"
	FunctionCallingConfigModeAny  FunctionCallingConfigMode = "ANY"
	FunctionCallingConfigModeNone FunctionCallingConfigMode = "NONE"
)

// FinishReason explains why the model stopped generating tokens.
type FinishReason string

const (
	FinishReasonUnspecified            FinishReason = "FINISH_REASON_UNSPECIFIED"
	FinishReasonStop                   FinishReason = "STOP"
	FinishReasonMaxTokens              FinishReason = "MAX_TOKENS"
	FinishReasonSafety                 FinishReason = "SAFETY"
	FinishReasonRecitation             FinishReason = "RECITATION"
	FinishReasonBlocklist              FinishReason = "BLOCKLIST"
	FinishReasonProhibitedContent      FinishReason = "PROHIBITED_CONTENT"
	FinishReasonSPII                   FinishReason = "SPII"
	FinishReasonMalformedFunctionCall  FinishReason = "MALFORMED_FUNCTION_CALL"
	FinishReasonUnexpectedToolCall     FinishReason = "UNEXPECTED_TOOL_CALL"
	FinishReasonImageSafety            FinishReason = "IMAGE_SAFETY"
	FinishReasonImageProhibitedContent FinishReason = "IMAGE_PROHIBITED_CONTENT"
	FinishReasonImageRecitation        FinishReason = "IMAGE_RECITATION"
	FinishReasonContinuation           FinishReason = "CONTINUATION"
	FinishReasonOther                  FinishReason = "OTHER"
)

// BlockedReason explains why a prompt was blocked.
type BlockedReason string

const (
	BlockedReasonUnspecified BlockedReason = "BLOCKED_REASON_UNSPECIFIED"
	BlockedReasonSafety      BlockedReason = "SAFETY"
	BlockedReasonOther       BlockedReason = "OTHER"
)

// Content is a single conversation turn.
type Content struct {
	Parts []*Part `json:"parts,omitempty"`
	Role  string  `json:"role,omitempty"`
}

// NewContentFromParts creates a Content with the given parts and role.
func NewContentFromParts(parts []*Part, role Role) *Content {
	return &Content{Parts: parts, Role: role}
}

// NewContentFromText creates a single-text-part Content.
func NewContentFromText(text string, role Role) *Content {
	return &Content{
		Parts: []*Part{{Text: text}},
		Role:  role,
	}
}

// Part is a single typed element within a Content turn.
type Part struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	ThoughtSignature []byte            `json:"thoughtSignature,omitempty"`
	InlineData       *Blob             `json:"inlineData,omitempty"`
	FunctionCall     *FunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *FunctionResponse `json:"functionResponse,omitempty"`
}

// NewPartFromText returns a text Part.
func NewPartFromText(text string) *Part {
	return &Part{Text: text}
}

// NewPartFromBytes returns an inline-data Part.
func NewPartFromBytes(data []byte, mimeType string) *Part {
	return &Part{
		InlineData: &Blob{
			Data:     data,
			MIMEType: mimeType,
		},
	}
}

// NewPartFromFunctionCall returns a functionCall Part.
func NewPartFromFunctionCall(name string, args map[string]any) *Part {
	return &Part{
		FunctionCall: &FunctionCall{
			Name: name,
			Args: args,
		},
	}
}

// NewPartFromFunctionResponse returns a functionResponse Part.
func NewPartFromFunctionResponse(name string, response map[string]any) *Part {
	return &Part{
		FunctionResponse: &FunctionResponse{
			Name:     name,
			Response: response,
		},
	}
}

// NewPartFromFunctionResponseWithParts returns a functionResponse Part with multimodal parts.
func NewPartFromFunctionResponseWithParts(name string, response map[string]any, parts []*FunctionResponsePart) *Part {
	return &Part{
		FunctionResponse: &FunctionResponse{
			Name:     name,
			Response: response,
			Parts:    parts,
		},
	}
}

// Blob holds inline binary data.
type Blob struct {
	DisplayName string `json:"displayName,omitempty"`
	Data        []byte `json:"data,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

// FunctionCall represents a tool call emitted by the model.
type FunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name,omitempty"`
	Args map[string]any `json:"args,omitempty"`
}

// FunctionResponse represents a tool result sent back to the model.
type FunctionResponse struct {
	ID       string                  `json:"id,omitempty"`
	Name     string                  `json:"name,omitempty"`
	Response map[string]any          `json:"response,omitempty"`
	Parts    []*FunctionResponsePart `json:"parts,omitempty"`
}

// FunctionResponsePart holds multimodal data attached to a FunctionResponse.
type FunctionResponsePart struct {
	InlineData *FunctionResponseBlob `json:"inlineData,omitempty"`
}

// FunctionResponseBlob holds inline binary data inside a FunctionResponsePart.
type FunctionResponseBlob struct {
	MIMEType string `json:"mimeType,omitempty"`
	Data     []byte `json:"data,omitempty"`
}

// NewFunctionResponsePartFromBytes creates a FunctionResponsePart from raw bytes.
func NewFunctionResponsePartFromBytes(data []byte, mimeType string) *FunctionResponsePart {
	return &FunctionResponsePart{
		InlineData: &FunctionResponseBlob{
			MIMEType: mimeType,
			Data:     data,
		},
	}
}

// Tool declares function tools or built-in server-side tools.
type Tool struct {
	FunctionDeclarations []*FunctionDeclaration `json:"functionDeclarations,omitempty"`
	GoogleSearch         *GoogleSearch          `json:"googleSearch,omitempty"`
	GoogleMaps           *GoogleMaps            `json:"googleMaps,omitempty"`
	CodeExecution        *ToolCodeExecution     `json:"codeExecution,omitempty"`
	URLContext           *URLContext            `json:"urlContext,omitempty"`
}

type (
	GoogleSearch      struct{}
	GoogleMaps        struct{}
	ToolCodeExecution struct{}
	URLContext        struct{}
)

// FunctionDeclaration describes a callable tool.
type FunctionDeclaration struct {
	Name        string  `json:"name,omitempty"`
	Description string  `json:"description,omitempty"`
	Parameters  *Schema `json:"parameters,omitempty"`
}

// Schema represents a Gemini OpenAPI 3.0 subset JSON schema.
type Schema struct {
	Type             string             `json:"type,omitempty"`
	Format           string             `json:"format,omitempty"`
	Title            string             `json:"title,omitempty"`
	Description      string             `json:"description,omitempty"`
	Nullable         *bool              `json:"nullable,omitempty"`
	Enum             []string           `json:"enum,omitempty"`
	Items            *Schema            `json:"items,omitempty"`
	Properties       map[string]*Schema `json:"properties,omitempty"`
	Required         []string           `json:"required,omitempty"`
	PropertyOrdering []string           `json:"propertyOrdering,omitempty"`
	AnyOf            []*Schema          `json:"anyOf,omitempty"`
	Default          any                `json:"default,omitempty"`
	Example          any                `json:"example,omitempty"`
	Minimum          *float64           `json:"minimum,omitempty"`
	Maximum          *float64           `json:"maximum,omitempty"`
	MinLength        *int64             `json:"minLength,omitempty"`
	MaxLength        *int64             `json:"maxLength,omitempty"`
	MinItems         *int64             `json:"minItems,omitempty"`
	MaxItems         *int64             `json:"maxItems,omitempty"`
	MinProperties    *int64             `json:"minProperties,omitempty"`
	MaxProperties    *int64             `json:"maxProperties,omitempty"`
	Pattern          string             `json:"pattern,omitempty"`
}

// ToolConfig configures function calling behavior.
type ToolConfig struct {
	FunctionCallingConfig            *FunctionCallingConfig `json:"functionCallingConfig,omitempty"`
	IncludeServerSideToolInvocations *bool                  `json:"includeServerSideToolInvocations,omitempty"`
}

// FunctionCallingConfig sets the function calling mode.
type FunctionCallingConfig struct {
	Mode                 FunctionCallingConfigMode `json:"mode,omitempty"`
	AllowedFunctionNames []string                  `json:"allowedFunctionNames,omitempty"`
}

// ThinkingConfig configures model reasoning / thinking output.
type ThinkingConfig struct {
	IncludeThoughts bool          `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int32        `json:"thinkingBudget,omitempty"`
	ThinkingLevel   ThinkingLevel `json:"thinkingLevel,omitempty"`
}

// GenerateContentConfig holds request-level configuration for GenerateContent.
type GenerateContentConfig struct {
	SystemInstruction  *Content
	Temperature        *float32
	TopP               *float32
	TopK               *float32
	MaxOutputTokens    int32
	FrequencyPenalty   *float32
	PresencePenalty    *float32
	ResponseMIMEType   string
	ResponseSchema     *Schema
	ResponseJSONSchema any
	ResponseModalities []string
	ThinkingConfig     *ThinkingConfig
	Tools              []*Tool
	ToolConfig         *ToolConfig
	ServiceTier        ServiceTier
}

// generationConfigWire is the JSON wire representation of "generationConfig".
type generationConfigWire struct {
	Temperature        *float32        `json:"temperature,omitempty"`
	TopP               *float32        `json:"topP,omitempty"`
	TopK               *float32        `json:"topK,omitempty"`
	MaxOutputTokens    int32           `json:"maxOutputTokens,omitempty"`
	FrequencyPenalty   *float32        `json:"frequencyPenalty,omitempty"`
	PresencePenalty    *float32        `json:"presencePenalty,omitempty"`
	ResponseMIMEType   string          `json:"responseMimeType,omitempty"`
	ResponseSchema     *Schema         `json:"responseSchema,omitempty"`
	ResponseJSONSchema any             `json:"responseJsonSchema,omitempty"`
	ResponseModalities []string        `json:"responseModalities,omitempty"`
	ThinkingConfig     *ThinkingConfig `json:"thinkingConfig,omitempty"`
}

// generateContentRequestWire is the top-level JSON body sent to :generateContent
// and :streamGenerateContent.
type generateContentRequestWire struct {
	Contents          []*Content            `json:"contents"`
	SystemInstruction *Content              `json:"systemInstruction,omitempty"`
	Tools             []*Tool               `json:"tools,omitempty"`
	ToolConfig        *ToolConfig           `json:"toolConfig,omitempty"`
	GenerationConfig  *generationConfigWire `json:"generationConfig,omitempty"`
	ServiceTier       ServiceTier           `json:"serviceTier,omitempty"`
}

// GenerateContentResponse is a response chunk from GenerateContent / GenerateContentStream.
type GenerateContentResponse struct {
	Candidates     []*Candidate                          `json:"candidates,omitempty"`
	PromptFeedback *PromptFeedback                       `json:"promptFeedback,omitempty"`
	UsageMetadata  *GenerateContentResponseUsageMetadata `json:"usageMetadata,omitempty"`
	ResponseID     string                                `json:"responseId,omitempty"`
}

// FunctionCalls returns all function calls present in the first candidate.
func (r *GenerateContentResponse) FunctionCalls() []*FunctionCall {
	if r == nil || len(r.Candidates) == 0 || r.Candidates[0] == nil || r.Candidates[0].Content == nil {
		return nil
	}
	var calls []*FunctionCall
	for _, part := range r.Candidates[0].Content.Parts {
		if part != nil && part.FunctionCall != nil {
			calls = append(calls, part.FunctionCall)
		}
	}
	return calls
}

// Candidate is a single response candidate.
type Candidate struct {
	Content           *Content     `json:"content,omitempty"`
	FinishReason      FinishReason `json:"finishReason,omitempty"`
	FinishMessage     string       `json:"finishMessage,omitempty"`
	ContinuationToken []byte       `json:"continuationToken,omitempty"`
}

// PromptFeedback reports prompt-level blocking information.
type PromptFeedback struct {
	BlockReason BlockedReason `json:"blockReason,omitempty"`
}

// GenerateContentResponsePromptFeedback is an alias for PromptFeedback.
type GenerateContentResponsePromptFeedback = PromptFeedback

// GenerateContentResponseUsageMetadata reports token usage counts.
type GenerateContentResponseUsageMetadata struct {
	PromptTokenCount        int32 `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount    int32 `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount         int32 `json:"totalTokenCount,omitempty"`
	CachedContentTokenCount int32 `json:"cachedContentTokenCount,omitempty"`
	ThoughtsTokenCount      int32 `json:"thoughtsTokenCount,omitempty"`
	ToolUsePromptTokenCount int32 `json:"toolUsePromptTokenCount,omitempty"`
}

// EmbedContentConfig configures embedding requests.
type EmbedContentConfig struct {
	OutputDimensionality *int32 `json:"outputDimensionality,omitempty"`
}

// ContentEmbedding holds a single embedding vector and optional statistics.
type ContentEmbedding struct {
	Values     []float32                   `json:"values,omitempty"`
	Statistics *ContentEmbeddingStatistics `json:"statistics,omitempty"`
}

// ContentEmbeddingStatistics holds token statistics for an embedding.
type ContentEmbeddingStatistics struct {
	TokenCount      float32 `json:"tokenCount,omitempty"`
	TokenCountSnake float32 `json:"token_count,omitempty"`
	Truncated       bool    `json:"truncated,omitempty"`
}

func (s *ContentEmbeddingStatistics) effectiveTokenCount() float32 {
	if s.TokenCount != 0 {
		return s.TokenCount
	}
	return s.TokenCountSnake
}

// APIError represents an error returned by the Gemini / Vertex AI REST API.
type APIError struct {
	Code    int              `json:"code"`
	Message string           `json:"message"`
	Status  string           `json:"status"`
	Details []map[string]any `json:"details,omitempty"`
}

func (e APIError) Error() string {
	return fmt.Sprintf("Error %d, Message: %s, Status: %s, Details: %v", e.Code, e.Message, e.Status, e.Details)
}
