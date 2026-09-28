package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	openaiprovider "github.com/dreamtrans/backend/internal/adapters/openai_provider"
)

// Figure reading for synced course materials: one page render in, the text
// on it plus a description of what is not text (charts, diagrams, tables,
// photos) out. The render itself is never stored; only this text is.

const (
	figureMaxOutputTokens = 1200
	figureTimeout         = 90 * time.Second
	// Providers bill images as input tokens without documenting the rate for
	// every model; reserve generously and settle on what was reported.
	figureReservedImageTokens = 4000
	figurePageTextLimit       = 2000
)

const figureInstruction = `You are reading one page of university course material, rendered as an image.
Write plain text for a student's study notes, in the page's own language:
1. Any text inside figures, charts, tables or images that is legible. Do not repeat the page text given below.
2. What the non-text content shows: for charts the axes, series and the trend or key values; for diagrams the parts and how they connect; for tables the rows, columns and notable entries; for photos what matters for the lesson.
Skip logos, page furniture and decoration. No preamble, no markdown headings, at most 250 words.
If the page has nothing beyond the given text, answer exactly: NONE`

// ErrFigureNothingToAdd means the model saw nothing beyond the page text.
var ErrFigureNothingToAdd = errors.New("figure adds nothing beyond the page text")

// DescribeFigure asks a vision-capable model to read one page render. The
// call is metered through the request's provider usage meter like chat: a
// conservative reservation first, settled on the reported usage.
func (s *Service) DescribeFigure(ctx context.Context, model string, png []byte, pageText string) (string, error) {
	cfg, err := s.chatConfigWithOverrides(&ChatOverrides{Model: model})
	if err != nil {
		return "", err
	}
	cfg.MaxOutputTokens = figureMaxOutputTokens
	cfg.Temperature = 0
	if strings.HasPrefix(strings.ToLower(cfg.Model), "gpt-5.6") {
		cfg.ReasoningEffort = "low"
	}
	if cfg.Timeout <= 0 || cfg.Timeout > figureTimeout {
		cfg.Timeout = figureTimeout
	}
	pageText = strings.TrimSpace(pageText)
	if runes := []rune(pageText); len(runes) > figurePageTextLimit {
		pageText = string(runes[:figurePageTextLimit])
	}
	instruction := figureInstruction + "\n\nPage text already extracted:\n" + pageText
	digest := sha256.Sum256(png)
	qualified := cfg.QualifiedModelID("")
	estimate := conservativeProviderTokens(instruction) + figureReservedImageTokens
	reservation, err := reserveProviderUsage(ctx, &ProviderUsage{
		Action:       "chat",
		Model:        qualified,
		InputTokens:  estimate,
		OutputTokens: cfg.MaxOutputTokens,
		OperationID: exactProviderOperationID(
			ctx, "figure", qualified, cfg.BaseURL, instruction, hex.EncodeToString(digest[:]),
		),
	})
	if err != nil {
		return "", err
	}
	out, usage, err := openaiprovider.NewTranslator(cfg).DescribeImageWithUsage(ctx, instruction, png)
	actual := ProviderUsage{
		Action:       "chat",
		Model:        qualified,
		InputTokens:  estimate,
		OutputTokens: cfg.MaxOutputTokens,
	}
	if usage != nil {
		actual.Model = usage.Model
		actual.InputTokens = usage.PromptTokens
		actual.CachedInputTokens = usage.CachedTokens
		actual.CacheWriteTokens = usage.CacheWriteTokens
		actual.OutputTokens = usage.CompletionTokens
	}
	if err != nil {
		if usage == nil {
			return "", refundProviderUsage(reservation, "figure provider request failed", wrapProviderRequest("figure", err))
		}
		if settleErr := settleProviderUsage(ctx, reservation, &actual); settleErr != nil {
			return "", errors.Join(wrapProviderRequest("figure", err), settleErr)
		}
		return "", wrapProviderRequest("figure", err)
	}
	if err := settleProviderUsage(ctx, reservation, &actual); err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" || strings.EqualFold(strings.Trim(out, ". "), "NONE") {
		return "", ErrFigureNothingToAdd
	}
	return out, nil
}
