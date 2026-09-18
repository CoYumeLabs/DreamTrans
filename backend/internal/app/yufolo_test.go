package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CoYumeLabs/YuAction/backend/internal/storage"
	"github.com/gorilla/websocket"
)

// Contract fixture for DreamTrans's existing auth, session and Speechmatics proxy APIs.
// It cannot call a speech provider or incur real charges.
type fakeYufolo struct {
	server                         *httptest.Server
	mu                             sync.Mutex
	sessions                       map[string]string
	archives                       map[string]map[string]any
	sessionStatus                  map[string]string
	starts, refreshes, audioFrames atomic.Int32
	archiveFailure, disabled       atomic.Bool
	shortToken                     bool
}

func newFakeYufolo(t *testing.T) *fakeYufolo {
	t.Helper()
	f := &fakeYufolo{sessions: map[string]string{}, archives: map[string]map[string]any{}, sessionStatus: map[string]string{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}
func (f *fakeYufolo) handle(w http.ResponseWriter, r *http.Request) {
	owner := "teacher"
	if strings.Contains(r.Header.Get("Authorization"), "other") {
		owner = "other"
	}
	reply := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	if r.URL.Path == "/api/auth/login" || r.URL.Path == "/api/auth/refresh" {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if strings.Contains(in["email"], "other") || strings.Contains(in["refresh_token"], "other") {
			owner = "other"
		}
		if r.URL.Path == "/api/auth/login" && in["password"] != "test-password" {
			w.WriteHeader(401)
			reply(map[string]string{"error": "invalid credentials"})
			return
		}
		expires := 900
		if r.URL.Path == "/api/auth/refresh" {
			f.refreshes.Add(1)
		} else if f.shortToken {
			expires = 1
		}
		reply(map[string]any{"user": accountUser{owner, "Test teacher", owner + "@example.com"}, "access_token": "access-" + owner, "refresh_token": "refresh-" + owner, "expires_in": expires})
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer access-") {
		w.WriteHeader(401)
		reply(map[string]string{"error": "unauthorized"})
		return
	}
	if f.disabled.Load() {
		w.WriteHeader(401)
		reply(map[string]string{"error": "account disabled"})
		return
	}
	if r.URL.Path == "/api/user/profile" {
		reply(map[string]any{"user": accountUser{owner, "Test teacher", owner + "@example.com"}})
		return
	}
	if r.URL.Path == "/api/auth/logout" {
		reply(map[string]bool{"success": true})
		return
	}
	if r.URL.Path == "/api/sessions" {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		id := in["client_session_id"]
		if existing := f.sessions[id]; existing != "" && existing != owner {
			w.WriteHeader(403)
			return
		}
		f.sessions[id] = owner
		reply(map[string]string{"id": id})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/sessions/") {
		parts := strings.Split(r.URL.Path, "/")
		id := parts[3]
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.sessions[id] != owner {
			w.WriteHeader(403)
			return
		}
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		if strings.HasSuffix(r.URL.Path, "/transcripts") {
			if f.archiveFailure.Load() {
				w.WriteHeader(503)
				reply(map[string]string{"error": "archive unavailable"})
				return
			}
			f.archives[in["client_segment_id"].(string)] = in
		} else {
			f.sessionStatus[id], _ = in["status"].(string)
		}
		reply(map[string]string{"id": id})
		return
	}
	if r.URL.Path == "/ws/speechmatics" {
		f.mu.Lock()
		valid := f.sessions[r.URL.Query().Get("session_id")] == owner
		f.mu.Unlock()
		if !valid {
			w.WriteHeader(403)
			return
		}
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		var start map[string]any
		if c.ReadJSON(&start) != nil || start["message"] != "StartRecognition" {
			return
		}
		f.starts.Add(1)
		_ = c.WriteJSON(map[string]string{"message": "RecognitionStarted"})
		emitted := false
		frames := 0
		for {
			kind, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				frames++
				f.audioFrames.Add(1)
				if !emitted {
					emitted = true
					if start["translation_config"] != nil {
						_ = c.WriteJSON(map[string]any{"message": "AddTranslation", "results": []any{map[string]any{"content": "Hello everyone", "start_time": 0, "end_time": 1}}})
					}
					final := map[string]any{"message": "AddTranscript", "metadata": map[string]any{"transcript": "欢迎来到课堂", "start_time": 0, "end_time": 1}}
					_ = c.WriteJSON(final)
					_ = c.WriteJSON(final) // provider duplicate must not duplicate the shared caption
				}
			} else {
				var end struct {
					Message string `json:"message"`
					Frames  int    `json:"last_seq_no"`
				}
				_ = json.Unmarshal(data, &end)
				if end.Message != "EndOfStream" || end.Frames != frames {
					_ = c.WriteJSON(map[string]string{"message": "Error", "reason": "incorrect audio sequence"})
					return
				}
				_ = c.WriteJSON(map[string]string{"message": "EndOfTranscript"})
				return
			}
		}
	}
	w.WriteHeader(404)
}
func integrationClient(t *testing.T, base, email string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	data := httpJSON(t, c, base+"/api/auth/login", "POST", map[string]string{"email": email, "password": "test-password"}, 200)
	if bytes.Contains(data, []byte("access-")) || bytes.Contains(data, []byte("refresh-")) {
		t.Fatal("upstream tokens leaked")
	}
	return c
}
func httpJSON(t *testing.T, c *http.Client, address, method string, body any, want int) []byte {
	t.Helper()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, address, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ = io.ReadAll(res.Body)
	if res.StatusCode != want {
		t.Fatalf("%s %s: got %d want %d: %s", method, address, res.StatusCode, want, data)
	}
	return data
}
func integratedRoom(t *testing.T, c *http.Client, base string) created {
	t.Helper()
	var result created
	_ = json.Unmarshal(httpJSON(t, c, base+"/api/rooms", "POST", map[string]string{"title": "Integrated classroom", "kind": "classroom"}, 201), &result)
	return result
}
func dialAudio(t *testing.T, c *http.Client, base, code string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u, _ := url.Parse(base + "/api")
	h := http.Header{}
	for _, cookie := range c.Jar.Cookies(u) {
		h.Add("Cookie", cookie.String())
	}
	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/api/rooms/"+code+"/audio?sampleRate=48000", h)
}
func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestYufoloLoginOwnershipRefreshAndCSRF(t *testing.T) {
	f := newFakeYufolo(t)
	f.shortToken = true
	app := New(storage.NewMemory(), Config{YufoloURL: f.server.URL, CreatorKey: "old-creator"})
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	teacher := integrationClient(t, server.URL, "teacher@example.com")
	other := integrationClient(t, server.URL, "other@example.com")
	room := integratedRoom(t, teacher, server.URL)
	base := server.URL + "/api/rooms/" + room.Room.Code
	httpJSON(t, teacher, base+"/host", "GET", nil, 200)
	httpJSON(t, other, base+"/host", "GET", nil, 401)
	httpJSON(t, http.DefaultClient, base+"/host", "GET", nil, 401)
	httpJSON(t, http.DefaultClient, base, "GET", nil, 200)
	mine := httpJSON(t, teacher, server.URL+"/api/my/rooms", "GET", nil, 200)
	if !bytes.Contains(mine, []byte(room.Room.Code)) {
		t.Fatal("owner cannot recover rooms")
	}
	if bytes.Contains(httpJSON(t, other, server.URL+"/api/my/rooms", "GET", nil, 200), []byte(room.Room.Code)) {
		t.Fatal("other account can list room")
	}
	req, _ := http.NewRequest("POST", server.URL+"/api/auth/logout", nil)
	req.Header.Set("Origin", "https://evil.example")
	res, err := teacher.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("cross-origin logout allowed")
	}
	if f.refreshes.Load() != 2 {
		t.Fatalf("expected serialized token refresh per account, got %d", f.refreshes.Load())
	}
	f.disabled.Store(true)
	httpJSON(t, teacher, base+"/host", "GET", nil, 401)
	f.disabled.Store(false)
	httpJSON(t, teacher, server.URL+"/api/auth/logout", "POST", nil, 200)
	httpJSON(t, teacher, base+"/host", "GET", nil, 401)
}

