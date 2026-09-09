package rag

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	openaiprovider "github.com/dreamtrans/backend/internal/adapters/openai_provider"
)

func TestNewServiceDoesNotCreateStoreWithoutProviderCredentials(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "rag.db")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("RAG_DB_PATH", databasePath)

	if service, err := NewServiceFromEnv(); err == nil || service != nil {
		t.Fatal("expected missing provider credentials to fail initialization")
	}
	if _, err := os.Stat(databasePath); !os.IsNotExist(err) {
		t.Fatalf("RAG database was created before provider validation: %v", err)
	}
}

func TestChatOverrideBaseRequiresRequestScopedKey(t *testing.T) {
	base := &openaiprovider.Config{
		BaseURL: "https://api.openai.com/v1",
		APIKey:  "server-secret",
		Model:   "server-model",
	}
	if _, err := applyChatOverrides(base, &ChatOverrides{APIBase: "https://example.com/v1"}); err == nil {
		t.Fatal("expected API base without request-scoped key to be rejected")
	}
	if base.BaseURL != "https://api.openai.com/v1" || base.APIKey != "server-secret" {
		t.Fatal("base configuration was mutated")
	}
}

func TestLiveSessionCacheIsBoundedAndExpires(t *testing.T) {
	service := &Service{
		live:            make(map[string]*liveBuffer),
		liveLastUsed:    make(map[string]time.Time),
		liveMaxEntries:  2,
		liveMaxSessions: 3,
		liveMaxAge:      time.Minute,
	}
	for i := 0; i < 10; i++ {
		service.recordLiveParagraph(fmt.Sprintf("session-%d", i), "speaker", "some text", "summary", 0, 1)
	}
	if got := len(service.live); got != service.liveMaxSessions {
		t.Fatalf("live session count = %d, want %d", got, service.liveMaxSessions)
	}

	service.liveMu.Lock()
	for sessionID := range service.liveLastUsed {
		service.liveLastUsed[sessionID] = time.Now().Add(-2 * service.liveMaxAge)
	}
	service.pruneLiveSessionsLocked(time.Now(), "fresh")
	service.liveMu.Unlock()
	if got := len(service.live); got != 0 {
		t.Fatalf("expired live session count = %d, want 0", got)
	}
}

func TestChatOverridesCloneProviderConfiguration(t *testing.T) {
	base := &openaiprovider.Config{
		BaseURL: "https://api.openai.com/v1",
		APIKey:  "server-secret",
		Model:   "server-model",
	}
	overridden, err := applyChatOverrides(base, &ChatOverrides{
		APIBase: "https://compatible.example/v1",
		APIKey:  "user-secret",
		Model:   "user-model",
	})
	if err != nil {
		t.Fatalf("apply overrides: %v", err)
	}
	if overridden.APIKey != "user-secret" || overridden.BaseURL != "https://compatible.example/v1" {
		t.Fatal("request overrides were not applied")
	}
	if base.APIKey != "server-secret" || base.BaseURL != "https://api.openai.com/v1" {
		t.Fatal("provider configuration was mutated across requests")
	}
}

func TestRequestScopedProviderDoesNotRequireServerDefault(t *testing.T) {
	service := &Service{}
	service.SetChatConfigProvider(func() (*openaiprovider.Config, error) {
		return nil, errors.New("default provider is not configured")
	})
	overridden, err := service.chatConfigWithOverrides(&ChatOverrides{
		APIBase: "https://compatible.example/v1", APIKey: "user-secret", Model: "user-model",
	})
	if err != nil || overridden.BaseURL != "https://compatible.example/v1" || overridden.APIKey != "user-secret" || overridden.Model != "user-model" || overridden.Provider != "" {
		t.Fatalf("request-scoped provider: %+v %v", overridden, err)
	}
	if _, err := service.chatConfigWithOverrides(&ChatOverrides{APIBase: "https://compatible.example/v1", Model: "user-model"}); err == nil {
		t.Fatal("custom endpoint without user credentials accepted")
	}
}
