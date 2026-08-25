package openai

import (
	"context"
	"fmt"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
)

// Embedder implements hwcloud.Embedder via OpenAI embeddings API.
type Embedder struct {
	client openaisdk.Client
	model  openaisdk.EmbeddingModel
}

// EmbedderOption configures an Embedder.
type EmbedderOption func(*Embedder)

// WithEmbeddingModel overrides the default embedding model.
func WithEmbeddingModel(model string) EmbedderOption {
	return func(e *Embedder) { e.model = openaisdk.EmbeddingModel(model) }
}

// NewEmbedder creates an Embedder using the OpenAI API.
func NewEmbedder(apiKey, baseURL string, opts ...EmbedderOption) *Embedder {
	e := &Embedder{
		client: openaisdk.NewClient(
			option.WithAPIKey(apiKey),
			option.WithBaseURL(baseURL),
		),
		model: openaisdk.EmbeddingModelTextEmbedding3Small,
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Embed returns the embedding vector for text.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float64, error) {
	params := openaisdk.EmbeddingNewParams{
		Input: openaisdk.EmbeddingNewParamsInputUnion{
			OfString: param.NewOpt(text),
		},
		Model: e.model,
	}
	resp, err := e.client.Embeddings.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("embed: empty response")
	}
	return resp.Data[0].Embedding, nil
}

// Dimensions returns the output dimension of the embedding model.
func (e *Embedder) Dimensions() int {
	switch e.model {
	case openaisdk.EmbeddingModelTextEmbedding3Small:
		return 1536
	case openaisdk.EmbeddingModelTextEmbedding3Large:
		return 3072
	case openaisdk.EmbeddingModelTextEmbeddingAda002:
		return 1536
	default:
		return 1536
	}
}

// Compile-time check
var _ hwcloud.Embedder = (*Embedder)(nil)
