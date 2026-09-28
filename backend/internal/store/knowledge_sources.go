package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/dreamtrans/backend/internal/models"
	"github.com/lib/pq"
)

type rowScanner interface {
	Scan(dest ...any) error
}

func scanKnowledgeSource(scanner rowScanner, source *models.KnowledgeSource) error {
	defer func() { source.HasOriginal = source.BlobPath != "" }()
	return scanner.Scan(
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
		&source.CreatedAt, &source.UpdatedAt,
	)
}

func (s *PostgresStore) GetKnowledgeSource(
	ctx context.Context, sourceID, projectID, tenantID, userID string,
) (*models.KnowledgeSource, error) {
	var source models.KnowledgeSource
	err := scanKnowledgeSource(s.db.QueryRowContext(ctx, `
		SELECT id, project_id, tenant_id, user_id, source_type, name, media_type,
		       size_bytes, sha256, blob_path, memory_content, ocr_languages,
		       status, error_message, chunk_count, extracted_text_bytes,
		       vector_bytes, index_status, embedding_model, embedding_dimensions,
		       embedded_chunk_count, index_error_message, indexed_at,
		       extract_lease_owner, extract_lease_expires_at, extract_attempts,
		       extract_max_attempts, created_at, updated_at
		FROM knowledge_sources
		WHERE id=$1 AND project_id=$2 AND tenant_id=$3 AND user_id=$4
	`, sourceID, projectID, tenantID, userID), &source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &source, nil
}

type memoryChunkStorage struct {
	extractedTextBytes int64
	vectorBytes        int64
}

func validateMemoryChunks(
	sourceID, projectID string,
	chunks []models.KnowledgeChunk,
) (memoryChunkStorage, error) {
	if len(chunks) == 0 {
		return memoryChunkStorage{}, fmt.Errorf("memory must contain at least one knowledge chunk")
	}
	var storage memoryChunkStorage
	for index := range chunks {
		chunk := &chunks[index]
		chunk.Content = strings.TrimSpace(chunk.Content)
		if chunk.Ordinal != index || chunk.TokenCount < 0 || chunk.Content == "" {
			return memoryChunkStorage{}, fmt.Errorf("invalid memory knowledge chunk")
		}
		chunk.TokenCount = conservativeTokenCount(chunk.Content, chunk.TokenCount)
		if chunk.SourceID != "" && sourceID != "" && chunk.SourceID != sourceID {
			return memoryChunkStorage{}, fmt.Errorf("memory chunk source does not match source")
		}
		if chunk.ProjectID != "" && chunk.ProjectID != projectID {
			return memoryChunkStorage{}, fmt.Errorf("memory chunk project does not match source")
		}
		contentBytes := int64(len(chunk.Content))
		if contentBytes > math.MaxInt64-storage.extractedTextBytes {
			return memoryChunkStorage{}, ErrStorageQuota
		}
		storage.extractedTextBytes += contentBytes
		if len(chunk.Vector) > math.MaxInt64/4 {
			return memoryChunkStorage{}, ErrStorageQuota
		}
		vectorBytes := int64(len(chunk.Vector)) * 4
		if vectorBytes > math.MaxInt64-storage.vectorBytes {
			return memoryChunkStorage{}, ErrStorageQuota
		}
		storage.vectorBytes += vectorBytes
	}
	return storage, nil
}

func normalizeMemorySource(
	source *models.KnowledgeSource,
	chunks []models.KnowledgeChunk,
) (memoryChunkStorage, error) {
	if source == nil {
		return memoryChunkStorage{}, fmt.Errorf("memory source is required")
	}
	source.SourceType = strings.ToLower(strings.TrimSpace(source.SourceType))
	source.Name = strings.TrimSpace(source.Name)
	source.Content = strings.TrimSpace(source.Content)
	if source.SourceType != "memory" {
		return memoryChunkStorage{}, fmt.Errorf("source type must be memory")
	}
	if source.ProjectID == "" || source.TenantID == "" || source.UserID == "" {
		return memoryChunkStorage{}, fmt.Errorf("memory project, tenant, and user are required")
	}
	if source.Name == "" || len([]rune(source.Name)) > 255 {
		return memoryChunkStorage{}, fmt.Errorf(
			"memory name is required and must be at most 255 characters",
		)
	}
	if source.Content == "" || len([]rune(source.Content)) > 1_000_000 {
		return memoryChunkStorage{}, fmt.Errorf(
			"memory content is required and must be at most 1000000 characters",
		)
	}
	if len(source.OCRLanguages) == 0 {
		source.OCRLanguages = []string{"eng", "chi_sim"}
	}
	if err := validateOCRLanguages(source.OCRLanguages); err != nil {
		return memoryChunkStorage{}, err
	}
	source.MediaType = "text/plain"
	source.SizeBytes = int64(len(source.Content))
	return validateMemoryChunks(source.ID, source.ProjectID, chunks)
}

func knowledgeStorageBytesExcludingSourceTx(
	ctx context.Context,
	tx *sql.Tx,
	tenantID string,
	sourceID *string,
) (int64, error) {
	var excludedSource any
	if sourceID != nil && strings.TrimSpace(*sourceID) != "" {
		excludedSource = strings.TrimSpace(*sourceID)
	}
	var usedBytes int64
	err := tx.QueryRowContext(ctx, `
		SELECT
		  COALESCE((
		    SELECT COALESCE(SUM(a.storage_bytes), 0) FROM billing_accounts a JOIN users u ON u.billing_account_id = a.id WHERE u.tenant_id = $1
		  ), 0)
		  + COALESCE((
		      SELECT SUM(size_bytes + extracted_text_bytes + vector_bytes)
		      FROM knowledge_sources
		      WHERE tenant_id=$1
		        AND ($2::uuid IS NULL OR id<>$2)
		    ), 0)
		  + COALESCE((
		      SELECT SUM(content_bytes) FROM ai_artifacts WHERE tenant_id=$1
		    ), 0)
		  + COALESCE((
		      SELECT SUM(
		        octet_length(content)::bigint
		        + CASE WHEN embedding IS NULL THEN 0 ELSE 1536 * 4 END
		      )
		      FROM session_ai_chunks WHERE tenant_id=$1
		    ), 0)
	`, tenantID, excludedSource).Scan(&usedBytes)
	return usedBytes, err
}

func ensureMemoryStorageQuotaTx(
	ctx context.Context,
	tx *sql.Tx,
	tenantID string,
	sourceID *string,
	quotaGB int,
	rawBytes int64,
	chunkStorage memoryChunkStorage,
) error {
	if rawBytes < 0 || chunkStorage.extractedTextBytes < 0 ||
		chunkStorage.vectorBytes < 0 {
		return ErrStorageQuota
	}
	var additionalBytes int64
	for _, additional := range []int64{
		rawBytes,
		chunkStorage.extractedTextBytes,
		chunkStorage.vectorBytes,
	} {
		if additional > math.MaxInt64-additionalBytes {
			return ErrStorageQuota
		}
		additionalBytes += additional
	}
	usedBytes, err := knowledgeStorageBytesExcludingSourceTx(
		ctx, tx, tenantID, sourceID,
	)
	if err != nil {
		return err
	}
	if additionalBytes > math.MaxInt64-usedBytes {
		return ErrStorageQuota
	}
	usedBytes += additionalBytes
	if exceedsStorageQuota(quotaGB, usedBytes) {
		return ErrStorageQuota
	}
	return nil
}

func insertMemoryChunksTx(
	ctx context.Context,
	tx *sql.Tx,
	source *models.KnowledgeSource,
	chunks []models.KnowledgeChunk,
) error {
	for index := range chunks {
		chunk := &chunks[index]
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO knowledge_chunks(
				source_id, project_id, ordinal, content, vector, token_count
			) VALUES(
				$1,$2,$3,$4,
				COALESCE($5::REAL[], ARRAY[]::REAL[]),
				$6
			)
		`, source.ID, source.ProjectID, chunk.Ordinal, chunk.Content,
			pq.Array(chunk.Vector), chunk.TokenCount); err != nil {
			return err
		}
	}
	return nil
}

// CreateMemorySourceWithChunks persists an explicit memory and all of its
// derived chunks in one transaction. The source never becomes visible without
// a complete, quota-accounted chunk set.
func (s *PostgresStore) CreateMemorySourceWithChunks(
	ctx context.Context,
	source *models.KnowledgeSource,
	chunks []models.KnowledgeChunk,
) error {
	_, err := s.CreateMemorySourceWithChunksAndCancelIndexJobs(
		ctx,
		source,
		chunks,
	)
	return err
}

func (s *PostgresStore) CreateMemorySourceWithChunksAndCancelIndexJobs(
	ctx context.Context,
	source *models.KnowledgeSource,
	chunks []models.KnowledgeChunk,
) ([]string, error) {
	chunkStorage, err := normalizeMemorySource(source, chunks)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	cancelledJobIDs, err := coordinateAIIndexScopeMutationTx(
		ctx,
		tx,
		source.TenantID,
		source.UserID,
		"project",
		source.ProjectID,
		aiIndexMutationProject,
		"",
	)
	if err != nil {
		return nil, err
	}
	quotaGB, err := lockTenantStorageQuota(ctx, tx, source.TenantID)
	if err != nil {
		return nil, err
	}
	if err := resetCancelledAIIndexTargetsTx(
		ctx,
		tx,
		source.TenantID,
		source.UserID,
		cancelledJobIDs,
	); err != nil {
		return nil, err
	}
	if err := ensureMemoryStorageQuotaTx(
		ctx,
		tx,
		source.TenantID,
		nil,
		quotaGB,
		source.SizeBytes,
		chunkStorage,
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
			index_error_message
		)
		SELECT $1,$2,$3,'memory',$4,'text/plain',$5,'','',$6,$7,
		       'ready','',$8,$9,$10,'unindexed',0,''
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
		source.SizeBytes, source.Content, pq.Array(source.OCRLanguages),
		len(chunks), chunkStorage.extractedTextBytes,
		chunkStorage.vectorBytes), &created)
	if err != nil {
		return nil, err
	}
	if err := insertMemoryChunksTx(ctx, tx, &created, chunks); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	*source = created
	return cancelledJobIDs, nil
}

// UpdateMemorySourceWithChunks atomically updates an explicit memory. When
// content changes, the old chunks remain intact until the replacement chunks,
// source metadata, and final quota check can all commit together.
func (s *PostgresStore) UpdateMemorySourceWithChunks(
	ctx context.Context,
	sourceID, projectID, tenantID, userID string,
	name, content *string,
	chunks []models.KnowledgeChunk,
) (*models.KnowledgeSource, error) {
	return s.updateMemorySourceWithChunks(
		ctx,
		sourceID,
		projectID,
		tenantID,
		userID,
		name,
		content,
		chunks,
		nil,
	)
}

func (s *PostgresStore) UpdateMemorySourceWithChunksAndCancelIndexJobs(
	ctx context.Context,
	sourceID, projectID, tenantID, userID string,
	name, content *string,
	chunks []models.KnowledgeChunk,
) (*models.KnowledgeSource, []string, error) {
	var cancelledJobIDs []string
	source, err := s.updateMemorySourceWithChunks(
		ctx,
		sourceID,
		projectID,
		tenantID,
		userID,
		name,
		content,
		chunks,
		&cancelledJobIDs,
	)
	return source, cancelledJobIDs, err
}

func (s *PostgresStore) updateMemorySourceWithChunks(
	ctx context.Context,
	sourceID, projectID, tenantID, userID string,
	name, content *string,
	chunks []models.KnowledgeChunk,
	cancelledJobIDsResult *[]string,
) (*models.KnowledgeSource, error) {
	if name == nil && content == nil {
		return nil, fmt.Errorf("memory name or content is required")
	}
	var nextName string
	if name != nil {
		nextName = strings.TrimSpace(*name)
		if nextName == "" || len([]rune(nextName)) > 255 {
			return nil, fmt.Errorf(
				"memory name is required and must be at most 255 characters",
			)
		}
	}
	var (
		nextContent  string
		chunkStorage memoryChunkStorage
		err          error
	)
	if content != nil {
		nextContent = strings.TrimSpace(*content)
		if nextContent == "" || len([]rune(nextContent)) > 1_000_000 {
			return nil, fmt.Errorf(
				"memory content is required and must be at most 1000000 characters",
			)
		}
		chunkStorage, err = validateMemoryChunks(sourceID, projectID, chunks)
		if err != nil {
			return nil, err
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	mutationImpact := aiIndexMutationLockOnly
	if content != nil {
		mutationImpact = aiIndexMutationSource
	}
	cancelledJobIDs, err := coordinateAIIndexScopeMutationTx(
		ctx,
		tx,
		tenantID,
		userID,
		"project",
		projectID,
		mutationImpact,
		sourceID,
	)
	if err != nil {
		return nil, err
	}
	quotaGB, err := lockTenantStorageQuota(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	if err := resetCancelledAIIndexTargetsTx(
		ctx,
		tx,
		tenantID,
		userID,
		cancelledJobIDs,
	); err != nil {
		return nil, err
	}

	var current models.KnowledgeSource
	err = scanKnowledgeSource(tx.QueryRowContext(ctx, `
		SELECT id, project_id, tenant_id, user_id, source_type, name, media_type,
		       size_bytes, sha256, blob_path, memory_content, ocr_languages,
		       status, error_message, chunk_count, extracted_text_bytes,
		       vector_bytes, index_status, embedding_model, embedding_dimensions,
		       embedded_chunk_count, index_error_message, indexed_at,
		       extract_lease_owner, extract_lease_expires_at, extract_attempts,
		       extract_max_attempts, created_at, updated_at
		FROM knowledge_sources
		WHERE id=$1 AND project_id=$2 AND tenant_id=$3 AND user_id=$4
		  AND source_type='memory'
		FOR UPDATE
	`, sourceID, projectID, tenantID, userID), &current)
	if err != nil {
		return nil, err
	}
	if name != nil {
		current.Name = nextName
	}
	contentChanged := content != nil && nextContent != current.Content
	if contentChanged {
		current.Content = nextContent
		current.SizeBytes = int64(len(nextContent))
		if err := ensureMemoryStorageQuotaTx(
			ctx,
			tx,
			tenantID,
			&sourceID,
			quotaGB,
			current.SizeBytes,
			chunkStorage,
		); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM knowledge_chunks WHERE source_id=$1
		`, sourceID); err != nil {
			return nil, err
		}
		if err := insertMemoryChunksTx(ctx, tx, &current, chunks); err != nil {
			return nil, err
		}
	}

	var updated models.KnowledgeSource
	err = scanKnowledgeSource(tx.QueryRowContext(ctx, `
		UPDATE knowledge_sources
		SET name=$1,
		    memory_content=CASE WHEN $2 THEN $3 ELSE memory_content END,
		    size_bytes=CASE WHEN $2 THEN $4 ELSE size_bytes END,
		    status=CASE WHEN $2 THEN 'ready' ELSE status END,
		    error_message=CASE WHEN $2 THEN '' ELSE error_message END,
		    chunk_count=CASE WHEN $2 THEN $5 ELSE chunk_count END,
		    extracted_text_bytes=CASE WHEN $2 THEN $6 ELSE extracted_text_bytes END,
		    vector_bytes=CASE WHEN $2 THEN $7 ELSE vector_bytes END,
		    index_status=CASE
		      WHEN NOT $2 THEN index_status
		      WHEN index_status IN ('ready', 'stale') THEN 'stale'
		      ELSE 'unindexed'
		    END,
		    embedded_chunk_count=CASE WHEN $2 THEN 0 ELSE embedded_chunk_count END,
		    index_error_message=CASE WHEN $2 THEN '' ELSE index_error_message END
		WHERE id=$8 AND project_id=$9 AND tenant_id=$10 AND user_id=$11
		  AND source_type='memory'
		RETURNING id, project_id, tenant_id, user_id, source_type, name, media_type,
		          size_bytes, sha256, blob_path, memory_content, ocr_languages,
		          status, error_message, chunk_count, extracted_text_bytes,
		          vector_bytes, index_status, embedding_model, embedding_dimensions,
		          embedded_chunk_count, index_error_message, indexed_at,
		          extract_lease_owner, extract_lease_expires_at, extract_attempts,
		          extract_max_attempts, created_at, updated_at
	`, current.Name, contentChanged, current.Content, current.SizeBytes,
		len(chunks), chunkStorage.extractedTextBytes, chunkStorage.vectorBytes,
		sourceID, projectID, tenantID, userID), &updated)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if cancelledJobIDsResult != nil {
		*cancelledJobIDsResult = cancelledJobIDs
	}
	return &updated, nil
}

