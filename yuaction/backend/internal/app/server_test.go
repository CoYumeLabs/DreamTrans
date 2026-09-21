package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CoYumeLabs/YuAction/backend/internal/storage"
)

type created struct {
	Room Room   `json:"room"`
	Key  string `json:"hostKey"`
}

func request(t *testing.T, h http.Handler, method, path, key string, body any, want int) []byte {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s: got %d, want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	return w.Body.Bytes()
}
func createRoom(t *testing.T, h http.Handler, title string) created {
	t.Helper()
	var result created
	data := request(t, h, "POST", "/api/rooms", "", map[string]string{"title": title, "kind": "classroom"}, 201)
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Key == "" || result.Room.Code == "" {
		t.Fatal("missing room credential")
	}
	return result
}
func readRoom(t *testing.T, h http.Handler, code string) Room {
	t.Helper()
	var room Room
	data := request(t, h, "GET", "/api/rooms/"+code, "", nil, 200)
	if err := json.Unmarshal(data, &room); err != nil {
		t.Fatal(err)
	}
	return room
}
func addQuestion(t *testing.T, h http.Handler, code, content string) string {
	t.Helper()
	var room Room
	data := request(t, h, "POST", "/api/rooms/"+code+"/questions", "", map[string]string{"content": content}, 201)
	if err := json.Unmarshal(data, &room); err != nil {
		t.Fatal(err)
	}
	return room.Questions[len(room.Questions)-1].ID
}

func TestHostAuthorizationAndRoomIsolation(t *testing.T) {
	h := New(storage.NewMemory(), Config{Demo: true}).Handler()
	a := createRoom(t, h, "课堂 A")
	b := createRoom(t, h, "课堂 B")
	qa := addQuestion(t, h, a.Room.Code, "问题 A")
	qb := addQuestion(t, h, b.Room.Code, "问题 B")
	pathA := "/api/rooms/" + a.Room.Code + "/questions/" + qa
	state := map[string]string{"status": "showing"}
	request(t, h, "PATCH", pathA, "", state, 401)
	request(t, h, "PATCH", pathA, b.Key, state, 401)
	request(t, h, "PATCH", pathA, a.Key, state, 200)
	request(t, h, "PATCH", "/api/rooms/"+b.Room.Code+"/questions/"+qb, b.Key, state, 200)
	if readRoom(t, h, a.Room.Code).Questions[0].Status != "showing" {
		t.Fatal("room B changed room A display")
	}
	qa2 := addQuestion(t, h, a.Room.Code, "第二个问题")
	request(t, h, "PATCH", "/api/rooms/"+a.Room.Code+"/questions/"+qa2, a.Key, state, 200)
	r := readRoom(t, h, a.Room.Code)
	if r.Questions[0].Status != "pending" || r.Questions[1].Status != "showing" {
		t.Fatal("only one question should be showing per room")
	}
	request(t, h, "PATCH", "/api/rooms/"+a.Room.Code+"/questions/"+qb, a.Key, state, 404)
	public := request(t, h, "GET", "/api/rooms/"+a.Room.Code, "", nil, 200)
	for _, secret := range []string{a.Key, digest(a.Key), "hostKey", "host_hash"} {
		if bytes.Contains(public, []byte(secret)) {
			t.Fatal("public response leaked host credential")
		}
	}
}

func TestRoomLifecycleAndInputValidation(t *testing.T) {
	h := New(storage.NewMemory(), Config{Demo: true}).Handler()
	a := createRoom(t, h, "演讲")
	base := "/api/rooms/" + a.Room.Code
	request(t, h, "POST", base+"/questions", "", map[string]string{"content": "  "}, 400)
	request(t, h, "POST", base+"/questions", "", map[string]string{"content": strings.Repeat("问", 1001)}, 400)
	request(t, h, "POST", base+"/questions", "", map[string]string{"content": "问题", "segmentId": "another-room"}, 400)
	request(t, h, "PATCH", base, a.Key, map[string]string{"status": "ended"}, 200)
	request(t, h, "POST", base+"/questions", "", map[string]string{"content": "新问题"}, 409)
	request(t, h, "POST", base+"/demo-segments", a.Key, map[string]string{"id": "s1", "text": "字幕"}, 409)
	request(t, h, "PATCH", base, a.Key, map[string]string{"status": "live"}, 200)
	addQuestion(t, h, a.Room.Code, "现在可以提问")
}

