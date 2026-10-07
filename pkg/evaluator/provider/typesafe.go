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
	defaultBaseURL   = "https://api.typesafe.ai"
	maxResponseBytes = 1 << 20
	// The API may round each probability and the weighted score independently.
	roundingTolerance = 1e-3
)

type typesafe struct {
	*connection

	model           string
	resultType      string
	questionType    string
	question        json.RawMessage
	probabilityKeys []string
	timeout         time.Duration
	cost            *latest.CostConfig
	officialPricing bool
}

type typesafeQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type typesafeRequest struct {
	Model     string                     `json:"model"`
	State     json.RawMessage            `json:"state"`
	Questions map[string]json.RawMessage `json:"questions"`
}

type typesafeResponse struct {
	Model   json.RawMessage `json:"model"`
	Answers json.RawMessage `json:"answers"`
	Usage   json.RawMessage `json:"usage"`
}

type typesafeAnswer struct {
	Type          string              `json:"type"`
	Noul          *float64            `json:"noul"`
	Choice        *string             `json:"choice"`
	Score         *float64            `json:"score"`
	Probabilities map[string]*float64 `json:"probabilities"`
	Confidence    *float64            `json:"confidence"`
}

func newTypeSafe(ctx context.Context, cfg latest.EvaluatorConfig, env environment.Provider, opts []Option) (evaluator.Evaluator, error) {
	conn, err := newConnection(ctx, cfg, env, typesafeBackend, opts)
	if err != nil {
		return nil, err
	}

	question := typesafeQuestion{Type: cfg.Type, Instructions: cfg.Instructions}
	var probabilityKeys []string
	switch cfg.Type {
	case "boolean":
		question.Type = "noul"
	case "choice":
		question.Criteria = cfg.Choices
		probabilityKeys = slices.Sorted(maps.Keys(cfg.Choices))
	case "score":
		question.Criteria = cfg.Levels
		for i := range cfg.Levels {
			probabilityKeys = append(probabilityKeys, strconv.Itoa(i))
		}
	}
	questionJSON, err := json.Marshal(question)
	if err != nil {
		return nil, errors.New("invalid evaluator question")
	}

	return &typesafe{
		connection:      conn,
		model:           cfg.Model,
		resultType:      cfg.Type,
		questionType:    question.Type,
		question:        questionJSON,
		probabilityKeys: probabilityKeys,
		timeout:         timeoutOrDefault(cfg),
		cost:            copyCost(cfg.Cost),
		officialPricing: conn.gateway == "" && conn.endpoint == typesafeBackend.defaultBaseURL+typesafeBackend.path,
	}, nil
}

func (p *typesafe) Evaluate(ctx context.Context, state any) (*evaluator.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rawState, err := encodeState(state)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(typesafeRequest{
		Model:     p.model,
		State:     rawState,
		Questions: map[string]json.RawMessage{"evaluation": p.question},
	})
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

	var response typesafeResponse
	var decodeErr, modelErr error
	var model string
	if x.usable() {
		decodeErr = json.Unmarshal(x.body, &response)
		modelErr = json.Unmarshal(response.Model, &model)
	}
	if strings.TrimSpace(model) != "" {
		record.Model = model
	}
	var usageErr error
	if x.usable() && decodeErr == nil {
		record.Usage, usageErr = reportedUsage(response.Usage)
		record.Cost = p.estimateCost(model, record.Usage)
	}
	if failure := p.failure(ctx, x); failure != nil {
		return nil, failure
	}
	if decodeErr != nil || (len(response.Model) != 0 && modelErr != nil) {
		return nil, errors.New("invalid evaluator response JSON")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("evaluator response is missing the model")
	}
	if usageErr != nil {
		// Accounting cannot be trusted, so this must end the run instead of falling back.
		return nil, &evaluator.TerminalError{Err: usageErr}
	}
	return p.result(response.Answers, record)
}

func (p *typesafe) result(rawAnswers json.RawMessage, record evaluator.UsageRecord) (*evaluator.Result, error) {
	var answers map[string]*typesafeAnswer
	if len(rawAnswers) != 0 {
		if err := json.Unmarshal(rawAnswers, &answers); err != nil {
			return nil, errors.New("invalid evaluator response JSON")
		}
	}
	answer := answers["evaluation"]
	if answer == nil {
		return nil, errors.New("evaluator response is missing the evaluation answer")
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
		if !validProbability(answer.Noul) {
			return nil, errors.New("evaluator answer has missing or invalid probability")
		}
		result.Probability = answer.Noul
		return result, nil
	}

	probabilities, err := p.probabilities(answer.Probabilities)
	if err != nil {
		return nil, err
	}
	result.Probabilities = probabilities
	switch p.resultType {
	case "choice":
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
	case "score":
		if answer.Score == nil || math.IsNaN(*answer.Score) || math.IsInf(*answer.Score, 0) || *answer.Score < -roundingTolerance || *answer.Score > float64(len(p.probabilityKeys)-1)+roundingTolerance {
			return nil, errors.New("evaluator answer has missing or invalid score")
		}
		result.Score = answer.Score
	}
	return result, nil
}

func (p *typesafe) probabilities(values map[string]*float64) (map[string]float64, error) {
	if len(values) != len(p.probabilityKeys) {
		return nil, errors.New("evaluator answer probability keys do not match the criteria")
	}
	probabilities := make(map[string]float64, len(values))
	var sum float64
	for _, key := range p.probabilityKeys {
		probability := values[key]
		if !validProbability(probability) {
			return nil, errors.New("evaluator answer has missing or invalid probabilities")
		}
		probabilities[key] = *probability
		sum += *probability
	}
	if math.Abs(sum-1) > roundingTolerance {
		return nil, errors.New("evaluator answer probabilities do not sum to one")
	}
	return probabilities, nil
}

func validProbability(value *float64) bool {
	return value != nil && !math.IsNaN(*value) && *value >= 0 && *value <= 1
}
