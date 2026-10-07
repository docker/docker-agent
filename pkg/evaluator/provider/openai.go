package provider

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/evaluator"
)

const (
	// questionName is fixed and short: gateways may size their response estimate from echoed names.
	questionName = "evaluation"
	// scoreTolerance absorbs independent rounding of the level probabilities and the score.
	scoreTolerance = 5e-2
)

// openai evaluates with the OpenAI Decisions API (POST /v1/decisions).
// It speaks HTTP directly to keep the OpenAI SDK out of the loader's dependencies.
type openai struct {
	*connection

	model           string
	resultType      string
	questionType    string
	question        json.RawMessage
	probabilityKeys []string
	timeout         time.Duration
	cost            *latest.CostConfig
}

type openaiQuestion struct {
	Type         string         `json:"type"`
	Name         string         `json:"name"`
	Instructions string         `json:"instructions"`
	Choices      []openaiChoice `json:"choices,omitempty"`
	Levels       []openaiLevel  `json:"levels,omitempty"`
}

type openaiChoice struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

type openaiLevel struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type openaiRequest struct {
	Model     string            `json:"model"`
	Input     string            `json:"input"`
	Questions []json.RawMessage `json:"questions"`
}

type openaiResponse struct {
	Model   json.RawMessage   `json:"model"`
	Answers []json.RawMessage `json:"answers"`
	Usage   json.RawMessage   `json:"usage"`
}

type openaiAnswer struct {
	Type          string              `json:"type"`
	Name          string              `json:"name"`
	Probability   *float64            `json:"probability"`
	Choice        *string             `json:"choice"`
	Score         *float64            `json:"score"`
	Probabilities []openaiProbability `json:"probabilities"`
	Confidence    *float64            `json:"confidence"`
}

type openaiProbability struct {
	Value       json.RawMessage `json:"value"`
	Probability *float64        `json:"probability"`
}

func newOpenAI(ctx context.Context, cfg latest.EvaluatorConfig, env environment.Provider, opts []Option) (evaluator.Evaluator, error) {
	conn, err := newConnection(ctx, cfg, env, openaiBackend, opts)
	if err != nil {
		return nil, err
	}

	question := openaiQuestion{Type: "predicate", Name: questionName, Instructions: cfg.Instructions}
	var probabilityKeys []string
	switch cfg.Type {
	case "choice":
		question.Type = "choice"
		probabilityKeys = slices.Sorted(maps.Keys(cfg.Choices))
		for _, key := range probabilityKeys {
			question.Choices = append(question.Choices, openaiChoice{Value: key, Description: cfg.Choices[key]})
		}
	case "score":
		question.Type = "score"
		for i, level := range cfg.Levels {
			key := strconv.Itoa(i)
			probabilityKeys = append(probabilityKeys, key)
			question.Levels = append(question.Levels, openaiLevel{Label: key, Description: level})
		}
	}
	questionJSON, err := json.Marshal(question)
	if err != nil {
		return nil, errors.New("invalid evaluator question")
	}

	return &openai{
		connection:      conn,
		model:           cfg.Model,
		resultType:      cfg.Type,
		questionType:    question.Type,
		question:        questionJSON,
		probabilityKeys: probabilityKeys,
		timeout:         timeoutOrDefault(cfg),
		cost:            copyCost(cfg.Cost),
	}, nil
}

func (p *openai) Evaluate(ctx context.Context, state any) (*evaluator.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	input, err := decisionInput(state)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(openaiRequest{Model: p.model, Input: input, Questions: []json.RawMessage{p.question}})
	if err != nil {
		return nil, errors.New("failed to encode evaluator request")
	}

	x, err := p.send(ctx, payload, p.model)
	record := evaluator.UsageRecord{Model: p.model}
	if x.attempted {
		defer func() { evaluator.ObserveUsage(ctx, record) }()
	}
	if err != nil {
		return nil, err
	}

	var response openaiResponse
	decoded := x.usable() && json.Unmarshal(x.body, &response) == nil
	var model string
	var modelErr error
	if decoded && len(response.Model) != 0 && string(response.Model) != "null" {
		if modelErr = json.Unmarshal(response.Model, &model); modelErr == nil && strings.TrimSpace(model) != "" {
			record.Model = model
		}
	}
	var usageErr error
	if decoded {
		record.Usage, usageErr = reportedUsage(response.Usage)
		record.Cost = estimateCost(p.cost, record.Usage)
	}
	if failure := p.failure(ctx, x); failure != nil {
		return nil, failure
	}
	if !decoded || modelErr != nil {
		return nil, errors.New("invalid evaluator response JSON")
	}
	if usageErr != nil {
		// Accounting cannot be trusted, so this must end the run instead of falling back.
		return nil, &evaluator.TerminalError{Err: usageErr}
	}
	return p.result(response.Answers, record)
}

// decisionInput maps a state to the text input: strings verbatim, objects and arrays as JSON text.
func decisionInput(state any) (string, error) {
	raw, err := encodeState(state)
	if err != nil {
		return "", err
	}
	if raw[0] != '"' {
		return string(raw), nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", errors.New("evaluator state must be JSON-serializable")
	}
	return text, nil
}

