package agent

import (
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/cache"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/structuredoutput"
)

type Opt func(a *Agent)

func WithInstruction(instruction string) Opt {
	return func(a *Agent) {
		a.instruction = instruction
	}
}

func WithToolSets(toolSet ...tools.ToolSet) Opt {
	var startableToolSet []*tools.StartableToolSet
	for _, ts := range toolSet {
		startableToolSet = append(startableToolSet, tools.NewStartable(ts))
	}

	return func(a *Agent) {
		a.toolsets = startableToolSet
	}
}

func WithTools(allTools ...tools.Tool) Opt {
	return func(a *Agent) {
		a.tools = allTools
	}
}

func WithDescription(description string) Opt {
	return func(a *Agent) {
		a.description = description
	}
}

func WithWelcomeMessage(welcomeMessage string) Opt {
	return func(a *Agent) {
		a.welcomeMessage = welcomeMessage
	}
}

func WithName(name string) Opt {
	return func(a *Agent) {
		a.name = name
	}
}

func WithModel(model provider.Provider) Opt {
	return func(a *Agent) {
		a.models = append(a.models, model)
	}
}

// WithFallbackModel adds a fallback model to try if the primary model fails.
// For retryable errors (5xx, timeouts), the same model is retried with backoff.
// For non-retryable errors (429), we immediately move to the next model in the chain.
func WithFallbackModel(model provider.Provider) Opt {
	return func(a *Agent) {
		a.fallbackModels = append(a.fallbackModels, model)
	}
}

// WithFallbackRetries sets the number of retries per fallback model with exponential backoff.
func WithFallbackRetries(retries int) Opt {
	return func(a *Agent) {
		a.fallbackRetries = retries
	}
}

// WithFallbackCooldown sets the duration to stick with a successful fallback model
// before retrying the primary. Only applies after a non-retryable error (e.g., 429).
func WithFallbackCooldown(cooldown time.Duration) Opt {
	return func(a *Agent) {
		a.fallbackCooldown = cooldown
	}
}

// WithTitleModel sets a dedicated model for session-title generation, letting
// a heavyweight primary model delegate the cheap title call to a smaller one.
func WithTitleModel(model provider.Provider) Opt {
	return func(a *Agent) {
		a.titleModel = model
	}
}

// WithCompactionModel sets a dedicated model for session compaction (summary
// generation), letting a heavyweight primary model delegate the slow, expensive
// whole-conversation summary call to a smaller/faster one.
func WithCompactionModel(model provider.Provider) Opt {
	return func(a *Agent) {
		a.compactionModel = model
	}
}

// WithCompactionThreshold sets the fraction of the context window at which
// proactive auto-compaction triggers. Values outside (0, 1] are ignored and
// the default (0.9) applies; config validation rejects them before this
// point, so the guard only protects programmatic callers.
func WithCompactionThreshold(threshold float64) Opt {
	return func(a *Agent) {
		if threshold > 0 && threshold <= 1 {
			a.compactionThreshold = threshold
		}
	}
}

// WithSessionCompaction toggles automatic session compaction (proactive
// threshold trigger and post-overflow auto-recovery) for this agent.
// Enabled by default; the runtime only auto-compacts a session when both
// this flag and the runtime-level session-compaction option are on.
func WithSessionCompaction(enabled bool) Opt {
	return func(a *Agent) {
		a.sessionCompactionOff = !enabled
	}
}

func WithSubAgents(subAgents ...*Agent) Opt {
	return func(a *Agent) {
		a.subAgents = subAgents
		for _, subAgent := range subAgents {
			subAgent.parents = append(subAgent.parents, a)
		}
	}
}

func WithHandoffs(handoffs ...*Agent) Opt {
	return func(a *Agent) {
		a.handoffs = handoffs
	}
}

// WithForceHandoff sets the agent that unconditionally receives the
// conversation when this agent produces a final response. The runtime
// performs the switch itself, bypassing the LLM's tool-calling.
func WithForceHandoff(target *Agent) Opt {
	return func(a *Agent) {
		a.forceHandoff = target
	}
}

