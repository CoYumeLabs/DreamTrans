package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
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
	r.Link = Link{OwnerID: "integration-owner", SessionID: "linked-session", SourceLanguage: "zh", TargetLanguage: "en", Created: true, Offset: 12.5}
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
	if got.Link != r.Link {
		t.Fatal("private Yufolo linkage was not persisted")
	}
	owned, err := other.ListOwned(ctx, "integration-owner")
	if err != nil || len(owned) != 1 || owned[0].Code != r.Code {
		t.Fatalf("owner recovery failed: %v", err)
	}
	foreign, err := other.ListOwned(ctx, "unrelated-owner")
	if err != nil || len(foreign) != 0 {
		t.Fatal("owner listing leaked rooms")
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

func TestMigrationPreservesLegacyRooms(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL migration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("yuaction_migration_test_%d", time.Now().UnixNano())
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+name); err != nil {
		t.Fatal(err)
	}
	defer admin.ExecContext(context.Background(), "DROP SCHEMA "+name+" CASCADE")
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = name
	testDSN := stdlib.RegisterConnConfig(config)
	defer stdlib.UnregisterConnConfig(testDSN)
	legacy, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err = legacy.ExecContext(ctx, schema); err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.ExecContext(ctx, `INSERT INTO rooms(code,host_hash,revision,state) VALUES ('LEGACY01','old-hash',5,'{"title":"Existing room"}')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		p, err := OpenPostgres(ctx, testDSN)
		if err != nil {
			t.Fatal(err)
		}
		r, err := p.Get(ctx, "LEGACY01")
		p.Close()
		if err != nil || r.HostHash != "old-hash" || r.Revision != 5 || r.Link != (Link{}) {
			t.Fatalf("migration changed old room: %+v %v", r, err)
		}
	}
}

func TestSharedCredentialsAndRecordingOwnershipAcrossPools(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required")
	}
	first, err := OpenPostgres(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenPostgres(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	key := fmt.Sprintf("coordination-test-%d", time.Now().UnixNano())
	defer first.db.ExecContext(context.Background(), "DELETE FROM shared_state WHERE key=$1", key)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := first
			if i%2 == 1 {
				p = second
			}
			err := p.MutateShared(t.Context(), key, func(value []byte) ([]byte, error) {
				n := 0
				if len(value) > 0 {
					if err := json.Unmarshal(value, &n); err != nil {
						return nil, err
					}
				}
				return json.Marshal(n + 1)
			})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if err := first.MutateShared(t.Context(), key, func(value []byte) ([]byte, error) {
		if string(value) != "30" {
			t.Errorf("concurrent credential updates lost: %s", value)
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	release, err := first.TryLock(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if done, err := second.TryLock(t.Context(), key); err == nil {
		done()
		t.Fatal("two recording owners admitted")
	}
	if locked, err := second.Locked(t.Context(), key); err != nil || !locked {
		t.Fatalf("remote owner invisible: %v %v", locked, err)
	}
	release()
	release, err = second.TryLock(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	release()
}
