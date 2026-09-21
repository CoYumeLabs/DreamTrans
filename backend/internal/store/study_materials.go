package store

import (
	"context"

	"github.com/lib/pq"
)

type studyVersionKey struct{}

func WithStudyContentVersion(ctx context.Context, version string) context.Context {
	return context.WithValue(ctx, studyVersionKey{}, version)
}

func StudyContentVersion(ctx context.Context) string {
	version, _ := ctx.Value(studyVersionKey{}).(string)
	return version
}

// Fingerprint only learning inputs, not semantic-index or billing metadata.
// Chunk IDs change on re-extraction; transcript count/max(updated_at) detect
// appended, edited and deleted lines. All subqueries are owner scoped.
func (s *PostgresStore) StudyMaterialFingerprint(ctx context.Context, projectID, userID string) (string, bool, error) {
	var fingerprint string
	var pending bool
	err := s.db.QueryRowContext(ctx, `
	 SELECT md5(COALESCE((SELECT string_agg(row(s.id,s.name,s.status,s.sha256,
	   (SELECT string_agg(row(c.id,md5(c.content))::text, ',' ORDER BY c.ordinal) FROM knowledge_chunks c WHERE c.source_id=s.id))::text, '|' ORDER BY s.id)
	 FROM knowledge_sources s WHERE s.project_id=$1 AND s.user_id=$2), '') || '|' ||
	 COALESCE((SELECT string_agg(row(se.id,se.title,
	   (SELECT row(count(*),max(t.updated_at))::text FROM transcripts t WHERE t.session_id=se.id))::text, '|' ORDER BY se.id)
	 FROM project_sessions ps JOIN sessions se ON se.id=ps.session_id WHERE ps.project_id=$1 AND se.user_id=$2), '')),
	 EXISTS(SELECT 1 FROM knowledge_sources WHERE project_id=$1 AND user_id=$2 AND status IN ('queued','processing'))
	`, projectID, userID).Scan(&fingerprint, &pending)
	return fingerprint, pending, err
}

type StudyEvidence struct {
	SourceID, Title, Content string
	Ordinal                  int
}

// Free lexical retrieval, including evidence-source preference, requires no
// paid embeddings. Return bounded excerpts rather than whole uploaded files.
func (s *PostgresStore) StudyEvidence(ctx context.Context, projectID, userID, query string, sourceIDs []string) ([]StudyEvidence, error) {
	rows, err := s.db.QueryContext(ctx, `
	 SELECT c.source_id, s.name, c.ordinal, left(c.content,1800)
	 FROM knowledge_chunks c JOIN knowledge_sources s ON s.id=c.source_id
	 WHERE c.project_id=$1 AND s.user_id=$2 AND s.status='ready'
	 ORDER BY (c.source_id=ANY($4::uuid[])) DESC,
	 GREATEST(similarity(c.content,$3),word_similarity($3,c.content)) DESC, c.source_id,c.ordinal
	 LIMIT 8`, projectID, userID, query, pq.Array(sourceIDs))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []StudyEvidence
	for rows.Next() {
		var item StudyEvidence
		if err := rows.Scan(&item.SourceID, &item.Title, &item.Ordinal, &item.Content); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
