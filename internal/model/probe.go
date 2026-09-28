package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/anthropics/anthropic-sdk-go"
)

// maxProbeModels bounds the models a probe returns for a form to offer.
const maxProbeModels = 1000

// Probe checks a provider's key with one cheap read call and returns the
// models the provider offers, sorted. OpenRouter lists its models to
// anyone, so its key is checked against its key endpoint first. An empty
// baseURL is the provider's default endpoint; client may be nil.
func Probe(ctx context.Context, t ProviderType, baseURL, apiKey string, client *http.Client) ([]string, error) {
	s, err := NewStepper(t, baseURL, apiKey, nil, client)
	if err != nil {
		return nil, err
	}
	var ids []string
	switch p := s.(type) {
	case *OpenAI:
		if p.openRouter {
			var key json.RawMessage
			if err := p.client.Get(ctx, "key", nil, &key); err != nil {
				return nil, fmt.Errorf("model: check key: %w", err)
			}
		}
		pager := p.client.Models.ListAutoPaging(ctx)
		for len(ids) < maxProbeModels && pager.Next() {
			ids = append(ids, pager.Current().ID)
		}
		if err := pager.Err(); err != nil {
			return nil, fmt.Errorf("model: list models: %w", err)
		}
	case *Anthropic:
		pager := p.client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{Limit: anthropic.Int(100)})
		for len(ids) < maxProbeModels && pager.Next() {
			ids = append(ids, pager.Current().ID)
		}
		if err := pager.Err(); err != nil {
			return nil, fmt.Errorf("model: list models: %w", err)
		}
	}
	slices.Sort(ids)
	return ids, nil
}
