package openaiprovider

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestIsRetryableErrorSeparatesTransientAndPermanentFailures(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded,
		errors.New("openai api error: status 429"),
		errors.New("openai api error: status 503"),
		errors.New("connection reset by peer"),
		errors.New("unexpected EOF"),
	} {
		if !IsRetryableError(err) {
			t.Fatalf("transient error was terminal: %v", err)
		}
	}
	for _, err := range []error{
		context.Canceled,
		errors.New("openai api error: status 400"),
		errors.New("openai api error: status 401"),
		errors.New("no choices returned"),
	} {
		if IsRetryableError(err) {
			t.Fatalf("permanent error was retryable: %v", err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestParseUsageCanonicalSnakeCase(t *testing.T) {
	usage := parseUsageCanonical(
		[]byte(`{"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`),
		"model-a",
	)
	if usage == nil {
		t.Fatal("canonical usage was not parsed")
	}
	if usage.PromptTokens != 11 || usage.CompletionTokens != 7 || usage.TotalTokens != 18 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}

func TestParseUsageAltSnakeCase(t *testing.T) {
	raw := []byte(`{"model":"model-b","usage":{"input_tokens":13,"output_tokens":5,"total_tokens":18}}`)
	if usage := parseUsageCanonical(raw, ""); usage != nil {
		t.Fatalf("alternative usage was misclassified as canonical: %+v", usage)
	}
	usage := parseUsageAlt(
		raw,
		"",
	)
	if usage == nil {
		t.Fatal("alternative usage was not parsed")
	}
	if usage.PromptTokens != 13 || usage.CompletionTokens != 5 ||
		usage.TotalTokens != 18 || usage.Model != "model-b" {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}

func TestParseUsageLooseRejectsUnsafeCounts(t *testing.T) {
	for _, raw := range []string{
		`{"usage":{"prompt_tokens":-1,"completion_tokens":2}}`,
		`{"usage":{"prompt_tokens":1.5,"completion_tokens":2}}`,
		`{"usage":{"prompt_tokens":"1000000001","completion_tokens":2}}`,
	} {
		if usage := parseUsageLoose([]byte(raw), "model"); usage != nil {
			t.Fatalf("unsafe usage %s parsed as %+v", raw, usage)
		}
	}
}

func TestNewTranslatorSnapshotsAndNormalizesConfig(t *testing.T) {
	cfg := &Config{
		BaseURL:     "https://provider.example/v1",
		APIKey:      "key-a",
		Model:       "model-a",
		Temperature: math.NaN(),
		Timeout:     0,
	}
	translator := NewTranslator(cfg)
	cfg.APIKey = "key-b"
	cfg.Model = "model-b"

	if translator.cfg.APIKey != "key-a" || translator.cfg.Model != "model-a" {
		t.Fatalf("translator retained mutable caller config: %+v", translator.cfg)
	}
	if translator.cfg.Temperature != 0 {
		t.Fatalf("temperature = %v, want 0", translator.cfg.Temperature)
	}
	if translator.httpClient.Timeout != defaultProviderTimeout {
		t.Fatalf("timeout = %v, want %v", translator.httpClient.Timeout, defaultProviderTimeout)
	}
}

func TestChatWithUsageParsesUsageAndDoesNotLeakErrorBody(t *testing.T) {
	translator := NewTranslator(&Config{
		BaseURL:         "https://provider.example/v1",
		APIKey:          "secret-key",
		Model:           "model-a",
		Timeout:         time.Second,
		MaxOutputTokens: 321,
	})
	responseBody := `{"model":"model-a","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	translator.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://provider.example/v1/chat/completions" {
			t.Fatalf("unexpected endpoint %q", request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer secret-key" {
			t.Fatal("authorization header missing")
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"max_completion_tokens":321`) {
			t.Fatalf("chat output bound missing from request: %s", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(responseBody)),
		}, nil
	})}

	content, usage, err := translator.ChatWithUsage(
		context.Background(),
		[]map[string]string{{"role": "user", "content": "hello"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if content != "ok" || usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("content=%q usage=%+v", content, usage)
	}

	translator.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`private transcript echoed by provider`)),
		}, nil
	})}
	_, _, err = translator.ChatWithUsage(
		context.Background(),
		[]map[string]string{{"role": "user", "content": "hello"}},
	)
	if err == nil {
		t.Fatal("provider error unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), "private transcript") {
		t.Fatalf("provider response body leaked through error: %v", err)
	}
}

