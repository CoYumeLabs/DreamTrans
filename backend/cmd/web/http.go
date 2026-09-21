package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type databasePinger interface {
	PingContext(context.Context) error
}

const (
	httpReadHeaderTimeout = 10 * time.Second
	httpReadTimeout       = 5 * time.Minute
	httpWriteTimeout      = 15 * time.Minute
	// Keep the origin connection alive longer than the idle pools used by
	// common loopback reverse proxies (cloudflared defaults to 90 seconds and
	// Caddy to 2 minutes).
	// Closing first creates a narrow stale-connection race where the proxy can
	// return a gateway error before a request reaches an application handler.
	httpIdleTimeout = 3 * time.Minute
)

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
}

func probeHandler(pinger databasePinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if pinger != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := pinger.PingContext(ctx); err != nil {
				http.Error(w, "not ready", http.StatusServiceUnavailable)
				return
			}
		}

		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("ok\n"))
		}
	}
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func safeRequestLogValue(value string) string {
	const maxLogFieldBytes = 512
	value = strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return -1
		}
		return character
	}, value)
	if len(value) > maxLogFieldBytes {
		return value[:maxLogFieldBytes]
	}
	return value
}

// logServerFailures records application-generated 5xx responses with the
// Cloudflare Ray ID when present. A Cloudflare 502 without a matching origin
// log entry can then be identified as a tunnel/edge failure instead of being
// confused with a provider or database response from this process.
func logServerFailures(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Do not wrap WebSocket responses: gorilla/websocket requires direct
		// access to optional interfaces such as http.Hijacker.
		if strings.HasPrefix(r.URL.Path, "/ws/") {
			next.ServeHTTP(w, r)
			return
		}
		startedAt := time.Now()
		response := &statusResponseWriter{ResponseWriter: w}
		next.ServeHTTP(response, r)
		if response.status >= http.StatusInternalServerError {
			//nolint:gosec // G706: every request-derived field is control-stripped and length-bounded above.
			log.Printf(
				"http server failure method=%s path=%q status=%d duration_ms=%d cf_ray=%q",
				safeRequestLogValue(r.Method),
				safeRequestLogValue(r.URL.Path),
				response.status,
				time.Since(startedAt).Milliseconds(),
				safeRequestLogValue(strings.TrimSpace(r.Header.Get("CF-Ray"))),
			)
		}
	})
}

func safePublicPath(publicDir, requestPath string) (string, bool) {
	// Treat backslashes as separators as well so the same validation remains
	// safe if the server is built for Windows.
	cleanURLPath := path.Clean("/" + strings.ReplaceAll(requestPath, `\`, "/"))
	relativePath := strings.TrimPrefix(cleanURLPath, "/")
	candidate := filepath.Join(publicDir, filepath.FromSlash(relativePath))
	relativeToRoot, err := filepath.Rel(publicDir, candidate)
	if err != nil || relativeToRoot == ".." ||
		strings.HasPrefix(relativeToRoot, ".."+string(filepath.Separator)) {
		return "", false
	}
	return candidate, true
}

func maxRequestBody(limit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

func corsOrigins() []string {
	raw := strings.TrimSpace(os.Getenv("CORS_ALLOWED_ORIGINS"))
	if raw == "" {
		return []string{"http://localhost:5173", "http://127.0.0.1:5173"}
	}
	seen := make(map[string]struct{})
	origins := make([]string, 0)
	for _, value := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(value)
		if origin == "" || origin == "*" {
			continue
		}
		if _, ok := seen[origin]; ok {
			continue
		}
		seen[origin] = struct{}{}
		origins = append(origins, origin)
	}
	return origins
}
