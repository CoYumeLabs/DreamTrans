package edgeruntime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMainControlCallsUseHTTP1(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Edge fixture" {
			t.Error("runtime control transport or identity changed")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"sequence":7`) {
			t.Error("control payload lost")
		}
		calls.Add(1)
		_, _ = io.WriteString(w, `{"saved":true}`)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	client, err := NewMainClient(server.URL, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.HTTP.CloseIdleConnections)
	client.HTTP.Transport.(*http.Transport).TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	for _, path := range []string{"heartbeat", "connect", "events", "archive"} {
		var output struct{ Saved bool }
		if err := client.Call(t.Context(), path, map[string]int{"sequence": 7}, &output); err != nil || !output.Saved {
			t.Fatalf("%s failed: %v", path, err)
		}
	}
	if calls.Load() != 4 {
		t.Fatal("lost or retried a control operation")
	}
}
