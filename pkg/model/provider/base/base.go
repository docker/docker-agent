// Package base provides compatibility aliases for shared provider state.
package base

import "github.com/docker/docker-agent/pkg/model/provider/contracts"

const NoDesktopTokenErrorMessage = "failed to get Docker Desktop token for Gateway. Is Docker Desktop running and are you signed in?"

// Config is the common configuration embedded by provider clients.
type Config = contracts.Config

// RebuildProviderFunc reconstructs a provider with adjusted options.
type RebuildProviderFunc = contracts.RebuildProviderFunc

// EmbeddingResult contains an embedding and its usage.
type EmbeddingResult = contracts.EmbeddingResult

// BatchEmbeddingResult contains embeddings and their aggregate usage.
type BatchEmbeddingResult = contracts.BatchEmbeddingResult
