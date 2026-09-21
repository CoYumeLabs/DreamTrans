package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type TokenGenerator struct {
	apiKey string
	client *http.Client
}

// NewTokenGenerator mints temporary realtime keys from the default provider
// account (SM_API_KEY).
func NewTokenGenerator() (*TokenGenerator, error) {
	apiKey := os.Getenv("SM_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("SM_API_KEY environment variable not set")
	}
	return NewTokenGeneratorForKey(apiKey)
}

// NewTokenGeneratorForKey mints temporary realtime keys from one specific
// provider account, so callers can hold one generator per account.
func NewTokenGeneratorForKey(apiKey string) (*TokenGenerator, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("speechmatics api key is empty")
	}
	return &TokenGenerator{apiKey: apiKey}, nil
}

// GenerateToken calls Speechmatics API to get a temporary key
func (tg *TokenGenerator) GenerateToken() (string, error) {
	return tg.GenerateTokenContext(context.Background())
}

// GenerateTokenContext calls Speechmatics while observing caller cancellation.
func (tg *TokenGenerator) GenerateTokenContext(ctx context.Context) (string, error) {
	return tg.GenerateTokenTTLContext(ctx, 600)
}

// GenerateTokenTTLContext mints a provider credential for connection establishment.
// Its expiry does not enforce a running session's budget.
func (tg *TokenGenerator) GenerateTokenTTLContext(ctx context.Context, ttl int) (string, error) {
	if ttl < 60 || ttl > 86400 {
		return "", fmt.Errorf("invalid Speechmatics temporary key TTL")
	}
	requestBody := map[string]interface{}{
		"ttl": ttl,
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %w", err)
	}

	// Create request to Speechmatics temporary key endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://mp.speechmatics.com/v1/api_keys?type=rt", bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tg.apiKey)

	// Make request
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if tg.client != nil {
		client = tg.client
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to make request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read response body
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	// Check for success status (200 OK or 201 Created)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		// Do not propagate provider response bodies: upstream errors can contain
		// request details or credentials and callers commonly log this error.
		return "", fmt.Errorf("speechmatics API returned status %d", resp.StatusCode)
	}

	// Parse response
	var response struct {
		KeyValue string `json:"key_value"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	response.KeyValue = strings.TrimSpace(response.KeyValue)
	if response.KeyValue == "" || len(response.KeyValue) > 16<<10 {
		return "", fmt.Errorf("no key_value in response")
	}

	return response.KeyValue, nil
}
