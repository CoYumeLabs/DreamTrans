package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/dreamtrans/backend/internal/aiproviders"
	"os"
	"strings"
)

// ProviderUsage describes one logical provider operation. Reserved values are
// conservative upper bounds; settled values reflect authoritative provider
// usage. When a compatible provider omits usage metadata, callers keep the
// conservative reservation instead of undercharging unknown work.
type ProviderUsage struct {
	Action            string
	Model             string
	InputTokens       int
	CachedInputTokens int
	CacheWriteTokens  int
	OutputTokens      int
	// OperationID identifies one logical provider call inside a durable
	// workflow. HTTP accounting uses it to derive a stable billing key across
	// lease recovery without exposing the raw identifier in the ledger.
	OperationID string
	// CustomerFunded marks an operation executed with a request-scoped provider
	// credential. It still consumes tenant/API quota and may incur the configured
	// platform service fee, but must not debit the provider-cost portion that the
	// user pays directly.
	CustomerFunded bool
}

var ErrInvalidEmbeddingDimension = errors.New("invalid embedding dimension")

// ProviderUsageReservation controls the lifecycle of one provider operation.
// A successful provider response must be settled before its result is persisted
// or returned. Failed provider calls are refunded.
type ProviderUsageReservation interface {
	Settle(context.Context, *ProviderUsage) error
	Refund(string) error
}

// ProviderUsageMeter reserves billing/quota before provider work starts.
type ProviderUsageMeter interface {
	ReserveProviderUsage(context.Context, *ProviderUsage) (ProviderUsageReservation, error)
}

type providerUsageMeterContextKey struct{}
type providerOperationIDContextKey struct{}

// ErrProviderRequest identifies failures produced while calling an AI
// provider. HTTP handlers use it to keep genuine upstream 502 responses
// separate from local storage, configuration, and context-assembly failures.
var ErrProviderRequest = errors.New("AI provider request failed")

func wrapProviderRequest(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %s: %w", ErrProviderRequest, operation, err)
}

// WithProviderUsageMeter attaches request-scoped metering to RAG provider
// operations. Services used outside a billed HTTP request remain unchanged.
func WithProviderUsageMeter(ctx context.Context, meter ProviderUsageMeter) context.Context {
	if meter == nil {
		return ctx
	}
	return context.WithValue(ctx, providerUsageMeterContextKey{}, meter)
}

// WithProviderOperationID binds the next provider operation to a durable
// caller-supplied identity. It is primarily used by indexing batches whose
// progress and vector writes are committed separately.
func WithProviderOperationID(ctx context.Context, operationID string) context.Context {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return ctx
	}
	return context.WithValue(ctx, providerOperationIDContextKey{}, operationID)
}

func providerOperationIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	operationID, _ := ctx.Value(providerOperationIDContextKey{}).(string)
	return strings.TrimSpace(operationID)
}

// exactProviderOperationID binds a durable workflow operation to the exact
// provider-visible input. The caller-supplied operation ID remains the stable
// workflow root, while model/config/input changes produce a different billing
// fingerprint instead of silently reusing an old settlement.
func exactProviderOperationID(
	ctx context.Context,
	kind string,
	immutableParts ...string,
) string {
	root := providerOperationIDFromContext(ctx)
	if root == "" {
		return ""
	}
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(root))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(strings.TrimSpace(kind)))
	for _, part := range immutableParts {
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(part))
	}
	return strings.TrimSpace(kind) + ":" + hex.EncodeToString(hasher.Sum(nil))
}

func providerUsageMeterFromContext(ctx context.Context) ProviderUsageMeter {
	if ctx == nil {
		return nil
	}
	meter, _ := ctx.Value(providerUsageMeterContextKey{}).(ProviderUsageMeter)
	return meter
}