func TestChatWithUsageRejectsEmptyLengthLimitedOutputAndKeepsUsage(t *testing.T) {
	translator := NewTranslator(&Config{
		BaseURL:         "https://provider.example/v1",
		APIKey:          "key",
		Model:           "gpt-5.6-sol",
		Timeout:         time.Second,
		MaxOutputTokens: 2048,
		ReasoningEffort: "low",
	})
	translator.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"reasoning_effort":"low"`) {
			t.Fatalf("chat reasoning effort missing from request: %s", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"model":"gpt-5.6-sol","choices":[{"message":{"content":""},` +
					`"finish_reason":"length"}],"usage":{"prompt_tokens":42591,` +
					`"completion_tokens":2048,"total_tokens":44639}}`,
			)),
		}, nil
	})}

	content, usage, err := translator.ChatWithUsage(
		context.Background(),
		[]map[string]string{{"role": "user", "content": "summarize"}},
	)
	if err == nil || !strings.Contains(err.Error(), "max output tokens exhausted") {
		t.Fatalf("err = %v, want exhausted output error", err)
	}
	if content != "" || usage == nil || usage.PromptTokens != 42591 ||
		usage.CompletionTokens != 2048 {
		t.Fatalf("content/usage = %q/%+v", content, usage)
	}
}

func TestResponsesCompleteUsesInputTextAndParsesUsage(t *testing.T) {
	translator := NewTranslator(&Config{
		BaseURL:         "https://provider.example/v1",
		APIKey:          "key",
		Model:           "model-a",
		Timeout:         time.Second,
		MaxOutputTokens: 123,
		UseResponsesAPI: true,
	})
	translator.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), `"type":"text"`) ||
			!strings.Contains(string(body), `"type":"input_text"`) {
			t.Fatalf("invalid Responses API content type: %s", body)
		}
		if !strings.Contains(string(body), `"max_output_tokens":123`) {
			t.Fatalf("Responses API output bound missing from request: %s", body)
		}
		if !strings.Contains(string(body), `"store":false`) {
			t.Fatalf("Responses API request must disable provider storage: %s", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"model":"model-a","output":[{"content":[{"text":"ok"}]}],` +
					`"usage":{"input_tokens":4,"output_tokens":3,"total_tokens":7}}`,
			)),
		}, nil
	})}

	content, usage, err := translator.responsesComplete(
		context.Background(), "system", "context", "user", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if content != "ok" || usage == nil || usage.PromptTokens != 4 ||
		usage.CompletionTokens != 3 || usage.TotalTokens != 7 {
		t.Fatalf("content=%q usage=%+v", content, usage)
	}
}

func TestResponsesCompleteSendsNoneReasoningEffort(t *testing.T) {
	translator := NewTranslator(&Config{
		BaseURL:         "https://api.openai.com/v1",
		APIKey:          "key",
		Model:           "gpt-5.6-luna",
		Timeout:         time.Second,
		MaxOutputTokens: 2048,
		ReasoningEffort: "none",
		UseResponsesAPI: true,
	})
	translator.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"reasoning":{"effort":"none"}`) {
			t.Fatalf("expected explicit none reasoning effort, got: %s", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"model":"gpt-5.6-luna","output":[{"content":[{"text":"ok"}]}],` +
					`"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`,
			)),
		}, nil
	})}

	content, _, err := translator.responsesComplete(
		context.Background(), "system", "context", "user", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if content != "ok" {
		t.Fatalf("content = %q, want ok", content)
	}
}