func TestSharedTranscriptionSingleProducerArchiveAndStop(t *testing.T) {
	f := newFakeYufolo(t)
	app := New(storage.NewMemory(), Config{YufoloURL: f.server.URL})
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	teacher := integrationClient(t, server.URL, "teacher@example.com")
	other := integrationClient(t, server.URL, "other@example.com")
	room := integratedRoom(t, teacher, server.URL)
	base := server.URL + "/api/rooms/" + room.Room.Code
	prepare := map[string]string{"sourceLanguage": "zh", "targetLanguage": "en"}
	first := httpJSON(t, teacher, base+"/transcription", "POST", prepare, 200)
	second := httpJSON(t, teacher, base+"/transcription", "POST", prepare, 200)
	if !bytes.Equal(first, second) {
		t.Fatal("prepare is not idempotent")
	}
	httpJSON(t, other, base+"/transcription", "POST", prepare, 403)
	httpJSON(t, teacher, base+"/audio?sampleRate=48000", "GET", nil, 400)
	release, err := app.beginControl(room.Room.Code)
	if err != nil {
		t.Fatal(err)
	}
	blocked, blockedResponse, blockedErr := dialAudio(t, teacher, server.URL, room.Room.Code)
	release()
	if blocked != nil {
		blocked.Close()
	}
	if blockedErr == nil || blockedResponse == nil || blockedResponse.StatusCode != 409 {
		t.Fatal("recording accepted during lifecycle change")
	}
	blockedResponse.Body.Close()
	if f.starts.Load() != 0 {
		t.Fatal("blocked requests started a paid stream")
	}
	socket, _, err := dialAudio(t, teacher, server.URL, room.Room.Code)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	_ = socket.SetReadDeadline(time.Now().Add(5 * time.Second))
	var event map[string]string
	if err = socket.ReadJSON(&event); err != nil || event["type"] != "ready" {
		t.Fatalf("not ready: %v %v", event, err)
	}
	dupe, res, err := dialAudio(t, teacher, server.URL, room.Room.Code)
	if dupe != nil {
		dupe.Close()
	}
	if err == nil || res.StatusCode != 409 {
		t.Fatal("duplicate producer accepted")
	}
	res.Body.Close()
	denied, res, err := dialAudio(t, other, server.URL, room.Room.Code)
	if denied != nil {
		denied.Close()
	}
	if err == nil || res.StatusCode != 403 {
		t.Fatal("other user started recording")
	}
	res.Body.Close()
	if err = socket.WriteMessage(websocket.BinaryMessage, make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, v := range f.archives {
			if v["translation"] == "Hello everyone" {
				return true
			}
		}
		return false
	})
	var shared Room
	_ = json.Unmarshal(httpJSON(t, http.DefaultClient, base, "GET", nil, 200), &shared)
	if len(shared.Segments) != 1 || shared.Segments[0].Translation != "Hello everyone" {
		t.Fatalf("shared caption wrong: %+v", shared.Segments)
	}
	public := httpJSON(t, http.DefaultClient, base, "GET", nil, 200)
	for _, secret := range []string{"access-teacher", "refresh-teacher", "ownerId", "sessionId"} {
		if bytes.Contains(public, []byte(secret)) {
			t.Fatal("private link leaked")
		}
	}
	httpJSON(t, teacher, base, "PATCH", map[string]string{"status": "ended"}, 200)
	if err = socket.ReadJSON(&event); err != nil || event["type"] != "stopped" {
		t.Fatalf("did not stop: %v %v", event, err)
	}
	f.mu.Lock()
	for _, v := range f.sessionStatus {
		if v != "completed" {
			t.Errorf("upstream not ended: %s", v)
		}
	}
	f.mu.Unlock()
	if f.starts.Load() != 1 || f.audioFrames.Load() != 1 {
		t.Fatal("spectators/duplicate start created another stream")
	}
}

