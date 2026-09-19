package rag

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq" // PostgreSQL storage driver
)

// NewPostgresStore uses the externally migrated shared database. It never opens SQLite.
func NewPostgresStore(dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var ready bool
	err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_metadata WHERE key='legacy_import_complete')`).Scan(&ready)
	if err == nil && !ready {
		err = errors.New("legacy RAG/config import must complete before enabling PostgreSQL RAG")
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, postgres: true}, nil
}

// query translates only internal SQL, never user input or table identifiers.
func (s *Store) query(q string) string {
	if !s.postgres {
		return q
	}
	q = strings.NewReplacer("session_summary", "legacy_rag_session_summary", "documents", "legacy_rag_documents", "embeddings", "legacy_rag_embeddings").Replace(q)
	var out strings.Builder
	n := 0
	for _, c := range q {
		if c == '?' {
			n++
			out.WriteString("$" + strconv.Itoa(n))
		} else {
			out.WriteRune(c)
		}
	}
	return out.String()
}

func (s *Store) insertPostgresDocument(tx *sql.Tx, doc *Document, hash string) (int64, error) {
	var id int64
	err := tx.QueryRow(`INSERT INTO legacy_rag_documents(session_id,speaker,start_time,end_time,original_text,summary,hash,created_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(hash) DO UPDATE SET hash=excluded.hash RETURNING id`,
		doc.SessionID, doc.Speaker, doc.StartTime, doc.EndTime, doc.Original, doc.Summary, hash, time.Now().UTC()).Scan(&id)
	return id, err
}

// ImportLegacy copies a quiescent SQLite database transactionally. The original stays intact.
// Call only after stopping the legacy writer; a completion marker prevents a replay from
// overwriting post-cutover data. No paid embedding or other provider request is made.
func ImportLegacy(ctx context.Context, db *sql.DB, sqlitePath, configJSON string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1146243412,53)`); err != nil {
		return err
	}
	var complete bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_metadata WHERE key='legacy_import_complete')`).Scan(&complete); err != nil {
		return err
	}
	if complete {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployment_metadata(key,value) VALUES('server_config',$1) ON CONFLICT(key) DO NOTHING`, configJSON); err != nil {
		return err
	}
	if _, err = os.Stat(sqlitePath); err == nil {
		abs, absErr := filepath.Abs(sqlitePath)
		if absErr != nil {
			return absErr
		}
		uri := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}
		src, openErr := sql.Open("sqlite", uri.String())
		if openErr != nil {
			return openErr
		}
		defer func() { _ = src.Close() }()
		if err := importLegacyRows(ctx, tx, src); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployment_metadata(key,value) VALUES('legacy_import_complete','1')`); err != nil {
		return err
	}
	return tx.Commit()
}

func importLegacyRows(ctx context.Context, tx *sql.Tx, src *sql.DB) error {
	rows, err := src.QueryContext(ctx, `SELECT d.session_id,coalesce(d.speaker,''),coalesce(d.start_time,0),coalesce(d.end_time,0),d.original_text,coalesce(d.summary,''),d.hash,coalesce(e.dim,0),coalesce(e.norm,0),coalesce(e.vector_json,'null'),d.created_at FROM documents d LEFT JOIN embeddings e ON e.doc_id=d.id`)
	if err != nil {
		return fmt.Errorf("read legacy RAG: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var session, speaker, text, summary, hash, vector string
		var start, end, norm float64
		var dim int
		var id int64
		var created time.Time
		if err := rows.Scan(&session, &speaker, &start, &end, &text, &summary, &hash, &dim, &norm, &vector, &created); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `INSERT INTO legacy_rag_documents(session_id,speaker,start_time,end_time,original_text,summary,hash,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(hash) DO UPDATE SET hash=excluded.hash RETURNING id`, session, speaker, start, end, text, summary, hash, created).Scan(&id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO legacy_rag_embeddings(doc_id,dim,norm,vector_json) VALUES($1,$2,$3,$4) ON CONFLICT(doc_id) DO NOTHING`, id, dim, norm, vector); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	summaries, err := src.QueryContext(ctx, `SELECT session_id,coalesce(summary,''),coalesce(title,''),updated_at FROM session_summary`)
	if err != nil {
		return err
	}
	defer func() { _ = summaries.Close() }()
	for summaries.Next() {
		var session, summary, title string
		var updated time.Time
		if err := summaries.Scan(&session, &summary, &title, &updated); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO legacy_rag_session_summary(session_id,summary,title,updated_at) VALUES($1,$2,$3,$4) ON CONFLICT(session_id) DO NOTHING`, session, summary, title, updated); err != nil {
			return err
		}
	}
	return summaries.Err()
}