func (p *openai) result(rawAnswers []json.RawMessage, record evaluator.UsageRecord) (*evaluator.Result, error) {
	answer, err := selectAnswer(rawAnswers)
	if err != nil {
		return nil, err
	}
	if answer.Type == "refusal" {
		return nil, errors.New("evaluator refused to assess the state")
	}
	if answer.Type != p.questionType {
		return nil, errors.New("evaluator answer type does not match the question")
	}
	if answer.Confidence != nil && !validProbability(answer.Confidence) {
		return nil, errors.New("evaluator answer has invalid confidence")
	}
	result := &evaluator.Result{
		Type:       p.resultType,
		Model:      record.Model,
		Confidence: answer.Confidence,
		Cost:       record.Cost,
	}
	if record.Usage != nil {
		result.Usage = *record.Usage
	}

	if p.resultType == "boolean" {
		if !validProbability(answer.Probability) {
			return nil, errors.New("evaluator answer has missing or invalid probability")
		}
		result.Probability = answer.Probability
		return result, nil
	}

	probabilities, err := p.probabilities(answer.Probabilities)
	if err != nil {
		return nil, err
	}
	result.Probabilities = probabilities
	if p.resultType == "choice" {
		if answer.Choice == nil {
			return nil, errors.New("evaluator answer is missing the choice")
		}
		selected, ok := probabilities[*answer.Choice]
		if !ok {
			return nil, errors.New("evaluator answer contains an unknown choice")
		}
		for _, probability := range probabilities {
			if probability > selected {
				return nil, errors.New("evaluator choice is not a highest-probability option")
			}
		}
		result.Choice = *answer.Choice
		return result, nil
	}

	last := float64(len(p.probabilityKeys) - 1)
	if answer.Score == nil || math.IsNaN(*answer.Score) || math.IsInf(*answer.Score, 0) || *answer.Score < -roundingTolerance || *answer.Score > last+roundingTolerance {
		return nil, errors.New("evaluator answer has missing or invalid score")
	}
	var expected float64
	for i, key := range p.probabilityKeys {
		expected += float64(i) * probabilities[key]
	}
	if math.Abs(*answer.Score-expected) > scoreTolerance {
		return nil, errors.New("evaluator score does not match its probabilities")
	}
	result.Score = answer.Score
	return result, nil
}

// selectAnswer finds the one answer to the question. A refusal may omit its name.
func selectAnswer(raw []json.RawMessage) (*openaiAnswer, error) {
	var matches []*openaiAnswer
	for _, item := range raw {
		var head struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(item, &head); err != nil {
			return nil, errors.New("invalid evaluator response JSON")
		}
		if head.Name != questionName && (head.Type != "refusal" || head.Name != "") {
			continue
		}
		var answer openaiAnswer
		if err := json.Unmarshal(item, &answer); err != nil {
			return nil, errors.New("invalid evaluator response JSON")
		}
		matches = append(matches, &answer)
	}
	switch len(matches) {
	case 0:
		return nil, errors.New("evaluator response is missing the evaluation answer")
	case 1:
		return matches[0], nil
	}
	return nil, errors.New("evaluator response has duplicate evaluation answers")
}

// probabilities converts the option array to the normalized map, validating it against the criteria.
func (p *openai) probabilities(options []openaiProbability) (map[string]float64, error) {
	if len(options) != len(p.probabilityKeys) {
		return nil, errors.New("evaluator answer probability keys do not match the criteria")
	}
	probabilities := make(map[string]float64, len(options))
	var sum float64
	for _, option := range options {
		key, err := p.probabilityKey(option.Value)
		if err != nil {
			return nil, err
		}
		if _, duplicate := probabilities[key]; duplicate {
			return nil, errors.New("evaluator answer has duplicate probability keys")
		}
		if !validProbability(option.Probability) {
			return nil, errors.New("evaluator answer has missing or invalid probabilities")
		}
		probabilities[key] = *option.Probability
		sum += *option.Probability
	}
	for _, key := range p.probabilityKeys {
		if _, ok := probabilities[key]; !ok {
			return nil, errors.New("evaluator answer probability keys do not match the criteria")
		}
	}
	if math.Abs(sum-1) > roundingTolerance {
		return nil, errors.New("evaluator answer probabilities do not sum to one")
	}
	return probabilities, nil
}

// probabilityKey turns an option value into its configured key: a string for choices, an integer level index for scores.
func (p *openai) probabilityKey(value json.RawMessage) (string, error) {
	errInvalid := errors.New("evaluator answer probability keys do not match the criteria")
	if p.resultType == "choice" {
		var key string
		if json.Unmarshal(value, &key) != nil {
			return "", errInvalid
		}
		return key, nil
	}
	var index float64
	if json.Unmarshal(value, &index) != nil || index != math.Trunc(index) || index < 0 || index >= float64(len(p.probabilityKeys)) {
		return "", errInvalid
	}
	return strconv.Itoa(int(index)), nil
}
