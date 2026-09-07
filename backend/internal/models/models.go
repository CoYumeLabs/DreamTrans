// Package models defines DreamTrans persistence and API data structures.
package models

import (
	"encoding/json"
	"time"
)

// Tenant represents an organization/company in the multi-tenant system
type Tenant struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
	Plan string `json:"plan"` // free, pro, enterprise
	// Kind is personal or institution; institutions never reach the
	// training provider account. SpeechmaticsRoute pins the whole tenant.
	Kind              string    `json:"kind"`
	SpeechmaticsRoute string    `json:"speechmatics_route"`
	APIQuotaMonthly   int       `json:"api_quota_monthly"`
	StorageQuotaGB    int       `json:"storage_quota_gb"`
	MaxSessions       int       `json:"max_sessions"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// User represents a user account
type User struct {
	ID            string `json:"id"`
	TenantID      string `json:"tenant_id"`
	Email         string `json:"email"`
	PasswordHash  string `json:"-"` // Never expose password hash in JSON
	Name          string `json:"name"`
	Role          string `json:"role"` // user, admin, super_admin
	IsActive      bool   `json:"is_active"`
	EmailVerified bool   `json:"email_verified"`
	// TrainingOptIn records the user's answer to the Speechmatics training
	// program: nil = not asked yet, false = declined, true = joined.
	TrainingOptIn *bool `json:"training_opt_in"`
	// SpeechmaticsRoute is an administrator pin ("training"/"standard"),
	// empty when the account follows the normal rules.
	SpeechmaticsRoute string     `json:"speechmatics_route"`
	LastLoginAt       *time.Time `json:"last_login_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// UserWithTenant includes tenant information
type UserWithTenant struct {
	User
	Tenant *Tenant `json:"tenant,omitempty"`
}

