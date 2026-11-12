package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
)

type openAI struct {
	evaluatorClient
	assessment

	question json.RawMessage
}

type openAIQuestion struct {
	Type         string         `json:"type"`
	Name         string         `json:"name"`
	Instructions string         `json:"instructions"`
	Choices      []openAIChoice `json:"choices,omitempty"`
	Levels       []openAILevel  `json:"levels,omitempty"`
}

type openAIChoice struct {
	Value       string `json:"value"`
	Description string `json:"description"`
}

type openAILevel struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type openAIAnswer struct {
	Type          string   `json:"type"`
	Name          string   `json:"name"`
	Probability   *float64 `json:"probability"`
	Choice        *string  `json:"choice"`
	Score         *float64 `json:"score"`
	Confidence    *float64 `json:"confidence"`
	Probabilities []struct {
		Value       json.RawMessage `json:"value"`
		Probability *float64        `json:"probability"`
	} `json:"probabilities"`
}

func newOpenAI(connection evaluatorClient, result assessment, cfg latest.EvaluatorConfig) (*openAI, error) {
	question := openAIQuestion{Type: cfg.Type, Name: "evaluation", Instructions: cfg.Instructions}
	switch cfg.Type {
	case "boolean":
		question.Type = "predicate"
	case "choice":
		for _, key := range result.probabilityKeys {
			question.Choices = append(question.Choices, openAIChoice{Value: key, Description: cfg.Choices[key]})
		}
	case "score":
		for i, description := range cfg.Levels {
			question.Levels = append(question.Levels, openAILevel{Label: strconv.Itoa(i), Description: description})
		}
	}
	encoded, err := json.Marshal(question)
	if err != nil {
		return nil, errors.New("invalid evaluator question")
	}
	return &openAI{evaluatorClient: connection, assessment: result, question: encoded}, nil
}

func (p *openAI) Evaluate(ctx context.Context, state any) (*evaluator.Result, error) {
	return p.evaluate(ctx, state, p.payload, p.result)
}

func (p *openAI) payload(rawState json.RawMessage) ([]byte, error) {
	input := string(rawState)
	if rawState[0] == '"' {
		if err := json.Unmarshal(rawState, &input); err != nil {
			return nil, err
		}
	}
	// Consumers supply arbitrary JSON state, not native Decisions messages.
	return json.Marshal(struct {
		Model     string            `json:"model"`
		Input     string            `json:"input"`
		Questions []json.RawMessage `json:"questions"`
	}{Model: p.model, Input: input, Questions: []json.RawMessage{p.question}})
}

func (p *openAI) result(rawAnswers json.RawMessage, record evaluator.UsageRecord) (*evaluator.Result, error) {
	var answers []*openAIAnswer
	if err := json.Unmarshal(rawAnswers, &answers); err != nil {
		return nil, errors.New("invalid evaluator response JSON")
	}
	if len(answers) != 1 || answers[0] == nil || answers[0].Name != "evaluation" {
		return nil, errors.New("evaluator response is missing the evaluation answer")
	}
	answer := answers[0]
	kind := answer.Type
	if p.resultType == "boolean" {
		if kind != "predicate" {
			return nil, errors.New("evaluator answer type does not match the question")
		}
		kind = "noul"
	}
	normalized := &typesafeAnswer{
		Type: kind, Noul: answer.Probability, Choice: answer.Choice,
		Score: answer.Score, Confidence: answer.Confidence,
	}
	if kind != p.questionType {
		return nil, errors.New("evaluator answer type does not match the question")
	}
	if p.resultType != "boolean" {
		normalized.Probabilities = make(map[string]*float64, len(answer.Probabilities))
		for _, probability := range answer.Probabilities {
			var key string
			if p.resultType == "score" {
				var index *int
				if err := json.Unmarshal(probability.Value, &index); err != nil || index == nil {
					return nil, errors.New("evaluator answer has invalid level indices")
				}
				key = strconv.Itoa(*index)
			} else {
				var value *string
				if err := json.Unmarshal(probability.Value, &value); err != nil || value == nil {
					return nil, errors.New("evaluator answer has invalid choice values")
				}
				key = *value
			}
			if _, duplicate := normalized.Probabilities[key]; duplicate {
				return nil, errors.New("evaluator answer has duplicate probabilities")
			}
			normalized.Probabilities[key] = probability.Probability
		}
	}
	return p.answerResult(normalized, record)
}
