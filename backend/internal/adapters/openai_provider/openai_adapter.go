// Package openaiprovider implements bounded OpenAI-compatible API clients.
package openaiprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAPIBaseURL        = "https://api.openai.com/v1"
	defaultProviderTimeout   = 60 * time.Second
	maxProviderTimeout       = 10 * time.Minute
	maxProviderRequestBytes  = 8 << 20
	maxProviderResponseBytes = 8 << 20
	maxReportedTokenCount    = 1_000_000_000
	maxRetryAttempts         = 5
)

var gptModelFamilyPattern = regexp.MustCompile(`(?i)^gpt-(\d+)(?:\.(\d+))?(?:-|$)`)

// Config holds OpenAI-style API configuration.
type Config struct {
	// Provider names the registered endpoint this configuration was built
	// for; empty means the default OPENAI_* endpoint.
	Provider    string
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	Timeout     time.Duration
	// MaxOutputTokens bounds chat-completion output when non-zero. Callers that
	// reserve usage before an upstream request must set this so the reservation
	// is a real upper bound rather than an estimate.
	MaxOutputTokens int
	// ReasoningEffort is sent only when a caller deliberately selects a
	// reasoning level supported by its model. Leaving it empty preserves the
	// provider default.
	ReasoningEffort string
	// Experimental provider-level prompt caching via Responses API
	UseResponsesAPI   bool
	EnablePromptCache bool
	PromptCacheTTL    int // seconds
	// FallbackModels are tried after Model fails. nil keeps the historical
	// OPENAI_FALLBACK_MODELS / gpt-5 family behavior for the default
	// provider; other providers get no fallback unless configured.
	FallbackModels []string
}

// NewConfigFromEnv builds Config from environment variables with sensible defaults.
// OPENAI_API_BASE, OPENAI_API_KEY, OPENAI_MODEL, OPENAI_TEMPERATURE
func NewConfigFromEnv() (*Config, error) {
	base := os.Getenv("OPENAI_API_BASE")
	if base == "" {
		// Backward compatibility for deployments created by older installers.
		base = os.Getenv("OPENAI_BASE")
	}
	if base == "" {
		base = defaultAPIBaseURL
	}

	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY not set")
	}

	model := os.Getenv("OPENAI_MODEL")
	if model == "" {
		// Default general chat/summarize model
		model = "gpt-5.6-sol"
	}

	// Default to 0 to omit the field (OpenAI defaults to 1)
	temp := 0.0
	if v := os.Getenv("OPENAI_TEMPERATURE"); v != "" {
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil {
			temp = f
		}
	}

	cfg := &Config{
		BaseURL:     base,
		APIKey:      key,
		Model:       model,
		Temperature: temp,
		Timeout:     60 * time.Second,
		// Responses is the primary OpenAI path. Compatible/custom providers
		// remain on Chat Completions unless explicitly enabled.
		UseResponsesAPI:   IsOfficialOpenAIBase(base),
		EnablePromptCache: IsOfficialOpenAIBase(base),
	}
	if v := strings.TrimSpace(os.Getenv("OPENAI_USE_RESPONSES")); v != "" {
		cfg.UseResponsesAPI = v == "1" || strings.EqualFold(v, "true")
	}
	if v := strings.TrimSpace(os.Getenv("OPENAI_PROMPT_CACHE")); v != "" {
		cfg.EnablePromptCache = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("OPENAI_PROMPT_CACHE_TTL"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			cfg.PromptCacheTTL = n
		}
	}
	if cfg.PromptCacheTTL == 0 {
		cfg.PromptCacheTTL = 1800
	}
	return normalizedConfig(cfg), nil
}

// Translator provides translation and summarization using an OpenAI-compatible Chat Completions API.
type Translator struct {
	cfg        *Config
	httpClient *http.Client
}

