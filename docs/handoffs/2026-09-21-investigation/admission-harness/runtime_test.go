package deployment

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCooperativeHandoffNeverCancelsWork(t *testing.T) {
	r := &Runtime{mode: "active"}
	offers := make(chan any, 4)
	cleanup := r.NotifyHandoff(func(v any) error { offers <- v; return nil })
	defer cleanup()
	finish, _ := r.BeginTask()
	if r.RequestHandoff() == nil {
		t.Fatal("active instance offered migration")
	}
	if err := r.SetMode("draining"); err != nil {
		t.Fatal(err)
	}
	if err := r.RequestHandoff(); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-offers:
		if value.(map[string]any)["message"] != "DeploymentHandoff" {
			t.Fatal(value)
		}
	case <-time.After(time.Second):
		t.Fatal("offer not delivered")
	}
	if r.Status().Drained || r.Status().Tasks != 1 {
		t.Fatal("handoff cancelled accepted work")
	}
	finish()
	cleanup()
	if r.Status().HandoffStreams != 0 || !r.Status().Drained {
		t.Fatal(r.Status())
	}
	if err := r.SetMode("active"); err != nil {
		t.Fatal(err)
	}
	if r.RequestHandoff() == nil {
		t.Fatal("rollback still offers migration")
	}
}

func TestShutdownPreservesRestartModeAndRejectsNewWork(t *testing.T) {
	for _, mode := range []string{"active", "standby", "canary", "draining"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mode")
			r := &Runtime{path: path}
			if err := r.SetMode(mode); err != nil {
				t.Fatal(err)
			}
			finish, accepted := r.BeginTask()
			r.BeginShutdown()
			if _, ok := r.BeginTask(); ok {
				t.Fatal("new task accepted during process shutdown")
			}
			response := httptest.NewRecorder()
			r.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("new request accepted during process shutdown")
			})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/auth/login", nil))
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("shutdown response: %d", response.Code)
			}
			if accepted {
				if r.Status().Drained {
					t.Fatal("accepted task was forgotten during shutdown")
				}
				finish()
			}
			if !r.Status().Drained {
				t.Fatal("completed work did not drain")
			}
			if err := r.SetMode("active"); err == nil {
				t.Fatal("shutdown process was reactivated")
			}
			data, err := os.ReadFile(path)
			if err != nil || strings.TrimSpace(string(data)) != mode {
				t.Fatalf("persisted admission mode changed: %q, %v", data, err)
			}
			previous := Default
			t.Cleanup(func() { Default = previous })
			t.Setenv("DREAMTRANS_ROLE", "edge")
			t.Setenv("DREAMTRANS_DEPLOYMENT_MODE", "standby")
			t.Setenv("DREAMTRANS_DEPLOYMENT_STATE", path)
			if err := Configure(); err != nil {
				t.Fatal(err)
			}
			if got := Default.Status().Mode; got != mode {
				t.Fatalf("restarted mode = %s, want %s", got, mode)
			}
		})
	}
}

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