func TestResponsesCompleteRejectsIncompleteOutputAndKeepsUsage(t *testing.T) {
	translator := NewTranslator(&Config{
		BaseURL:         "https://api.openai.com/v1",
		APIKey:          "key",
		Model:           "gpt-5.6-sol",
		Timeout:         time.Second,
		MaxOutputTokens: 8192,
		ReasoningEffort: "low",
		UseResponsesAPI: true,
	})
	translator.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"max_output_tokens":8192`) ||
			!strings.Contains(string(body), `"reasoning":{"effort":"low"}`) {
			t.Fatalf("artifact response controls missing from request: %s", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"model":"gpt-5.6-sol","status":"incomplete",` +
					`"incomplete_details":{"reason":"max_output_tokens"},"output":[],` +
					`"usage":{"input_tokens":42591,"output_tokens":8192,"total_tokens":50783}}`,
			)),
		}, nil
	})}

	content, usage, err := translator.responsesComplete(
		context.Background(), "system", "context", "summarize", false,
	)
	if err == nil || !strings.Contains(err.Error(), "max output tokens exhausted") {
		t.Fatalf("err = %v, want incomplete output error", err)
	}
	if content != "" || usage == nil || usage.PromptTokens != 42591 ||
		usage.CompletionTokens != 8192 {
		t.Fatalf("content/usage = %q/%+v", content, usage)
	}
}

func TestResponsesCompleteSkipsReasoningAndCollectsMessageOutput(t *testing.T) {
	translator := NewTranslator(&Config{
		BaseURL:         "https://api.openai.com/v1",
		APIKey:          "key",
		Model:           "gpt-5.6-sol",
		Timeout:         time.Second,
		MaxOutputTokens: 123,
		UseResponsesAPI: true,
	})
	translator.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"store":false`) {
			t.Fatalf("Responses API request must disable provider storage: %s", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"model":"gpt-5.6-sol","output":[` +
					`{"type":"reasoning","summary":[]},` +
					`{"type":"message","content":[` +
					`{"type":"output_text","text":"first"},` +
					`{"type":"output_text","text":"second"}]}],` +
					`"usage":{"input_tokens":4,"output_tokens":3,"total_tokens":7}}`,
			)),
		}, nil
	})}

	content, _, err := translator.responsesComplete(
		context.Background(), "system", "context", "user", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if content != "first\nsecond" {
		t.Fatalf("content = %q, want collected message output", content)
	}
}

func TestParseUsageCanonicalIncludesPromptCacheDetails(t *testing.T) {
	usage := parseUsageCanonical(
		[]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107,`+
			`"prompt_tokens_details":{"cached_tokens":60,"cache_write_tokens":20}}}`),
		"model-cache",
	)
	if usage == nil || usage.CachedTokens != 60 || usage.CacheWriteTokens != 20 {
		t.Fatalf("unexpected cache usage: %+v", usage)
	}
}

func TestResponsesCompleteUsesOfficialExplicitCacheFields(t *testing.T) {
	translator := NewTranslator(&Config{
		BaseURL:           "https://api.openai.com/v1",
		APIKey:            "key",
		Model:             "gpt-5.6-sol",
		Timeout:           time.Second,
		MaxOutputTokens:   123,
		UseResponsesAPI:   true,
		EnablePromptCache: true,
	})
	translator.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		payload := string(body)
		for _, expected := range []string{
			`"prompt_cache_key":"session-1"`,
			`"prompt_cache_options":{"mode":"explicit","ttl":"30m"}`,
			`"prompt_cache_breakpoint":{"mode":"explicit"}`,
		} {
			if !strings.Contains(payload, expected) {
				t.Fatalf("missing %s in %s", expected, payload)
			}
		}
		if strings.Contains(payload, `"type":"prompt_cache_breakpoint"`) {
			t.Fatalf("breakpoint must be attached to a cacheable content block: %s", payload)
		}
		if strings.Contains(payload, `"cache_control"`) {
			t.Fatalf("legacy cache_control leaked into Responses request: %s", payload)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"model":"gpt-5.6-sol","output":[{"content":[{"text":"ok"}]}],` +
					`"usage":{"input_tokens":100,"output_tokens":3,"total_tokens":103,` +
					`"input_tokens_details":{"cached_tokens":60,"cache_write_tokens":20}}}`,
			)),
		}, nil
	})}
	_, usage, err := translator.responsesComplete(
		context.Background(), "system", "stable context", "question", true, "session-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if usage == nil || usage.CachedTokens != 60 || usage.CacheWriteTokens != 20 {
		t.Fatalf("cache usage = %#v", usage)
	}
}