func TestArchiveFailureKeepsFinalsAndReplaysOnResume(t *testing.T) {
	f := newFakeYufolo(t)
	app := New(storage.NewMemory(), Config{YufoloURL: f.server.URL})
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	teacher := integrationClient(t, server.URL, "teacher@example.com")
	room := integratedRoom(t, teacher, server.URL)
	base := server.URL + "/api/rooms/" + room.Room.Code
	httpJSON(t, teacher, base+"/transcription", "POST", map[string]string{"sourceLanguage": "zh", "targetLanguage": ""}, 200)
	f.archiveFailure.Store(true)
	socket, _, err := dialAudio(t, teacher, server.URL, room.Room.Code)
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]string
	_ = socket.SetReadDeadline(time.Now().Add(5 * time.Second))
	_ = socket.ReadJSON(&event)
	_ = socket.WriteMessage(websocket.BinaryMessage, make([]byte, 4096))
	_ = socket.ReadJSON(&event)
	socket.Close()
	if event["type"] != "error" {
		t.Fatal("archive failure not surfaced")
	}
	eventually(t, func() bool { app.streamMu.Lock(); defer app.streamMu.Unlock(); return len(app.streams) == 0 })
	var shared Room
	_ = json.Unmarshal(httpJSON(t, http.DefaultClient, base, "GET", nil, 200), &shared)
	if len(shared.Segments) != 1 || shared.Segments[0].Archived {
		t.Fatal("unarchived final lost")
	}
	f.archiveFailure.Store(false)
	socket, _, err = dialAudio(t, teacher, server.URL, room.Room.Code)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	_ = socket.SetReadDeadline(time.Now().Add(5 * time.Second))
	_ = socket.ReadJSON(&event)
	f.mu.Lock()
	count := len(f.archives)
	f.mu.Unlock()
	if count != 1 {
		t.Fatal("resume did not replay durable final")
	}
	_ = socket.WriteJSON(map[string]string{"type": "stop"})
	_ = socket.ReadJSON(&event)
}

