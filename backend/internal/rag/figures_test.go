package rag

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openaiprovider "github.com/dreamtrans/backend/internal/adapters/openai_provider"
	"github.com/dreamtrans/backend/internal/aiproviders"
)

func TestDescribeFigureMetersTheCallAndRecognisesNothingToAdd(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		reply      string
		wantText   string
		wantErr    error
		wantSettle bool
	}{
		{"described", http.StatusOK, "Bar chart: test-retest r rises from .62 to .81 across weeks.", "Bar chart: test-retest r rises from .62 to .81 across weeks.", nil, true},
		{"nothing beyond the text", http.StatusOK, "NONE", "", ErrFigureNothingToAdd, true},
		{"provider down", http.StatusInternalServerError, "", "", ErrProviderRequest, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), "data:image/png;base64,") || !strings.Contains(string(body), "Reliability over time") {
					t.Errorf("request lacks the image or the page text: %.200s", body)
				}
				if tc.status != http.StatusOK {
					w.WriteHeader(tc.status)
					return
				}
				_, _ = io.WriteString(w, `{"model":"org/vision","choices":[{"message":{"content":"`+tc.reply+`"},"finish_reason":"stop"}],`+
					`"usage":{"prompt_tokens":1105,"completion_tokens":42,"total_tokens":1147}}`)
			}))
			defer server.Close()
			service, _ := newIngestTestService(t)
			service.SetEmbedEnabled(false)
			t.Setenv("OPENAI_API_KEY", "")
			t.Setenv("AI_PROVIDERS", "cerebras="+server.URL)
			t.Setenv("AI_PROVIDER_KEYS", "cerebras=test")
			t.Setenv("AI_PROVIDER_OPTIONS", "")
			t.Setenv("AI_EMBEDDING_PROVIDER", "cerebras")
			service.SetChatConfigProvider(func() (*openaiprovider.Config, error) { return aiproviders.ConfigFor("") })
			meter := &meterTestMeter{}
			ctx := WithProviderUsageMeter(t.Context(), meter)

			text, err := service.DescribeFigure(ctx, "cerebras::org/vision", []byte("\x89PNG render"), "Reliability over time")
			if text != tc.wantText || (tc.wantErr == nil) != (err == nil) || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("text=%q err=%v", text, err)
			}
			calls := meter.snapshot()
			if len(calls) != 1 || calls[0].reserved.Action != "chat" || calls[0].reserved.Model != "cerebras::org/vision" {
				t.Fatalf("reservation: %+v", calls)
			}
			if calls[0].reserved.InputTokens < figureReservedImageTokens || calls[0].reserved.OutputTokens != figureMaxOutputTokens {
				t.Fatalf("reservation must cover an image: %+v", calls[0].reserved)
			}
			if tc.wantSettle {
				if !calls[0].settled || calls[0].actual.InputTokens != 1105 || calls[0].actual.OutputTokens != 42 {
					t.Fatalf("settlement: %+v", calls[0])
				}
			} else if !calls[0].refunded || calls[0].settled {
				t.Fatalf("a failed call without usage must be refunded: %+v", calls[0])
			}
		})
	}
}
