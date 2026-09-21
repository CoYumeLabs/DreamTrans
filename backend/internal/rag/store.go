package rag

import (
	"context"
	"crypto/sha1" //nolint:gosec // used only for legacy dedup compatibility
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // sqlite driver for database/sql (pure Go)
)

// Store manages on-disk storage of documents, summaries and embeddings.
type Store struct {
	borrowedDB             bool
	postgres               bool
	db                     *sql.DB
	path                   string
	totalBudgetBytes       int64
	checkpointTriggerBytes int64
}

const maxRAGSerializedWriteBytes = 16 * 1024 * 1024

var (
	// ErrRAGStorageBudget indicates that a retained WAL could not be brought
	// back under its reserved share before another write.
	ErrRAGStorageBudget = errors.New("RAG SQLite storage budget exceeded")
	// ErrRAGWriteTooLarge caps accepted serialized input so a single mutation
	// cannot create an arbitrarily large WAL before SQLite can checkpoint it.
	ErrRAGWriteTooLarge = errors.New("RAG SQLite write payload is too large")

	// NewServiceFromEnv can create several Store instances for the same file.
	// SQLite serializes commits, and this process-wide guard makes the budget
	// check/commit/checkpoint sequence atomic across those instances too.
	ragSQLiteWriteMu sync.Mutex
)

// NewStore opens/creates the SQLite database at given path.
func NewStore(path string) (*Store, error) {
	normalizedPath, err := normalizeSQLitePath(path)
	if err != nil {
		return nil, err
	}
	// RAG_DB_PATH is an operator-controlled startup setting; request session
	// identifiers are stored inside the database and never joined into paths.
	//nolint:gosec // G703: creating the configured database directory is intentional.
	if err := os.MkdirAll(filepath.Dir(normalizedPath), 0o750); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}

	maxDatabaseMB, err := loadRAGMaxDatabaseMB()
	if err != nil {
		return nil, err
	}
	pageSize, err := detectSQLitePageSize(normalizedPath)
	if err != nil {
		return nil, err
	}
	config := makeSQLiteStorageConfig(maxDatabaseMB, pageSize)
	db, err := openConfiguredSQLite(normalizedPath, config)
	if err != nil {
		return nil, err
	}

	// A newly created database normally uses 4 KiB pages. Verify instead of
	// relying on that build default, and reopen with a corrected max_page_count
	// before the first application write if a different default is in use.
	actualPageSize, err := readSQLiteIntPragma(db, "page_size")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read SQLite page size: %w", err)
	}
	if !validSQLitePageSize(actualPageSize) {
		_ = db.Close()
		return nil, fmt.Errorf("invalid SQLite page size %d", actualPageSize)
	}
	if actualPageSize != pageSize {
		if err := db.Close(); err != nil {
			return nil, fmt.Errorf("reopen SQLite with detected page size: %w", err)
		}
		config = makeSQLiteStorageConfig(maxDatabaseMB, actualPageSize)
		db, err = openConfiguredSQLite(normalizedPath, config)
		if err != nil {
			return nil, err
		}
	}

	s := &Store{
		db:                     db,
		path:                   normalizedPath,
		totalBudgetBytes:       config.totalBudgetBytes,
		checkpointTriggerBytes: config.journalLimitBytes,
	}
	if err := verifySQLiteSettings(db, config); err != nil {
		_ = db.Close()
		return nil, err
	}
	ragSQLiteWriteMu.Lock()
	migrateErr := s.prepareSQLiteWrite()
	if migrateErr == nil {
		migrateErr = s.migrate()
	}
	if migrateErr == nil {
		// A previous unclean shutdown can leave a large WAL behind. Migrations
		// can also create one, so checkpoint before accepting traffic.
		migrateErr = s.checkpointWAL()
	}
	ragSQLiteWriteMu.Unlock()
	if migrateErr != nil {
		_ = db.Close()
		return nil, migrateErr
	}
	return s, nil
}

