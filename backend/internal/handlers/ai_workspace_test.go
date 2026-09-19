package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/dreamtrans/backend/internal/models"
)

func TestParseAIProjectRouteRequiresExactUUIDPaths(t *testing.T) {
	const (
		projectID = "11111111-1111-4111-8111-111111111111"
		sessionID = "22222222-2222-4222-8222-222222222222"
		sourceID  = "33333333-3333-4333-8333-333333333333"
	)
	tests := []struct {
		name       string
		path       string
		want       aiProjectRoute
		wantStatus int
		wantError  bool
	}{
		{
			name: "collection", path: "/api/ai/projects",
			wantStatus: http.StatusOK,
		},
		{
			name: "project", path: "/api/ai/projects/" + projectID,
			want:       aiProjectRoute{ProjectID: projectID},
			wantStatus: http.StatusOK,
		},
		{
			name:       "session collection",
			path:       "/api/ai/projects/" + projectID + "/sessions",
			want:       aiProjectRoute{ProjectID: projectID, Resource: "sessions"},
			wantStatus: http.StatusOK,
		},
		{
			name: "session item",
			path: "/api/ai/projects/" + projectID + "/sessions/" + sessionID,
			want: aiProjectRoute{
				ProjectID: projectID, Resource: "sessions", ResourceID: sessionID,
			},
			wantStatus: http.StatusOK,
		},
		{
			name:       "source collection",
			path:       "/api/ai/projects/" + projectID + "/sources",
			want:       aiProjectRoute{ProjectID: projectID, Resource: "sources"},
			wantStatus: http.StatusOK,
		},
		{
			name: "source item",
			path: "/api/ai/projects/" + projectID + "/sources/" + sourceID,
			want: aiProjectRoute{
				ProjectID: projectID, Resource: "sources", ResourceID: sourceID,
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "source retry",
			path: "/api/ai/projects/" + projectID + "/sources/" + sourceID + "/retry",
			want: aiProjectRoute{
				ProjectID: projectID, Resource: "sources",
				ResourceID: sourceID, Action: "retry",
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "invalid project UUID", path: "/api/ai/projects/not-a-uuid/sources",
			wantStatus: http.StatusBadRequest, wantError: true,
		},
		{
			name: "prefix lookalike", path: "/api/ai/projects-not-the-route",
			wantStatus: http.StatusNotFound, wantError: true,
		},
		{
			name:       "invalid session UUID",
			path:       "/api/ai/projects/" + projectID + "/sessions/not-a-uuid",
			wantStatus: http.StatusBadRequest, wantError: true,
		},
		{
			name:       "invalid source UUID",
			path:       "/api/ai/projects/" + projectID + "/sources/not-a-uuid",
			wantStatus: http.StatusBadRequest, wantError: true,
		},
		{
			name:       "unknown source action",
			path:       "/api/ai/projects/" + projectID + "/sources/" + sourceID + "/unknown",
			wantStatus: http.StatusNotFound, wantError: true,
		},
		{
			name:       "extra source path",
			path:       "/api/ai/projects/" + projectID + "/sources/" + sourceID + "/retry/extra",
			wantStatus: http.StatusNotFound, wantError: true,
		},
		{
			name:       "extra session path",
			path:       "/api/ai/projects/" + projectID + "/sessions/" + sessionID + "/extra",
			wantStatus: http.StatusNotFound, wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, status, err := parseAIProjectRoute(test.path)
			if status != test.wantStatus {
				t.Fatalf("status = %d, want %d", status, test.wantStatus)
			}
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError=%v", err, test.wantError)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("route = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestKnowledgeChunksAreBoundedAndIndexed(t *testing.T) {
	source := &models.KnowledgeSource{ID: "source-1", ProjectID: "project-1"}
	text := strings.Repeat("long knowledge paragraph ", 200)
	chunks := makeKnowledgeChunks(source, text)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for _, chunk := range chunks {
		if got := len([]rune(chunk.Content)); got > 1_400 {
			t.Fatalf("chunk has %d runes", got)
		}
		if len(chunk.Vector) != knowledgeVectorDimensions {
			t.Fatalf("vector dimensions = %d", len(chunk.Vector))
		}
	}
}

func TestOnlyNonemptyIndexPreviewsBecomeContextTargets(t *testing.T) {
	if hasIndexableAIChunks(nil) {
		t.Fatal("nil preview became an index target")
	}
	if hasIndexableAIChunks(&models.AIIndexPreview{
		IndexStatus: models.AIIndexStatusUnindexed,
	}) {
		t.Fatal("zero-chunk project became an index target")
	}
	if !hasIndexableAIChunks(&models.AIIndexPreview{
		ChunkCount:  1,
		IndexStatus: models.AIIndexStatusUnindexed,
	}) {
		t.Fatal("nonempty preview was omitted from index targets")
	}
}

func TestExtractWorksheetResolvesSharedStrings(t *testing.T) {
	xmlText := `<worksheet><sheetData><row>` +
		`<c t="s"><v>0</v></c><c><v>42</v></c>` +
		`</row></sheetData></worksheet>`
	text, err := extractWorksheetText(strings.NewReader(xmlText), []string{"Heading"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(text) != "Heading | 42" {
		t.Fatalf("worksheet text = %q", text)
	}
}

func TestRetrieveKnowledgeRanksMatchingMultilingualChunk(t *testing.T) {
	chunks := []models.KnowledgeChunk{
		{ID: "unrelated", Content: "Quarterly financial planning and budget.", Vector: localKnowledgeVector("Quarterly financial planning and budget.")},
		{ID: "matching", Content: "新加坡独有的语言文化值得自豪。", Vector: localKnowledgeVector("新加坡独有的语言文化值得自豪。")},
	}
	result := retrieveKnowledge("新加坡语言文化", chunks, 1)
	if len(result) != 1 || result[0].ID != "matching" {
		t.Fatalf("unexpected retrieval result: %#v", result)
	}
}

func TestKnowledgeMetadataPollingOmitsContentWithoutErasingIt(t *testing.T) {
	admin, claims := consoleTestAdmin(t)
	project := &models.AIProject{TenantID: claims.TenantID, UserID: claims.UserID, Name: "Metadata test", ContextMode: "retrieval", MaxContextTokens: 16000}
	if err := admin.store.CreateAIProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	source := &models.KnowledgeSource{TenantID: claims.TenantID, UserID: claims.UserID, ProjectID: project.ID, SourceType: "memory", Name: "Private material", MediaType: "text/plain", Status: "ready", Content: "DO_NOT_REPEAT_WHOLE_DOCUMENT"}
	if err := admin.store.CreateKnowledgeSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	handler := &RAGHandler{store: admin.store}
	for _, metadata := range []bool{true, false} {
		path := "/api/ai/projects/" + project.ID + "/sources"
		if metadata {
			path += "?metadata_only=true"
		}
		res := httptest.NewRecorder()
		handler.handleKnowledgeSources(res, httptest.NewRequest(http.MethodGet, path, nil), project)
		if res.Code != http.StatusOK {
			t.Fatalf("list: %d %s", res.Code, res.Body.String())
		}
		var result struct {
			Sources []models.KnowledgeSource `json:"sources"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Sources) != 1 || result.Sources[0].Name != source.Name {
			t.Fatalf("lost metadata: %+v", result)
		}
		if metadata && result.Sources[0].Content != "" {
			t.Fatal("poll returned full document")
		}
		if !metadata && result.Sources[0].Content != source.Content {
			t.Fatal("poll erased stored content")
		}
	}
}
