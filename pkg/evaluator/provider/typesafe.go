package provider

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/docker/docker-agent/pkg/evaluator"
)

const (
	maxResponseBytes = 1 << 20
	// The API may round each probability and the weighted score independently.
	roundingTolerance = 1e-3
)

type typesafe struct {
	evaluatorClient
	assessment

	question json.RawMessage
}

type assessment struct {
	resultType      string
	questionType    string
	probabilityKeys []string
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

func (p *typesafe) Evaluate(ctx context.Context, state any) (*evaluator.Result, error) {
	return p.evaluate(ctx, state, p.payload, p.result)
}

func (p *typesafe) payload(rawState json.RawMessage) ([]byte, error) {
	return json.Marshal(typesafeRequest{
		Model:     p.model,
		State:     rawState,
		Questions: map[string]json.RawMessage{"evaluation": p.question},
	})
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
	return p.answerResult(answer, record)
}

func (p *assessment) answerResult(answer *typesafeAnswer, record evaluator.UsageRecord) (*evaluator.Result, error) {
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

func (p *assessment) probabilities(values map[string]*float64) (map[string]float64, error) {
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
