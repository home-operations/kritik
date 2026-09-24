// Package model is the worker's view of a language model and an embedder.
// v1 speaks to OpenRouter through Fantasy for completions and through the
// OpenAI SDK for embeddings; both sit behind these interfaces so either can
// be swapped without touching the worker.
package model

import (
	"context"

	"charm.land/fantasy/schema"
)

// CompletionRequest is one structured-output call.
type CompletionRequest struct {
	System string
	User   string
	// Model is the primary model id in the provider's namespace.
	Model string
	// Fallbacks are tried by the provider, in order, if Model fails.
	Fallbacks []string
	// Schema constrains the answer; the response carries the raw JSON.
	Schema     schema.Schema
	SchemaName string
	MaxTokens  int64
}

// CompletionResponse is the answer plus what it cost.
type CompletionResponse struct {
	// Raw is the JSON the model produced.
	Raw string
	// Model is the model that was asked for; providers that fall back do
	// not always say which one answered.
	Model string
	// Upstream is the provider that served the request, when known.
	Upstream string
	// InputTokens is the whole prompt, cached part included; CachedTokens
	// is the part the provider served from its prompt cache, which it
	// bills at a discount.
	InputTokens  int64
	CachedTokens int64
	OutputTokens int64
	// CostUSD is the provider's reported cost, zero when it reports none.
	CostUSD float64
}

// Completer produces structured answers.
type Completer interface {
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}

// Embedder turns texts into vectors and reports the tokens it spent.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) (vectors [][]float32, tokens int64, err error)
}
