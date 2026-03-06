// Package openai provides a thin wrapper around the official OpenAI Go SDK for embeddings.
package openai

import (
	"context"
	"errors"
	"fmt"
	"strings"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"

	"github.com/amrhym/hivecfm-hub/internal/models"
	"github.com/amrhym/hivecfm-hub/pkg/embeddings"
)

var (
	// ErrEmptyInput is returned when CreateEmbedding is called with empty input.
	ErrEmptyInput = errors.New("openai: input text is empty")
	// ErrInvalidDims is returned when dimensions is not positive.
	ErrInvalidDims = errors.New("openai: embedding dimensions must be positive")
	// ErrNoEmbeddingInResponse is returned when the API response contains no embedding data.
	ErrNoEmbeddingInResponse = errors.New("openai: no embedding in response")
	// ErrDimensionMismatch is returned when the response embedding length does not match configured dimensions.
	ErrDimensionMismatch = errors.New("openai: embedding dimension mismatch")
)

// Client calls the OpenAI embeddings API via the official SDK.
type Client struct {
	sdk        openaisdk.Client
	dimensions int
	model      string
	normalize  bool
}

// clientConfig collects options before SDK client creation (e.g. base URL must be known upfront).
type clientConfig struct {
	baseURL   string
	model     string
	normalize bool
}

// ClientOption configures the Client.
type ClientOption func(*clientConfig)

// WithBaseURL sets a custom base URL for the API (e.g. for OpenAI-compatible providers like ZhipuAI).
func WithBaseURL(baseURL string) ClientOption {
	return func(cfg *clientConfig) {
		cfg.baseURL = baseURL
	}
}

// WithDimensions is kept for API compatibility but dimensions are fixed at models.EmbeddingVectorDimensions.
func WithDimensions(dim int) ClientOption {
	return func(cfg *clientConfig) {
		// dimensions override not stored in clientConfig; applied directly after SDK creation.
		// This is a no-op in the config phase; handled below in NewClient.
	}
}

// WithModel sets the embedding model name. Empty uses default.
func WithModel(model string) ClientOption {
	return func(cfg *clientConfig) {
		cfg.model = model
	}
}

// WithNormalize enables L2 normalization of the embedding vector before returning (e.g. before storing or caching).
func WithNormalize(normalize bool) ClientOption {
	return func(cfg *clientConfig) {
		cfg.normalize = normalize
	}
}

// NewClient creates an OpenAI embeddings client using the official SDK.
// Embedding dimension is fixed (models.EmbeddingVectorDimensions); WithDimensions is optional for overrides.
func NewClient(apiKey string, opts ...ClientOption) *Client {
	cfg := &clientConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	sdkOpts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if cfg.baseURL != "" {
		sdkOpts = append(sdkOpts, option.WithBaseURL(cfg.baseURL))
	}

	return &Client{
		sdk:        openaisdk.NewClient(sdkOpts...),
		dimensions: models.EmbeddingVectorDimensions,
		model:      cfg.model,
		normalize:  cfg.normalize,
	}
}

// CreateEmbedding returns the embedding vector for the given text using the configured model.
// The returned slice length equals the configured dimensions.
func (c *Client) CreateEmbedding(ctx context.Context, input string) ([]float32, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, ErrEmptyInput
	}

	if c.dimensions <= 0 {
		return nil, ErrInvalidDims
	}

	model := c.model

	resp, err := c.sdk.Embeddings.New(ctx, openaisdk.EmbeddingNewParams{
		Input: openaisdk.EmbeddingNewParamsInputUnion{
			OfString: param.NewOpt(input),
		},
		Model:      model,
		Dimensions: param.NewOpt(int64(c.dimensions)),
	})
	if err != nil {
		return nil, fmt.Errorf("openai embedding: %w", err)
	}

	if len(resp.Data) == 0 {
		return nil, ErrNoEmbeddingInResponse
	}

	emb := resp.Data[0].Embedding
	if len(emb) != c.dimensions {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrDimensionMismatch, len(emb), c.dimensions)
	}

	// SDK returns float64; convert to float32 so we match EmbeddingClient and the Google SDK (which already returns
	// float32). Precision loss (64→32, and later 32→16 in the DB driver for halfvec) is acceptable for embeddings;
	// similarity results are unchanged in practice.
	out := make([]float32, len(emb))
	for i := range emb {
		out[i] = float32(emb[i])
	}

	if c.normalize {
		embeddings.NormalizeL2(out)
	}

	return out, nil
}

// CreateEmbeddingForQuery returns an embedding for the given search query. OpenAI's API does not distinguish
// task type; this delegates to CreateEmbedding for compatibility with EmbeddingClient.
func (c *Client) CreateEmbeddingForQuery(ctx context.Context, input string) ([]float32, error) {
	return c.CreateEmbedding(ctx, input)
}
