package handlers

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dreamtrans/backend/internal/models"
)

func TestVerificationBaseURLRejectsUnsafeConfiguration(t *testing.T) {
	for _, raw := range []string{"", "/relative", "javascript:alert(1)", "https://", "https://user:pass@app.test", "https://app.test/path", "https://app.test?x=1", "https://app.test#fragment", "https://app.test/\""} {
		t.Setenv("APP_BASE_URL", raw)
		if _, err := verificationBaseURL(); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"https://app.test/", "http://localhost:8080"} {
		t.Setenv("APP_BASE_URL", raw)
		base, err := verificationBaseURL()
		if err != nil || base != strings.TrimSuffix(raw, "/") {
			t.Fatalf("base=%q err=%v", base, err)
		}
	}
}

func TestVerificationEmailRejectsHostFallbackBeforeDatabaseWrite(t *testing.T) {
	t.Setenv("APP_BASE_URL", "")
	// The mail function accepts no request, so it cannot fall back to Host.
	// A nil store proves rejection happens before any token is written.
	h := &AuthHandler{}
	if err := h.issueVerificationEmail(t.Context(), &models.User{}); err == nil {
		t.Fatal("accepted request host as mail origin")
	}
}

func TestCommandBudgetCancellationAndRelease(t *testing.T) {
	b := &commandBudget{slots: make(chan struct{}, 1)}
	if err := b.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := b.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error=%v", err)
	}
	b.release()
	if err := b.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	b.release()
	if err := b.acquire(ctx); err == nil {
		t.Fatal("cancelled work acquired a slot")
	}
}

func TestDerivedUploadBudgetBoundsUsers(t *testing.T) {
	t.Setenv("KNOWLEDGE_EXTRACT_WORKERS", "2")
	b := &derivedUploadBudget{users: make(map[string]bool)}
	if !b.acquire("a") || b.acquire("a") || !b.acquire("b") || b.acquire("c") {
		t.Fatal("global or per-user admission failed")
	}
	b.release("a")
	if !b.acquire("c") {
		t.Fatal("released slot was not reusable")
	}
}

func TestDerivedUploadRejectsBusyBeforeReadingBody(t *testing.T) {
	user := "busy-review-user"
	if !derivedUploads.acquire(user) {
		t.Fatal("cannot reserve test slot")
	}
	defer derivedUploads.release(user)
	r := httptest.NewRequest("POST", "/", strings.NewReader("invalid JSON"))
	w := httptest.NewRecorder()
	(&RAGHandler{}).handleDerivedSourceUpload(w, r, &models.AIProject{UserID: user})
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d headers=%v", w.Code, w.Header())
	}
}

func TestDerivedRenderStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := validDerivedRequest()
	text := renderDerivedText(ctx, &req, func(context.Context, []byte, []string) string { t.Fatal("OCR ran after cancellation"); return "" })
	if text != "" {
		t.Fatal("cancelled extraction returned partial material")
	}
}
