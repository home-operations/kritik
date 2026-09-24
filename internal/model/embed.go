package model

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
)

// OpenAIEmbedder is an Embedder over any OpenAI-compatible embeddings
// endpoint, OpenRouter included. Fantasy has no embedding interface yet;
// when it gains one this becomes a Fantasy provider and the SDK dependency
// goes away.
type OpenAIEmbedder struct {
	client openai.Client
	model  string
	dims   int64
	// MaxBatch, MaxBatchChars and MaxItemChars bound one request; servers
	// differ widely in what they accept.
	MaxBatch, MaxBatchChars, MaxItemChars int
}

// NewOpenAIEmbedder builds an embedder. dims is sent to the server so the
// vectors match the table; models that ignore it return their native size
// and the caller must check.
func NewOpenAIEmbedder(baseURL, apiKey, model string, dims int) *OpenAIEmbedder {
	opts := make([]option.RequestOption, 0, 2+len(attribution))
	opts = append(opts, option.WithBaseURL(baseURL), option.WithAPIKey(apiKey))
	for k, v := range attribution {
		opts = append(opts, option.WithHeader(k, v))
	}
	return &OpenAIEmbedder{
		client: openai.NewClient(opts...), model: model, dims: int64(dims),
		MaxBatch: 64, MaxBatchChars: 200_000, MaxItemChars: 16_000,
	}
}

// Embed implements Embedder, splitting inputs into batches that respect the
// limits and truncating over-long items rather than failing the batch.
func (e *OpenAIEmbedder) Embed(ctx context.Context, inputs []string) ([][]float32, int64, error) {
	out := make([][]float32, 0, len(inputs))
	var tokens int64
	for start := 0; start < len(inputs); {
		end, chars := start, 0
		for end < len(inputs) && end-start < e.MaxBatch {
			item := len(inputs[end])
			if item > e.MaxItemChars {
				item = e.MaxItemChars
			}
			if end > start && chars+item > e.MaxBatchChars {
				break
			}
			chars += item
			end++
		}
		batch := make([]string, 0, end-start)
		for _, s := range inputs[start:end] {
			if len(s) > e.MaxItemChars {
				s = s[:e.MaxItemChars]
			}
			batch = append(batch, s)
		}
		params := openai.EmbeddingNewParams{
			Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: batch},
			Model: e.model,
		}
		if e.dims > 0 {
			params.Dimensions = param.NewOpt(e.dims)
		}
		resp, err := e.client.Embeddings.New(ctx, params)
		if err != nil {
			return nil, tokens, fmt.Errorf("model: embed batch %d..%d: %w", start, end, err)
		}
		if len(resp.Data) != len(batch) {
			return nil, tokens, fmt.Errorf("model: embed returned %d vectors for %d inputs", len(resp.Data), len(batch))
		}
		tokens += resp.Usage.PromptTokens
		for _, d := range resp.Data {
			v := make([]float32, len(d.Embedding))
			for i, f := range d.Embedding {
				v[i] = float32(f)
			}
			if e.dims > 0 && int64(len(v)) != e.dims {
				return nil, tokens, fmt.Errorf("model: %s returned %d dimensions, configured %d", e.model, len(v), e.dims)
			}
			out = append(out, v)
		}
		start = end
	}
	return out, tokens, nil
}

// VectorLiteral renders a vector the way pgvector parses it: "[1,2,3]".
func VectorLiteral(v []float32) string {
	var b strings.Builder
	b.Grow(len(v)*10 + 2)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
