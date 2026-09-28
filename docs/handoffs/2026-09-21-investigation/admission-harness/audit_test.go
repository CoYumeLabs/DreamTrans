package deployment

import (
 "io"
 "net/http"
 "net/http/httptest"
 "os"
 "path/filepath"
 "syscall"
 "testing"
 "time"
)

func TestBlockedHandoffSendDoesNotHoldAdmissionMutex(t *testing.T) {
 r := &Runtime{mode: "draining"}
 sending, release := make(chan struct{}), make(chan struct{})
 stop := r.NotifyHandoff(func(any) error { close(sending); <-release; return nil })
 defer stop()
 defer close(release)
 if err := r.RequestHandoff(); err != nil { t.Fatal(err) }
 <-sending
 done := make(chan struct{})
 go func() {
  _ = r.Status()
  if err := r.SetMode("active"); err != nil { panic(err) }
  r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/system/settings", nil))
  close(done)
 }()
 select {
 case <-done: t.Log("Status, SetMode(active), and ordinary HTTP completed while handoff send was blocked")
 case <-time.After(time.Second): t.Fatal("handoff send blocks admission")
 }
}

func TestInjectedModePersistenceStallBlocksHTTPButNotAcceptedStream(t *testing.T) {
 // FIFO replaces the mode temporary file only in this isolated experiment.
 // This simulates a blocked filesystem call; it is not production evidence.
 p := filepath.Join(t.TempDir(), "mode")
 if err := syscall.Mkfifo(p+".tmp", 0600); err != nil { t.Fatal(err) }
 r := &Runtime{mode: "active", path: p}
 admitted, finish, streamDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
 frames := make(chan struct{}, 1)
 handler := r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
  if req.URL.Path == "/ws/fake" {
   close(admitted)
   ticker := time.NewTicker(time.Millisecond)
   defer ticker.Stop()
   for { select { case <-finish: return; case <-ticker.C: select { case frames <- struct{}{}: default: } } }
  }
  w.WriteHeader(200)
 }))
 wsReq := httptest.NewRequest("GET", "/ws/fake", nil)
 wsReq.Header.Set("Upgrade", "websocket")
 go func() { handler.ServeHTTP(httptest.NewRecorder(), wsReq); close(streamDone) }()
 <-admitted
 modeDone := make(chan error, 1)
 go func() { modeDone <- r.SetMode("active") }()
 deadline := time.Now().Add(time.Second)
 for r.mu.TryLock() { r.mu.Unlock(); if time.Now().After(deadline) { t.Fatal("mode writer did not start") }; time.Sleep(time.Millisecond) }
 requestDone := make(chan time.Duration, 1)
 go func() { start := time.Now(); handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/system/settings", nil)); requestDone <- time.Since(start) }()
 for _, path := range []string{"/healthz", "/readyz"} {
  start := time.Now()
  response := httptest.NewRecorder()
  handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
  if response.Code != 200 { t.Fatal(response.Code) }
  t.Logf("%s bypass completed in %s during injected mode-file stall", path, time.Since(start))
 }
 select { case <-frames: case <-time.After(time.Second): t.Fatal("accepted stream stopped") }
 select { case <-requestDone: t.Fatal("ordinary HTTP bypassed blocked admission"); case <-time.After(100*time.Millisecond): }
 reader, err := os.Open(p+".tmp")
 if err != nil { t.Fatal(err) }
 _, err = io.ReadAll(reader)
 if err != nil { t.Fatal(err) }
 _ = reader.Close()
 if err := <-modeDone; err != nil { t.Fatal(err) }
 t.Logf("ordinary HTTP completed in %s after mode-file I/O was unblocked", <-requestDone)
 close(finish)
 <-streamDone
}
