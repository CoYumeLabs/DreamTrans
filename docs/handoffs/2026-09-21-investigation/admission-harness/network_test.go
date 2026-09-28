package deployment

import (
 "fmt"
 "io"
 "net/http"
 "net/http/httptest"
 "os"
 "path/filepath"
 "strings"
 "sync"
 "syscall"
 "testing"
 "time"
 "github.com/gorilla/websocket"
)

func echoRuntime(t *testing.T, r *Runtime) (*httptest.Server, *websocket.Conn) {
 t.Helper()
 upgrader := websocket.Upgrader{}
 server := httptest.NewServer(r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
  if req.URL.Path != "/ws/fake" { _, _ = w.Write([]byte("ok\n")); return }
  peer, err := upgrader.Upgrade(w, req, nil)
  if err != nil { return }
  defer peer.Close()
  for { kind, body, err := peer.ReadMessage(); if err != nil { return }; if err := peer.WriteMessage(kind, body); err != nil { return } }
 })))
 t.Cleanup(server.Close)
 peer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL,"http")+"/ws/fake", nil)
 if err != nil { t.Fatal(err) }
 t.Cleanup(func() { _ = peer.Close() })
 return server, peer
}

func roundtripWS(peer *websocket.Conn) error {
 _ = peer.SetWriteDeadline(time.Now().Add(time.Second))
 _ = peer.SetReadDeadline(time.Now().Add(time.Second))
 if err := peer.WriteMessage(websocket.TextMessage, []byte("frame")); err != nil { return err }
 _, body, err := peer.ReadMessage()
 if err != nil { return err }
 if string(body) != "frame" { return fmt.Errorf("unexpected frame: %q", body) }
 return nil
}

func TestRealHTTPAndWSDuringInjectedModeFileStall(t *testing.T) {
 path := filepath.Join(t.TempDir(), "mode")
 r := &Runtime{mode:"active", path:path}
 server, peer := echoRuntime(t,r)
 if err := syscall.Mkfifo(path+".tmp", 0600); err != nil { t.Fatal(err) }
 modeDone := make(chan error,1)
 go func() { modeDone <- r.SetMode("active") }()
 until := time.Now().Add(time.Second)
 for r.mu.TryLock() { r.mu.Unlock(); if time.Now().After(until) { t.Fatal("mode write did not block") }; time.Sleep(time.Millisecond) }
 requestDone := make(chan time.Duration,1)
 requestErr := make(chan error,1)
 client := &http.Client{Timeout:2*time.Second}
 go func() {
  start := time.Now()
  res, err := client.Get(server.URL+"/api/system/settings")
  if err == nil { _, err = io.Copy(io.Discard,res.Body); res.Body.Close() }
  requestDone <- time.Since(start)
  requestErr <- err
 }()
 for _, suffix := range []string{"/healthz","/readyz"} {
  start := time.Now()
  res, err := client.Get(server.URL+suffix)
  if err != nil { t.Fatal(err) }
  _, _ = io.Copy(io.Discard,res.Body); res.Body.Close()
  t.Logf("real HTTP %s during injected stall: %s", suffix,time.Since(start))
 }
 start := time.Now()
 for range 20 { if err := roundtripWS(peer); err != nil { t.Fatal(err) } }
 t.Logf("20 already accepted WebSocket roundtrips during injected stall: %s",time.Since(start))
 select { case <-requestDone: t.Fatal("ordinary HTTP did not wait"); case <-time.After(100*time.Millisecond): }
 reader, err := os.Open(path+".tmp")
 if err != nil { t.Fatal(err) }
 _, _ = io.ReadAll(reader); reader.Close()
 if err := <-modeDone; err != nil { t.Fatal(err) }
 t.Logf("real business HTTP after releasing injected stall: %s", <-requestDone)
 if err := <-requestErr; err != nil { t.Fatal(err) }
}

func TestRealHTTPAndWSWithConcurrentNormalModeWritesAndStatus(t *testing.T) {
 r := &Runtime{mode:"active",path:filepath.Join(t.TempDir(),"mode")}
 server, peer := echoRuntime(t,r)
 done := make(chan struct{})
 errors := make(chan error,8)
 var worker sync.WaitGroup
 worker.Add(2)
 go func() { defer worker.Done(); for range 1000 { if err := r.SetMode("active"); err != nil { errors<-err; return } }; close(done) }()
 go func() { defer worker.Done(); for { select { case <-done:return; default: _=r.Status() } } }()
 var loads sync.WaitGroup
 var maxima sync.Mutex
 maxHTTP, maxWS := time.Duration(0),time.Duration(0)
 client := &http.Client{Timeout:2*time.Second}
 for range 4 {
  loads.Add(1)
  go func() { defer loads.Done(); for range 100 { start:=time.Now(); res,err:=client.Get(server.URL+"/api/system/settings"); if err != nil { errors<-err;return }; _,_=io.Copy(io.Discard,res.Body);res.Body.Close(); elapsed:=time.Since(start);maxima.Lock();if elapsed>maxHTTP{maxHTTP=elapsed};maxima.Unlock() } }()
 }
 for range 100 { start:=time.Now(); if err:=roundtripWS(peer);err!=nil{t.Fatal(err)};if elapsed:=time.Since(start);elapsed>maxWS{maxWS=elapsed} }
 loads.Wait();worker.Wait()
 select { case err:=<-errors:t.Fatal(err);default: }
 t.Logf("1000 normal mode writes + continuous Status + 400 real HTTP + 100 WS roundtrips: maxHTTP=%s maxWS=%s",maxHTTP,maxWS)
}
