package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dreamtrans/backend/internal/models"
)

func TestDerivedSourceOriginalIsOptInAndHashBound(t *testing.T) {
	admin, claims := consoleTestAdmin(t)
	t.Setenv("KNOWLEDGE_DATA_PATH", t.TempDir())
	project := &models.AIProject{TenantID: claims.TenantID, UserID: claims.UserID, Name: "PSY2041", ContextMode: "retrieval", MaxContextTokens: 16000}
	if err := admin.store.CreateAIProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	original := []byte("%PDF-1.4\n% Week 6 slides\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF\n")
	digest := sha256.Sum256(original)
	source := &models.KnowledgeSource{
		ProjectID: project.ID, TenantID: project.TenantID, UserID: project.UserID,
		SourceType: "lms", Name: "Week 6 · Week6_slides.pdf", MediaType: "application/pdf",
		SizeBytes: int64(len(original)), SHA256: hex.EncodeToString(digest[:]),
		Content: "## 第 1 页\nCorrelation is not causation.", Status: "ready",
		LMS: json.RawMessage(`{"host":"learning.monash.edu","cmid":777}`),
	}
	if _, err := admin.store.CreateLMSSourceWithChunks(t.Context(), source, makeKnowledgeChunks(source, source.Content)); err != nil {
		t.Fatal(err)
	}
	handler := &RAGHandler{store: admin.store}
	path := "/api/ai/projects/" + project.ID + "/sources/" + source.ID + "/original"
	call := func(method string, body []byte) *httptest.ResponseRecorder {
		res := httptest.NewRecorder()
		handler.handleKnowledgeSourceOriginal(res, httptest.NewRequest(method, path, bytes.NewReader(body)), project, source.ID)
		return res
	}

	// Text-only by default: nothing to download until the user opts in.
	if res := call(http.MethodGet, nil); res.Code != http.StatusNotFound {
		t.Fatalf("download before opt-in: %d", res.Code)
	}
	refs, err := admin.store.ListLMSSources(t.Context(), project.ID, project.UserID)
	if err != nil || len(refs) != 1 || refs[0].HasOriginal {
		t.Fatalf("refs before attach: %+v %v", refs, err)
	}

	// Same size, different bytes: an original cannot be swapped in.
	forged := bytes.Clone(original)
	forged[len(forged)-2] = 'X'
	if res := call(http.MethodPut, forged); res.Code != http.StatusBadRequest {
		t.Fatalf("forged original: %d %s", res.Code, res.Body.String())
	}
	if res := call(http.MethodPut, append(bytes.Clone(original), 'Z')); res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized original: %d %s", res.Code, res.Body.String())
	}

	res := call(http.MethodPut, original)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"has_original":true`) {
		t.Fatalf("attach: %d %s", res.Code, res.Body.String())
	}
	if res := call(http.MethodPut, original); res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"duplicate":true`) {
		t.Fatalf("second attach: %d %s", res.Code, res.Body.String())
	}
	refs, err = admin.store.ListLMSSources(t.Context(), project.ID, project.UserID)
	if err != nil || len(refs) != 1 || !refs[0].HasOriginal {
		t.Fatalf("refs after attach: %+v %v", refs, err)
	}

	download := call(http.MethodGet, nil)
	if download.Code != http.StatusOK || !bytes.Equal(download.Body.Bytes(), original) {
		t.Fatalf("download: %d %q", download.Code, download.Body.String())
	}
	if got := download.Header().Get("Content-Disposition"); got != "attachment; filename*=UTF-8''Week6_slides.pdf" {
		t.Fatalf("disposition: %q", got)
	}
	if download.Header().Get("Content-Type") != "application/pdf" || download.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers: %v", download.Header())
	}

	// Only synced materials take an original through this path.
	memory := &models.KnowledgeSource{TenantID: claims.TenantID, UserID: claims.UserID, ProjectID: project.ID, SourceType: "memory", Name: "Notes", MediaType: "text/plain", Status: "ready", Content: "notes"}
	if err := admin.store.CreateKnowledgeSource(t.Context(), memory); err != nil {
		t.Fatal(err)
	}
	other := httptest.NewRecorder()
	handler.handleKnowledgeSourceOriginal(other, httptest.NewRequest(http.MethodPut, path, bytes.NewReader([]byte("notes"))), project, memory.ID)
	if other.Code != http.StatusNotFound {
		t.Fatalf("memory source took an original: %d", other.Code)
	}
}

func TestOriginalDownloadNameKeepsTheStoredExtension(t *testing.T) {
	cases := map[string]string{
		"Week 6 · Week6_slides.pdf": "Week6_slides.pdf",
		"Week 6 · Tutorial 3":       "Tutorial 3.pdf",
		"a/b\\c.PDF":                "a_b_c.PDF",
		"   ":                       "material.pdf",
	}
	for name, want := range cases {
		if got := originalDownloadName(name, ".pdf"); got != want {
			t.Fatalf("%q → %q, want %q", name, got, want)
		}
	}
}
