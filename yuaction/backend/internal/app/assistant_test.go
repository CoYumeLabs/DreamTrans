package app

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/CoYumeLabs/YuAction/backend/internal/storage"
	"github.com/gorilla/websocket"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func uploadFixture(t *testing.T, c *http.Client, url, key, name, content string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	p, err := w.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = p.Write([]byte(content))
	_ = w.Close()
	req, _ := http.NewRequest("POST", url, &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 202 {
		t.Fatalf("upload status %d", res.StatusCode)
	}
}
func waitDraft(t *testing.T, s *Server, code, id string) answerDraft {
	t.Helper()
	var a answerDraft
	eventually(t, func() bool {
		rec, err := s.store.GetPrivate(context.Background(), code, "answer", id)
		if err != nil {
			return false
		}
		_ = json.Unmarshal(rec.Data, &a)
		return a.Generic.Status != "processing" && a.Knowledge.Status != "processing"
	})
	return a
}
func TestYufoloAssistantOwnershipReuseAndInvalidation(t *testing.T) {
	f := newFakeYufolo(t)
	s := New(storage.NewMemory(), Config{YufoloURL: f.server.URL})
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	teacher := integrationClient(t, server.URL, "teacher@example.com")
	other := integrationClient(t, server.URL, "other@example.com")
	room := integratedRoom(t, teacher, server.URL)
	base := server.URL + "/api/rooms/" + room.Room.Code
	httpJSON(t, teacher, base+"/assistant", "GET", nil, 200)
	httpJSON(t, http.DefaultClient, base+"/assistant", "GET", nil, 401)
	httpJSON(t, other, base+"/assistant", "GET", nil, 403)
	uploadFixture(t, teacher, base+"/assistant/documents", "", "lecture.txt", "Private teaching materials")
	cfg, err := s.assistantSettings(context.Background(), room.Room.Code)
	if err != nil || !safeUpstreamID(cfg.ProjectID) {
		t.Fatalf("project missing: %+v %v", cfg, err)
	}
	cfg.AutoAnswer = true
	cfg.GenericPrompt = "Give a teaching example"
	cfg.KBPrompt = "Use supplied evidence only"
	httpJSON(t, teacher, base+"/assistant/settings", "PUT", cfg, 200)
	id := addQuestion(t, s.Handler(), room.Room.Code, "What is design thinking?")
	a := waitDraft(t, s, room.Room.Code, id)
	if a.Generic.Status != "ready" || a.Knowledge.Status != "ready" || len(a.Knowledge.Sources) != 1 {
		t.Fatalf("draft: %+v", a)
	}
	f.mu.Lock()
	asks := append([]map[string]any(nil), f.ai.asks...)
	f.mu.Unlock()
	if len(asks) != 2 {
		t.Fatalf("expected exactly generic and KB calls, got %d", len(asks))
	}
	for _, ask := range asks {
		if ask["stateless"] != true || ask["session_id"] != nil {
			t.Fatalf("unexpected history/session: %+v", ask)
		}
		conf := ask["config"].(map[string]any)
		if conf["model"] != nil || conf["api_key"] != nil {
			t.Fatal("must reuse Yufolo model/key")
		}
	}
	public := httpJSON(t, http.DefaultClient, base, "GET", nil, 200)
	if bytes.Contains(public, []byte("Private teaching")) || bytes.Contains(public, []byte("通用建议")) || bytes.Contains(public, []byte(cfg.ProjectID)) {
		t.Fatal("private workspace leaked")
	}
	httpJSON(t, teacher, base+"/assistant/index-preview", "POST", nil, 200)
	httpJSON(t, teacher, base+"/assistant/index", "POST", map[string]any{"requestId": "index-test", "confirmationToken": "fixture-confirm", "confirmed": false}, 400)
	httpJSON(t, teacher, base+"/assistant/index", "POST", map[string]any{"requestId": "index-test", "confirmationToken": "fixture-confirm", "confirmed": true}, 202)
	cfg.ProjectID = ""
	httpJSON(t, teacher, base+"/assistant/settings", "PUT", cfg, 200)
	a = waitDraft(t, s, room.Room.Code, id)
	if a.Knowledge.Status != "outdated" {
		t.Fatal("old project draft retained")
	}
	httpJSON(t, teacher, base+"/questions/"+id, "DELETE", nil, 200)
	if _, err = s.store.GetPrivate(context.Background(), room.Room.Code, "answer", id); err != storage.ErrNotFound {
		t.Fatal("deleted answer retained")
	}
}
func TestMicroFinalsAndParticipantTranslationCache(t *testing.T) {
	f := newFakeYufolo(t)
	f.microFinals = true
	s := New(storage.NewMemory(), Config{YufoloURL: f.server.URL})
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	teacher := integrationClient(t, server.URL, "teacher@example.com")
	room := integratedRoom(t, teacher, server.URL)
	base := server.URL + "/api/rooms/" + room.Room.Code
	httpJSON(t, teacher, base+"/assistant", "GET", nil, 200)
	httpJSON(t, teacher, base+"/transcription", "POST", map[string]string{"sourceLanguage": "en"}, 200)
	ws, _, err := dialAudio(t, teacher, server.URL, room.Room.Code)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	var ready map[string]any
	if err = ws.ReadJSON(&ready); err != nil {
		t.Fatal(err)
	}
	if err = ws.WriteMessage(websocket.BinaryMessage, make([]byte, 960)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		r := readRoom(t, s.Handler(), room.Room.Code)
		return len(r.Segments) == 1 && r.Segments[0].Text == "Hi. Hello. Could you hear me?"
	})
	httpJSON(t, http.DefaultClient, base+"/translations", "POST", map[string]string{"language": "cmn"}, 202)
	if f.translations.Load() != 0 {
		t.Fatal("translated growing caption")
	}
	if err = ws.WriteJSON(map[string]string{"type": "stop"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return readRoom(t, s.Handler(), room.Room.Code).Transcription != "recording" })
	for i := 0; i < 2; i++ {
		httpJSON(t, http.DefaultClient, base+"/translations", "POST", map[string]string{"language": "cmn"}, 202)
	}
	eventually(t, func() bool { return readRoom(t, s.Handler(), room.Room.Code).Segments[0].Translations["cmn"] != "" })
	httpJSON(t, http.DefaultClient, base+"/translations", "POST", map[string]string{"language": "cmn"}, 202)
	if f.translations.Load() != 1 {
		t.Fatal("duplicate viewer translation charge")
	}
	httpJSON(t, http.DefaultClient, base+"/translations", "POST", map[string]string{"language": "ja"}, 202)
	eventually(t, func() bool { return readRoom(t, s.Handler(), room.Room.Code).Segments[0].Translations["ja"] != "" })
	httpJSON(t, http.DefaultClient, base+"/translations", "POST", map[string]string{"language": "en"}, 202)
	eventually(t, func() bool {
		return readRoom(t, s.Handler(), room.Room.Code).Segments[0].Translations["en"] == "Hi. Hello. Could you hear me?"
	})
	if f.translations.Load() != 2 || f.starts.Load() != 1 {
		t.Fatal("same source translation or second audio stream charged")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.archives) != 1 {
		t.Fatalf("micro finals created %d archive cards", len(f.archives))
	}
}
func TestStandaloneRAGIsolationAndPrivateDrafts(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "embeddings") {
			items := in["input"].([]any)
			data := []any{}
			for i := range items {
				data = append(data, map[string]any{"index": i, "embedding": []float64{1, 0, 0}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			return
		}
		payload, _ := json.Marshal(in)
		if bytes.Contains(payload, []byte("OTHER_ROOM_SECRET")) {
			t.Error("cross-room document reached provider")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "Grounded answer"}}}})
	}))
	defer provider.Close()
	cfg := AIConfig{Key: "fake", ChatURL: provider.URL + "/chat/completions", Model: "test", EmbeddingKey: "fake", EmbeddingURL: provider.URL + "/embeddings", EmbeddingModel: "test-embedding"}
	s := New(storage.NewMemory(), Config{AI: cfg, Demo: true})
	h := s.Handler()
	server := httptest.NewServer(h)
	defer server.Close()
	a := createRoom(t, h, "A")
	b := createRoom(t, h, "B")
	base := "/api/rooms/" + a.Room.Code
	uploadFixture(t, http.DefaultClient, server.URL+base+"/assistant/documents", a.Key, "a.txt", "A_ROOM_FACT")
	uploadFixture(t, http.DefaultClient, server.URL+"/api/rooms/"+b.Room.Code+"/assistant/documents", b.Key, "b.txt", "OTHER_ROOM_SECRET")
	for _, code := range []string{a.Room.Code, b.Room.Code} {
		eventually(t, func() bool {
			rows, _ := s.store.ListPrivate(context.Background(), code, "document")
			var d knowledgeDocument
			if len(rows) == 0 {
				return false
			}
			_ = json.Unmarshal(rows[0].Data, &d)
			return d.Status == "ready"
		})
	}
	id := addQuestion(t, h, a.Room.Code, "What do the lecture notes say?")
	request(t, h, "GET", base+"/assistant", b.Key, nil, 403)
	request(t, h, "POST", base+"/assistant/answers/"+id, a.Key, nil, 202)
	draft := waitDraft(t, s, a.Room.Code, id)
	if draft.Generic.Status != "ready" || draft.Knowledge.Status != "ready" || len(draft.Knowledge.Sources) != 1 || draft.Knowledge.Sources[0].Name != "a.txt" {
		t.Fatalf("unexpected draft %+v", draft)
	}
	public := request(t, h, "GET", base, "", nil, 200)
	if bytes.Contains(public, []byte("Grounded answer")) || bytes.Contains(public, []byte("A_ROOM_FACT")) {
		t.Fatal("private AI leaked")
	}
	rows, _ := s.store.ListPrivate(context.Background(), a.Room.Code, "document")
	request(t, h, "DELETE", base+"/assistant/documents/"+rows[0].ID, a.Key, nil, 200)
	draft = waitDraft(t, s, a.Room.Code, id)
	if draft.Knowledge.Status != "outdated" {
		t.Fatal("deleted document retained as evidence")
	}
	s.cfg.AI.EmbeddingModel = "changed"
	part := s.knowledgeAnswer(context.Background(), b.Room.Code, "question", assistantSettings{TopK: 3})
	if part.Status != "empty" {
		t.Fatalf("stale model accepted: %+v", part)
	}
}
func TestSegmentBoundariesAndNormalization(t *testing.T) {
	if got := normalizeSegmentText("你  好 ， 世 界 ！"); got != "你好，世界！" {
		t.Fatal(got)
	}
	if got := joinSegmentText("안녕", "하세요"); got != "안녕하세요" {
		t.Fatal(got)
	}
	base := Segment{Text: "Could you", Source: "yufolo", Speaker: "S1", EndTime: 1, Parts: 1}
	next := Segment{Text: "hear me?", Source: "yufolo", Speaker: "S1", StartTime: 4, EndTime: 5}
	if !canMergeSegment(base, next) {
		t.Fatal("unfinished sentence split at short pause")
	}
	base.Text = "Hello."
	if canMergeSegment(base, next) {
		t.Fatal("finished sentence crossed pause")
	}
	base.Text = "Could you"
	next.Speaker = "S2"
	if canMergeSegment(base, next) {
		t.Fatal("speakers merged")
	}
	base.Text = strings.Repeat("字", 140)
	next.Speaker = "S1"
	next.StartTime = 1
	if canMergeSegment(base, next) {
		t.Fatal("oversized caption")
	}
	if !endsSentence("听到了吗？”") {
		t.Fatal("quoted sentence boundary")
	}
}