// UpdateMemorySource is retained for name-only compatibility. Content changes
// must provide their replacement chunks through UpdateMemorySourceWithChunks;
// accepting a two-step content update here would reintroduce a data-loss window.
func (s *PostgresStore) UpdateMemorySource(
	ctx context.Context,
	sourceID, projectID, tenantID, userID string,
	name, content *string,
) (*models.KnowledgeSource, error) {
	if content != nil {
		return nil, fmt.Errorf(
			"memory content updates require atomic replacement chunks",
		)
	}
	return s.UpdateMemorySourceWithChunks(
		ctx,
		sourceID,
		projectID,
		tenantID,
		userID,
		name,
		nil,
		nil,
	)
}

func (s *PostgresStore) RetryKnowledgeSource(
	ctx context.Context, sourceID, projectID, tenantID, userID string,
) (*models.KnowledgeSource, error) {
	var source models.KnowledgeSource
	err := scanKnowledgeSource(s.db.QueryRowContext(ctx, `
		UPDATE knowledge_sources
		SET status='queued',
		    error_message='',
		    extract_lease_owner='',
		    extract_lease_expires_at=NULL,
		    extract_attempts=0
		WHERE id=$1 AND project_id=$2 AND tenant_id=$3 AND user_id=$4
		  AND source_type='file' AND status='error'
		RETURNING id, project_id, tenant_id, user_id, source_type, name, media_type,
		          size_bytes, sha256, blob_path, memory_content, ocr_languages,
		          status, error_message, chunk_count, extracted_text_bytes,
		          vector_bytes, index_status, embedding_model, embedding_dimensions,
		          embedded_chunk_count, index_error_message, indexed_at,
		          extract_lease_owner, extract_lease_expires_at, extract_attempts,
		          extract_max_attempts, created_at, updated_at
	`, sourceID, projectID, tenantID, userID), &source)
	if err != nil {
		return nil, err
	}
	return &source, nil
}