func reserveProviderUsage(
	ctx context.Context,
	usage *ProviderUsage,
) (ProviderUsageReservation, error) {
	meter := providerUsageMeterFromContext(ctx)
	if meter == nil {
		return nil, nil
	}
	if usage == nil {
		return nil, errors.New("provider usage is required")
	}
	if strings.TrimSpace(usage.OperationID) == "" {
		usage.OperationID = providerOperationIDFromContext(ctx)
	}
	reservation, err := meter.ReserveProviderUsage(ctx, usage)
	if err != nil {
		return nil, fmt.Errorf("reserve provider usage: %w", err)
	}
	if reservation == nil {
		return nil, errors.New("provider usage meter returned a nil reservation")
	}
	return reservation, nil
}

func refundProviderUsage(reservation ProviderUsageReservation, reason string, operationErr error) error {
	if reservation == nil {
		return operationErr
	}
	if refundErr := reservation.Refund(reason); refundErr != nil {
		return fmt.Errorf("%w; refund provider usage: %w", operationErr, refundErr)
	}
	return operationErr
}

func settleProviderUsage(
	ctx context.Context,
	reservation ProviderUsageReservation,
	actual *ProviderUsage,
) error {
	if reservation == nil {
		return nil
	}
	if actual == nil {
		return errors.New("actual provider usage is required")
	}
	if err := reservation.Settle(ctx, actual); err != nil {
		return fmt.Errorf("settle provider usage: %w", err)
	}
	return nil
}

// embeddingModelName is the qualified id of the embedding model: the
// configured model under the registry's embedding provider.
func embeddingModelName() string {
	model := strings.TrimSpace(os.Getenv("OPENAI_EMBEDDING_MODEL"))
	if model == "" {
		model = "text-embedding-3-small"
	}
	provider := aiproviders.Default
	if registry, err := aiproviders.Current(); err == nil {
		if p, err := registry.EmbeddingProvider(); err == nil {
			provider = p.Name
		}
	}
	return aiproviders.Qualify(provider, model)
}

// EmbeddingModelName returns the configured model used for semantic indexes.
func EmbeddingModelName() string {
	return embeddingModelName()
}

// EmbeddingDimensions returns the only vector width accepted by production indexes.
func EmbeddingDimensions() int {
	return productionEmbeddingDimensions
}

// conservativeProviderTokens is an upper bound for byte-level tokenizers: one
// token cannot encode less than one input byte. The small fixed allowance
// covers message framing added by compatible chat APIs.
func conservativeProviderTokens(parts ...string) int {
	const framingAllowance = 256
	total := framingAllowance
	for _, part := range parts {
		total += len(part)
	}
	if total < 1 {
		return 1
	}
	return total
}

func (s *Service) embedWithMeter(
	ctx context.Context,
	text string,
	expectedDimensions int,
) ([]float32, error) {
	model := embeddingModelName()
	reservation, err := reserveProviderUsage(ctx, &ProviderUsage{
		Action:      "embedding",
		Model:       model,
		InputTokens: conservativeProviderTokens(text),
		OperationID: exactProviderOperationID(ctx, "embedding", model, text),
	})
	if err != nil {
		return nil, err
	}

	actualInputTokens := conservativeProviderTokens(text)
	var vector []float32
	if provider, ok := s.embedder.(embeddingUsageProvider); ok {
		vector, actualInputTokens, err = provider.EmbedWithUsage(ctx, text)
		if actualInputTokens <= 0 {
			// An otherwise compatible provider may omit usage metadata. Keeping
			// the conservative reservation avoids turning missing metadata into
			// free upstream work.
			actualInputTokens = conservativeProviderTokens(text)
		}
	} else {
		vector, err = s.embedder.Embed(ctx, text)
	}
	if err != nil {
		return nil, refundProviderUsage(
			reservation,
			"RAG embedding provider request failed",
			wrapProviderRequest("embedding", err),
		)
	}
	if expectedDimensions > 0 && len(vector) != expectedDimensions {
		return nil, refundProviderUsage(
			reservation,
			"embedding provider returned an invalid dimension",
			wrapProviderRequest("embedding response", fmt.Errorf(
				"%w: %d (want %d)",
				ErrInvalidEmbeddingDimension,
				len(vector),
				expectedDimensions,
			)),
		)
	}
	if err := settleProviderUsage(ctx, reservation, &ProviderUsage{
		Action:      "embedding",
		Model:       model,
		InputTokens: actualInputTokens,
	}); err != nil {
		return nil, err
	}
	return vector, nil
}

