package deployment

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
)

func TestDrainPreservesAcceptedWorkAndRejectsNewWork(t *testing.T) {
	r := &Runtime{path: filepath.Join(t.TempDir(), "mode")}
	if err := r.SetMode("active"); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{})
	finish := make(chan struct{})
	handler := r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { close(accepted); <-finish; w.WriteHeader(200) }))
	req := httptest.NewRequest("GET", "/ws/speechmatics", nil)
	req.Header.Set("Upgrade", "websocket")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); handler.ServeHTTP(httptest.NewRecorder(), req) }()
	<-accepted
	task, ok := r.BeginTask()
	if !ok {
		t.Fatal("active task rejected")
	}
	if err := r.SetMode("draining"); err != nil {
		t.Fatal(err)
	}
	if _, ok = r.BeginTask(); ok {
		t.Fatal("new task accepted while draining")
	}
	snapshot := r.Status()
	if snapshot.Drained || snapshot.WebSockets != 1 || snapshot.Tasks != 1 {
		t.Fatalf("bad status: %+v", snapshot)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != 503 {
		t.Fatal(response.Code)
	}
	close(finish)
	wg.Wait()
	if r.Status().Drained {
		t.Fatal("task was forgotten")
	}
	task()
	r.SetPending(func() int { return 1 })
	if r.Status().Drained {
		t.Fatal("durable outbox forgotten")
	}
	r.SetPending(func() int { return 0 })
	if !r.Status().Drained {
		t.Fatal("completed work not drained")
	}
}
func TestStandbyAndCanaryDoNotClaimJobs(t *testing.T) {
	r := &Runtime{}
	for _, mode := range []string{"standby", "canary"} {
		if err := r.SetMode(mode); err != nil {
			t.Fatal(err)
		}
		if _, ok := r.BeginTask(); ok {
			t.Fatal(mode)
		}
	}
	response := httptest.NewRecorder()
	r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(response, httptest.NewRequest("GET", "/pro", nil))
	if response.Code != 204 {
		t.Fatal("canary smoke blocked")
	}
}
