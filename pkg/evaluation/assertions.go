package evaluation

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/docker/docker-agent/pkg/session"
)

// AssertionResult records the outcome of a single assertion evaluation.
type AssertionResult struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Passed bool   `json:"passed"`
	Reason string `json:"reason,omitempty"`
}

// runAssertions evaluates all assertions against the agent response and
// auxiliary data (cost, tool calls).
func runAssertions(assertions []session.Assertion, response string, cost float64, toolCalls []string) []AssertionResult {
	results := make([]AssertionResult, len(assertions))
	for i, a := range assertions {
		results[i] = evalAssertion(a, response, cost, toolCalls)
	}
	return results
}

func evalAssertion(a session.Assertion, response string, cost float64, toolCalls []string) AssertionResult {
	r := AssertionResult{Name: a.Name, Type: a.Type}
	switch strings.ToLower(a.Type) {
	case "contains":
		r.Passed = strings.Contains(response, a.Value)
		if !r.Passed {
			r.Reason = fmt.Sprintf("response does not contain %q", a.Value)
		}
	case "not_contains":
		r.Passed = !strings.Contains(response, a.Value)
		if !r.Passed {
			r.Reason = fmt.Sprintf("response contains %q", a.Value)
		}
	case "equals":
		r.Passed = strings.TrimSpace(response) == strings.TrimSpace(a.Value)
		if !r.Passed {
			r.Reason = "response does not equal expected value"
		}
	case "starts_with":
		r.Passed = strings.HasPrefix(response, a.Value)
		if !r.Passed {
			r.Reason = fmt.Sprintf("response does not start with %q", a.Value)
		}
	case "ends_with":
		r.Passed = strings.HasSuffix(strings.TrimSpace(response), a.Value)
		if !r.Passed {
			r.Reason = fmt.Sprintf("response does not end with %q", a.Value)
		}
	case "regex":
		re, err := regexp.Compile(a.Value)
		if err != nil {
			r.Reason = fmt.Sprintf("invalid regex %q: %v", a.Value, err)
		} else {
			r.Passed = re.MatchString(response)
			if !r.Passed {
				r.Reason = fmt.Sprintf("response does not match pattern %q", a.Value)
			}
		}
	case "cost_threshold":
		threshold, err := strconv.ParseFloat(a.Value, 64)
		if err != nil {
			r.Reason = fmt.Sprintf("invalid cost threshold %q: %v", a.Value, err)
		} else {
			r.Passed = cost <= threshold
			if !r.Passed {
				r.Reason = fmt.Sprintf("cost $%.6f exceeds threshold $%.6f", cost, threshold)
			}
		}
	case "tool_called":
		r.Passed = slices.Contains(toolCalls, a.Value)
		if !r.Passed {
			r.Reason = fmt.Sprintf("tool %q was not called", a.Value)
		}
	default:
		r.Reason = fmt.Sprintf("unknown assertion type %q", a.Type)
	}
	return r
}
