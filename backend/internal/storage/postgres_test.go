package storage

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestPostgresPersistenceAndCompareAndSwap(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// The integration database must be dedicated to tests. This fixed record is
	// removed before/after the test; no production tables are truncated.
	_, err = p.db.ExecContext(ctx, `DELETE FROM rooms WHERE code='TEST0001'`)
	if err != nil {
		t.Fatal(err)
	}
	defer p.db.ExecContext(context.Background(), `DELETE FROM rooms WHERE code='TEST0001'`)
	r := Record{Code: "TEST0001", HostHash: "hash", Revision: 1, Data: []byte(`{"title":"持久化测试"}`)}
	if err = p.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err = p.Create(ctx, r); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected duplicate conflict, got %v", err)
	}
	other, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	got, err := other.Get(ctx, r.Code)
	if err != nil || got.HostHash != r.HostHash || got.Revision != 1 {
		t.Fatalf("reopen lost state: %+v %v", got, err)
	}
	r.Revision = 2
	r.Data = []byte(`{"title":"已更新"}`)
	if err = p.Save(ctx, 1, r); err != nil {
		t.Fatal(err)
	}
	if err = other.Save(ctx, 1, r); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write accepted: %v", err)
	}
	got, err = other.Get(ctx, r.Code)
	if err != nil || got.Revision != 2 {
		t.Fatalf("failed read after update: %+v %v", got, err)
	}
}
