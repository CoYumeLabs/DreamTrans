package rag

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestPostgresLegacyImportAndConcurrentWriters(t *testing.T) {
	dsn := os.Getenv("DREAMTRANS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("isolated PostgreSQL not configured")
	}
	base, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = base.Close() }()
	schema := "rag_test_" + uuid.NewString()[:8]
	if _, err = base.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = base.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) }()
	for _, table := range []string{"deployment_metadata", "legacy_rag_documents", "legacy_rag_embeddings", "legacy_rag_session_summary"} {
		if _, err = base.Exec(`CREATE TABLE ` + schema + `.` + table + ` (LIKE public.` + table + ` INCLUDING ALL)`); err != nil {
			t.Fatal(err)
		}
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := sql.Open("postgres", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(7)
	if _, err := NewPostgresStoreWithDB(db); err == nil {
		t.Fatal("borrowed store accepted missing legacy import")
	}
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatal("failed borrowed initialization closed the owner pool")
	}
	sqlitePath := filepath.Join(t.TempDir(), "rag.db")
	original, err := NewStore(sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	doc := &Document{SessionID: "owner:session", Speaker: "S1", Original: "before upgrade", Summary: "summary", StartTime: 1, EndTime: 2}
	if _, err = original.InsertDocumentWithEmbedding(doc, []float32{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err = original.UpdateSessionTitle(doc.SessionID, "original title"); err != nil {
		t.Fatal(err)
	}
	if err = original.Close(); err != nil {
		t.Fatal(err)
	}
	if err = ImportLegacy(t.Context(), db, sqlitePath, `{}`); err != nil {
		t.Fatal(err)
	}
	borrowed, err := NewPostgresStoreWithDB(db)
	if err != nil {
		t.Fatal(err)
	}
	if borrowed.db != db || db.Stats().MaxOpenConnections != 7 {
		t.Fatal("RAG replaced or reconfigured the application pool")
	}
	if err := borrowed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatal("closing borrowed RAG storage closed the application pool")
	}
	blue, err := NewPostgresStore(parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blue.Close() }()
	green, err := NewPostgresStore(parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = green.Close() }()
	if docs, err := blue.RecentDocuments(doc.SessionID, 10); err != nil || len(docs) != 1 || docs[0].Original != doc.Original {
		t.Fatalf("import lost data: %+v %v", docs, err)
	}
	errs := make(chan error, 2)
	for _, writer := range []*Store{blue, green} {
		go func(writer *Store) {
			_, err := writer.InsertDocumentWithEmbedding(&Document{SessionID: doc.SessionID, Speaker: "S1", Original: "after cutover", Summary: "new", StartTime: 3, EndTime: 4}, []float32{3, 4})
			errs <- err
		}(writer)
	}
	for range 2 {
		if err = <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if err = ImportLegacy(t.Context(), db, sqlitePath, `{}`); err != nil {
		t.Fatal(err)
	}
	docs, err := blue.RecentDocuments(doc.SessionID, 10)
	if err != nil || len(docs) != 2 {
		t.Fatalf("concurrent dedup/import replay: %+v %v", docs, err)
	}
	if title, err := green.GetSessionTitle(doc.SessionID); err != nil || title != "original title" {
		t.Fatalf("title lost: %q %v", title, err)
	}
	vectors, err := green.LoadEmbeddingsForDocs([]int64{docs[0].ID, docs[1].ID})
	if err != nil || len(vectors) != 2 {
		t.Fatalf("vectors lost: %+v %v", vectors, err)
	}
}
