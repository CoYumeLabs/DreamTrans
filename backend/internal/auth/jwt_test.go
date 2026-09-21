package auth

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestTokenGeneratorForKeyKeepsItsOwnAccountKey(t *testing.T) {
	training, err := NewTokenGeneratorForKey("training-key")
	if err != nil {
		t.Fatal(err)
	}
	clean, err := NewTokenGeneratorForKey("  clean-key ")
	if err != nil {
		t.Fatal(err)
	}
	if training.apiKey != "training-key" || clean.apiKey != "clean-key" {
		t.Fatalf("generators hold %q and %q", training.apiKey, clean.apiKey)
	}
	if _, err := NewTokenGeneratorForKey(" "); err == nil {
		t.Fatal("an empty account key was accepted")
	}
}

type tokenRoundTrip func(*http.Request) (*http.Response, error)

func (f tokenRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestTemporaryProviderTokenTTLAndFailureRedaction(t *testing.T) {
	generator, err := NewTokenGeneratorForKey("long-lived-fixture")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	generator.client = &http.Client{Transport: tokenRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		var body struct {
			TTL int `json:"ttl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TTL != 60 || r.URL.String() != "https://mp.speechmatics.com/v1/api_keys?type=rt" || r.Header.Get("Authorization") != "Bearer long-lived-fixture" {
			t.Fatal("provider mint request invalid")
		}
		if calls == 1 {
			return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(`{"key_value":"short-lived-fixture"}`))}, nil
		}
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("long-lived-fixture private details"))}, nil
	})}
	if token, err := generator.GenerateTokenTTLContext(t.Context(), 60); err != nil || token != "short-lived-fixture" {
		t.Fatal("temporary token not parsed", err)
	}
	if _, err := generator.GenerateTokenTTLContext(t.Context(), 60); err == nil || strings.Contains(err.Error(), "fixture") || strings.Contains(err.Error(), "private") {
		t.Fatal("provider response leaked", err)
	}
	if _, err := generator.GenerateTokenTTLContext(t.Context(), 59); err == nil || calls != 2 {
		t.Fatal("invalid TTL sent to provider")
	}
}