// Session represents a transcription session
type Session struct {
	ID              string `json:"id"`
	UserID          string `json:"user_id"`
	TenantID        string `json:"tenant_id"`
	Title           string `json:"title"`
	SourceLanguage  string `json:"source_language"`
	TargetLanguage  string `json:"target_language"`
	DurationSeconds int    `json:"duration_seconds"`
	Status          string `json:"status"` // active, paused, completed, archived
	// AI project this session is linked to (project_sessions row), when any.
	// Only list endpoints populate it; a session belongs to at most one project.
	ProjectID *string `json:"project_id,omitempty"`
	// How the project link was made: "manual" or "timetable". Only the
	// project session list populates it.
	AssignedBy string     `json:"assigned_by,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// SessionWithTranscripts includes transcript data
type SessionWithTranscripts struct {
	Session
	// Keep the collection present for empty sessions. Returning an omitted or
	// null field forces every client to special-case a perfectly valid session
	// that simply contains no speech yet.
	Transcripts []Transcript `json:"transcripts"`
}

// Transcript represents a single transcript segment
type Transcript struct {
	ID                 string    `json:"id"`
	SessionID          string    `json:"session_id"`
	ClientSegmentID    string    `json:"client_segment_id"`
	TranslationGroupID *string   `json:"translation_group_id,omitempty"`
	Speaker            string    `json:"speaker"`
	Text               string    `json:"text"`
	Translation        *string   `json:"translation,omitempty"`
	StartTime          float64   `json:"start_time"`
	EndTime            *float64  `json:"end_time,omitempty"`
	Status             string    `json:"status"` // partial, confirmed, translated
	IsPartial          bool      `json:"is_partial"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

const (
	AIIndexStatusUnindexed  = "unindexed"
	AIIndexStatusQueued     = "queued"
	AIIndexStatusProcessing = "processing"
	AIIndexStatusReady      = "ready"
	AIIndexStatusStale      = "stale"
	AIIndexStatusError      = "error"

	AIIndexJobStatusQueued     = "queued"
	AIIndexJobStatusProcessing = "processing"
	AIIndexJobStatusReady      = "ready"
	AIIndexJobStatusError      = "error"
	AIIndexJobStatusCancelled  = "cancelled"

	AIRetrievalModeNone            = "none"
	AIRetrievalModeHybrid          = "hybrid"
	AIRetrievalModeSemantic        = "semantic"
	AIRetrievalModeLexicalFallback = "lexical_fallback"
	AIRetrievalModeLegacy          = "legacy"

	AIGenerationStatusProcessing = "processing"
	AIGenerationStatusReady      = "ready"
	AIGenerationStatusError      = "error"

	AIGenerationOutcomeAcquired     = "acquired"
	AIGenerationOutcomeReplay       = "ready_replay"
	AIGenerationOutcomeInProgress   = "in_progress"
	AIGenerationOutcomeHashConflict = "hash_conflict"
)

type AIArtifact struct {
	ID              string          `json:"id"`
	TenantID        string          `json:"tenant_id,omitempty"`
	UserID          string          `json:"user_id,omitempty"`
	SessionID       *string         `json:"session_id,omitempty"`
	ProjectID       *string         `json:"project_id,omitempty"`
	ArtifactType    string          `json:"artifact_type"`
	Title           string          `json:"title"`
	Content         string          `json:"content"`
	ContextPolicy   map[string]any  `json:"context_policy"`
	ContextTokens   int             `json:"context_tokens"`
	Model           string          `json:"model,omitempty"`
	ClientRequestID string          `json:"client_request_id,omitempty"`
	RequestHash     string          `json:"-"`
	ReplayResponse  json.RawMessage `json:"-"`
	ContentBytes    int64           `json:"content_bytes,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type AIProject struct {
	ID               string `json:"id"`
	TenantID         string `json:"tenant_id,omitempty"`
	UserID           string `json:"user_id,omitempty"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	ContextMode      string `json:"context_mode"`
	MaxContextTokens int    `json:"max_context_tokens"`
	// Monday of teaching week 1 (YYYY-MM-DD); nil = inferred from sessions.
	WeekStart *string   `json:"week_start,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CourseSlot is one weekly class time of a course (an AI project). Times are
// wall-clock "HH:MM" in Timezone; a slot never crosses midnight.
type CourseSlot struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	// Set only by the user-wide listing so a calendar can name the course.
	ProjectName string `json:"project_name,omitempty"`
	// ISO weekday: 1 = Monday … 7 = Sunday.
	Weekday     int       `json:"weekday"`
	StartMinute int       `json:"-"`
	EndMinute   int       `json:"-"`
	Start       string    `json:"start"`
	End         string    `json:"end"`
	Timezone    string    `json:"timezone"`
	Label       string    `json:"label"`
	CreatedAt   time.Time `json:"created_at"`
}

type KnowledgeSource struct {
	ID           string   `json:"id"`
	ProjectID    string   `json:"project_id"`
	TenantID     string   `json:"tenant_id,omitempty"`
	UserID       string   `json:"user_id,omitempty"`
	SourceType   string   `json:"source_type"`
	Name         string   `json:"name"`
	MediaType    string   `json:"media_type"`
	SizeBytes    int64    `json:"size_bytes"`
	SHA256       string   `json:"sha256,omitempty"`
	BlobPath     string   `json:"-"`
	Content      string   `json:"content,omitempty"`
	OCRLanguages []string `json:"ocr_languages,omitempty"`
	// Where an LMS-synced source came from (source_type "lms" only).
	LMS                   json.RawMessage `json:"lms,omitempty"`
	Status                string          `json:"status"`
	ErrorMessage          string          `json:"error_message,omitempty"`
	ChunkCount            int             `json:"chunk_count"`
	ExtractedTextBytes    int64           `json:"extracted_text_bytes,omitempty"`
	VectorBytes           int64           `json:"vector_bytes,omitempty"`
	IndexStatus           string          `json:"index_status"`
	EmbeddingModel        string          `json:"embedding_model,omitempty"`
	EmbeddingDimensions   int             `json:"embedding_dimensions,omitempty"`
	EmbeddedChunkCount    int             `json:"embedded_chunk_count"`
	IndexErrorMessage     string          `json:"index_error_message,omitempty"`
	IndexedAt             *time.Time      `json:"indexed_at,omitempty"`
	ExtractLeaseOwner     string          `json:"-"`
	ExtractLeaseExpiresAt *time.Time      `json:"-"`
	ExtractAttempts       int             `json:"-"`
	ExtractMaxAttempts    int             `json:"-"`
	CreatedAt             time.Time       `json:"created_at"`
	UpdatedAt             time.Time       `json:"updated_at"`
}

type KnowledgeChunk struct {
	ID              string     `json:"id"`
	SourceID        string     `json:"source_id"`
	ProjectID       string     `json:"project_id"`
	Ordinal         int        `json:"ordinal"`
	Content         string     `json:"content"`
	Vector          []float64  `json:"-"`
	Embedding       []float64  `json:"-"`
	EmbeddingModel  string     `json:"embedding_model,omitempty"`
	EmbeddingStatus string     `json:"embedding_status,omitempty"`
	EmbeddingError  string     `json:"embedding_error,omitempty"`
	TokenCount      int        `json:"token_count,omitempty"`
	EmbeddedAt      *time.Time `json:"embedded_at,omitempty"`
	SourceName      string     `json:"source_name,omitempty"`
	RetrievalScore  float64    `json:"retrieval_score,omitempty"`
}

type SessionAIChunk struct {
	ID              string     `json:"id"`
	TenantID        string     `json:"tenant_id,omitempty"`
	UserID          string     `json:"user_id,omitempty"`
	SessionID       string     `json:"session_id"`
	Ordinal         int        `json:"ordinal"`
	Content         string     `json:"content"`
	TokenCount      int        `json:"token_count,omitempty"`
	Embedding       []float64  `json:"-"`
	EmbeddingModel  string     `json:"embedding_model,omitempty"`
	EmbeddingStatus string     `json:"embedding_status"`
	EmbeddingError  string     `json:"embedding_error,omitempty"`
	EmbeddedAt      *time.Time `json:"embedded_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	RetrievalScore  float64    `json:"retrieval_score,omitempty"`
}

type AIIndexJob struct {
	ID                string     `json:"id"`
	TenantID          string     `json:"tenant_id,omitempty"`
	UserID            string     `json:"user_id,omitempty"`
	TargetType        string     `json:"target_type"`
	TargetID          string     `json:"target_id"`
	Model             string     `json:"model"`
	Dimensions        int        `json:"dimensions"`
	Status            string     `json:"status"`
	ChunkCount        int        `json:"chunk_count"`
	ProcessedChunks   int        `json:"processed_chunks"`
	EstimatedTokens   int64      `json:"estimated_tokens"`
	ActualTokens      int64      `json:"actual_tokens,omitempty"`
	EstimatedDP       float64    `json:"estimated_dp"`
	ContentDigest     string     `json:"content_digest,omitempty"`
	ClientRequestID   string     `json:"client_request_id,omitempty"`
	ErrorMessage      string     `json:"error_message,omitempty"`
	LeaseOwner        string     `json:"-"`
	LeaseExpiresAt    *time.Time `json:"lease_expires_at,omitempty"`
	AttemptCount      int        `json:"attempt_count"`
	MaxAttempts       int        `json:"max_attempts"`
	CancelRequestedAt *time.Time `json:"cancel_requested_at,omitempty"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type SkillMapJob struct {
	ID              string     `json:"id"`
	TenantID        string     `json:"tenant_id,omitempty"`
	UserID          string     `json:"user_id,omitempty"`
	ProjectID       string     `json:"project_id"`
	Status          string     `json:"status"`
	Model           string     `json:"model,omitempty"`
	ReasoningEffort string     `json:"reasoning_effort,omitempty"`
	RequestHash     string     `json:"request_hash,omitempty"`
	ClientRequestID string     `json:"client_request_id,omitempty"`
	ChunkCount      int        `json:"chunk_count"`
	ProcessedChunks int        `json:"processed_chunks"`
	CostUSD         float64    `json:"cost_usd"`
	ErrorMessage    string     `json:"error_message,omitempty"`
	LeaseOwner      string     `json:"-"`
	LeaseExpiresAt  *time.Time `json:"lease_expires_at,omitempty"`
	AttemptCount    int        `json:"attempt_count"`
	MaxAttempts     int        `json:"max_attempts"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type AIGenerationRequest struct {
	ID              string          `json:"id"`
	TenantID        string          `json:"tenant_id,omitempty"`
	UserID          string          `json:"user_id,omitempty"`
	SessionID       *string         `json:"session_id,omitempty"`
	ClientRequestID string          `json:"client_request_id"`
	RequestKind     string          `json:"request_kind"`
	RequestHash     string          `json:"request_hash,omitempty"`
	Status          string          `json:"status"`
	ResponseJSON    json.RawMessage `json:"response_json,omitempty"`
	LeaseOwner      string          `json:"-"`
	LeaseExpiresAt  *time.Time      `json:"lease_expires_at,omitempty"`
	AttemptCount    int             `json:"attempt_count"`
	ErrorMessage    string          `json:"error_message,omitempty"`
	CompletedAt     *time.Time      `json:"completed_at,omitempty"`
	ExpiresAt       time.Time       `json:"expires_at"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type AIGenerationBeginResult struct {
	Outcome string              `json:"outcome"`
	Created bool                `json:"created"`
	Request AIGenerationRequest `json:"request"`
}

type AIIndexPreview struct {
	TargetType        string      `json:"target_type"`
	TargetID          string      `json:"target_id"`
	Model             string      `json:"model"`
	Dimensions        int         `json:"dimensions"`
	SourceCount       int         `json:"source_count"`
	ChunkCount        int         `json:"chunk_count"`
	IndexedChunks     int         `json:"indexed_chunks"`
	PendingChunks     int         `json:"pending_chunks"`
	EstimatedTokens   int64       `json:"estimated_tokens"`
	EstimatedDP       float64     `json:"estimated_dp"`
	ContentDigest     string      `json:"content_digest,omitempty"`
	ConfirmationToken string      `json:"confirmation_token,omitempty"`
	IndexStatus       string      `json:"index_status"`
	CurrentModel      string      `json:"current_model,omitempty"`
	RequiresIndexing  bool        `json:"requires_indexing"`
	ActiveJob         *AIIndexJob `json:"active_job,omitempty"`
}

type AIProjectList struct {
	Projects        []AIProject `json:"projects"`
	LinkedProjectID *string     `json:"linked_project_id"`
}

type KnowledgeSearchResult struct {
	Chunks        []KnowledgeChunk `json:"chunks"`
	RetrievalMode string           `json:"retrieval_mode"`
}

type SessionSearchResult struct {
	Chunks        []SessionAIChunk `json:"chunks"`
	RetrievalMode string           `json:"retrieval_mode"`
}

// UsageLog represents a usage record for quota tracking
type UsageLog struct {
	ID        string                 `json:"id"`
	TenantID  string                 `json:"tenant_id"`
	UserID    string                 `json:"user_id"`
	Action    string                 `json:"action"` // transcription, translation, rag_query, storage
	Quantity  float64                `json:"quantity"`
	SessionID *string                `json:"session_id,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
	MonthKey  string                 `json:"month_key"`
}

// RefreshToken represents a JWT refresh token
type RefreshToken struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	TokenHash string     `json:"-"`
	ExpiresAt time.Time  `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// UsageSummary represents aggregated usage for a tenant
type UsageSummary struct {
	TenantID             string  `json:"tenant_id"`
	MonthKey             string  `json:"month_key"`
	TranscriptionMinutes float64 `json:"transcription_minutes"`
	TranslationCount     int     `json:"translation_count"`
	RAGQueryCount        int     `json:"rag_query_count"`
	StorageMB            float64 `json:"storage_mb"`
	APIRequestCount      int64   `json:"api_request_count"`
}