func (s *Store) checkpointWAL() error {
	if s.postgres {
		return nil
	}
	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(sqliteBusyTimeoutMS)*time.Millisecond,
	)
	defer cancel()
	var busy, logFrames, checkpointedFrames int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE);`).Scan(
		&busy,
		&logFrames,
		&checkpointedFrames,
	); err != nil {
		return fmt.Errorf("checkpoint RAG SQLite WAL: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf(
			"checkpoint RAG SQLite WAL remained busy (%d log frames, %d checkpointed)",
			logFrames,
			checkpointedFrames,
		)
	}
	return nil
}

func (s *Store) checkpointWALIfOversize() error {
	return s.enforceSQLiteStorageBudget(false)
}

func (s *Store) prepareSQLiteWrite() error {
	return s.enforceSQLiteStorageBudget(true)
}

func (s *Store) enforceSQLiteStorageBudget(checkpointAtWALLimit bool) error {
	if s.path == "" || s.checkpointTriggerBytes <= 0 {
		return nil
	}
	totalBytes, walBytes, err := s.sqliteDiskUsage()
	if err != nil {
		return err
	}
	walOverBudget := walBytes > s.checkpointTriggerBytes ||
		(checkpointAtWALLimit && walBytes == s.checkpointTriggerBytes)
	totalOverBudget := s.totalBudgetBytes >= 0 && totalBytes > s.totalBudgetBytes
	if !walOverBudget && !totalOverBudget {
		return nil
	}
	if err := s.checkpointWAL(); err != nil {
		return fmt.Errorf(
			"%w: total is %d bytes and WAL is %d bytes: %v",
			ErrRAGStorageBudget,
			totalBytes,
			walBytes,
			err,
		)
	}
	totalBytes, walBytes, err = s.sqliteDiskUsage()
	if err != nil {
		return err
	}
	if walBytes > s.checkpointTriggerBytes ||
		(s.totalBudgetBytes >= 0 && totalBytes > s.totalBudgetBytes) {
		return fmt.Errorf(
			"%w: total is %d/%d bytes and WAL is %d/%d bytes",
			ErrRAGStorageBudget,
			totalBytes,
			s.totalBudgetBytes,
			walBytes,
			s.checkpointTriggerBytes,
		)
	}
	return nil
}

func (s *Store) sqliteDiskUsage() (totalBytes, walBytes int64, err error) {
	paths := []string{s.path, s.path + "-wal", s.path + "-shm"}
	for index, path := range paths {
		//nolint:gosec // G703: path is the normalized operator-owned SQLite path plus fixed WAL suffixes.
		info, statErr := os.Stat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return 0, 0, fmt.Errorf("inspect RAG SQLite storage: %w", statErr)
		}
		totalBytes += info.Size()
		if index == 1 {
			walBytes = info.Size()
		}
	}
	return totalBytes, walBytes, nil
}

func (s *Store) Close() error {
	if s.borrowedDB {
		return nil
	}
	ragSQLiteWriteMu.Lock()
	defer ragSQLiteWriteMu.Unlock()
	checkpointErr := s.checkpointWAL()
	closeErr := s.db.Close()
	return errors.Join(checkpointErr, closeErr)
}

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS documents (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            session_id TEXT NOT NULL,
            speaker TEXT,
            start_time REAL,
            end_time REAL,
            original_text TEXT NOT NULL,
            summary TEXT,
            hash TEXT UNIQUE,
            created_at DATETIME NOT NULL
        );`,
		`CREATE INDEX IF NOT EXISTS idx_docs_session_time ON documents(session_id, start_time);`,
		`CREATE TABLE IF NOT EXISTS embeddings (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            doc_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
            dim INTEGER NOT NULL,
            norm REAL NOT NULL,
            vector_json TEXT NOT NULL
        );`,
		// Old releases did not make doc_id unique, so INSERT OR REPLACE could
		// append duplicate vectors. Keep the newest copy before adding the
		// invariant used by the upsert below.
		`DELETE FROM embeddings
		 WHERE id NOT IN (SELECT MAX(id) FROM embeddings GROUP BY doc_id);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_embeddings_doc_id ON embeddings(doc_id);`,
		`CREATE TABLE IF NOT EXISTS session_summary (
            session_id TEXT PRIMARY KEY,
            summary TEXT,
            updated_at DATETIME NOT NULL
        );`,
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return err
		}
	}
	// Add title column if not exists (SQLite lacks easy IF NOT EXISTS for ADD COLUMN)
	if _, err := s.db.Exec(`ALTER TABLE session_summary ADD COLUMN title TEXT`); err != nil {
		// ignore duplicate column error; modernc sqlite error text contains 'duplicate column name'
		// other errors should be returned
		if !containsIgnoreCase(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

type Document struct {
	ID        int64
	SessionID string
	Speaker   string
	StartTime float64
	EndTime   float64
	Original  string
	Summary   string
	CreatedAt time.Time
	Ephemeral bool
}

func hashKey(sessionID, text string, start, end float64) string {
	digest := sha256.New()
	text = dedupText(text)
	for _, field := range []string{
		sessionID,
		strconv.FormatFloat(start, 'f', 3, 64),
		strconv.FormatFloat(end, 'f', 3, 64),
		text,
	} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = digest.Write(size[:])
		_, _ = digest.Write([]byte(field))
	}
	return fmt.Sprintf("sha256:%x", digest.Sum(nil))
}

func legacyHashKey(sessionID, text string, start, end float64) string {
	// Old databases used SHA-1. Keep read compatibility so an upgrade does not
	// purchase and persist a second embedding for the same paragraph.
	sum := sha1.Sum([]byte(text)) //nolint:gosec // compatibility lookup only; new keys use SHA-256
	return fmt.Sprintf("%s|%.3f|%.3f|%x", sessionID, start, end, sum[:])
}

// HasDocument avoids paying for an embedding again when a client retries an
// already persisted paragraph.
func (s *Store) HasDocument(sessionID, text string, start, end float64) (bool, error) {
	var exists int
	err := s.db.QueryRow(s.query(`SELECT 1 FROM documents WHERE hash IN (?, ?) LIMIT 1`),
		hashKey(sessionID, text, start, end),
		legacyHashKey(sessionID, text, start, end),
	).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// InsertDocumentWithEmbedding stores a document and its embedding atomically.
func (s *Store) InsertDocumentWithEmbedding(doc *Document, vec []float32) (int64, error) {
	if doc == nil {
		return 0, errors.New("RAG document must not be nil")
	}
	// Avoid allocating an oversized JSON buffer before the exact serialized
	// size check below. Thirty-two bytes comfortably covers one float32 plus
	// its separator in JSON.
	if len(vec) > maxRAGSerializedWriteBytes/32 {
		return 0, fmt.Errorf(
			"%w: maximum serialized payload is %d bytes",
			ErrRAGWriteTooLarge,
			maxRAGSerializedWriteBytes,
		)
	}
	jb, err := json.Marshal(vec)
	if err != nil {
		return 0, fmt.Errorf("encode RAG embedding: %w", err)
	}
	if err := validateRAGWriteSize(
		len(doc.SessionID),
		len(doc.Speaker),
		len(doc.Original),
		len(doc.Summary),
		len(jb),
	); err != nil {
		return 0, err
	}

	ragSQLiteWriteMu.Lock()
	defer ragSQLiteWriteMu.Unlock()
	if err := s.prepareSQLiteWrite(); err != nil {
		return 0, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	h := hashKey(doc.SessionID, doc.Original, doc.StartTime, doc.EndTime)
	legacyHash := legacyHashKey(doc.SessionID, doc.Original, doc.StartTime, doc.EndTime)
	var id int64
	//nolint:gosec // G701: query only rewrites static SQL identifiers/placeholders; all input values remain bound parameters.
	err = tx.QueryRow(s.query(`SELECT id FROM documents WHERE hash IN (?, ?) LIMIT 1`), h, legacyHash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) && s.postgres {
		id, err = s.insertPostgresDocument(tx, doc, h)
		if err != nil {
			return 0, err
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		//nolint:gosec // G701: query only rewrites static SQL identifiers/placeholders; all input values remain bound parameters.
		res, insertErr := tx.Exec(s.query(`INSERT OR IGNORE INTO documents(session_id, speaker, start_time, end_time, original_text, summary, hash, created_at)
			VALUES(?,?,?,?,?,?,?,?)`),
			doc.SessionID, doc.Speaker, doc.StartTime, doc.EndTime, doc.Original, doc.Summary, h, time.Now().UTC(),
		)
		if insertErr != nil {
			return 0, insertErr
		}
		inserted, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return 0, rowsErr
		}
		if inserted == 1 {
			id, err = res.LastInsertId()
		} else {
			// Another process may have inserted the SHA-256 key after the
			// compatibility lookup but before INSERT OR IGNORE.
			//nolint:gosec // G701: query only rewrites static SQL identifiers/placeholders; all input values remain bound parameters.
			err = tx.QueryRow(s.query(`SELECT id FROM documents WHERE hash=?`), h).Scan(&id)
		}
		if err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}

	// store embedding
	// compute norm
	var norm float64
	for _, v := range vec {
		norm += float64(v * v)
	}
	//nolint:gosec // G701: query only rewrites static SQL identifiers/placeholders; all input values remain bound parameters.
	if _, err := tx.Exec(s.query(`
		INSERT INTO embeddings(doc_id, dim, norm, vector_json)
		VALUES(?,?,?,?)
		ON CONFLICT(doc_id) DO UPDATE SET
			dim=excluded.dim,
			norm=excluded.norm,
			vector_json=excluded.vector_json
	`), id, len(vec), norm, string(jb)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := s.checkpointWALIfOversize(); err != nil {
		return id, err
	}
	return id, nil
}

// UpdateSessionSummary upserts the running summary for the session.
func (s *Store) UpdateSessionSummary(sessionID, summary string) error {
	if err := validateRAGWriteSize(len(sessionID), len(summary)); err != nil {
		return err
	}
	ragSQLiteWriteMu.Lock()
	defer ragSQLiteWriteMu.Unlock()
	if err := s.prepareSQLiteWrite(); err != nil {
		return err
	}
	_, err := s.db.Exec(s.query(`INSERT INTO session_summary(session_id, summary, updated_at) VALUES(?,?,?)
        ON CONFLICT(session_id) DO UPDATE SET summary=excluded.summary, updated_at=excluded.updated_at`), sessionID, summary, time.Now().UTC())
	if err != nil {
		return err
	}
	return s.checkpointWALIfOversize()
}

// GetSessionSummary returns the current summary for the session.
func (s *Store) GetSessionSummary(sessionID string) (string, error) {
	//nolint:gosec // G701: query only rewrites static SQL identifiers/placeholders; all input values remain bound parameters.
	row := s.db.QueryRow(s.query(`SELECT summary FROM session_summary WHERE session_id=?`), sessionID)
	var summary string
	if err := row.Scan(&summary); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return summary, nil
}

// UpdateSessionTitle upserts the session title.
func (s *Store) UpdateSessionTitle(sessionID, title string) error {
	if err := validateRAGWriteSize(len(sessionID), len(title)); err != nil {
		return err
	}
	ragSQLiteWriteMu.Lock()
	defer ragSQLiteWriteMu.Unlock()
	if err := s.prepareSQLiteWrite(); err != nil {
		return err
	}
	// ensure row exists; reuse UpdateSessionSummary path
	_, err := s.db.Exec(s.query(`INSERT INTO session_summary(session_id, summary, updated_at, title) VALUES(?,?,?,?)
        ON CONFLICT(session_id) DO UPDATE SET title=excluded.title, updated_at=excluded.updated_at`), sessionID, "", time.Now().UTC(), title)
	if err != nil {
		return err
	}
	return s.checkpointWALIfOversize()
}

func validateRAGWriteSize(sizes ...int) error {
	total := 0
	for _, size := range sizes {
		if size < 0 || size > maxRAGSerializedWriteBytes-total {
			return fmt.Errorf(
				"%w: maximum serialized payload is %d bytes",
				ErrRAGWriteTooLarge,
				maxRAGSerializedWriteBytes,
			)
		}
		total += size
	}
	return nil
}

// GetSessionTitle returns the cached session title (may be empty).
func (s *Store) GetSessionTitle(sessionID string) (string, error) {
	//nolint:gosec // G701: query only rewrites static SQL identifiers/placeholders; all input values remain bound parameters.
	row := s.db.QueryRow(s.query(`SELECT title FROM session_summary WHERE session_id=?`), sessionID)
	var title sql.NullString
	if err := row.Scan(&title); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if !title.Valid {
		return "", nil
	}
	return title.String, nil
}

func containsIgnoreCase(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// RecentDocuments returns up to N recent docs for a session (for candidate selection).
func (s *Store) RecentDocuments(sessionID string, limit int) ([]Document, error) {
	query := `SELECT id, speaker, start_time, end_time, original_text, summary, created_at
		FROM documents WHERE session_id=? ORDER BY start_time DESC`
	args := []any{sessionID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	//nolint:gosec // G701: query only rewrites static SQL identifiers/placeholders; all input values remain bound parameters.
	rows, err := s.db.Query(s.query(query), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Document
	for rows.Next() {
		var d Document
		d.SessionID = sessionID
		var created string
		if err := rows.Scan(&d.ID, &d.Speaker, &d.StartTime, &d.EndTime, &d.Original, &d.Summary, &created); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339Nano, created)
		d.CreatedAt = t
		out = append(out, d)
	}
	return out, rows.Err()
}

func dedupText(text string) string {
	text = strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(text)), " "))
	return strings.TrimRight(text, ".!?。！？;； ")
}

// DeleteSession removes all RAG state associated with a deleted transcript
// session. Embeddings are removed by the foreign-key cascade.
func (s *Store) DeleteSession(sessionID string) error {
	ragSQLiteWriteMu.Lock()
	defer ragSQLiteWriteMu.Unlock()
	if err := s.prepareSQLiteWrite(); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	//nolint:gosec // G701: query only rewrites static SQL identifiers/placeholders; all input values remain bound parameters.
	if _, err := tx.Exec(s.query(`DELETE FROM documents WHERE session_id=?`), sessionID); err != nil {
		return err
	}
	//nolint:gosec // G701: query only rewrites static SQL identifiers/placeholders; all input values remain bound parameters.
	if _, err := tx.Exec(s.query(`DELETE FROM session_summary WHERE session_id=?`), sessionID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.checkpointWALIfOversize()
}

// LoadEmbeddingsForDocs loads vectors for a set of doc IDs.
func (s *Store) LoadEmbeddingsForDocs(ids []int64) (map[int64][]float32, error) {
	if len(ids) == 0 {
		return map[int64][]float32{}, nil
	}
	// Build simple IN clause
	placeholders := make([]any, 0, len(ids))
	q := "SELECT doc_id, vector_json FROM embeddings WHERE doc_id IN ("
	for i, id := range ids {
		if i > 0 {
			q += ","
		}
		q += "?"
		placeholders = append(placeholders, id)
	}
	q += ")"
	//nolint:gosec // G701: query only rewrites static SQL identifiers/placeholders; all input values remain bound parameters.
	rows, err := s.db.Query(s.query(q), placeholders...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	res := make(map[int64][]float32)
	for rows.Next() {
		var id int64
		var js string
		if err := rows.Scan(&id, &js); err != nil {
			return nil, err
		}
		var vec []float32
		if err := json.Unmarshal([]byte(js), &vec); err != nil {
			return nil, err
		}
		res[id] = vec
	}
	return res, rows.Err()
}