func WithAddDate(addDate bool) Opt {
	return func(a *Agent) {
		a.addDate = addDate
	}
}

func WithAddEnvironmentInfo(addEnvironmentInfo bool) Opt {
	return func(a *Agent) {
		a.addEnvironmentInfo = addEnvironmentInfo
	}
}

// WithRedactSecrets enables all three halves of the redact_secrets
// feature: the pre_tool_use builtin (via ApplyAgentDefaults), the
// runtime's before_llm_call message transform, and the dispatcher's
// tool-output scrub.
func WithRedactSecrets(redactSecrets bool) Opt {
	return func(a *Agent) {
		a.redactSecrets = redactSecrets
	}
}

// WithSafety sets the author-declared safety-mode default applied to new
// sessions started on this agent when the user has not chosen a mode.
func WithSafety(mode latest.SafetyMode) Opt {
	return func(a *Agent) {
		a.safety = mode
	}
}

func WithAddDescriptionParameter(addDescriptionParameter bool) Opt {
	return func(a *Agent) {
		a.addDescriptionParameter = addDescriptionParameter
	}
}

func WithAddPromptFiles(addPromptFiles []string) Opt {
	return func(a *Agent) {
		a.addPromptFiles = addPromptFiles
	}
}

func WithMaxIterations(maxIterations int) Opt {
	return func(a *Agent) {
		a.maxIterations = maxIterations
	}
}

// WithMaxConsecutiveToolCalls sets the threshold for consecutive identical tool
// call detection. 0 means "use runtime default of 5". Negative values are
// ignored.
func WithMaxConsecutiveToolCalls(n int) Opt {
	return func(a *Agent) {
		if n >= 0 {
			a.maxConsecutiveToolCalls = n
		}
	}
}

// WithMaxOldToolCallTokens sets the maximum token budget for old tool call content.
// Positive values enable truncation; 0 and -1 disable truncation (unlimited tool content).
func WithMaxOldToolCallTokens(n int) Opt {
	return func(a *Agent) {
		a.maxOldToolCallTokens = n
	}
}

// WithMaxToolResultTokens sets the per-tool-result token cap applied when a
// tool result enters a session. Positive values enable middle-out truncation;
// 0 and -1 disable the cap (unbounded tool results).
func WithMaxToolResultTokens(n int) Opt {
	return func(a *Agent) {
		a.maxToolResultTokens = n
	}
}

func WithNumHistoryItems(numHistoryItems int) Opt {
	return func(a *Agent) {
		a.numHistoryItems = numHistoryItems
	}
}

func WithCommands(commands types.Commands) Opt {
	return func(a *Agent) {
		a.commands = commands
	}
}

func WithHarness(harness *latest.HarnessConfig) Opt {
	return func(a *Agent) {
		if harness == nil {
			a.harness = nil
			return
		}
		cfg := *harness
		a.harness = &cfg
	}
}

func WithLoadTimeWarnings(warnings []string) Opt {
	return func(a *Agent) {
		for _, w := range warnings {
			a.AddToolWarning(w)
		}
	}
}

func WithHooks(hooks *latest.HooksConfig) Opt {
	return func(a *Agent) {
		a.hooks = hooks
	}
}

// WithStructuredOutput keeps the agent's original structured-output
// configuration so the runtime can enforce tool-mode structured output.
// In tool mode it also (re)arms the lazy compile-once cache behind
// [Agent.StructuredOutputTool], so a reconfigured schema is recompiled.
func WithStructuredOutput(structuredOutput *latest.StructuredOutput) Opt {
	return func(a *Agent) {
		a.structuredOutput = structuredOutput
		a.structuredOutputTool = nil
		if structuredOutput.ToolMode() {
			a.structuredOutputTool = sync.OnceValues(func() (*structuredoutput.OutputTool, error) {
				return structuredoutput.New(structuredOutput)
			})
		}
	}
}

// WithCache attaches a response cache to the agent. Pass nil to disable.
func WithCache(c *cache.Cache) Opt {
	return func(a *Agent) {
		a.cache = c
	}
}
