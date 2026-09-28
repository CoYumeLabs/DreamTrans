package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dreamtrans/backend/internal/models"
	"github.com/lib/pq"
)

// LMS-synced knowledge sources (migration 032). The browser extension sends
// derived text only; the row stores that text like a memory, plus the
// provenance JSON it needs for incremental sync.

// LMSSourceRef is what the extension asks for before syncing: enough to skip
// modules the server already holds.
type LMSSourceRef struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	SHA256    string          `json:"sha256"`
	SizeBytes int64           `json:"size_bytes"`
	LMS       json.RawMessage `json:"lms"`
	CreatedAt string          `json:"created_at"`
	// HasOriginal tells the extension whether the file itself is kept, so an
	// opted-in sync can attach originals to materials synced text-only.
	HasOriginal bool `json:"has_original"`
}

func normalizeLMSSource(
	source *models.KnowledgeSource,
	chunks []models.KnowledgeChunk,
) (memoryChunkStorage, error) {
	if source == nil {
		return memoryChunkStorage{}, fmt.Errorf("lms source is required")
	}
	source.SourceType = strings.ToLower(strings.TrimSpace(source.SourceType))
	source.Name = strings.TrimSpace(source.Name)
	source.Content = strings.TrimSpace(source.Content)
	source.SHA256 = strings.ToLower(strings.TrimSpace(source.SHA256))
	if source.SourceType != "lms" {
		return memoryChunkStorage{}, fmt.Errorf("source type must be lms")
	}
	if source.ProjectID == "" || source.TenantID == "" || source.UserID == "" {
		return memoryChunkStorage{}, fmt.Errorf("lms project, tenant, and user are required")
	}
	if source.Name == "" || len([]rune(source.Name)) > 255 {
		return memoryChunkStorage{}, fmt.Errorf("lms source name is required and must be at most 255 characters")
	}
	if source.Content == "" || len([]rune(source.Content)) > 1_000_000 {
		return memoryChunkStorage{}, fmt.Errorf("lms source content is required and must be at most 1000000 characters")
	}
	if len(source.SHA256) != 64 {
		return memoryChunkStorage{}, fmt.Errorf("lms source sha256 is required")
	}
	if strings.TrimSpace(source.MediaType) == "" {
		source.MediaType = "text/plain"
	}
	if len(source.LMS) == 0 || !json.Valid(source.LMS) {
		source.LMS = json.RawMessage(`{}`)
	}
	if len(source.OCRLanguages) == 0 {
		source.OCRLanguages = []string{"eng", "chi_sim"}
	}
	if err := validateOCRLanguages(source.OCRLanguages); err != nil {
		return memoryChunkStorage{}, err
	}
	if source.SizeBytes <= 0 {
		source.SizeBytes = int64(len(source.Content))
	}
	return validateMemoryChunks(source.ID, source.ProjectID, chunks)
}

// CreateLMSSourceWithChunks stores one synced material atomically: the row,
// its chunks, and the tenant storage quota check. The partial unique index
// on (project_id, sha256) rejects a second copy of the same file in the
// same course; callers look it up first and treat that as a duplicate.
func (s *PostgresStore) CreateLMSSourceWithChunks(
	ctx context.Context,
	source *models.KnowledgeSource,
	chunks []models.KnowledgeChunk,
) ([]string, error) {
	chunkStorage, err := normalizeLMSSource(source, chunks)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	cancelledJobIDs, err := coordinateAIIndexScopeMutationTx(
		ctx, tx, source.TenantID, source.UserID, "project", source.ProjectID,
		aiIndexMutationProject, "",
	)
	if err != nil {
		return nil, err
	}
	quotaGB, err := lockTenantStorageQuota(ctx, tx, source.TenantID)
	if err != nil {
		return nil, err
	}
	if err := resetCancelledAIIndexTargetsTx(
		ctx, tx, source.TenantID, source.UserID, cancelledJobIDs,
	); err != nil {
		return nil, err
	}
	if err := ensureMemoryStorageQuotaTx(
		ctx, tx, source.TenantID, nil, quotaGB, source.SizeBytes, chunkStorage,
	); err != nil {
		return nil, err
	}

	var created models.KnowledgeSource
	err = scanKnowledgeSource(tx.QueryRowContext(ctx, `
		INSERT INTO knowledge_sources (
			project_id, tenant_id, user_id, source_type, name, media_type,
			size_bytes, sha256, blob_path, memory_content, ocr_languages,
			status, error_message, chunk_count, extracted_text_bytes,
			vector_bytes, index_status, embedded_chunk_count,
			index_error_message, lms
		)
		SELECT $1,$2,$3,'lms',$4,$5,$6,$7,'',$8,$9,
		       'ready','',$10,$11,$12,'unindexed',0,'',$13
		FROM ai_projects
		WHERE id=$1 AND tenant_id=$2 AND user_id=$3
		RETURNING id, project_id, tenant_id, user_id, source_type, name, media_type,
		          size_bytes, sha256, blob_path, memory_content, ocr_languages,
		          status, error_message, chunk_count, extracted_text_bytes,
		          vector_bytes, index_status, embedding_model, embedding_dimensions,
		          embedded_chunk_count, index_error_message, indexed_at,
		          extract_lease_owner, extract_lease_expires_at, extract_attempts,
		          extract_max_attempts, created_at, updated_at
	`, source.ProjectID, source.TenantID, source.UserID, source.Name,
		source.MediaType, source.SizeBytes, source.SHA256, source.Content,
		pq.Array(source.OCRLanguages), len(chunks),
		chunkStorage.extractedTextBytes, chunkStorage.vectorBytes,
		[]byte(source.LMS)), &created)
	if err != nil {
		return nil, err
	}
	if err := insertMemoryChunksTx(ctx, tx, &created, chunks); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	created.LMS = source.LMS
	*source = created
	return cancelledJobIDs, nil
}