func NewTranslator(cfg *Config) *Translator {
	normalized := normalizedConfig(cfg)
	return &Translator{
		// Keep an immutable snapshot. Several translators are shared between
		// worker goroutines, while callers commonly reuse their Config value.
		cfg: normalized,
		httpClient: &http.Client{
			Timeout: normalized.Timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func normalizedConfig(cfg *Config) *Config {
	normalized := Config{
		BaseURL:        defaultAPIBaseURL,
		Model:          "gpt-5.6-sol",
		Timeout:        defaultProviderTimeout,
		PromptCacheTTL: 1800,
	}
	if cfg != nil {
		normalized = *cfg
	}
	normalized.BaseURL = strings.TrimSpace(normalized.BaseURL)
	if normalized.BaseURL == "" {
		normalized.BaseURL = defaultAPIBaseURL
	}
	normalized.Model = strings.TrimSpace(normalized.Model)
	if normalized.Model == "" {
		normalized.Model = "gpt-5.6-sol"
	}
	if normalized.Timeout <= 0 {
		normalized.Timeout = defaultProviderTimeout
	} else if normalized.Timeout > maxProviderTimeout {
		normalized.Timeout = maxProviderTimeout
	}
	if math.IsNaN(normalized.Temperature) || math.IsInf(normalized.Temperature, 0) ||
		normalized.Temperature < 0 || normalized.Temperature > 2 {
		normalized.Temperature = 0
	}
	if normalized.MaxOutputTokens < 0 || normalized.MaxOutputTokens > maxReportedTokenCount {
		normalized.MaxOutputTokens = 0
	}
	switch normalized.ReasoningEffort {
	case "none", "low", "medium", "high", "xhigh", "max":
	default:
		normalized.ReasoningEffort = ""
	}
	if normalized.PromptCacheTTL <= 0 {
		normalized.PromptCacheTTL = 1800
	} else if normalized.PromptCacheTTL > 86400 {
		normalized.PromptCacheTTL = 86400
	}
	return &normalized
}

func providerEndpoint(baseURL, suffix string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("invalid provider base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("provider base URL must use http or https")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid provider base URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + suffix
	parsed.RawPath = ""
	return parsed.String(), nil
}

func marshalProviderRequest(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("failed to encode provider request: %w", err)
	}
	if len(payload) > maxProviderRequestBytes {
		return nil, fmt.Errorf("provider request exceeds %d bytes", maxProviderRequestBytes)
	}
	return payload, nil
}

// openAIChatRequest represents minimal Chat Completions payload.
type openAIChatRequest struct {
	Model               string              `json:"model"`
	Messages            []map[string]string `json:"messages"`
	Temperature         float64             `json:"temperature,omitempty"`
	MaxCompletionTokens int                 `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string              `json:"reasoning_effort,omitempty"`
	Stream              bool                `json:"stream,omitempty"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

type providerOutputError struct {
	API    string
	Reason string
}

func (e *providerOutputError) Error() string {
	if strings.TrimSpace(e.Reason) == "" {
		return e.API + " returned no output text"
	}
	return e.API + " returned no usable output: " + e.Reason
}

func validateProviderText(api, content, finishReason string) error {
	finishReason = strings.TrimSpace(finishReason)
	if finishReason == "length" || finishReason == "max_tokens" {
		return &providerOutputError{API: api, Reason: "max output tokens exhausted"}
	}
	if strings.TrimSpace(content) == "" {
		return &providerOutputError{API: api, Reason: "empty text"}
	}
	return nil
}

// nolint:gocyclo // fallback and retry logic is intentionally explicit for clarity
func (t *Translator) chatComplete(ctx context.Context, messages []map[string]string) (string, error) {
	// Helper to call API with a specific model + temp
	sendWith := func(model string, temp float64) (string, int, string, error) {
		reqBody := openAIChatRequest{
			Model:               model,
			Messages:            messages,
			Temperature:         temp, // omitted when 0 due to omitempty
			MaxCompletionTokens: t.cfg.MaxOutputTokens,
			ReasoningEffort:     t.cfg.ReasoningEffort,
			Stream:              false,
		}
		b, err := marshalProviderRequest(reqBody)
		if err != nil {
			return "", 0, "", err
		}
		endpoint, err := providerEndpoint(t.cfg.BaseURL, "/chat/completions")
		if err != nil {
			return "", 0, "", err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
		if err != nil {
			return "", 0, "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+t.cfg.APIKey)

		resp, err := t.httpClient.Do(req)
		if err != nil {
			return "", 0, "", err
		}
		defer func() { _ = resp.Body.Close() }()

		rawBytes, readErr := readLimitedResponse(resp.Body)
		if readErr != nil {
			return "", resp.StatusCode, "", readErr
		}
		raw := bytes.NewBuffer(rawBytes)

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// Keep the bounded body for internal compatibility detection, but
			// do not propagate it: providers occasionally echo prompt text.
			return "", resp.StatusCode, raw.String(), fmt.Errorf("openai api error: status %d", resp.StatusCode)
		}

		var out openAIChatResponse
		if err := json.Unmarshal(raw.Bytes(), &out); err != nil {
			return "", resp.StatusCode, raw.String(), err
		}
		if len(out.Choices) == 0 {
			return "", resp.StatusCode, raw.String(), fmt.Errorf("no choices returned")
		}
		content := out.Choices[0].Message.Content
		if err := validateProviderText(
			"chat completions API",
			content,
			out.Choices[0].FinishReason,
		); err != nil {
			return "", resp.StatusCode, raw.String(), err
		}
		return content, resp.StatusCode, raw.String(), nil
	}

	// Build model candidates: primary + fallbacks. The default provider keeps
	// the historical env / gpt-5 family list; any other provider only falls
	// back to models explicitly configured for it, never across providers.
	modelPrimary := t.cfg.Model
	fallbacks := []string{}
	candidates := t.cfg.FallbackModels
	if candidates == nil && (t.cfg.Provider == "" || t.cfg.Provider == "openai-compatible") {
		if v := os.Getenv("OPENAI_FALLBACK_MODELS"); v != "" {
			candidates = strings.Split(v, ",")
		} else {
			candidates = []string{"gpt-5.6-luna", "gpt-5-mini"}
		}
	}
	for _, p := range candidates {
		p = strings.TrimSpace(p)
		if p != "" && !strings.EqualFold(p, modelPrimary) {
			fallbacks = append(fallbacks, p)
		}
	}
	models := append([]string{modelPrimary}, fallbacks...)

	var lastErr error
	for _, model := range models {
		// First attempt with configured temperature
		content, code, body, err := sendWith(model, t.cfg.Temperature)
		if os.Getenv("OPENAI_DEBUG") == "1" {
			//nolint:gosec // G706: environment/provider values are escaped with strconv.Quote.
			log.Printf(
				"openai.chat model=%s code=%d err=%s",
				strconv.Quote(model),
				code,
				strconv.Quote(fmt.Sprint(err)),
			)
		}
		if err == nil {
			return content, nil
		}
		lastErr = err
		bl := strings.ToLower(body)
		// If temp rejected, retry without temp
		if code == http.StatusBadRequest && (strings.Contains(bl, "temperature") || strings.Contains(bl, "unsupported_value")) {
			content2, code2, _, err2 := sendWith(model, 0)
			if err2 == nil {
				return content2, nil
			}
			if os.Getenv("OPENAI_DEBUG") == "1" {
				//nolint:gosec // G706: environment/provider values are escaped with strconv.Quote.
				log.Printf(
					"openai.chat retry(no-temp) model=%s code=%d err=%s",
					strconv.Quote(model),
					code2,
					strconv.Quote(fmt.Sprint(err2)),
				)
			}
			lastErr = err2
		}
		// If model invalid, continue to next fallback
		if code == http.StatusBadRequest || code == http.StatusNotFound {
			if strings.Contains(bl, "model") || strings.Contains(bl, "unknown") || strings.Contains(bl, "not found") || strings.Contains(bl, "unsupported") {
				continue
			}
		}
		// For other errors, break and return
		break
	}
	return "", lastErr
}

func readLimitedResponse(body io.Reader) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(body, maxProviderResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxProviderResponseBytes {
		return nil, fmt.Errorf("provider response exceeds %d bytes", maxProviderResponseBytes)
	}
	return payload, nil
}

// -------------------- Responses API (optional) --------------------
// Some providers support caching hint via Responses API with content parts.
// We construct input with cacheable system/context parts when enabled.

type respContentPart map[string]any

type responsesRequest struct {
	Model                string             `json:"model"`
	Input                []map[string]any   `json:"input"`
	Store                bool               `json:"store"`
	Modalities           []string           `json:"modalities,omitempty"`
	Temperature          float64            `json:"temperature,omitempty"`
	MaxOutputTokens      int                `json:"max_output_tokens,omitempty"`
	Reasoning            *reasoningConfig   `json:"reasoning,omitempty"`
	PromptCacheKey       string             `json:"prompt_cache_key,omitempty"`
	PromptCacheOptions   *promptCacheOption `json:"prompt_cache_options,omitempty"`
	PromptCacheRetention string             `json:"prompt_cache_retention,omitempty"`
}

func IsOfficialOpenAIBase(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && strings.EqualFold(parsed.Hostname(), "api.openai.com")
}

type promptCacheOption struct {
	Mode string `json:"mode"`
	TTL  string `json:"ttl"`
}

type reasoningConfig struct {
	Effort string `json:"effort"`
}

func supportsExplicitPromptCaching(model string) bool {
	match := gptModelFamilyPattern.FindStringSubmatch(strings.TrimSpace(model))
	if len(match) == 0 {
		return false
	}
	major, majorErr := strconv.Atoi(match[1])
	if majorErr != nil || major < 5 {
		return false
	}
	if major > 5 {
		return true
	}
	if match[2] == "" {
		return false
	}
	minor, minorErr := strconv.Atoi(match[2])
	return minorErr == nil && minor >= 6
}

func supportsLegacyExtendedPromptCache(model string) bool {
	normalized := strings.ToLower(strings.TrimSpace(model))
	for _, family := range []string{
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.2",
		"gpt-5.1-codex-max",
		"gpt-5.1-chat-latest",
		"gpt-5.1-codex-mini",
		"gpt-5.1-codex",
		"gpt-5.1",
		"gpt-5-codex",
		"gpt-4.1",
	} {
		if normalized == family || strings.HasPrefix(normalized, family+"-") {
			return true
		}
	}
	return normalized == "gpt-5" || strings.HasPrefix(normalized, "gpt-5-20")
}

type providerHTTPError struct {
	API        string
	StatusCode int
}

func (e *providerHTTPError) Error() string {
	return fmt.Sprintf("%s api error: status %d", e.API, e.StatusCode)
}

func (e *providerHTTPError) UnsupportedRouteOrField() bool {
	return e.StatusCode == http.StatusBadRequest ||
		e.StatusCode == http.StatusNotFound ||
		e.StatusCode == http.StatusMethodNotAllowed ||
		e.StatusCode == http.StatusUnprocessableEntity
}

func shouldFallbackResponses(err error) bool {
	var providerErr *providerHTTPError
	return errors.As(err, &providerErr) && providerErr.UnsupportedRouteOrField()
}

//nolint:gocyclo // Encoding, protocol validation, and compatibility parsing form one request lifecycle.
func (t *Translator) responsesComplete(
	ctx context.Context,
	systemPrompt, contextText, userText string,
	withCache bool,
	cacheKeys ...string,
) (string, *Usage, error) {
	sysPart := respContentPart{"type": "input_text", "text": systemPrompt}
	ctxPart := respContentPart{"type": "input_text", "text": "<context>\n" + contextText + "\n</context>"}
	explicitCache := withCache && supportsExplicitPromptCaching(t.cfg.Model)
	if explicitCache {
		ctxPart["prompt_cache_breakpoint"] = map[string]string{"mode": "explicit"}
	}
	input := []map[string]any{
		{"role": "system", "content": []respContentPart{sysPart}},
		{"role": "system", "content": []respContentPart{ctxPart}},
		{"role": "user", "content": []respContentPart{{"type": "input_text", "text": userText}}},
	}
	reqBody := responsesRequest{
		Model:           t.cfg.Model,
		Input:           input,
		Store:           false,
		MaxOutputTokens: t.cfg.MaxOutputTokens,
	}
	if t.cfg.ReasoningEffort != "" {
		reqBody.Reasoning = &reasoningConfig{Effort: t.cfg.ReasoningEffort}
	}
	if withCache {
		if explicitCache {
			// GPT-5.6+ currently accepts only 30m here. The configured TTL is
			// retained for older models whose 24h policy uses a different field.
			reqBody.PromptCacheOptions = &promptCacheOption{Mode: "explicit", TTL: "30m"}
		} else if t.cfg.PromptCacheTTL > 1800 && supportsLegacyExtendedPromptCache(t.cfg.Model) {
			reqBody.PromptCacheRetention = "24h"
		}
		if len(cacheKeys) > 0 {
			reqBody.PromptCacheKey = strings.TrimSpace(cacheKeys[0])
		}
	}
	if t.cfg.Temperature != 0 {
		reqBody.Temperature = t.cfg.Temperature
	}
	b, err := marshalProviderRequest(reqBody)
	if err != nil {
		return "", nil, err
	}
	endpoint, err := providerEndpoint(t.cfg.BaseURL, "/responses")
	if err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.cfg.APIKey)
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	rawBytes, readErr := readLimitedResponse(resp.Body)
	if readErr != nil {
		return "", nil, readErr
	}
	raw := bytes.NewBuffer(rawBytes)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil, &providerHTTPError{API: "responses", StatusCode: resp.StatusCode}
	}
	// Parse minimal responses shape
	var out struct {
		Status            string `json:"status"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
			InputDetails struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw.Bytes(), &out); err != nil {
		return "", nil, err
	}
	contentParts := make([]string, 0, len(out.Output))
	for _, item := range out.Output {
		if item.Type != "" && item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type != "" && part.Type != "output_text" {
				continue
			}
			if strings.TrimSpace(part.Text) != "" {
				contentParts = append(contentParts, part.Text)
			}
		}
	}
	content := strings.Join(contentParts, "\n")
	if strings.TrimSpace(content) == "" {
		// Fallback: try to parse alternative content layout
		var alt struct {
			OutputText string `json:"output_text"`
		}
		_ = json.Unmarshal(raw.Bytes(), &alt)
		content = alt.OutputText
	}
	u := validUsage(out.Usage.InputTokens, out.Usage.OutputTokens, out.Usage.TotalTokens, out.Model)
	if u != nil {
		u.CachedTokens = boundedUsageSubset(
			out.Usage.InputDetails.CachedTokens,
			u.PromptTokens,
		)
		u.CacheWriteTokens = boundedUsageSubset(
			out.Usage.InputDetails.CacheWriteTokens,
			u.PromptTokens-u.CachedTokens,
		)
	}
	if os.Getenv("OPENAI_DEBUG") == "1" {
		if u != nil {
			log.Printf("openai.responses usage model=%s prompt=%d completion=%d total=%d", u.Model, u.PromptTokens, u.CompletionTokens, u.TotalTokens)
		} else {
			hasUsage := bytes.Contains(raw.Bytes(), []byte("\"usage\""))
			log.Printf("openai.responses usage missing model=%s body_has_usage_key=%v len=%d", out.Model, hasUsage, len(raw.Bytes()))
		}
	}
	if out.Status == "incomplete" {
		reason := strings.TrimSpace(out.IncompleteDetails.Reason)
		if reason == "max_output_tokens" || reason == "max_tokens" {
			reason = "max output tokens exhausted"
		}
		return "", u, &providerOutputError{API: "responses API", Reason: reason}
	}
	if err := validateProviderText("responses API", content, ""); err != nil {
		return "", u, err
	}
	return content, u, nil
}

// Chat exposes a generic chat completion using the configured model.
func (t *Translator) Chat(ctx context.Context, messages []map[string]string) (string, error) {
	return t.chatComplete(ctx, messages)
}

// Usage includes token accounting for a chat completion.
type Usage struct {
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	Model            string `json:"model"`
	CachedTokens     int    `json:"cached_tokens,omitempty"`
	CacheWriteTokens int    `json:"cache_write_tokens,omitempty"`
}

// RespondWithUsage uses Responses for the official OpenAI endpoint and falls
// back to Chat Completions only when the endpoint or a request field is clearly
// unsupported. Timeouts, rate limits and server failures remain visible.
func (t *Translator) RespondWithUsage(
	ctx context.Context,
	systemPrompt, stableContext, history, userText, cacheKey string,
) (string, *Usage, error) {
	user := strings.TrimSpace(userText)
	if strings.TrimSpace(history) != "" {
		user = "[Chat History]\n" + strings.TrimSpace(history) + "\n\n[Question]\n" + user
	}
	if t.cfg.UseResponsesAPI {
		out, usage, err := t.responsesComplete(
			ctx,
			systemPrompt,
			stableContext,
			user,
			t.cfg.EnablePromptCache && strings.TrimSpace(stableContext) != "",
			cacheKey,
		)
		if err == nil {
			return out, usage, nil
		}
		if !shouldFallbackResponses(err) {
			return "", usage, err
		}
	}
	messages := []map[string]string{{"role": "system", "content": systemPrompt}}
	if strings.TrimSpace(stableContext) != "" {
		messages = append(messages, map[string]string{
			"role": "system", "content": "[Context]\n" + stableContext,
		})
	}
	messages = append(messages, map[string]string{"role": "user", "content": user})
	return t.ChatWithUsage(ctx, messages)
}

func (t *Translator) RespondWithUsageRetry(
	ctx context.Context,
	systemPrompt, stableContext, history, userText, cacheKey string,
	attempts int,
) (string, *Usage, error) {
	attempts = boundedRetryAttempts(attempts)
	var lastErr error
	var lastUsage *Usage
	for attempt := 0; attempt < attempts; attempt++ {
		content, usage, err := t.RespondWithUsage(
			ctx, systemPrompt, stableContext, history, userText, cacheKey,
		)
		if err == nil {
			return content, usage, nil
		}
		lastErr = err
		lastUsage = usage
		if !IsRetryableError(err) {
			break
		}
		select {
		case <-ctx.Done():
			return "", lastUsage, ctx.Err()
		case <-time.After(backoff(attempt)):
		}
	}
	return "", lastUsage, lastErr
}

// ChatWithUsage calls the API once with the configured model and returns usage if provided by the server.
// Note: This does not include model fallback logic to keep response parsing simple; callers can handle errors.
//
//nolint:gocyclo // Usage compatibility parsing intentionally supports three upstream shapes.
func (t *Translator) ChatWithUsage(ctx context.Context, messages []map[string]string) (string, *Usage, error) {
	reqBody := openAIChatRequest{
		Model:               t.cfg.Model,
		Messages:            messages,
		Temperature:         t.cfg.Temperature,
		MaxCompletionTokens: t.cfg.MaxOutputTokens,
		ReasoningEffort:     t.cfg.ReasoningEffort,
		Stream:              false,
	}
	b, err := marshalProviderRequest(reqBody)
	if err != nil {
		return "", nil, err
	}
	endpoint, err := providerEndpoint(t.cfg.BaseURL, "/chat/completions")
	if err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.cfg.APIKey)
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	rawBytes, readErr := readLimitedResponse(resp.Body)
	if readErr != nil {
		return "", nil, readErr
	}
	raw := bytes.NewBuffer(rawBytes)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", nil, fmt.Errorf("openai api error: status %d", resp.StatusCode)
	}
	content, model, finishReason, err := parseChatContent(raw.Bytes())
	if err != nil {
		return "", nil, err
	}
	usage := parseUsageCanonical(raw.Bytes(), model)
	if usage == nil {
		usage = parseUsageAlt(raw.Bytes(), model)
	}
	if usage == nil {
		usage = parseUsageLoose(raw.Bytes(), model)
	}
	if err := validateProviderText("chat completions API", content, finishReason); err != nil {
		return "", usage, err
	}
	if usage != nil {
		if os.Getenv("OPENAI_DEBUG") == "1" {
			log.Printf("openai.chat usage model=%s prompt=%d completion=%d total=%d", usage.Model, usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
		}
		return content, usage, nil
	}
	if os.Getenv("OPENAI_DEBUG") == "1" {
		hasUsage := bytes.Contains(raw.Bytes(), []byte("\"usage\""))
		log.Printf("openai.chat usage missing model=%s body_has_usage_key=%v len=%d", model, hasUsage, len(raw.Bytes()))
	}
	// No usage information present: return without usage (OpenAI should provide usage)
	return content, nil, nil
}

// IsRetryableError reports whether a provider failure is safe to retry with
// the same idempotent application request. Authentication, validation, and
// other permanent 4xx failures deliberately remain terminal.
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	s := strings.ToLower(err.Error())
	// Common transient patterns seen from proxies/providers
	for _, p := range []string{
		"timeout", "temporarily unavailable", "connection reset", "before headers", "upstream connect error",
		"connection refused", "unexpected eof", "econnreset", "429", "500", "502", "503", "504",
		"reset reason", "gateway", "retry later", "deadline exceeded",
	} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

func shouldRetryErr(err error) bool {
	return IsRetryableError(err)
}

func backoff(attempt int) time.Duration {
	// clamp and use safe shifting to avoid overflow/casts
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 8 {
		attempt = 8
	}
	base := 200 * time.Millisecond
	factor := time.Duration(1) << attempt // 1,2,4,8,...
	d := base * factor
	// small jitter
	jitter := time.Duration((attempt+1)*37) * time.Millisecond
	return d + jitter
}

// ChatWithUsageRetry retries ChatWithUsage for transient upstream errors.
func (t *Translator) ChatWithUsageRetry(ctx context.Context, messages []map[string]string, attempts int) (string, *Usage, error) {
	attempts = boundedRetryAttempts(attempts)
	var lastErr error
	var lastUsage *Usage
	for i := 0; i < attempts; i++ {
		content, usage, err := t.ChatWithUsage(ctx, messages)
		if err == nil {
			return content, usage, nil
		}
		lastErr = err
		lastUsage = usage
		if !shouldRetryErr(err) {
			break
		}
		// sleep with backoff unless context is done
		select {
		case <-ctx.Done():
			return "", lastUsage, ctx.Err()
		case <-time.After(backoff(i)):
		}
	}
	return "", lastUsage, lastErr
}

func boundedRetryAttempts(attempts int) int {
	if attempts < 1 {
		return 1
	}
	if attempts > maxRetryAttempts {
		return maxRetryAttempts
	}
	return attempts
}

// parseChatContent extracts first choice content and model.
func parseChatContent(raw []byte) (content, model, finishReason string, err error) {
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Model string `json:"model"`
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return
	}
	if len(out.Choices) == 0 {
		err = fmt.Errorf("no choices returned")
		return
	}
	content = out.Choices[0].Message.Content
	finishReason = out.Choices[0].FinishReason
	model = out.Model
	return
}

// parseUsageCanonical reads usage.prompt_tokens/completion_tokens/total_tokens if present.
func parseUsageCanonical(raw []byte, model string) *Usage {
	var out struct {
		Usage struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
			TotalTokens      *int `json:"total_tokens"`
			CacheWriteTokens *int `json:"cache_creation_input_tokens"`
			PromptDetails    struct {
				CachedTokens     *int `json:"cached_tokens"`
				CacheWriteTokens *int `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	if out.Usage.PromptTokens == nil && out.Usage.CompletionTokens == nil {
		return nil
	}
	usage := validUsage(
		valueOrZero(out.Usage.PromptTokens),
		valueOrZero(out.Usage.CompletionTokens),
		valueOrZero(out.Usage.TotalTokens),
		model,
	)
	if usage != nil {
		usage.CachedTokens = boundedUsageSubset(valueOrZero(out.Usage.PromptDetails.CachedTokens), usage.PromptTokens)
		cacheWrite := valueOrZero(out.Usage.PromptDetails.CacheWriteTokens)
		if cacheWrite == 0 {
			cacheWrite = valueOrZero(out.Usage.CacheWriteTokens)
		}
		usage.CacheWriteTokens = boundedUsageSubset(cacheWrite, usage.PromptTokens-usage.CachedTokens)
	}
	return usage
}

// parseUsageAlt supports providers returning input_tokens/output_tokens/total_tokens.
func parseUsageAlt(raw []byte, model string) *Usage {
	var outAlt struct {
		Usage struct {
			InputTokens  *int `json:"input_tokens"`
			OutputTokens *int `json:"output_tokens"`
			TotalTokens  *int `json:"total_tokens"`
			InputDetails struct {
				CachedTokens     *int `json:"cached_tokens"`
				CacheWriteTokens *int `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &outAlt); err != nil {
		return nil
	}
	if outAlt.Usage.InputTokens == nil && outAlt.Usage.OutputTokens == nil {
		return nil
	}
	m := model
	if outAlt.Model != "" {
		m = outAlt.Model
	}
	usage := validUsage(
		valueOrZero(outAlt.Usage.InputTokens),
		valueOrZero(outAlt.Usage.OutputTokens),
		valueOrZero(outAlt.Usage.TotalTokens),
		m,
	)
	if usage != nil {
		usage.CachedTokens = boundedUsageSubset(valueOrZero(outAlt.Usage.InputDetails.CachedTokens), usage.PromptTokens)
		usage.CacheWriteTokens = boundedUsageSubset(valueOrZero(outAlt.Usage.InputDetails.CacheWriteTokens), usage.PromptTokens-usage.CachedTokens)
	}
	return usage
}

func boundedUsageSubset(value, maximum int) int {
	if value < 0 || maximum < 0 || value > maximum {
		return 0
	}
	return value
}

func valueOrZero(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

// parseIntLoose converts supported numeric-like values to int.
func parseIntLoose(v any) int {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) || t <= 0 ||
			t > maxReportedTokenCount || math.Trunc(t) != t {
			return 0
		}
		return int(t)
	case json.Number:
		return parseTokenCount(string(t))
	case string:
		return parseTokenCount(t)
	default:
		return 0
	}
}

func parseTokenCount(value string) int {
	count, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || count <= 0 || count > maxReportedTokenCount {
		return 0
	}
	return int(count)
}

func validReportedTokenValue(value any) bool {
	switch count := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(string(count), 10, 64)
		return err == nil && parsed >= 0 && parsed <= maxReportedTokenCount
	case float64:
		return !math.IsNaN(count) && !math.IsInf(count, 0) &&
			count >= 0 && count <= maxReportedTokenCount && math.Trunc(count) == count
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(count), 10, 64)
		return err == nil && parsed >= 0 && parsed <= maxReportedTokenCount
	default:
		return false
	}
}

func validUsage(promptTokens, completionTokens, totalTokens int, model string) *Usage {
	if promptTokens < 0 || completionTokens < 0 || totalTokens < 0 ||
		promptTokens > maxReportedTokenCount ||
		completionTokens > maxReportedTokenCount ||
		totalTokens > maxReportedTokenCount {
		return nil
	}
	if totalTokens == 0 && (promptTokens > 0 || completionTokens > 0) {
		if promptTokens > maxReportedTokenCount-completionTokens {
			return nil
		}
		totalTokens = promptTokens + completionTokens
	}
	if promptTokens == 0 && completionTokens == 0 && totalTokens == 0 {
		return nil
	}
	if len(model) > 512 {
		model = ""
	}
	return &Usage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		Model:            model,
	}
}

// pickFirstInt returns the first positive int for keys in order.
func pickFirstInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if n := parseIntLoose(m[k]); n > 0 {
			return n
		}
	}
	return 0
}

// pickModel prefers explicit model string from response if caller model is empty.
func pickModel(root, usage map[string]any, fallback string) string {
	if fallback != "" {
		return fallback
	}
	if ms, ok := root["model"].(string); ok && ms != "" {
		return ms
	}
	if ms, ok := usage["model"].(string); ok && ms != "" {
		return ms
	}
	return fallback
}

// parseUsageLoose attempts to parse usage with relaxed typing and multiple key names
// to accommodate providers that return strings or floats for token counts.
func parseUsageLoose(raw []byte, model string) *Usage {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil
	}
	uRaw, ok := root["usage"].(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{
		"prompt_tokens",
		"input_tokens",
		"completion_tokens",
		"output_tokens",
		"total_tokens",
	} {
		if value, exists := uRaw[key]; exists && !validReportedTokenValue(value) {
			return nil
		}
	}
	pt := pickFirstInt(uRaw, "prompt_tokens", "input_tokens")
	ct := pickFirstInt(uRaw, "completion_tokens", "output_tokens")
	tot := parseIntLoose(uRaw["total_tokens"])
	if tot == 0 && (pt > 0 || ct > 0) {
		tot = pt + ct
	}
	m := pickModel(root, uRaw, model)
	result := validUsage(pt, ct, tot, m)
	if result == nil {
		return nil
	}
	for _, detailsKey := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if details, ok := uRaw[detailsKey].(map[string]any); ok {
			result.CachedTokens = boundedUsageSubset(
				pickFirstInt(details, "cached_tokens"),
				result.PromptTokens,
			)
			result.CacheWriteTokens = boundedUsageSubset(
				pickFirstInt(details, "cache_write_tokens"),
				result.PromptTokens-result.CachedTokens,
			)
		}
	}
	if result.CacheWriteTokens == 0 {
		result.CacheWriteTokens = boundedUsageSubset(
			pickFirstInt(uRaw, "cache_creation_input_tokens"),
			result.PromptTokens-result.CachedTokens,
		)
	}
	return result
}

// approximateTokenCount provides a rough token estimate when providers don't return usage.
// Heuristic: if string contains significant non-ASCII, treat each rune as ~1 token; otherwise ~4 chars per token.
// approximateTokenCount was removed to avoid inaccurate token estimation.

// polishedTranslatePrompt keeps strict separation of context and text and asks for fluency polishing.
func polishedTranslatePrompt(contextText, segment string) []map[string]string {
	system := `您是一位专业的同声传译翻译，你正在把英文的口语内容翻译成中文易于理解的话，请使用 <context> 来帮助你理解上下文和当前场景并作出适当的纠错和润色。请仅翻译 <text>...</text> 里的文本变成中文，然后对中文进行润色，使其流畅、自然、易读，同时保留原文含义和语气。请尽量使用简洁、地道的措辞；根据需要合并不完整的句子；修改不合适的词序；删除填充词。请保持专业术语的准确性；保留数字/单位；并在适当的情况下将标点符号标准化为中文格式。请勿在输出中包含 <context> 中的任何内容。请勿添加解释、引述、说话者标签、时间戳或语言标签。仅返回最终润色后的中文句子，其他内容请勿返回。`

	user := "<context>\n" + contextText + "\n</context>\n<text>\n" + segment + "\n</text>"
	return []map[string]string{
		{"role": "system", "content": system},
		{"role": "user", "content": user},
	}
}

// polishedTranslatePromptWithHint appends optional user guidance to the system prompt.
func polishedTranslatePromptWithHint(contextText, segment, hint string) []map[string]string {
	sys := strings.Join([]string{
		"You are a professional EN->ZH translator and copy editor.",
		"Use the <context> only to understand semantics and terms.",
		"Translate ONLY the text inside <text>...</text> into Simplified Chinese.",
		"Then polish the Chinese so it is fluent, natural, and easy to read while preserving meaning and tone.",
		"Prefer concise, idiomatic phrasing; merge fragments as needed; fix awkward word order; remove filler.",
		"Keep technical terminology accurate; keep numbers/units; standardize punctuation to Chinese style when appropriate.",
		"Do NOT include any content from <context> in the output.",
		"Do NOT add explanations, quotes, speaker labels, timestamps, or language tags.",
		"Return only the final polished Chinese sentence(s), nothing else.",
	}, " ")
	if strings.TrimSpace(hint) != "" {
		sys = sys + " Additional guidance: " + hint
	}
	user := "<context>\n" + contextText + "\n</context>\n<text>\n" + segment + "\n</text>"
	return []map[string]string{
		{"role": "system", "content": sys},
		{"role": "user", "content": user},
	}
}

// summarizePrompt asks the model to compress prior context while preserving important details.
func summarizePrompt(previousSummary, backlog string) []map[string]string {
	system := "You are a precise context compressor. Summarize the transcribed conversation text for downstream translation. Keep names, entities, topics, and unresolved references. Keep it concise and information-dense. Write the summary in the same language as the transcript."
	user := "Previous summary (may be empty):\n" + previousSummary + "\n---\nNew backlog to compress:\n" + backlog + "\n---\nUpdate the summary."
	return []map[string]string{
		{"role": "system", "content": system},
		{"role": "user", "content": user},
	}
}

func summarizePromptWithHint(previousSummary, backlog, hint string) []map[string]string {
	system := "You are a precise context compressor. Summarize the transcribed conversation text for downstream translation. Keep names, entities, topics, and unresolved references. Keep it concise and information-dense. Write the summary in the same language as the transcript."
	if strings.TrimSpace(hint) != "" {
		system = system + " Additional guidance: " + hint
	}
	user := "Previous summary (may be empty):\n" + previousSummary + "\n---\nNew backlog to compress:\n" + backlog + "\n---\nUpdate the summary."
	return []map[string]string{
		{"role": "system", "content": system},
		{"role": "user", "content": user},
	}
}

// Translate produces a Chinese translation for the given segment using optional rolling or summarized context.
func (t *Translator) Translate(ctx context.Context, contextText, segment string) (string, error) {
	msgs := polishedTranslatePrompt(contextText, segment)
	out, err := t.chatComplete(ctx, msgs)
	if err != nil {
		return "", err
	}
	return sanitizeTranslationOutput(contextText, segment, out), nil
}

// TranslateWithSystemPrompt uses the provided system prompt verbatim.
func (t *Translator) TranslateWithSystemPrompt(ctx context.Context, contextText, segment, systemPrompt string) (string, error) {
	if strings.TrimSpace(systemPrompt) == "" {
		return t.Translate(ctx, contextText, segment)
	}
	user := "<context>\n" + contextText + "\n</context>\n<text>\n" + segment + "\n</text>"
	msgs := []map[string]string{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": user},
	}
	out, err := t.chatComplete(ctx, msgs)
	if err != nil {
		return "", err
	}
	return sanitizeTranslationOutput(contextText, segment, out), nil
}

// TranslateWithHint adds an optional instruction hint to the system prompt used for translation.
func (t *Translator) TranslateWithHint(ctx context.Context, contextText, segment, hint string) (string, error) {
	msgs := polishedTranslatePromptWithHint(contextText, segment, hint)
	out, err := t.chatComplete(ctx, msgs)
	if err != nil {
		return "", err
	}
	return sanitizeTranslationOutput(contextText, segment, out), nil
}

// Summarize compresses backlog into an updated summary used by compressed-context mode.
func (t *Translator) Summarize(ctx context.Context, previousSummary, backlog string) (string, error) {
	msgs := summarizePrompt(previousSummary, backlog)
	return t.chatComplete(ctx, msgs)
}

// SummarizeWithSystemPrompt uses the provided system prompt verbatim.
func (t *Translator) SummarizeWithSystemPrompt(ctx context.Context, previousSummary, backlog, systemPrompt string) (string, error) {
	if strings.TrimSpace(systemPrompt) == "" {
		return t.Summarize(ctx, previousSummary, backlog)
	}
	user := "Previous summary (may be empty):\n" + previousSummary + "\n---\nNew backlog to compress:\n" + backlog + "\n---\nUpdate the summary."
	msgs := []map[string]string{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": user},
	}
	return t.chatComplete(ctx, msgs)
}

// SummarizeWithHint adds an optional instruction hint to the system prompt for summarization.
func (t *Translator) SummarizeWithHint(ctx context.Context, previousSummary, backlog, hint string) (string, error) {
	msgs := summarizePromptWithHint(previousSummary, backlog, hint)
	return t.chatComplete(ctx, msgs)
}

// sanitizeTranslationOutput removes any leaked context/source and common prefixes the model might add.
func sanitizeTranslationOutput(contextText, segment, out string) string {
	s := strings.TrimSpace(out)
	// Remove common prefixes (ASCII-only to avoid encoding issues)
	for _, p := range []string{"Translation:", "Translated:", "Result:", "Output:"} {
		if strings.HasPrefix(strings.ToLower(s), strings.ToLower(p)) {
			s = strings.TrimSpace(s[len(p):])
			break
		}
	}
	// Strip code fences or quotes
	s = strings.Trim(s, "`\"")
	s = strings.TrimSpace(s)

	// If the model echoed the whole context block, remove it.
	if contextText != "" {
		s = strings.ReplaceAll(s, contextText, "")
	}
	// Drop lines that only echo the source text or a context label. A line
	// equal to the source is removed only when other lines remain: numbers,
	// names, and same-script pairs legitimately translate to themselves, and
	// blanking those produced "empty result" errors for every language pair.
	echo := strings.TrimSpace(segment)
	lines := strings.Split(s, "\n")
	filtered := make([]string, 0, len(lines))
	ctxLabelRe := regexp.MustCompile(`(?i)^(context)\s*:`)
	for _, line := range lines {
		L := strings.TrimSpace(line)
		if L == "" {
			continue
		}
		if ctxLabelRe.MatchString(L) {
			continue
		}
		filtered = append(filtered, L)
	}
	if len(filtered) > 1 && echo != "" {
		withoutEcho := make([]string, 0, len(filtered))
		for _, L := range filtered {
			if L != echo {
				withoutEcho = append(withoutEcho, L)
			}
		}
		if len(withoutEcho) > 0 {
			filtered = withoutEcho
		}
	}
	s = strings.Join(filtered, "\n")

	// Final trim; collapse excessive whitespace
	s = strings.TrimSpace(s)
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	return s
}

// TranslateWithUsage returns sanitized translation plus usage if provided by server.
func (t *Translator) TranslateWithUsage(ctx context.Context, contextText, segment string) (string, *Usage, error) {
	if t.cfg.UseResponsesAPI {
		out, u, err := t.responsesComplete(ctx, polishedTranslatePrompt(contextText, segment)[0]["content"], contextText, segment, t.cfg.EnablePromptCache)
		if err == nil {
			return sanitizeTranslationOutput(contextText, segment, out), u, nil
		}
		if !shouldFallbackResponses(err) {
			return "", nil, err
		}
		if os.Getenv("OPENAI_DEBUG") == "1" {
			log.Printf("responses fallback translate err=%v", err)
		}
		// fallback to chat
	}
	msgs := polishedTranslatePrompt(contextText, segment)
	out, u, err := t.ChatWithUsage(ctx, msgs)
	if err != nil {
		return "", nil, err
	}
	return sanitizeTranslationOutput(contextText, segment, out), u, nil
}

// TranslateWithSystemPromptUsageRetry retries system-prompt translation for transient errors.
func (t *Translator) TranslateWithSystemPromptUsageRetry(ctx context.Context, contextText, segment, systemPrompt string, attempts int) (string, *Usage, error) {
	attempts = boundedRetryAttempts(attempts)
	var lastErr error
	for i := 0; i < attempts; i++ {
		out, u, err := t.TranslateWithSystemPromptUsage(ctx, contextText, segment, systemPrompt)
		if err == nil {
			return out, u, nil
		}
		lastErr = err
		if !shouldRetryErr(err) {
			break
		}
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(backoff(i)):
		}
	}
	return "", nil, lastErr
}

// TranslateWithSystemPromptUsage uses provided system prompt verbatim and returns usage.
func (t *Translator) TranslateWithSystemPromptUsage(ctx context.Context, contextText, segment, systemPrompt string) (string, *Usage, error) {
	if strings.TrimSpace(systemPrompt) == "" {
		return t.TranslateWithUsage(ctx, contextText, segment)
	}
	if t.cfg.UseResponsesAPI {
		out, u, err := t.responsesComplete(ctx, systemPrompt, contextText, segment, t.cfg.EnablePromptCache)
		if err == nil {
			return sanitizeTranslationOutput(contextText, segment, out), u, nil
		}
		if !shouldFallbackResponses(err) {
			return "", nil, err
		}
		if os.Getenv("OPENAI_DEBUG") == "1" {
			log.Printf("responses fallback translate(sys) err=%v", err)
		}
		// fallback to chat
	}
	user := "<context>\n" + contextText + "\n</context>\n<text>\n" + segment + "\n</text>"
	msgs := []map[string]string{{"role": "system", "content": systemPrompt}, {"role": "user", "content": user}}
	out, u, err := t.ChatWithUsage(ctx, msgs)
	if err != nil {
		return "", nil, err
	}
	return sanitizeTranslationOutput(contextText, segment, out), u, nil
}

// SummarizeWithSystemPromptUsage summarizes with a custom system prompt and returns usage.
func (t *Translator) SummarizeWithSystemPromptUsage(ctx context.Context, previousSummary, backlog, systemPrompt string) (string, *Usage, error) {
	if strings.TrimSpace(systemPrompt) == "" {
		s, err := t.Summarize(ctx, previousSummary, backlog)
		return s, nil, err
	}
	if t.cfg.UseResponsesAPI {
		out, u, err := t.responsesComplete(ctx, systemPrompt, previousSummary, backlog, t.cfg.EnablePromptCache)
		if err == nil {
			return out, u, nil
		}
		if !shouldFallbackResponses(err) {
			return "", nil, err
		}
		if os.Getenv("OPENAI_DEBUG") == "1" {
			log.Printf("responses fallback summarize err=%v", err)
		}
		// fallback to chat
	}
	user := "Previous summary (may be empty):\n" + previousSummary + "\n---\nNew backlog to compress:\n" + backlog + "\n---\nUpdate the summary."
	msgs := []map[string]string{{"role": "system", "content": systemPrompt}, {"role": "user", "content": user}}
	out, u, err := t.ChatWithUsage(ctx, msgs)
	return out, u, err
}

// SummarizeWithSystemPromptUsageRetry retries for transient upstream errors.
func (t *Translator) SummarizeWithSystemPromptUsageRetry(ctx context.Context, previousSummary, backlog, systemPrompt string, attempts int) (string, *Usage, error) {
	attempts = boundedRetryAttempts(attempts)
	var lastErr error
	for i := 0; i < attempts; i++ {
		out, u, err := t.SummarizeWithSystemPromptUsage(ctx, previousSummary, backlog, systemPrompt)
		if err == nil {
			return out, u, nil
		}
		lastErr = err
		if !shouldRetryErr(err) {
			break
		}
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(backoff(i)):
		}
	}
	return "", nil, lastErr
}