// EmbedForRetrieval embeds one search query with normal provider metering.
func (s *Service) EmbedForRetrieval(
	ctx context.Context,
	text string,
) ([]float32, string, error) {
	vector, err := s.embedWithMeter(ctx, text, productionEmbeddingDimensions)
	if err != nil {
		return nil, embeddingModelName(), err
	}
	return vector, embeddingModelName(), nil
}

// EmbedBatchForIndex embeds a bounded indexing batch under one provider usage
// reservation. It is used by durable project/session indexing workers.
func (s *Service) EmbedBatchForIndex(
	ctx context.Context,
	inputs []string,
) ([][]float32, int, string, error) {
	if len(inputs) == 0 || len(inputs) > 64 {
		return nil, 0, "", fmt.Errorf("embedding batch must contain between 1 and 64 inputs")
	}
	model := embeddingModelName()
	reservedTokens := conservativeProviderTokens(inputs...)
	reservation, err := reserveProviderUsage(ctx, &ProviderUsage{
		Action:      "embedding",
		Model:       model,
		InputTokens: reservedTokens,
		OperationID: exactProviderOperationID(
			ctx,
			"embedding-batch",
			append([]string{model}, inputs...)...,
		),
	})
	if err != nil {
		return nil, 0, model, err
	}

	actualTokens := 0
	var vectors [][]float32
	if provider, ok := s.embedder.(BatchEmbeddingProvider); ok {
		vectors, actualTokens, err = provider.EmbedBatchWithUsage(ctx, inputs)
	} else {
		vectors = make([][]float32, 0, len(inputs))
		actualTokens = 0
		for _, input := range inputs {
			var vector []float32
			var tokens int
			if provider, ok := s.embedder.(embeddingUsageProvider); ok {
				vector, tokens, err = provider.EmbedWithUsage(ctx, input)
			} else {
				vector, err = s.embedder.Embed(ctx, input)
				tokens = conservativeProviderTokens(input)
			}
			if err != nil {
				break
			}
			vectors = append(vectors, vector)
			actualTokens += tokens
		}
	}
	if err != nil {
		return nil, 0, model, refundProviderUsage(
			reservation,
			"AI indexing embedding provider request failed",
			wrapProviderRequest("embedding batch", err),
		)
	}
	if actualTokens <= 0 {
		actualTokens = reservedTokens
	}
	if len(vectors) != len(inputs) {
		return nil, 0, model, refundProviderUsage(
			reservation,
			"AI indexing provider returned an incomplete batch",
			wrapProviderRequest("embedding batch response", fmt.Errorf(
				"embedding response count %d does not match input count %d",
				len(vectors), len(inputs),
			)),
		)
	}
	for index, vector := range vectors {
		if len(vector) != productionEmbeddingDimensions {
			return nil, 0, model, refundProviderUsage(
				reservation,
				"AI indexing provider returned an invalid dimension",
				wrapProviderRequest("embedding batch response", fmt.Errorf(
					"%w for vector %d: %d (want %d)",
					ErrInvalidEmbeddingDimension,
					index,
					len(vector),
					productionEmbeddingDimensions,
				)),
			)
		}
	}
	if err := settleProviderUsage(ctx, reservation, &ProviderUsage{
		Action:      "embedding",
		Model:       model,
		InputTokens: actualTokens,
	}); err != nil {
		return nil, 0, model, err
	}
	return vectors, actualTokens, model, nil
}
