// Package rag provides tenant-scoped retrieval, summaries, and embeddings.
package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/dreamtrans/backend/internal/aiproviders"
	"io"
	"net/http"
	"strings"
	"time"
)

// EmbeddingProvider defines an interface to obtain vector embeddings for text.
type EmbeddingProvider interface {
	Embed(ctx context.Context, input string) ([]float32, error)
}

// BatchEmbeddingProvider is implemented by providers that can embed a bounded
// batch and return authoritative aggregate input usage.
type BatchEmbeddingProvider interface {
	EmbedBatchWithUsage(ctx context.Context, inputs []string) ([][]float32, int, error)
}

// embeddingUsageProvider is implemented by providers that expose authoritative
// token usage. Third-party embedders can continue implementing EmbeddingProvider;
// metered requests keep their conservative reservation when exact usage is absent.
type embeddingUsageProvider interface {
	EmbedWithUsage(ctx context.Context, input string) ([]float32, int, error)
}

type openAIEmbeddingProvider struct {
	baseURL    string
	apiKey     string
	model      string
	dimensions int
	timeout    time.Duration
	httpClient *http.Client
}

const productionEmbeddingDimensions = 1536

// NewOpenAIEmbeddingFromEnv creates an embedding provider compatible with OpenAI APIs.
// The endpoint is the registry's embedding provider (AI_EMBEDDING_PROVIDER,
// default the OPENAI_* endpoint); the model is OPENAI_EMBEDDING_MODEL.
func NewOpenAIEmbeddingFromEnv() (EmbeddingProvider, error) {
	registry, err := aiproviders.Current()
	if err != nil {
		return nil, err
	}
	provider, err := registry.EmbeddingProvider()
	if err != nil {
		return nil, fmt.Errorf("OPENAI_API_KEY not set")
	}
	_, model := aiproviders.Split(EmbeddingModelName())
	return &openAIEmbeddingProvider{
		baseURL:    provider.BaseURL,
		apiKey:     provider.APIKey,
		model:      model,
		dimensions: productionEmbeddingDimensions,
		timeout:    60 * time.Second,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

type embeddingsRequest struct {
	Input      any    `json:"input"`
	Model      string `json:"model"`
	Dimensions int    `json:"dimensions"`
}

type embeddingsResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

func (p *openAIEmbeddingProvider) Embed(ctx context.Context, input string) ([]float32, error) {
	embedding, _, err := p.EmbedWithUsage(ctx, input)
	return embedding, err
}

func (p *openAIEmbeddingProvider) EmbedWithUsage(
	ctx context.Context,
	input string,
) ([]float32, int, error) {
	vectors, inputTokens, err := p.EmbedBatchWithUsage(ctx, []string{input})
	if err != nil {
		return nil, 0, err
	}
	return vectors[0], inputTokens, nil
}

func (p *openAIEmbeddingProvider) EmbedBatchWithUsage(
	ctx context.Context,
	inputs []string,
) ([][]float32, int, error) {
	if len(inputs) == 0 || len(inputs) > 64 {
		return nil, 0, fmt.Errorf("embedding batch must contain between 1 and 64 inputs")
	}
	for _, input := range inputs {
		if strings.TrimSpace(input) == "" {
			return nil, 0, fmt.Errorf("embedding input must not be empty")
		}
	}
	dimensions := p.dimensions
	if dimensions <= 0 {
		dimensions = productionEmbeddingDimensions
	}
	payload := embeddingsRequest{
		Input:      inputs,
		Model:      p.model,
		Dimensions: dimensions,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("encode embedding request: %w", err)
	}
	url := strings.TrimRight(p.baseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, 0, fmt.Errorf("embedding api error: %s", resp.Status)
	}
	const maxEmbeddingResponse = 16 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxEmbeddingResponse+1))
	if err != nil {
		return nil, 0, err
	}
	if len(raw) > maxEmbeddingResponse {
		return nil, 0, fmt.Errorf("embedding response is too large")
	}
	var out embeddingsResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, 0, err
	}
	if len(out.Data) == 0 {
		return nil, 0, fmt.Errorf("no embedding returned")
	}
	if len(out.Data) != len(inputs) {
		return nil, 0, fmt.Errorf(
			"embedding response count %d does not match input count %d",
			len(out.Data),
			len(inputs),
		)
	}
	vectors := make([][]float32, len(inputs))
	for _, item := range out.Data {
		if item.Index < 0 || item.Index >= len(vectors) || vectors[item.Index] != nil {
			return nil, 0, fmt.Errorf("invalid embedding response index: %d", item.Index)
		}
		if len(item.Embedding) != dimensions {
			return nil, 0, fmt.Errorf(
				"%w: %d (want %d)",
				ErrInvalidEmbeddingDimension,
				len(item.Embedding),
				dimensions,
			)
		}
		vectors[item.Index] = item.Embedding
	}
	inputTokens := out.Usage.PromptTokens
	if inputTokens <= 0 {
		inputTokens = out.Usage.TotalTokens
	}
	return vectors, inputTokens, nil
}
