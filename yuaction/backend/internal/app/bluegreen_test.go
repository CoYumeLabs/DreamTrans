package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CoYumeLabs/YuAction/backend/internal/storage"
	"github.com/dreamtrans/backend/pkg/deployment"
	"github.com/gorilla/websocket"
)

func TestBlueGreenSharedLoginRecordingAndRollback(t *testing.T) {
	for _, database := range []bool{false, true} {
		name := "memory"
		if database {
			name = "postgres"
		}
		t.Run(name, func(t *testing.T) {
			var stores [2]storage.Store
			if database {
				dsn := os.Getenv("TEST_DATABASE_URL")
				if dsn == "" {
					t.Skip("TEST_DATABASE_URL required for cross-process persistence")
				}
				for i := range stores {
					p, err := storage.OpenPostgres(context.Background(), dsn)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = p.Close() })
					stores[i] = p
				}
			} else {
				stores[0] = storage.NewMemory()
				stores[1] = stores[0]
			}
			f := newFakeYufolo(t)
			runtimes := [2]*deployment.Runtime{{}, {}}
			servers := [2]*httptest.Server{}
			for i := range servers {
				app := New(stores[i], Config{YufoloURL: f.server.URL, CreatorKey: strings.Repeat("k", 32), Deployment: runtimes[i]})
				servers[i] = httptest.NewServer(app.Handler())
				t.Cleanup(servers[i].Close)
				if err := runtimes[i].SetMode("active"); err != nil {
					t.Fatal(err)
				}
			}
			teacher := integrationClient(t, servers[0].URL, "teacher@example.com")
			room := integratedRoom(t, teacher, servers[0].URL)
			path := "/api/rooms/" + room.Room.Code
			httpJSON(t, teacher, servers[0].URL+path+"/transcription", "POST", map[string]string{"sourceLanguage": "cmn"}, 200)
			httpJSON(t, teacher, servers[1].URL+path+"/host", "GET", nil, 200)
			connect := func(index int) *websocket.Conn {
				u, _ := url.Parse(servers[index].URL + "/api")
				h := http.Header{}
				for _, cookie := range teacher.Jar.Cookies(u) {
					h.Add("Cookie", cookie.String())
				}
				ws, res, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(servers[index].URL, "http")+path+"/audio?sampleRate=48000&protocol=1&capture=test-recording-client", h)
				if err != nil {
					if res != nil {
						res.Body.Close()
					}
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ws.Close() })
				return ws
			}
			read := func(ws *websocket.Conn, want string) {
				t.Helper()
				_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
				var event map[string]any
				if err := ws.ReadJSON(&event); err != nil {
					t.Fatal(err)
				}
				if event["type"] != want {
					t.Fatalf("got %v, want %s", event, want)
				}
			}
			active := 0
			ws := connect(active)
			read(ws, "ready")
			for turn := 0; turn < 3; turn++ {
				if err := ws.WriteMessage(websocket.BinaryMessage, bytes.Repeat([]byte{byte(turn + 1), 0}, 4800)); err != nil {
					t.Fatal(err)
				}
				eventually(t, func() bool { return f.audioFrames.Load() == int32(turn+1) })
				var info struct {
					Active bool `json:"active"`
				}
				_ = json.Unmarshal(httpJSON(t, teacher, servers[1-active].URL+path+"/transcription", "GET", nil, 200), &info)
				if !info.Active {
					t.Fatal("remote live owner reported inactive")
				}
				duplicate, res, err := dialAudio(t, teacher, servers[1-active].URL, room.Room.Code)
				if err == nil {
					duplicate.Close()
					t.Fatal("second replica admitted duplicate paid recording")
				}
				if res == nil || res.StatusCode != 409 {
					t.Fatalf("duplicate response: %v %v", res, err)
				}
				res.Body.Close()
				if turn == 2 {
					break
				}
				if err := runtimes[active].SetMode("draining"); err != nil {
					t.Fatal(err)
				}
				if err := runtimes[active].RequestHandoff(); err != nil {
					t.Fatal(err)
				}
				read(ws, "handoff")
				if err := ws.WriteJSON(map[string]string{"type": "handoff"}); err != nil {
					t.Fatal(err)
				}
				read(ws, "migrated")
				ws.Close()
				// The released slot is reserved for the original microphone
				// while its buffered audio moves to the successor.
				intruder, res, err := dialAudio(t, teacher, servers[1-active].URL, room.Room.Code)
				if err == nil {
					intruder.Close()
					t.Fatal("another browser stole a pending handoff")
				}
				if res == nil || res.StatusCode != 409 {
					t.Fatalf("handoff reservation: %v %v", res, err)
				}
				res.Body.Close()
				old := active
				active = 1 - active
				if err := runtimes[active].SetMode("active"); err != nil {
					t.Fatal(err)
				}
				eventually(t, func() bool { return runtimes[old].Status().Drained })
				ws = connect(active)
				read(ws, "ready")
				if err := runtimes[old].SetMode("active"); err != nil {
					t.Fatal(err)
				}
			}
			if err := ws.WriteJSON(map[string]string{"type": "stop"}); err != nil {
				t.Fatal(err)
			}
			read(ws, "stopped")
			ws.Close()
			eventually(t, func() bool { return f.connections.Load() == 0 })
			if f.maxConnections.Load() != 1 || f.starts.Load() != 3 {
				t.Fatalf("paid streams overlapped or duplicated: max=%d starts=%d", f.maxConnections.Load(), f.starts.Load())
			}
			f.mu.Lock()
			archives := len(f.archives)
			f.mu.Unlock()
			if archives != 3 {
				t.Fatalf("finals lost/duplicated across handoffs: %d", archives)
			}
			httpJSON(t, teacher, servers[1].URL+"/api/auth/logout", "POST", nil, 200)
			httpJSON(t, teacher, servers[0].URL+path+"/host", "GET", nil, 401)
		})
	}
}