func TestResponsesCompleteKeepsGPT56CacheTTLAtThirtyMinutes(t *testing.T) {
	translator := NewTranslator(&Config{
		BaseURL:           "https://api.openai.com/v1",
		APIKey:            "key",
		Model:             "gpt-5.6-sol",
		Timeout:           time.Second,
		UseResponsesAPI:   true,
		EnablePromptCache: true,
		PromptCacheTTL:    86400,
	})
	translator.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		payload := string(body)
		if !strings.Contains(payload, `"prompt_cache_options":{"mode":"explicit","ttl":"30m"}`) {
			t.Fatalf("GPT-5.6 cache TTL must remain 30m: %s", payload)
		}
		if strings.Contains(payload, `"prompt_cache_retention"`) {
			t.Fatalf("legacy retention leaked into GPT-5.6 request: %s", payload)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"output":[{"content":[{"text":"ok"}]}]}`)),
		}, nil
	})}
	if _, _, err := translator.responsesComplete(
		context.Background(), "system", "stable context", "question", true, "session-1",
	); err != nil {
		t.Fatal(err)
	}
}

func TestResponsesCompleteUsesLegacyRetentionForOlderModels(t *testing.T) {
	translator := NewTranslator(&Config{
		BaseURL:           "https://api.openai.com/v1",
		APIKey:            "key",
		Model:             "gpt-5.5",
		Timeout:           time.Second,
		UseResponsesAPI:   true,
		EnablePromptCache: true,
		PromptCacheTTL:    86400,
	})
	translator.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		payload := string(body)
		if !strings.Contains(payload, `"prompt_cache_retention":"24h"`) {
			t.Fatalf("missing legacy 24h retention: %s", payload)
		}
		for _, invalid := range []string{`"prompt_cache_options"`, `"prompt_cache_breakpoint"`} {
			if strings.Contains(payload, invalid) {
				t.Fatalf("GPT-5.5 request contains unsupported %s: %s", invalid, payload)
			}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"output":[{"content":[{"text":"ok"}]}]}`)),
		}, nil
	})}
	if _, _, err := translator.responsesComplete(
		context.Background(), "system", "stable context", "question", true, "session-1",
	); err != nil {
		t.Fatal(err)
	}
}

func TestRespondDoesNotFallbackOnResponsesServerError(t *testing.T) {
	translator := NewTranslator(&Config{
		BaseURL: "https://api.openai.com/v1", APIKey: "key", Model: "gpt-5.6-sol",
		Timeout: time.Second, UseResponsesAPI: true,
	})
	calls := 0
	translator.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"temporary"}`)),
		}, nil
	})}
	_, _, err := translator.RespondWithUsage(
		context.Background(), "system", "context", "", "question", "session-1",
	)
	if err == nil || calls != 1 {
		t.Fatalf("err/calls = %v/%d, want Responses error without fallback", err, calls)
	}
}

func TestRespondFallsBackWhenResponsesRouteIsUnsupported(t *testing.T) {
	translator := NewTranslator(&Config{
		BaseURL: "https://provider.example/v1", APIKey: "key", Model: "model-a",
		Timeout: time.Second, UseResponsesAPI: true,
	})
	var paths []string
	translator.httpClient = &http.Client{Transport: roundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		if request.URL.Path == "/v1/responses" {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":"unsupported"}`)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"model":"model-a","choices":[{"message":{"content":"fallback ok"}}],` +
					`"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`,
			)),
		}, nil
	})}
	content, usage, err := translator.RespondWithUsage(
		context.Background(), "system", "context", "", "question", "session-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if content != "fallback ok" || usage == nil || usage.TotalTokens != 6 {
		t.Fatalf("content=%q usage=%#v", content, usage)
	}
	if strings.Join(paths, ",") != "/v1/responses,/v1/chat/completions" {
		t.Fatalf("provider call paths = %#v", paths)
	}
}

