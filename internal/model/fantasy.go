package model

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
)

// Attribution headers OpenRouter shows in its dashboard; harmless elsewhere.
var attribution = map[string]string{
	"HTTP-Referer": "https://github.com/home-operations/kritik",
	"X-Title":      "kritik",
}

// FantasyCompleter is a Completer over one Fantasy provider. Models are
// resolved lazily and cached; the provider is either OpenRouter, which
// understands server-side fallback and reports cost, or any
// OpenAI-compatible endpoint, which gets neither.
type FantasyCompleter struct {
	provider   fantasy.Provider
	openrouter bool

	mu     sync.Mutex
	models map[string]fantasy.LanguageModel
}

// NewOpenRouter builds a completer for OpenRouter. client may be nil.
func NewOpenRouter(apiKey string, client *http.Client) (*FantasyCompleter, error) {
	opts := []openrouter.Option{openrouter.WithAPIKey(apiKey), openrouter.WithHeaders(attribution)}
	if client != nil {
		opts = append(opts, openrouter.WithHTTPClient(client))
	}
	p, err := openrouter.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("model: openrouter: %w", err)
	}
	return &FantasyCompleter{provider: p, openrouter: true, models: map[string]fantasy.LanguageModel{}}, nil
}

// NewOpenAICompatible builds a completer for any OpenAI-compatible endpoint
// (LiteLLM, vLLM, Ollama, or a test server).
func NewOpenAICompatible(name, baseURL, apiKey string) (*FantasyCompleter, error) {
	p, err := openaicompat.New(
		openaicompat.WithName(name), openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey), openaicompat.WithHeaders(attribution),
	)
	if err != nil {
		return nil, fmt.Errorf("model: %s: %w", name, err)
	}
	return &FantasyCompleter{provider: p, models: map[string]fantasy.LanguageModel{}}, nil
}

func (c *FantasyCompleter) model(ctx context.Context, id string) (fantasy.LanguageModel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.models[id]; ok {
		return m, nil
	}
	m, err := c.provider.LanguageModel(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("model: %s/%s: %w", c.provider.Name(), id, err)
	}
	c.models[id] = m
	return m, nil
}

// Complete implements Completer with a schema-constrained generation.
func (c *FantasyCompleter) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	m, err := c.model(ctx, req.Model)
	if err != nil {
		return CompletionResponse{}, err
	}
	call := fantasy.ObjectCall{
		Prompt:     fantasy.Prompt{fantasy.NewSystemMessage(req.System), fantasy.NewUserMessage(req.User)},
		Schema:     req.Schema,
		SchemaName: req.SchemaName,
	}
	if req.MaxTokens > 0 {
		call.MaxOutputTokens = &req.MaxTokens
	}
	if c.openrouter && len(req.Fallbacks) > 0 {
		// OpenRouter's server-side fallback: the request names the primary
		// first, then the alternatives, and the provider walks the list.
		models := append([]string{req.Model}, req.Fallbacks...)
		call.ProviderOptions = fantasy.ProviderOptions{
			openrouter.Name: &openrouter.ProviderOptions{ExtraBody: map[string]any{"models": models}},
		}
	}
	resp, err := m.GenerateObject(ctx, call)
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("model: %s/%s: %w", c.provider.Name(), req.Model, err)
	}
	// Fantasy reports input net of cached tokens; the prompt was still
	// sent whole, so caps and usage count both.
	out := CompletionResponse{
		Raw: strings.TrimSpace(resp.RawText), Model: req.Model,
		InputTokens:  resp.Usage.InputTokens + resp.Usage.CacheReadTokens,
		CachedTokens: resp.Usage.CacheReadTokens,
		OutputTokens: resp.Usage.OutputTokens,
	}
	// The OpenRouter provider is built on the OpenAI one, which files
	// metadata under its own name; look under both.
	for _, key := range []string{openrouter.Name, openai.Name} {
		if meta, ok := resp.ProviderMetadata[key].(*openrouter.ProviderMetadata); ok && meta != nil {
			out.Upstream = strings.Trim(meta.Provider, `"`)
			out.CostUSD = meta.Usage.Cost
			if out.InputTokens == 0 {
				out.InputTokens, out.OutputTokens = meta.Usage.PromptTokens, meta.Usage.CompletionTokens
			}
			break
		}
	}
	return out, nil
}