func TestIngestAuthenticationIdempotencyAndQuote(t *testing.T) {
	h := New(storage.NewMemory(), Config{Demo: true, IngestKey: "test-ingest-key"}).Handler()
	a := createRoom(t, h, "字幕课堂")
	path := "/api/internal/rooms/" + a.Room.Code + "/segments"
	seg := map[string]string{"id": "provider-001", "text": "今天讨论细胞", "translation": "Today we discuss cells."}
	request(t, h, "POST", path, "", seg, 401)
	request(t, h, "POST", path, a.Key, seg, 401)
	request(t, h, "POST", path, "test-ingest-key", seg, 200)
	request(t, h, "POST", path, "test-ingest-key", seg, 200)
	room := readRoom(t, h, a.Room.Code)
	if len(room.Segments) != 1 || room.Segments[0].Source != "yufolo" {
		t.Fatal("provider retry duplicated segment")
	}
	request(t, h, "POST", "/api/rooms/"+a.Room.Code+"/questions", "", map[string]string{"content": "这是什么细胞？", "segmentId": "provider-001"}, 201)
	if readRoom(t, h, a.Room.Code).Questions[0].QuotedText != seg["text"] {
		t.Fatal("quoted context was not preserved")
	}
	seg["text"] = "different payload"
	request(t, h, "POST", path, "test-ingest-key", seg, 409)
}

func TestNonDemoRequiresCreatorKeyAndHidesDemo(t *testing.T) {
	h := New(storage.NewMemory(), Config{CreatorKey: "creator-key"}).Handler()
	input := map[string]string{"title": "真实模式", "kind": "talk"}
	request(t, h, "POST", "/api/rooms", "", input, 401)
	var a created
	_ = json.Unmarshal(request(t, h, "POST", "/api/rooms", "creator-key", input, 201), &a)
	request(t, h, "POST", "/api/rooms/"+a.Room.Code+"/demo-segments", a.Key, map[string]string{"id": "1", "text": "演示"}, 404)
	request(t, h, "POST", "/api/internal/rooms/"+a.Room.Code+"/segments", "", map[string]string{"id": "1", "text": "演示"}, 401)
}

func TestConcurrentQuestionsDoNotLoseAcceptedWrites(t *testing.T) {
	h := New(storage.NewMemory(), Config{Demo: true}).Handler()
	a := createRoom(t, h, "并发课堂")
	const n = 24
	statuses := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := strings.NewReader(fmt.Sprintf(`{"content":"question %d"}`, i))
			r := httptest.NewRequest("POST", "/api/rooms/"+a.Room.Code+"/questions", body)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			statuses <- w.Code
		}(i)
	}
	wg.Wait()
	close(statuses)
	success := 0
	for status := range statuses {
		if status == 201 {
			success++
		} else if status != 409 {
			t.Fatalf("unexpected concurrent status: %d", status)
		}
	}
	if got := len(readRoom(t, h, a.Room.Code).Questions); got != success || success == 0 {
		t.Fatalf("got %d questions for %d successful writes", got, success)
	}
}

func TestSSEBroadcastAndReconnectSnapshot(t *testing.T) {
	h := New(storage.NewMemory(), Config{Demo: true}).Handler()
	a := createRoom(t, h, "实时课堂")
	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connect := func() (*http.Response, *bufio.Reader) {
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/rooms/"+a.Room.Code+"/events", nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 200 {
			t.Fatal(res.Status)
		}
		return res, bufio.NewReader(res.Body)
	}
	readEvent := func(reader *bufio.Reader) Room {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(line, "data: ") {
				var room Room
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &room); err != nil {
					t.Fatal(err)
				}
				return room
			}
		}
	}
	one, r1 := connect()
	defer one.Body.Close()
	two, r2 := connect()
	defer two.Body.Close()
	if readEvent(r1).Revision != 1 || readEvent(r2).Revision != 1 {
		t.Fatal("missing initial snapshot")
	}
	addQuestion(t, h, a.Room.Code, "两个听众应该同时看到")
	if len(readEvent(r1).Questions) != 1 || len(readEvent(r2).Questions) != 1 {
		t.Fatal("broadcast missing")
	}
	one.Body.Close()
	addQuestion(t, h, a.Room.Code, "离线期间的新问题")
	reconnected, r3 := connect()
	defer reconnected.Body.Close()
	if len(readEvent(r3).Questions) != 2 {
		t.Fatal("reconnect did not recover latest state")
	}
}