func (s *PostgresStore) ClaimKnowledgeSourcesForExtraction(
	ctx context.Context, workerID string, limit int, leaseDuration time.Duration,
) ([]models.KnowledgeSource, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return nil, fmt.Errorf("worker id is required")
	}
	if limit <= 0 || limit > 128 {
		limit = 16
	}
	if leaseDuration <= 0 || leaseDuration > time.Hour {
		leaseDuration = 3 * time.Minute
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		UPDATE knowledge_sources
		SET status='error',
		    error_message='extraction retry limit reached',
		    extract_lease_owner='',
		    extract_lease_expires_at=NULL
		WHERE source_type='file'
		  AND status='processing'
		  AND (
		    extract_lease_expires_at IS NULL
		    OR extract_lease_expires_at < NOW()
		  )
		  AND extract_attempts >= extract_max_attempts
	`); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, `
		WITH candidates AS (
		  SELECT id
		  FROM knowledge_sources
		  WHERE source_type='file'
		    AND (
		      status='queued'
		      OR (
		        status='processing'
		        AND (
		          extract_lease_expires_at IS NULL
		          OR extract_lease_expires_at < NOW()
		        )
		      )
		    )
		    AND extract_attempts < extract_max_attempts
		  ORDER BY created_at ASC
		  FOR UPDATE SKIP LOCKED
		  LIMIT $1
		)
		UPDATE knowledge_sources ks
		SET status='processing',
		    error_message='',
		    extract_lease_owner=$2,
		    extract_lease_expires_at=NOW() + ($3 * INTERVAL '1 second'),
		    extract_attempts=ks.extract_attempts + 1
		FROM candidates c
		WHERE ks.id=c.id
		RETURNING ks.id, ks.project_id, ks.tenant_id, ks.user_id,
		          ks.source_type, ks.name, ks.media_type, ks.size_bytes,
		          ks.sha256, ks.blob_path, ks.memory_content, ks.ocr_languages,
		          ks.status, ks.error_message, ks.chunk_count,
		          ks.extracted_text_bytes, ks.vector_bytes, ks.index_status,
		          ks.embedding_model, ks.embedding_dimensions,
		          ks.embedded_chunk_count, ks.index_error_message, ks.indexed_at,
		          ks.extract_lease_owner, ks.extract_lease_expires_at,
		          ks.extract_attempts, ks.extract_max_attempts,
		          ks.created_at, ks.updated_at
	`, limit, workerID, leaseDuration.Seconds())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	sources := make([]models.KnowledgeSource, 0, limit)
	for rows.Next() {
		var source models.KnowledgeSource
		if err := scanKnowledgeSource(rows, &source); err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sources, nil
}

func (s *PostgresStore) RenewKnowledgeSourceExtractionLease(
	ctx context.Context, sourceID, workerID string, leaseDuration time.Duration,
) (bool, error) {
	if leaseDuration <= 0 || leaseDuration > time.Hour {
		leaseDuration = 3 * time.Minute
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE knowledge_sources
		SET extract_lease_expires_at=NOW() + ($3 * INTERVAL '1 second')
		WHERE id=$1 AND status='processing' AND extract_lease_owner=$2
		  AND extract_lease_expires_at > NOW()
	`, sourceID, workerID, leaseDuration.Seconds())
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