// GetKnowledgeSourceBySHA256 finds the course's copy of a file by content
// hash, or nil. Used to make the extension's uploads idempotent.
func (s *PostgresStore) GetKnowledgeSourceBySHA256(
	ctx context.Context, projectID, userID, sha256 string,
) (*models.KnowledgeSource, error) {
	sha256 = strings.ToLower(strings.TrimSpace(sha256))
	if sha256 == "" {
		return nil, nil
	}
	var source models.KnowledgeSource
	var lms []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT id, project_id, tenant_id, user_id, source_type, name, media_type,
		       size_bytes, sha256, blob_path, memory_content, ocr_languages,
		       status, error_message, chunk_count, extracted_text_bytes,
		       vector_bytes, index_status, embedding_model, embedding_dimensions,
		       embedded_chunk_count, index_error_message, indexed_at,
		       extract_lease_owner, extract_lease_expires_at, extract_attempts,
		       extract_max_attempts, created_at, updated_at, lms
		FROM knowledge_sources
		WHERE project_id=$1 AND user_id=$2 AND sha256=$3
		LIMIT 1
	`, projectID, userID, sha256).Scan(
		&source.ID, &source.ProjectID, &source.TenantID, &source.UserID,
		&source.SourceType, &source.Name, &source.MediaType,
		&source.SizeBytes, &source.SHA256, &source.BlobPath, &source.Content,
		pq.Array(&source.OCRLanguages), &source.Status, &source.ErrorMessage,
		&source.ChunkCount, &source.ExtractedTextBytes, &source.VectorBytes,
		&source.IndexStatus, &source.EmbeddingModel,
		&source.EmbeddingDimensions, &source.EmbeddedChunkCount,
		&source.IndexErrorMessage, &source.IndexedAt,
		&source.ExtractLeaseOwner, &source.ExtractLeaseExpiresAt,
		&source.ExtractAttempts, &source.ExtractMaxAttempts,
		&source.CreatedAt, &source.UpdatedAt, &lms,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(lms) > 0 {
		source.LMS = json.RawMessage(lms)
	}
	source.HasOriginal = source.BlobPath != ""
	// The stored text is not needed by callers that only check existence.
	source.Content = ""
	return &source, nil
}

// ListLMSSources returns every synced material in a course with its
// provenance, newest first.
func (s *PostgresStore) ListLMSSources(
	ctx context.Context, projectID, userID string,
) ([]LMSSourceRef, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, sha256, size_bytes, lms, created_at, blob_path <> ''
		FROM knowledge_sources
		WHERE project_id=$1 AND user_id=$2 AND source_type='lms'
		ORDER BY created_at DESC
	`, projectID, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	refs := make([]LMSSourceRef, 0)
	for rows.Next() {
		var ref LMSSourceRef
		var lms []byte
		var createdAt sql.NullTime
		if err := rows.Scan(
			&ref.ID, &ref.Name, &ref.SHA256, &ref.SizeBytes, &lms, &createdAt, &ref.HasOriginal,
		); err != nil {
			return nil, err
		}
		if len(lms) > 0 {
			ref.LMS = json.RawMessage(lms)
		} else {
			ref.LMS = json.RawMessage(`{}`)
		}
		if createdAt.Valid {
			ref.CreatedAt = createdAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// AttachLMSSourceOriginal records where a synced material's original file
// was stored. It only fills an empty slot: false means the source is gone,
// is not a synced material, or already has its original, and the caller
// removes the blob it just wrote.
func (s *PostgresStore) AttachLMSSourceOriginal(
	ctx context.Context, sourceID, projectID, tenantID, userID, blobPath string,
) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE knowledge_sources
		SET blob_path=$5, updated_at=NOW()
		WHERE id=$1 AND project_id=$2 AND tenant_id=$3 AND user_id=$4
		  AND source_type='lms' AND blob_path=''
	`, sourceID, projectID, tenantID, userID, blobPath)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}
