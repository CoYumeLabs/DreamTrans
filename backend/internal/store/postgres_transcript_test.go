package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestTranscriptUpsertDoesNotRegressTranslatedSegment(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE transcripts (
			id TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(16)))),
			session_id TEXT NOT NULL,
			client_segment_id TEXT NOT NULL,
			speaker TEXT,
			text TEXT NOT NULL,
			translation TEXT,
			translation_group_id TEXT,
			start_time REAL NOT NULL,
			end_time REAL,
			status TEXT NOT NULL,
			is_partial BOOLEAN NOT NULL,
            edit_count INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(session_id, client_segment_id)
		)
	`); err != nil {
		t.Fatal(err)
	}

	upsert := func(speaker, text, status string, translation any, start, end float64, partial bool) {
		t.Helper()
		var translationGroup any
		if translation != nil && translation != "" {
			translationGroup = "translation-group-1"
		}
		var id, createdAt, updatedAt string
		err := db.QueryRowContext(
			context.Background(), transcriptUpsertQuery,
			"session-1", "segment-1", speaker, text, translation,
			translationGroup, start, end, status, partial,
		).Scan(&id, &createdAt, &updatedAt)
		if err != nil {
			t.Fatal(err)
		}
	}

	upsert("final speaker", "final text", "translated", "保留的翻译", 1, 2, false)
	upsert("stale speaker", "stale confirmed text", "confirmed", nil, 10, 20, false)
	upsert("stale speaker", "stale partial text", "partial", "", 30, 40, true)

	var speaker, text, translation, translationGroup, status string
	var start, end float64
	var partial bool
	if err := db.QueryRow(`
		SELECT speaker, text, translation, translation_group_id, start_time, end_time, status, is_partial
		FROM transcripts
		WHERE session_id = ? AND client_segment_id = ?
	`, "session-1", "segment-1").Scan(
		&speaker, &text, &translation, &translationGroup, &start, &end, &status, &partial,
	); err != nil {
		t.Fatal(err)
	}
	if speaker != "final speaker" || text != "final text" || start != 1 || end != 2 {
		t.Fatalf("final content regressed: speaker=%q text=%q start=%v end=%v", speaker, text, start, end)
	}
	if translation != "保留的翻译" {
		t.Fatalf("translation = %q, want preserved translation", translation)
	}
	if translationGroup != "translation-group-1" {
		t.Fatalf("translation group = %q, want preserved group", translationGroup)
	}
	if status != "translated" {
		t.Fatalf("status = %q, want translated", status)
	}
	if partial {
		t.Fatal("translated segment regressed to partial")
	}

	upsert("corrected speaker", "corrected text", "translated", "更正的翻译", 3, 4, false)
	if err := db.QueryRow(`
		SELECT speaker, text, translation, translation_group_id, start_time, end_time, status, is_partial
		FROM transcripts
		WHERE session_id = ? AND client_segment_id = ?
	`, "session-1", "segment-1").Scan(
		&speaker, &text, &translation, &translationGroup, &start, &end, &status, &partial,
	); err != nil {
		t.Fatal(err)
	}
	if speaker != "corrected speaker" || text != "corrected text" ||
		translation != "更正的翻译" || start != 3 || end != 4 {
		t.Fatalf("same-status correction was ignored: speaker=%q text=%q translation=%q start=%v end=%v",
			speaker, text, translation, start, end)
	}
}

func TestRegisterBatchJobRejectsConflictingReservation(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE batch_transcription_jobs (
			job_id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			tenant_id TEXT NOT NULL,
			reservation_key TEXT,
			completed_at TEXT,
			training_route INTEGER NOT NULL DEFAULT 1
		);
		CREATE UNIQUE INDEX idx_batch_jobs_reservation_key
			ON batch_transcription_jobs(reservation_key)
			WHERE reservation_key IS NOT NULL;
	`); err != nil {
		t.Fatal(err)
	}
	store := &PostgresStore{db: db}
	ctx := context.Background()
	if err := store.RegisterBatchJob(ctx, "job-1", "user-1", "tenant-1", "reservation-1", true); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterBatchJob(ctx, "job-1", "user-1", "tenant-1", "reservation-1", true); err != nil {
		t.Fatalf("idempotent registration failed: %v", err)
	}
	if err := store.RegisterBatchJob(ctx, "job-1", "user-1", "tenant-1", "reservation-2", true); !errors.Is(err, ErrBatchJobConflict) {
		t.Fatalf("reservation collision error = %v, want ErrBatchJobConflict", err)
	}
	if err := store.RegisterBatchJob(ctx, "job-1", "user-2", "tenant-1", "reservation-1", true); !errors.Is(err, ErrBatchJobConflict) {
		t.Fatalf("owner collision error = %v, want ErrBatchJobConflict", err)
	}
	key, completed, err := store.GetBatchJobBillingState(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if key != "reservation-1" || completed {
		t.Fatalf("unexpected initial billing state: key=%q completed=%v", key, completed)
	}
	if err := store.MarkBatchJobCompleted(ctx, "job-1", "user-1"); err != nil {
		t.Fatal(err)
	}
	_, completed, err = store.GetBatchJobBillingState(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if !completed {
		t.Fatal("completed state was not persisted")
	}
}

func TestGetTranscriptsPageBySessionUsesStableKeysetCursor(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE transcripts (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			client_segment_id TEXT NOT NULL,
			speaker TEXT NOT NULL,
			text TEXT NOT NULL,
			translation TEXT,
			translation_group_id TEXT,
			start_time REAL NOT NULL,
			end_time REAL,
			status TEXT NOT NULL,
			is_partial BOOLEAN NOT NULL,
            edit_count INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)
	`); err != nil {
		t.Fatal(err)
	}

	type row struct {
		id        string
		sessionID string
		startTime float64
	}
	rows := []row{
		{
			id:        "00000000-0000-4000-8000-000000000001",
			sessionID: "session-1",
			startTime: 1,
		},
		{
			id:        "00000000-0000-4000-8000-000000000002",
			sessionID: "session-1",
			startTime: 1,
		},
		{
			id:        "00000000-0000-4000-8000-000000000003",
			sessionID: "session-1",
			startTime: 2,
		},
		{
			id:        "00000000-0000-4000-8000-000000000004",
			sessionID: "session-1",
			startTime: 3,
		},
		{
			id:        "00000000-0000-4000-8000-000000000005",
			sessionID: "another-session",
			startTime: 0,
		},
	}
	now := time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)
	for index, transcript := range rows {
		if _, err := db.Exec(`
			INSERT INTO transcripts (
				id, session_id, client_segment_id, speaker, text,
				start_time, status, is_partial, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			transcript.id,
			transcript.sessionID,
			"segment-"+transcript.id,
			"S1",
			"text",
			transcript.startTime,
			"confirmed",
			false,
			now.Add(time.Duration(index)*time.Second),
			now.Add(time.Duration(index)*time.Second),
		); err != nil {
			t.Fatal(err)
		}
	}

	postgresStore := &PostgresStore{db: db}
	first, hasMore, err := postgresStore.GetTranscriptsPageBySession(
		context.Background(),
		"session-1",
		2,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMore {
		t.Fatal("first page hasMore = false, want true")
	}
	if len(first) != 2 {
		t.Fatalf("first page length = %d, want 2", len(first))
	}
	if first[0].ID != rows[0].id || first[1].ID != rows[1].id {
		t.Fatalf("first page ids = [%s, %s], want [%s, %s]",
			first[0].ID, first[1].ID, rows[0].id, rows[1].id)
	}

	cursor := &TranscriptPageCursor{
		StartTime: first[len(first)-1].StartTime,
		ID:        first[len(first)-1].ID,
	}
	second, hasMore, err := postgresStore.GetTranscriptsPageBySession(
		context.Background(),
		"session-1",
		2,
		cursor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if hasMore {
		t.Fatal("second page hasMore = true, want false")
	}
	if len(second) != 2 {
		t.Fatalf("second page length = %d, want 2", len(second))
	}
	if second[0].ID != rows[2].id || second[1].ID != rows[3].id {
		t.Fatalf("second page ids = [%s, %s], want [%s, %s]",
			second[0].ID, second[1].ID, rows[2].id, rows[3].id)
	}

	empty, hasMore, err := postgresStore.GetTranscriptsPageBySession(
		context.Background(),
		"session-1",
		2,
		&TranscriptPageCursor{
			StartTime: second[len(second)-1].StartTime,
			ID:        second[len(second)-1].ID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if hasMore || len(empty) != 0 {
		t.Fatalf("exhausted page = %d rows, hasMore=%v; want empty final page", len(empty), hasMore)
	}
}

func TestGetTranscriptsPageBySessionDescendingUsesStableKeysetCursor(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createTranscriptPagingTable(t, db)

	now := time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)
	ids := []string{
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000003",
		"00000000-0000-4000-8000-000000000004",
	}
	starts := []float64{1, 1, 2, 3}
	for index := range ids {
		insertTranscriptPagingRow(
			t,
			db,
			ids[index],
			starts[index],
			nil,
			"confirmed",
			false,
			now.Add(time.Duration(index)*time.Second),
		)
	}

	postgresStore := &PostgresStore{db: db}
	first, hasMore, err := postgresStore.GetTranscriptsPageBySessionDescending(
		context.Background(),
		"session-1",
		2,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMore || len(first) != 2 {
		t.Fatalf("first descending page = %d rows, hasMore=%v", len(first), hasMore)
	}
	if first[0].ID != ids[3] || first[1].ID != ids[2] {
		t.Fatalf(
			"first descending ids = [%s, %s], want [%s, %s]",
			first[0].ID,
			first[1].ID,
			ids[3],
			ids[2],
		)
	}

	cursor := &TranscriptPageCursor{
		StartTime: first[len(first)-1].StartTime,
		ID:        first[len(first)-1].ID,
	}
	second, hasMore, err := postgresStore.GetTranscriptsPageBySessionDescending(
		context.Background(),
		"session-1",
		2,
		cursor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if hasMore || len(second) != 2 {
		t.Fatalf("second descending page = %d rows, hasMore=%v", len(second), hasMore)
	}
	if second[0].ID != ids[1] || second[1].ID != ids[0] {
		t.Fatalf(
			"second descending ids = [%s, %s], want [%s, %s]",
			second[0].ID,
			second[1].ID,
			ids[1],
			ids[0],
		)
	}
}

func TestGetLatestCompleteTranscriptEndUsesCompleteWatermark(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createTranscriptPagingTable(t, db)

	now := time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)
	completeEnd := 7.0
	partialEnd := 99.0
	statusPartialEnd := 100.0
	insertTranscriptPagingRow(
		t, db, "complete-with-end", 1, &completeEnd,
		"confirmed", false, now,
	)
	insertTranscriptPagingRow(
		t, db, "complete-without-end", 5, nil,
		"confirmed", false, now,
	)
	insertTranscriptPagingRow(
		t, db, "flag-partial", 90, &partialEnd,
		"confirmed", true, now,
	)
	insertTranscriptPagingRow(
		t, db, "status-partial", 91, &statusPartialEnd,
		" PARTIAL ", false, now,
	)

	postgresStore := &PostgresStore{db: db}
	latest, ok, err := postgresStore.GetLatestCompleteTranscriptEnd(
		context.Background(),
		"session-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || latest != completeEnd {
		t.Fatalf("complete transcript watermark = %v, ok=%v; want %v", latest, ok, completeEnd)
	}
	_, ok, err = postgresStore.GetLatestCompleteTranscriptEnd(
		context.Background(),
		"missing-session",
	)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("empty session returned a transcript watermark")
	}
}

func createTranscriptPagingTable(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
		CREATE TABLE transcripts (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			client_segment_id TEXT NOT NULL,
			speaker TEXT NOT NULL,
			text TEXT NOT NULL,
			translation TEXT,
			translation_group_id TEXT,
			start_time REAL NOT NULL,
			end_time REAL,
			status TEXT NOT NULL,
			is_partial BOOLEAN NOT NULL,
            edit_count INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)
	`); err != nil {
		t.Fatal(err)
	}
}

func insertTranscriptPagingRow(
	t *testing.T,
	db *sql.DB,
	id string,
	startTime float64,
	endTime *float64,
	status string,
	partial bool,
	now time.Time,
) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO transcripts (
			id, session_id, client_segment_id, speaker, text,
			start_time, end_time, status, is_partial, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		id,
		"session-1",
		"segment-"+id,
		"S1",
		"text",
		startTime,
		endTime,
		status,
		partial,
		now,
		now,
	); err != nil {
		t.Fatal(err)
	}
}

func TestExceedsStorageQuota(t *testing.T) {
	tests := []struct {
		name      string
		quotaGB   int
		usedBytes int64
		want      bool
	}{
		{name: "unlimited", quotaGB: -1, usedBytes: 1 << 62, want: false},
		{name: "zero empty", quotaGB: 0, usedBytes: 0, want: false},
		{name: "zero nonempty", quotaGB: 0, usedBytes: 1, want: true},
		{name: "exact limit", quotaGB: 1, usedBytes: bytesPerGiB, want: false},
		{name: "over limit", quotaGB: 1, usedBytes: bytesPerGiB + 1, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := exceedsStorageQuota(test.quotaGB, test.usedBytes); got != test.want {
				t.Fatalf("exceedsStorageQuota(%d, %d) = %v, want %v",
					test.quotaGB, test.usedBytes, got, test.want)
			}
		})
	}
}
