//go:build e2e

package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CoYumeLabs/YuAction/backend/internal/storage"
	"github.com/dreamtrans/backend/pkg/deployment"
)

func TestBrowserFixture(t *testing.T) {
	upstream := newFakeYufolo(t)
	upstream.endDelay = 400 * time.Millisecond
	store := storage.NewMemory()
	runtimes := [2]*deployment.Runtime{{}, {}}
	proxies := [2]*httputil.ReverseProxy{}
	for i := range proxies {
		server := New(store, Config{YufoloURL: upstream.server.URL, CreatorKey: "browser-fixture-shared-key-only", Deployment: runtimes[i]})
		backend := httptest.NewServer(server.Handler())
		t.Cleanup(backend.Close)
		target, _ := url.Parse(backend.URL)
		proxies[i] = httputil.NewSingleHostReverseProxy(target)
		if err := runtimes[i].SetMode("active"); err != nil {
			t.Fatal(err)
		}
	}
	var active atomic.Int32
	var deployMu sync.Mutex
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/__fixture/deploy":
			deployMu.Lock()
			defer deployMu.Unlock()
			old := int(active.Load())
			next := 1 - old
			_ = runtimes[next].SetMode("active")
			active.Store(int32(next))
			_ = runtimes[old].SetMode("draining")
			_ = runtimes[old].RequestHandoff()
			respond(w, 200, map[string]int{"active": next})
		case "/api/__fixture/fail-candidate":
			deployMu.Lock()
			defer deployMu.Unlock()
			old := int(active.Load())
			_ = runtimes[old].SetMode("draining")
			_ = runtimes[old].RequestHandoff()
			// No ready replacement: preflight must fail without stopping the old audio.
			time.Sleep(250 * time.Millisecond)
			_ = runtimes[old].SetMode("active")
			respond(w, 200, map[string]bool{"keptOld": true})
		case "/api/__fixture/reset":
			if upstream.connections.Load() != 0 {
				w.WriteHeader(409)
				return
			}
			upstream.mu.Lock()
			upstream.audioHash.Reset()
			upstream.audioBytes = 0
			upstream.mu.Unlock()
			upstream.starts.Store(0)
			upstream.audioFrames.Store(0)
			upstream.maxConnections.Store(0)
			respond(w, 200, map[string]bool{"ok": true})
		case "/api/__fixture/stats":
			upstream.mu.Lock()
			hash := fmt.Sprintf("%016x", upstream.audioHash.Sum64())
			n := upstream.audioBytes
			upstream.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"starts": upstream.starts.Load(), "connections": upstream.connections.Load(), "maxConnections": upstream.maxConnections.Load(), "bytes": n, "hash": hash, "active": active.Load(), "colors": []deployment.Snapshot{runtimes[0].Status(), runtimes[1].Status()}})
		default:
			proxies[active.Load()].ServeHTTP(w, r)
		}
	})
	address := os.Getenv("YUACTION_BROWSER_FIXTURE_ADDR")
	if address == "" {
		address = "127.0.0.1:18086"
	}
	t.Logf("Two-generation Yufolo browser fixture listening on %s; no real provider calls", address)
	if err := http.ListenAndServe(address, handler); err != nil {
		t.Fatal(err)
	}
}

// Used only by the isolated production-image lifecycle test. Listen on the test
// host so containers can reach this fake upstream without any provider secret.
func TestDockerUpstreamFixture(t *testing.T) {
	address := os.Getenv("YUACTION_DOCKER_FIXTURE_ADDR")
	if address == "" {
		t.Skip("Docker fixture listener not requested")
	}
	upstream := newFakeYufolo(t)
	upstream.endDelay = 300 * time.Millisecond
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__fixture/stats" {
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			respond(w, 200, map[string]any{"bytes": upstream.audioBytes, "hash": fmt.Sprintf("%016x", upstream.audioHash.Sum64()), "starts": upstream.starts.Load(), "maxConnections": upstream.maxConnections.Load(), "connections": upstream.connections.Load(), "archives": len(upstream.archives)})
			return
		}
		upstream.handle(w, r)
	})
	if err := http.ListenAndServe(address, handler); err != nil {
		t.Fatal(err)
	}
}
