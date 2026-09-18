//go:build e2e

package app

import (
	"net/http"
	"os"
	"testing"

	"github.com/CoYumeLabs/YuAction/backend/internal/storage"
)

func TestBrowserFixture(t *testing.T) {
	upstream := newFakeYufolo(t)
	server := New(storage.NewMemory(), Config{YufoloURL: upstream.server.URL})
	address := os.Getenv("YUACTION_BROWSER_FIXTURE_ADDR")
	if address == "" {
		address = "127.0.0.1:18086"
	}
	t.Logf("Yufolo browser fixture listening on %s; no real provider calls", address)
	if err := http.ListenAndServe(address, server.Handler()); err != nil {
		t.Fatal(err)
	}
}