func TestProviderEndpointRejectsCredentialBearingURL(t *testing.T) {
	if _, err := providerEndpoint("https://user:password@example.com/v1", "/responses"); err == nil {
		t.Fatal("credential-bearing provider URL was accepted")
	}
}

func TestSanitizeTranslationOutputKeepsTranslationsEqualToSource(t *testing.T) {
	cases := []struct{ segment, out, want string }{
		// Numbers, names and same-script pairs legitimately translate to themselves.
		{"2024.", "2024.", "2024."},
		{"OK.", "OK.", "OK."},
		{"GPT-4.", "GPT-4。", "GPT-4。"},
		// A source substring inside a longer translation is not an echo.
		{"iPhone 15", "iPhone 15 发布了。", "iPhone 15 发布了。"},
		// A genuine echo (source line, then translation) drops the source line.
		{"Hello there.", "Hello there.\n你好。", "你好。"},
		{"Hello there.", "Translation: 你好。", "你好。"},
		{"Hello there.", "Context: irrelevant\n你好。", "你好。"},
		{"", "  spaced   out  ", "spaced out"},
	}
	for _, tc := range cases {
		if got := sanitizeTranslationOutput("ctx", tc.segment, tc.out); got != tc.want {
			t.Fatalf("sanitize(%q, %q) = %q, want %q", tc.segment, tc.out, got, tc.want)
		}
	}
}

func TestDescribeImageSendsInputImageAndFallsBackToChatParts(t *testing.T) {
	png := []byte("\x89PNG fake")
	for _, responses := range []bool{true, false} {
		var sawPath string
		translator := NewTranslator(&Config{
			BaseURL: "https://provider.example/v1", APIKey: "key", Model: "vision-a",
			Timeout: time.Second, MaxOutputTokens: 800, UseResponsesAPI: responses,
		})
		translator.httpClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			sawPath = request.URL.Path
			dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
			if !strings.Contains(string(body), dataURL) || !strings.Contains(string(body), "read this page") {
				t.Fatalf("image or instruction missing: %s", body)
			}
			reply := `{"model":"vision-a","choices":[{"message":{"role":"assistant","content":"A bar chart"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":1105,"completion_tokens":40,"total_tokens":1145}}`
			if responses {
				if !strings.Contains(string(body), `"type":"input_image"`) || !strings.Contains(string(body), `"detail":"high"`) {
					t.Fatalf("Responses image part missing: %s", body)
				}
				reply = `{"model":"vision-a","output":[{"content":[{"type":"output_text","text":"A bar chart"}]}],` +
					`"usage":{"input_tokens":1105,"output_tokens":40,"total_tokens":1145}}`
			} else if !strings.Contains(string(body), `"type":"image_url"`) {
				t.Fatalf("chat image part missing: %s", body)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(reply))}, nil
		})}
		content, usage, err := translator.DescribeImageWithUsage(context.Background(), "read this page", png)
		if err != nil {
			t.Fatal(err)
		}
		wantPath := "/v1/chat/completions"
		if responses {
			wantPath = "/v1/responses"
		}
		if sawPath != wantPath || content != "A bar chart" || usage == nil || usage.PromptTokens != 1105 || usage.CompletionTokens != 40 {
			t.Fatalf("responses=%v path=%s content=%q usage=%+v", responses, sawPath, content, usage)
		}
	}
	if _, _, err := NewTranslator(&Config{Model: "m"}).DescribeImageWithUsage(context.Background(), "x", nil); err == nil {
		t.Fatal("an empty image must be rejected before any request")
	}
}