func TestLogoutClosesMicrophoneAndRevokesHostAccess(t *testing.T) {
	f := newFakeYufolo(t)
	app := New(storage.NewMemory(), Config{YufoloURL: f.server.URL})
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	teacher := integrationClient(t, server.URL, "teacher@example.com")
	room := integratedRoom(t, teacher, server.URL)
	base := server.URL + "/api/rooms/" + room.Room.Code
	httpJSON(t, teacher, base+"/transcription", "POST", map[string]string{"sourceLanguage": "zh"}, 200)
	socket, _, err := dialAudio(t, teacher, server.URL, room.Room.Code)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	_ = socket.SetReadDeadline(time.Now().Add(5 * time.Second))
	var event map[string]string
	if err = socket.ReadJSON(&event); err != nil || event["type"] != "ready" {
		t.Fatalf("not ready: %v %v", event, err)
	}
	httpJSON(t, teacher, server.URL+"/api/auth/logout", "POST", nil, 200)
	if err = socket.ReadJSON(&event); err == nil {
		t.Fatal("logout left microphone connected")
	}
	eventually(t, func() bool { app.streamMu.Lock(); defer app.streamMu.Unlock(); return len(app.streams) == 0 })
	httpJSON(t, teacher, base+"/host", "GET", nil, 401)
	var shared Room
	_ = json.Unmarshal(httpJSON(t, http.DefaultClient, base, "GET", nil, 200), &shared)
	if shared.Transcription != "interrupted" {
		t.Fatalf("logout left stale recording status: %s", shared.Transcription)
	}
}

func TestEndActivityRequiresPendingArchiveToSucceed(t *testing.T) {
	f := newFakeYufolo(t)
	app := New(storage.NewMemory(), Config{YufoloURL: f.server.URL})
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	teacher := integrationClient(t, server.URL, "teacher@example.com")
	room := integratedRoom(t, teacher, server.URL)
	base := server.URL + "/api/rooms/" + room.Room.Code
	httpJSON(t, teacher, base+"/transcription", "POST", map[string]string{"sourceLanguage": "zh"}, 200)
	if err := app.changeRecord(context.Background(), room.Room.Code, func(room *Room, _ *storage.Record) error {
		room.Segments = append(room.Segments, Segment{ID: "live-recovered-final", Text: "Recovered final", Source: "yufolo", Speaker: "Speaker", EndTime: 1})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.archiveFailure.Store(true)
	httpJSON(t, teacher, base, "PATCH", map[string]string{"status": "ended"}, 502)
	var shared Room
	_ = json.Unmarshal(httpJSON(t, http.DefaultClient, base, "GET", nil, 200), &shared)
	if shared.Status != "live" {
		t.Fatal("ended before pending final was archived")
	}
	f.archiveFailure.Store(false)
	httpJSON(t, teacher, base, "PATCH", map[string]string{"status": "ended"}, 200)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.archives) != 1 || f.starts.Load() != 0 {
		t.Fatal("ending must archive pending finals without starting recognition")
	}
}
