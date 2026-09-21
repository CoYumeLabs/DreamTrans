package main

import (
	"os"
	"testing"

	"github.com/dreamtrans/backend/internal/store"
)

func TestApplicationClosesWorkersBeforeSharedDatabase(t *testing.T) {
	dsn := os.Getenv("DREAMTRANS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("isolated PostgreSQL not configured")
	}
	t.Setenv("DATABASE_URL", dsn)
	database, err := store.NewPostgresStore()
	if err != nil {
		t.Fatal(err)
	}
	app := newApplication(t.Context())
	app.Store = database
	t.Cleanup(app.Close)
	calls := 0
	app.cleanup = func() {
		calls++
		if app.ctx.Err() == nil {
			t.Error("workers outlived application cancellation")
		}
		if err := database.DB().PingContext(t.Context()); err != nil {
			t.Error("shared database closed before its borrowers stopped")
		}
	}
	app.Close()
	app.Close()
	if calls != 1 {
		t.Fatal("application cleanup was not idempotent")
	}
	if err := database.DB().PingContext(t.Context()); err == nil {
		t.Fatal("application leaked its database pool")
	}
}