func (s *PostgresStore) FailKnowledgeSourceExtraction(
	ctx context.Context, sourceID, workerID, errorMessage string,
) error {
	errorMessage = truncateRunes(strings.TrimSpace(errorMessage))
	result, err := s.db.ExecContext(ctx, `
		UPDATE knowledge_sources
		SET status='error',
		    error_message=$3,
		    extract_lease_owner='',
		    extract_lease_expires_at=NULL
		WHERE id=$1 AND status='processing' AND extract_lease_owner=$2
		  AND extract_lease_expires_at > NOW()
	`, sourceID, workerID, errorMessage)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrLeaseLost
	}
	return nil
}

func (s *PostgresStore) UpdateKnowledgeSourceIndexStatus(
	ctx context.Context,
	sourceID, projectID, tenantID, userID, status, model string,
	dimensions, embeddedChunkCount int,
	errorMessage string,
) error {
	if !validAIIndexStatus(status) {
		return fmt.Errorf("invalid index status")
	}
	if dimensions != 0 && dimensions != 1536 {
		return fmt.Errorf("embedding dimensions must be 1536")
	}
	if embeddedChunkCount < 0 {
		return fmt.Errorf("embedded chunk count cannot be negative")
	}
	errorMessage = truncateRunes(strings.TrimSpace(errorMessage))
	result, err := s.db.ExecContext(ctx, `
		UPDATE knowledge_sources
		SET index_status=$1,
		    embedding_model=$2,
		    embedding_dimensions=$3,
		    embedded_chunk_count=$4,
		    index_error_message=$5,
		    indexed_at=CASE WHEN $1='ready' THEN NOW() ELSE indexed_at END
		WHERE id=$6 AND project_id=$7 AND tenant_id=$8 AND user_id=$9
	`, status, model, dimensions, embeddedChunkCount, errorMessage,
		sourceID, projectID, tenantID, userID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func validateOCRLanguages(languages []string) error {
	if len(languages) == 0 || len(languages) > 4 {
		return fmt.Errorf("between one and four OCR languages are required")
	}
	allowed := map[string]struct{}{
		"eng": {}, "chi_sim": {}, "jpn": {}, "kor": {},
	}
	seen := make(map[string]struct{}, len(languages))
	for _, language := range languages {
		if _, ok := allowed[language]; !ok {
			return fmt.Errorf("unsupported OCR language %q", language)
		}
		if _, duplicate := seen[language]; duplicate {
			return fmt.Errorf("duplicate OCR language %q", language)
		}
		seen[language] = struct{}{}
	}
	return nil
}

func validAIIndexStatus(status string) bool {
	switch status {
	case models.AIIndexStatusUnindexed,
		models.AIIndexStatusQueued,
		models.AIIndexStatusProcessing,
		models.AIIndexStatusReady,
		models.AIIndexStatusStale,
		models.AIIndexStatusError:
		return true
	default:
		return false
	}
}

func truncateRunes(value string) string {
	const limit = 1_000
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}
