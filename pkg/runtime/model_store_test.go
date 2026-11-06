package runtime

import (
	"context"
	"errors"

	"github.com/docker/docker-agent/pkg/modelsdev"
)

type stubModelStore struct {
	db     *modelsdev.Database
	models map[string]*modelsdev.Model
}

func (s stubModelStore) GetModel(_ context.Context, id modelsdev.ID) (*modelsdev.Model, error) {
	if m, ok := s.models[id.String()]; ok {
		return m, nil
	}
	return nil, errors.New("model not found")
}

func (s stubModelStore) GetDatabase(context.Context) (*modelsdev.Database, error) {
	if s.db == nil {
		return nil, errors.New("no database")
	}
	return s.db, nil
}