func TestDeletedQuestionCannotBeResurrectedByAI(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "late answer"}}}})
	}))
	defer provider.Close()
	defer close(release)
	s := New(storage.NewMemory(), Config{Demo: true, AI: AIConfig{Key: "fake", ChatURL: provider.URL, Model: "test"}})
	h := s.Handler()
	room := createRoom(t, h, "Delete while generating")
	id := addQuestion(t, h, room.Room.Code, "Question")
	base := "/api/rooms/" + room.Room.Code
	request(t, h, "POST", base+"/assistant/answers/"+id, room.Key, nil, 202)
	<-started
	request(t, h, "DELETE", base+"/questions/"+id, room.Key, nil, 200)
	eventually(t, func() bool { s.aiMu.Lock(); defer s.aiMu.Unlock(); return len(s.aiJobs) == 0 })
	if _, err := s.store.GetPrivate(context.Background(), room.Room.Code, "answer", id); err != storage.ErrNotFound {
		t.Fatal("worker recreated deleted answer")
	}
}

func TestParticipantTranslationPollingDoesNotThrottleQuestions(t *testing.T) {
	s := New(storage.NewMemory(), Config{Demo: true})
	h := s.Handler()
	room := createRoom(t, h, "Shared classroom NAT")
	for i := 0; i < 150; i++ {
		request(t, h, "POST", "/api/rooms/"+room.Room.Code+"/translations", "", map[string]string{"language": "en"}, 409)
	}
	addQuestion(t, h, room.Room.Code, "A participant can still ask a question")
}
