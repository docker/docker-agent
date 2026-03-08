package database

import (
	"context"
	"errors"
)

var (
	ErrEmptyID        = errors.New("memory ID cannot be empty")
	ErrMemoryNotFound = errors.New("memory not found")
)

type UserMemory struct {
	ID        string `json:"id" description:"The ID of the memory"`
	CreatedAt string `json:"created_at" description:"The creation timestamp of the memory"`
	Memory    string `json:"memory" description:"The content of the memory"`
	Category  string `json:"category,omitempty" description:"The category of the memory"`
}

type Database interface {
	AddMemory(ctx context.Context, memory UserMemory) error
	GetMemories(ctx context.Context) ([]UserMemory, error)
	DeleteMemory(ctx context.Context, memory UserMemory) error
	SearchMemories(ctx context.Context, query, category string) ([]UserMemory, error)
	UpdateMemory(ctx context.Context, memory UserMemory) error
}