func TestBlueGreenSerializesRefreshAndJobs(t *testing.T) {
	store := storage.NewMemory()
	f := newFakeYufolo(t)
	f.shortToken = true
	a := New(store, Config{YufoloURL: f.server.URL, CreatorKey: "shared-key", Deployment: &deployment.Runtime{}})
	b := New(store, Config{YufoloURL: f.server.URL, CreatorKey: "shared-key", Deployment: &deployment.Runtime{}})
	first := httptest.NewServer(a.Handler())
	defer first.Close()
	second := httptest.NewServer(b.Handler())
	defer second.Close()
	client := integrationClient(t, first.URL, "teacher@example.com")
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			base := first.URL
			if i%2 == 1 {
				base = second.URL
			}
			res, err := client.Get(base + "/api/auth/me")
			if err != nil {
				t.Error(err)
				return
			}
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Errorf("me: %d", res.StatusCode)
			}
		}(i)
	}
	wg.Wait()
	if f.refreshes.Load() != 1 {
		t.Fatalf("same refresh token consumed %d times", f.refreshes.Load())
	}
	release, err := a.claimJob("room:answer:question")
	if err != nil {
		t.Fatal(err)
	}
	if !b.jobActive("room:answer:question") {
		t.Fatal("remote job reported interrupted")
	}
	if done, err := b.claimJob("room:answer:question"); err == nil {
		done()
		t.Fatal("duplicate job admitted")
	}
	if err := a.deploy.SetMode("draining"); err != nil {
		t.Fatal(err)
	}
	if a.deploy.Status().Drained {
		t.Fatal("deployment discarded background job")
	}
	release()
	if !a.deploy.Status().Drained {
		t.Fatal("finished task prevented drain")
	}
}

func TestRemoteRecordingOwnerDisappearanceUpdatesExistingSSE(t *testing.T) {
	store := storage.NewMemory()
	app := New(store, Config{Demo: true})
	room := createRoom(t, app.Handler(), "Shared owner state")
	if err := app.changeRecord(t.Context(), room.Room.Code, func(r *Room, _ *storage.Record) error { r.Transcription = "recording"; return nil }); err != nil {
		t.Fatal(err)
	}
	release, err := store.TryLock(t.Context(), "recording:"+room.Room.Code)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/rooms/"+room.Room.Code+"/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	scanner := bufio.NewScanner(res.Body)
	read := func() Room {
		t.Helper()
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "data: ") {
				var r Room
				if err := json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &r); err != nil {
					t.Fatal(err)
				}
				return r
			}
		}
		t.Fatal("SSE ended without ownership change")
		return Room{}
	}
	before := read()
	if before.Transcription != "recording" {
		t.Fatal(before.Transcription)
	}
	release()
	after := read()
	if after.Transcription != "interrupted" || after.Revision != before.Revision {
		t.Fatalf("owner disappearance was not reflected: %+v", after)
	}
}
