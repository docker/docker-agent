package genai

// Attribute keys defined by the OTel GenAI semantic conventions. All are
// Development stability — declared as constants here so call sites depend
// on a stable local symbol rather than a moving upstream import path.
const (
	AttrOperationName  = "gen_ai.operation.name"
	AttrProviderName   = "gen_ai.provider.name"
	AttrConversationID = "gen_ai.conversation.id"
	AttrOutputType     = "gen_ai.output.type"

	AttrAgentName        = "gen_ai.agent.name"
	AttrAgentID          = "gen_ai.agent.id"
	AttrAgentDescription = "gen_ai.agent.description"
	AttrAgentVersion     = "gen_ai.agent.version"

	AttrWorkflowName = "gen_ai.workflow.name"

	AttrRequestModel            = "gen_ai.request.model"
	AttrRequestStream           = "gen_ai.request.stream"
	AttrRequestMaxTokens        = "gen_ai.request.max_tokens"
	AttrRequestTemperature      = "gen_ai.request.temperature"
	AttrRequestTopP             = "gen_ai.request.top_p"
	AttrRequestTopK             = "gen_ai.request.top_k"
	AttrRequestFrequencyPenalty = "gen_ai.request.frequency_penalty"
	AttrRequestPresencePenalty  = "gen_ai.request.presence_penalty"
	AttrRequestStopSequences    = "gen_ai.request.stop_sequences"
	AttrRequestChoiceCount      = "gen_ai.request.choice.count"
	AttrRequestSeed             = "gen_ai.request.seed"
	AttrRequestEncodingFormats  = "gen_ai.request.encoding_formats"

	AttrResponseModel            = "gen_ai.response.model"
	AttrResponseID               = "gen_ai.response.id"
	AttrResponseFinishReasons    = "gen_ai.response.finish_reasons"
	AttrResponseTimeToFirstChunk = "gen_ai.response.time_to_first_chunk"

	AttrUsageInputTokens              = "gen_ai.usage.input_tokens"
	AttrUsageOutputTokens             = "gen_ai.usage.output_tokens"
	AttrUsageCacheReadInputTokens     = "gen_ai.usage.cache_read.input_tokens"
	AttrUsageCacheCreationInputTokens = "gen_ai.usage.cache_creation.input_tokens"
	AttrUsageReasoningOutputTokens    = "gen_ai.usage.reasoning.output_tokens"

	AttrTokenType = "gen_ai.token.type"

	AttrToolName          = "gen_ai.tool.name"
	AttrToolCallID        = "gen_ai.tool.call.id"
	AttrToolType          = "gen_ai.tool.type"
	AttrToolDescription   = "gen_ai.tool.description"
	AttrToolDefinitions   = "gen_ai.tool.definitions"
	AttrToolCallArguments = "gen_ai.tool.call.arguments"
	AttrToolCallResult    = "gen_ai.tool.call.result"

	AttrInputMessages      = "gen_ai.input.messages"
	AttrOutputMessages     = "gen_ai.output.messages"
	AttrSystemInstructions = "gen_ai.system_instructions"

	AttrPromptName = "gen_ai.prompt.name"

	AttrDataSourceID             = "gen_ai.data_source.id"
	AttrEmbeddingsDimensionCount = "gen_ai.embeddings.dimension.count"
	AttrRetrievalDocuments       = "gen_ai.retrieval.documents"
	AttrRetrievalQueryText       = "gen_ai.retrieval.query.text"

	AttrEvaluationName        = "gen_ai.evaluation.name"
	AttrEvaluationScoreLabel  = "gen_ai.evaluation.score.label"
	AttrEvaluationScoreValue  = "gen_ai.evaluation.score.value"
	AttrEvaluationExplanation = "gen_ai.evaluation.explanation"
)

// Operation names — values for AttrOperationName.
const (
	OperationChat            = "chat"
	OperationTextCompletion  = "text_completion"
	OperationGenerateContent = "generate_content"
	OperationEmbeddings      = "embeddings"
	OperationCreateAgent     = "create_agent"
	OperationInvokeAgent     = "invoke_agent"
	OperationInvokeWorkflow  = "invoke_workflow"
	OperationExecuteTool     = "execute_tool"
	OperationRetrieval       = "retrieval"
)

// Token types — values for AttrTokenType when recording the token usage
// histogram. Spec defines `input` and `output`; we use the cache_read /
// cache_creation / reasoning variants to mirror the per-token-type
// usage attributes for richer breakdowns.
const (
	TokenTypeInput         = "input"
	TokenTypeOutput        = "output"
	TokenTypeCacheRead     = "cache_read.input"
	TokenTypeCacheCreation = "cache_creation.input"
	TokenTypeReasoning     = "reasoning.output"
)

// Provider names — values for AttrProviderName. Names follow the values
// defined in the provider-specific GenAI semconv pages.
const (
	ProviderAnthropic   = "anthropic"
	ProviderOpenAI      = "openai"
	ProviderAWSBedrock  = "aws.bedrock"
	ProviderGCPVertexAI = "gcp.vertex_ai"
	ProviderGCPGenAI    = "gcp.gen_ai"
	ProviderAzureAI     = "azure.ai.inference"
	ProviderDMR         = "docker.dmr"
)
