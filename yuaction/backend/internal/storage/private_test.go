package storage

import (
	"context"
	"errors"
	"os"
	"testing"
)

func privateStoreContract(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	for _, code := range []string{"PRIVATE1", "PRIVATE2"} {
		if err := s.Create(ctx, Record{Code: code, HostHash: "hash", Revision: 1, Data: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	r := PrivateRecord{Code: "PRIVATE1", Kind: "document", ID: "same-id", Data: []byte(`{"text":"secret"}`)}
	if err := s.SavePrivate(ctx, 0, r); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePrivate(ctx, 0, r); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := s.GetPrivate(ctx, r.Code, r.Kind, r.ID)
	if err != nil || got.Revision != 1 {
		t.Fatalf("get: %+v %v", got, err)
	}
	got.Data[0] = 'x'
	again, _ := s.GetPrivate(ctx, r.Code, r.Kind, r.ID)
	if again.Data[0] != '{' {
		t.Fatal("read shared mutable memory")
	}
	for _, scope := range [][2]string{{"PRIVATE2", "document"}, {"PRIVATE1", "answer"}} {
		rows, e := s.ListPrivate(ctx, scope[0], scope[1])
		if e != nil || len(rows) != 0 {
			t.Fatal("scope leaked")
		}
	}
	if err = s.SavePrivate(ctx, 1, r); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePrivate(ctx, 1, r); !errors.Is(err, ErrConflict) {
		t.Fatal("stale update accepted")
	}
	if err = s.DeletePrivate(ctx, r.Code, r.Kind, r.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePrivate(ctx, 2, r); !errors.Is(err, ErrConflict) {
		t.Fatal("worker resurrected deleted item")
	}
	if _, err = s.GetPrivate(ctx, r.Code, r.Kind, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted item found")
	}
}
func TestPrivateMemory(t *testing.T) { privateStoreContract(t, NewMemory()) }
func TestPrivatePostgres(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL")
	}
	s, err := OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.db.Exec(`DELETE FROM rooms WHERE code IN ('PRIVATE1','PRIVATE2')`); err != nil {
		t.Fatal(err)
	}
	defer s.db.Exec(`DELETE FROM rooms WHERE code IN ('PRIVATE1','PRIVATE2')`)
	privateStoreContract(t, s)
}
