// Package deployment controls admission and draining for blue/green releases.
package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const socketPath = "/tmp/dreamtrans-deployment.sock"

// Runtime counts work from admission through completion, including hijacked sockets.
type Runtime struct {
	mu                       sync.Mutex
	mode                     string
	stopping                 bool
	path                     string
	requests, sockets, tasks int
	pending                  func() int
	handoffs                 map[chan struct{}]struct{}
}

// Default is disabled outside explicitly configured blue/green instances.
var Default = &Runtime{}

// Snapshot is the local deployment control protocol, versioned independently of HTTP APIs.
type Snapshot struct {
	Protocol         int    `json:"protocol"`
	Mode             string `json:"mode"`
	Requests         int    `json:"requests"`
	WebSockets       int    `json:"websockets"`
	Tasks            int    `json:"tasks"`
	Drained          bool   `json:"drained"`
	Pending          int    `json:"pending"`
	HandoffSupported bool   `json:"handoff_supported"`
	HandoffStreams   int    `json:"handoff_streams"`
}

// Configure fails closed: a restarted candidate cannot start production workers.
func Configure() error {
	mode := os.Getenv("DREAMTRANS_DEPLOYMENT_MODE")
	if mode == "" {
		return nil
	}
	if os.Getenv("DREAMTRANS_ROLE") != "edge" && (os.Getenv("RAG_STORAGE") != "postgres" || os.Getenv("DATABASE_URL") == "" || strings.EqualFold(os.Getenv("ALLOW_ANONYMOUS_API"), "true")) {
		return errors.New("blue/green requires PostgreSQL RAG, DATABASE_URL and disabled anonymous API")
	}
	path := os.Getenv("DREAMTRANS_DEPLOYMENT_STATE")
	if path == "" {
		return errors.New("deployment state path is required")
	}
	data, err := os.ReadFile(path) //nolint:gosec // operator-controlled deployment path
	if err == nil {
		mode = strings.TrimSpace(string(data))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	Default = &Runtime{path: path}
	return Default.SetMode(mode)
}

// Enabled reports whether this process participates in deployment control.
func (r *Runtime) Enabled() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.mode != "" }

// Status gives a consistent view of admission state and active work.
func (r *Runtime) Status() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := 0
	if r.pending != nil {
		pending = r.pending()
	}
	return Snapshot{HandoffSupported: true, HandoffStreams: len(r.handoffs), Pending: pending, Protocol: 1, Mode: r.mode, Requests: r.requests, WebSockets: r.sockets, Tasks: r.tasks,
		Drained: r.mode == "draining" && r.requests == 0 && r.sockets == 0 && r.tasks == 0 && pending == 0}
}

// SetMode persists intent before changing admission. It never cancels accepted work.
func (r *Runtime) SetMode(mode string) error {
	if mode != "active" && mode != "standby" && mode != "draining" && mode != "canary" {
		return errors.New("invalid deployment mode")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping && mode != "draining" {
		return errors.New("process is shutting down")
	}
	if r.path != "" {
		tmp := r.path + ".tmp"
		//nolint:gosec // G703: path is the operator-owned per-instance deployment state, never request input.
		if err := os.WriteFile(tmp, []byte(mode+"\n"), 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, r.path); err != nil {
			return err
		}
	}
	r.mode = mode
	return nil
}

// BeginShutdown drains this process without changing the operator's persisted
// admission mode. A host reboot must restore an active instance as active, while
// an explicitly drained or standby instance must retain that persisted state.
func (r *Runtime) BeginShutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopping = true
	r.mode = "draining"
}

// BeginTask covers the claim itself as well as execution, closing the drain/claim race.
func (r *Runtime) BeginTask() (func(), bool) {
	r.mu.Lock()
	if r.mode != "" && r.mode != "active" {
		r.mu.Unlock()
		return func() {}, false
	}
	r.tasks++
	r.mu.Unlock()
	return func() { r.mu.Lock(); r.tasks--; r.mu.Unlock() }, true
}

// Middleware deliberately passes the original ResponseWriter (including Hijacker).
func (r *Runtime) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/healthz" || req.URL.Path == "/readyz" {
			next.ServeHTTP(w, req)
			return
		}
		ws := strings.EqualFold(req.Header.Get("Upgrade"), "websocket")
		r.mu.Lock()
		if r.mode != "" && r.mode != "active" && r.mode != "canary" {
			r.mu.Unlock()
			w.Header().Set("Retry-After", "1")
			http.Error(w, "instance is not accepting new work", http.StatusServiceUnavailable)
			return
		}
		if ws {
			r.sockets++
		} else {
			r.requests++
		}
		r.mu.Unlock()
		defer func() {
			r.mu.Lock()
			if ws {
				r.sockets--
			} else {
				r.requests--
			}
			r.mu.Unlock()
		}()
		next.ServeHTTP(w, req)
	})
}

// ServeControl exposes no TCP port. Only container-local processes can change mode.
func (r *Runtime) ServeControl() (func(), error) {
	if !r.Enabled() {
		return func() {}, nil
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			action := strings.TrimPrefix(req.URL.Path, "/")
			var err error
			if action == "handoff" {
				err = r.RequestHandoff()
			} else {
				err = r.SetMode(action)
			}
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		} else if req.Method != http.MethodGet || req.URL.Path != "/status" {
			http.Error(w, "unsupported control", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.Status())
	})}
	go func() { _ = server.Serve(listener) }()
	return func() { _ = server.Close() }, nil
}

// Control is used by docker exec /app/server deploy-control; no bearer secret is exposed.
func Control(action string, output io.Writer) error {
	method := http.MethodPost
	if action == "status" {
		method = http.MethodGet
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	req, err := http.NewRequest(method, "http://local/"+action, http.NoBody)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 {
		return fmt.Errorf("deployment control returned %d", res.StatusCode)
	}
	_, err = io.Copy(output, res.Body)
	return err
}

// SetPending includes durable, unacknowledged events in drain completion.
func (r *Runtime) SetPending(count func() int) { r.mu.Lock(); defer r.mu.Unlock(); r.pending = count }

// RequestHandoff offers a cooperative reconnect. It never closes a connection:
// clients without this protocol continue normally until they finish.
func (r *Runtime) RequestHandoff() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mode != "draining" {
		return errors.New("handoff requires draining admission")
	}
	for ch := range r.handoffs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return nil
}

// NotifyHandoff serializes offers outside the runtime lock. The returned cleanup
// waits for the writer, so a handler cannot leave a goroutine using a closed peer.
func (r *Runtime) NotifyHandoff(send func(any) error) func() {
	ch, done, finished := make(chan struct{}, 1), make(chan struct{}), make(chan struct{})
	r.mu.Lock()
	if r.handoffs == nil {
		r.handoffs = make(map[chan struct{}]struct{})
	}
	r.handoffs[ch] = struct{}{}
	r.mu.Unlock()
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			case <-ch:
				if r.Status().Mode == "draining" {
					_ = send(map[string]any{"message": "DeploymentHandoff", "version": 1})
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.handoffs, ch)
			r.mu.Unlock()
			close(done)
			<-finished
		})
	}
}
