package handlers

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	aicontext "github.com/dreamtrans/backend/internal/ai"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/google/uuid"
)

const (
	defaultKnowledgeFileBytes  = int64(50 << 20)
	knowledgeMultipartOverhead = int64(1 << 20)
	knowledgeVectorDimensions  = 384
)

type aiProjectRoute struct {
	ProjectID  string
	Resource   string
	ResourceID string
	Action     string
}

func parseAIProjectRoute(path string) (aiProjectRoute, int, error) {
	const prefix = "/api/ai/projects"
	if path != prefix && !strings.HasPrefix(path, prefix+"/") {
		return aiProjectRoute{}, http.StatusNotFound, errors.New("not found")
	}
	path = strings.Trim(strings.TrimPrefix(path, prefix), "/")
	if path == "" {
		return aiProjectRoute{}, http.StatusOK, nil
	}
	parts := strings.Split(path, "/")
	if uuid.Validate(parts[0]) != nil {
		return aiProjectRoute{}, http.StatusBadRequest,
			errors.New("project id must be a UUID")
	}
	route := aiProjectRoute{ProjectID: parts[0]}
	if len(parts) == 1 {
		return route, http.StatusOK, nil
	}
	route.Resource = parts[1]
	switch route.Resource {
	case "skill-map":
		if len(parts) == 2 {
			return route, http.StatusOK, nil
		}
	case "study":
		if len(parts) == 3 {
			switch parts[2] {
			case "state", "next", "attempts", "costs", "lesson", "reveal", "weeks":
				route.Action = parts[2]
				return route, http.StatusOK, nil
			}
		}
	case "sessions":
		switch len(parts) {
		case 2:
			return route, http.StatusOK, nil
		case 3:
			if uuid.Validate(parts[2]) != nil {
				return aiProjectRoute{}, http.StatusBadRequest,
					errors.New("session_id must be a UUID")
			}
			route.ResourceID = parts[2]
			return route, http.StatusOK, nil
		}
	case "timetable":
		switch len(parts) {
		case 2:
			return route, http.StatusOK, nil
		case 3:
			if uuid.Validate(parts[2]) != nil {
				return aiProjectRoute{}, http.StatusBadRequest,
					errors.New("slot id must be a UUID")
			}
			route.ResourceID = parts[2]
			return route, http.StatusOK, nil
		}
	case "sources":
		switch {
		case len(parts) == 2:
			return route, http.StatusOK, nil
		case len(parts) == 3 && parts[2] == "derived":
			route.Action = "derived"
			return route, http.StatusOK, nil
		case len(parts) == 3:
			if uuid.Validate(parts[2]) != nil {
				return aiProjectRoute{}, http.StatusBadRequest,
					errors.New("source id must be a UUID")
			}
			route.ResourceID = parts[2]
			return route, http.StatusOK, nil
		case len(parts) == 4 && parts[3] == "retry":
			if uuid.Validate(parts[2]) != nil {
				return aiProjectRoute{}, http.StatusBadRequest,
					errors.New("source id must be a UUID")
			}
			route.ResourceID = parts[2]
			route.Action = parts[3]
			return route, http.StatusOK, nil
		}
	}
	return aiProjectRoute{}, http.StatusNotFound, errors.New("not found")
}

func (h *RAGHandler) HandleProjects(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		http.Error(w, "projects require PostgreSQL", http.StatusServiceUnavailable)
		return
	}
	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	route, statusCode, err := parseAIProjectRoute(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), statusCode)
		return
	}
	if route.ProjectID == "" {
		h.handleProjectCollection(w, r, claims)
		return
	}
	project, err := h.store.GetAIProject(
		r.Context(), route.ProjectID, claims.UserID,
	)
	if err != nil {
		http.Error(w, "failed to load project", http.StatusInternalServerError)
		return
	}
	if project == nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	if route.Resource == "" {
		h.handleProjectItem(w, r, project)
		return
	}
	switch route.Resource {
	case "skill-map":
		h.handleProjectSkillMap(w, r, project)
	case "study":
		h.handleProjectStudy(w, r, project, route.Action)
	case "sessions":
		h.handleProjectSession(w, r, project, route.ResourceID)
	case "timetable":
		h.handleProjectTimetable(w, r, project, route.ResourceID)
	case "sources":
		if route.Action == "derived" {
			h.handleDerivedSources(w, r, project)
		} else if route.Action == "retry" {
			h.handleKnowledgeSourceRetry(w, r, project, route.ResourceID)
		} else if route.ResourceID != "" {
			h.handleKnowledgeSourceItem(w, r, project, route.ResourceID)
		} else {
			h.handleKnowledgeSources(w, r, project)
		}
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (h *RAGHandler) handleProjectCollection(
	w http.ResponseWriter, r *http.Request, claims *auth.UserClaims,
) {
	switch r.Method {
	case http.MethodGet:
		sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
		if sessionID != "" && uuid.Validate(sessionID) != nil {
			http.Error(w, "session_id must be a UUID", http.StatusBadRequest)
			return
		}
		projects, err := h.store.ListAIProjectsWithLinked(
			r.Context(), claims.TenantID, claims.UserID, sessionID,
		)
		if err != nil {
			http.Error(w, "failed to list projects", http.StatusInternalServerError)
			return
		}
		WriteJSON(w, projects)
	case http.MethodPost:
		var project models.AIProject
		if err := json.NewDecoder(r.Body).Decode(&project); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if project.ContextMode == "" {
			project.ContextMode = "smart"
		}
		if project.MaxContextTokens == 0 {
			project.MaxContextTokens = aicontext.DefaultContextTokens
		}
		project.UserID = claims.UserID
		project.TenantID = claims.TenantID
		if err := store.ValidateAIProject(&project); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := h.store.CreateAIProject(r.Context(), &project); err != nil {
			http.Error(w, "failed to create project", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		WriteJSON(w, map[string]any{"project": project})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *RAGHandler) handleProjectItem(
	w http.ResponseWriter, r *http.Request, project *models.AIProject,
) {
	switch r.Method {
	case http.MethodGet:
		WriteJSON(w, map[string]any{"project": project})
	case http.MethodPatch, http.MethodPut:
		var update struct {
			Name             *string `json:"name"`
			Description      *string `json:"description"`
			ContextMode      *string `json:"context_mode"`
			MaxContextTokens *int    `json:"max_context_tokens"`
			// "" clears it; absent leaves it alone.
			WeekStart *string `json:"week_start"`
		}
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if update.Name != nil {
			project.Name = *update.Name
		}
		if update.Description != nil {
			project.Description = *update.Description
		}
		if update.ContextMode != nil {
			project.ContextMode = *update.ContextMode
		}
		if update.MaxContextTokens != nil {
			project.MaxContextTokens = *update.MaxContextTokens
		}
		if update.WeekStart != nil {
			project.WeekStart = update.WeekStart
		}
		if err := store.ValidateAIProject(project); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := h.store.UpdateAIProject(r.Context(), project); err != nil {
			http.Error(w, "failed to update project", http.StatusInternalServerError)
			return
		}
		WriteJSON(w, map[string]any{"project": project})
	case http.MethodDelete:
		cancelledJobIDs, err := h.store.DeleteAIProjectAndCancelIndexJobs(
			r.Context(),
			project.ID,
			project.UserID,
		)
		if err != nil {
			http.Error(w, "failed to delete project", http.StatusInternalServerError)
			return
		}
		cancelActiveAIIndexJobs(cancelledJobIDs)
		h.resumeKnowledgeIndexing()
		WriteJSON(w, map[string]bool{"success": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *RAGHandler) handleKnowledgeSourceItem(
	w http.ResponseWriter,
	r *http.Request,
	project *models.AIProject,
	sourceID string,
) {
	switch r.Method {
	case http.MethodPatch:
		var update struct {
			Name    *string `json:"name"`
			Content *string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if update.Name == nil && update.Content == nil {
			http.Error(w, "name or content is required", http.StatusBadRequest)
			return
		}
		if update.Name != nil {
			name := strings.TrimSpace(*update.Name)
			if name == "" || len([]rune(name)) > 255 {
				http.Error(w, "memory name is required and must be at most 255 characters", http.StatusBadRequest)
				return
			}
		}
		if update.Content != nil {
			content := strings.TrimSpace(*update.Content)
			if content == "" || len([]rune(content)) > 1_000_000 {
				http.Error(w, "memory content is required and must be at most 1000000 characters", http.StatusBadRequest)
				return
			}
		}
		var chunks []models.KnowledgeChunk
		if update.Content != nil {
			memory := &models.KnowledgeSource{
				ID: sourceID, ProjectID: project.ID,
			}
			chunks = makeKnowledgeChunks(memory, strings.TrimSpace(*update.Content))
		}
		source, cancelledJobIDs, err := h.store.UpdateMemorySourceWithChunksAndCancelIndexJobs(
			r.Context(), sourceID, project.ID, project.TenantID, project.UserID,
			update.Name, update.Content, chunks,
		)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "memory source not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, store.ErrStorageQuota) {
			http.Error(w, "tenant storage quota exceeded", http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			http.Error(w, "failed to update memory", http.StatusInternalServerError)
			return
		}
		cancelActiveAIIndexJobs(cancelledJobIDs)
		WriteJSON(w, map[string]any{"source": source})
	case http.MethodDelete:
		_, cancelledJobIDs, err := h.store.DeleteKnowledgeSourceAndCancelIndexJobs(
			r.Context(), sourceID, project.ID, project.UserID,
		)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "knowledge source not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "failed to delete knowledge source", http.StatusInternalServerError)
			return
		}
		cancelActiveAIIndexJobs(cancelledJobIDs)
		h.resumeKnowledgeIndexing()
		WriteJSON(w, map[string]bool{"success": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *RAGHandler) handleKnowledgeSourceRetry(
	w http.ResponseWriter,
	r *http.Request,
	project *models.AIProject,
	sourceID string,
) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	source, err := h.store.RetryKnowledgeSource(
		r.Context(),
		sourceID,
		project.ID,
		project.TenantID,
		project.UserID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "retryable knowledge source not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "failed to retry knowledge source", http.StatusInternalServerError)
		return
	}
	h.enqueueKnowledgeExtraction(source)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	WriteJSON(w, map[string]any{"source": source})
}

func (h *RAGHandler) handleProjectSession(
	w http.ResponseWriter,
	r *http.Request,
	project *models.AIProject,
	pathSessionID string,
) {
	if r.Method == http.MethodGet && pathSessionID == "" {
		sessions, err := h.store.ListProjectSessions(
			r.Context(), project.TenantID, project.UserID, project.ID,
		)
		if err != nil {
			http.Error(w, "failed to list project sessions", http.StatusInternalServerError)
			return
		}
		WriteJSON(w, map[string]any{"sessions": sessions})
		return
	}
	if r.Method == http.MethodDelete {
		pathSessionID = strings.TrimSpace(pathSessionID)
		if uuid.Validate(pathSessionID) != nil {
			http.Error(w, "session_id must be a UUID", http.StatusBadRequest)
			return
		}
		err := h.store.UnlinkProjectSession(
			r.Context(), project.ID, pathSessionID,
			project.TenantID, project.UserID,
		)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "project session link not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "failed to unlink session", http.StatusInternalServerError)
			return
		}
		WriteJSON(w, map[string]bool{"success": true})
		return
	}
	if r.Method != http.MethodPost || pathSessionID != "" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	if uuid.Validate(req.SessionID) != nil {
		http.Error(w, "session_id must be a UUID", http.StatusBadRequest)
		return
	}
	if err := h.store.LinkProjectSession(
		r.Context(), project.ID, req.SessionID, project.UserID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "session was not found or is not owned by this user", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to link session", http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]bool{"success": true})
}

func (h *RAGHandler) handleKnowledgeSources(
	w http.ResponseWriter, r *http.Request, project *models.AIProject,
) {
	if r.Method == http.MethodGet {
		sources, err := h.store.ListKnowledgeSources(r.Context(), project.ID, project.UserID)
		if err != nil {
			http.Error(w, "failed to list knowledge sources", http.StatusInternalServerError)
			return
		}
		// Workspaces poll metadata frequently; do not repeatedly transfer the
		// complete extracted contents of every document for a status refresh.
		if r.URL.Query().Get("metadata_only") == "true" {
			for index := range sources {
				sources[index].Content = ""
			}
		}
		WriteJSON(w, map[string]any{"sources": sources})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mediaType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(mediaType, "multipart/form-data") {
		h.handleKnowledgeFileUpload(w, r, project)
		return
	}
	var req struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Content = strings.TrimSpace(req.Content)
	if req.Name == "" || req.Content == "" || len([]rune(req.Content)) > 1_000_000 {
		http.Error(w, "memory name/content is required and content must be at most 1000000 characters", http.StatusBadRequest)
		return
	}
	source := &models.KnowledgeSource{
		ProjectID: project.ID, TenantID: project.TenantID, UserID: project.UserID,
		SourceType: "memory", Name: req.Name, MediaType: "text/plain",
		SizeBytes: int64(len(req.Content)), Content: req.Content, Status: "ready",
	}
	chunks := makeKnowledgeChunks(source, req.Content)
	cancelledJobIDs, err := h.store.CreateMemorySourceWithChunksAndCancelIndexJobs(
		r.Context(), source, chunks,
	)
	if err != nil {
		if errors.Is(err, store.ErrStorageQuota) {
			http.Error(w, "tenant storage quota exceeded", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "failed to create memory", http.StatusInternalServerError)
		return
	}
	cancelActiveAIIndexJobs(cancelledJobIDs)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	WriteJSON(w, map[string]any{"source": source})
}

func (h *RAGHandler) handleKnowledgeFileUpload(
	w http.ResponseWriter, r *http.Request, project *models.AIProject,
) {
	maxBytes := knowledgeFileLimit()
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+knowledgeMultipartOverhead)
	parseErr := r.ParseMultipartForm(knowledgeMultipartOverhead) //nolint:gosec // Request body is hard-limited above.
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}
	if parseErr != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(parseErr, &maxBytesError) {
			http.Error(w, "file upload is too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "file upload is malformed", http.StatusBadRequest)
		return
	}
	var ocrLanguageValues []string
	var (
		ocrLanguagesProvided bool
		sessionIDValues      []string
	)
	if r.MultipartForm != nil {
		ocrLanguageValues, ocrLanguagesProvided =
			r.MultipartForm.Value["ocr_language"]
		sessionIDValues = r.MultipartForm.Value["session_id"]
	}
	if len(sessionIDValues) > 1 {
		http.Error(w, "session_id must be provided at most once", http.StatusBadRequest)
		return
	}
	var sessionSourceLanguage string
	if len(sessionIDValues) == 1 {
		sessionID := strings.TrimSpace(sessionIDValues[0])
		if uuid.Validate(sessionID) != nil {
			http.Error(w, "session_id must be a UUID", http.StatusBadRequest)
			return
		}
		var lookupErr error
		sessionSourceLanguage, lookupErr = h.store.GetProjectSessionSourceLanguage(
			r.Context(),
			project.ID,
			sessionID,
			project.TenantID,
			project.UserID,
		)
		if errors.Is(lookupErr, sql.ErrNoRows) {
			http.Error(
				w,
				"session is not linked to this project",
				http.StatusNotFound,
			)
			return
		}
		if lookupErr != nil {
			http.Error(
				w,
				"failed to validate linked session",
				http.StatusInternalServerError,
			)
			return
		}
	}
	var (
		ocrLanguages []string
		ocrParseErr  error
	)
	if ocrLanguagesProvided {
		ocrLanguages, ocrParseErr =
			parseKnowledgeOCRLanguages(ocrLanguageValues)
	} else {
		ocrLanguages = defaultKnowledgeOCRLanguages(sessionSourceLanguage)
	}
	if ocrParseErr != nil {
		http.Error(w, ocrParseErr.Error(), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file field is required", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()
	extension := strings.ToLower(filepath.Ext(header.Filename))
	if !supportedKnowledgeExtension(extension) {
		http.Error(w, "unsupported file type", http.StatusUnsupportedMediaType)
		return
	}
	root, err := knowledgeStorageRoot()
	if err != nil {
		http.Error(w, "invalid knowledge storage path", http.StatusInternalServerError)
		return
	}
	projectID, err := uuid.Parse(project.ID)
	if err != nil {
		http.Error(w, "invalid project storage identifier", http.StatusInternalServerError)
		return
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		http.Error(w, "failed to create knowledge storage", http.StatusInternalServerError)
		return
	}
	storageRoot, err := os.OpenRoot(root)
	if err != nil {
		http.Error(w, "failed to open knowledge storage", http.StatusInternalServerError)
		return
	}
	defer func() { _ = storageRoot.Close() }()
	projectDir := projectID.String()
	if err := storageRoot.MkdirAll(projectDir, 0o750); err != nil {
		http.Error(w, "failed to create project storage", http.StatusInternalServerError)
		return
	}
	relativeBlobPath := filepath.Join(projectDir, uuid.NewString()+extension)
	destination, err := storageRoot.OpenFile(
		relativeBlobPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640,
	)
	if err != nil {
		http.Error(w, "failed to store file", http.StatusInternalServerError)
		return
	}
	blobPath := filepath.Join(root, relativeBlobPath)
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(destination, hasher), io.LimitReader(file, maxBytes+1))
	closeErr := destination.Close()
	if copyErr != nil || closeErr != nil || written > maxBytes {
		_ = removeKnowledgeBlobFromRoot(storageRoot, relativeBlobPath)
		http.Error(w, "failed to store file or file exceeds limit", http.StatusRequestEntityTooLarge)
		return
	}
	mediaType, err := validateKnowledgeUpload(
		blobPath,
		extension,
		header.Header.Get("Content-Type"),
	)
	if err != nil {
		_ = removeKnowledgeBlobFromRoot(storageRoot, relativeBlobPath)
		status := http.StatusUnsupportedMediaType
		if errors.Is(err, errKnowledgeOfficeTooLarge) ||
			errors.Is(err, errKnowledgeImageTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}
	source := &models.KnowledgeSource{
		ProjectID: project.ID, TenantID: project.TenantID, UserID: project.UserID,
		SourceType: "file", Name: safeKnowledgeFilename(header),
		MediaType: mediaType, SizeBytes: written, OCRLanguages: ocrLanguages,
		SHA256: hex.EncodeToString(hasher.Sum(nil)), BlobPath: blobPath, Status: "queued",
	}
	if err := h.store.CreateKnowledgeSource(r.Context(), source); err != nil {
		_ = removeKnowledgeBlobFromRoot(storageRoot, relativeBlobPath)
		if errors.Is(err, store.ErrStorageQuota) {
			http.Error(w, "tenant storage quota exceeded", http.StatusRequestEntityTooLarge)
			return
		}
		if errors.Is(err, store.ErrDuplicateKnowledgeSource) ||
			isDuplicateKnowledgeSourceError(err) {
			http.Error(w, "this file is already in the project", http.StatusConflict)
			return
		}
		http.Error(w, "failed to register file", http.StatusInternalServerError)
		return
	}
	// Keep the durable queued row as the source of truth while the bounded
	// background job parses and indexes the file.
	h.enqueueKnowledgeExtraction(source)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	WriteJSON(w, map[string]any{"source": source})
}

func knowledgeFileLimit() int64 {
	raw := strings.TrimSpace(os.Getenv("KNOWLEDGE_MAX_FILE_MB"))
	if raw == "" {
		return defaultKnowledgeFileBytes
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 1 || value > 500 {
		return defaultKnowledgeFileBytes
	}
	return value << 20
}

// KnowledgeUploadRequestLimit includes bounded multipart metadata overhead on
// top of the operator-configured file limit used by the shared project route.
func KnowledgeUploadRequestLimit() int64 {
	return knowledgeFileLimit() + (2 << 20)
}

func supportedKnowledgeExtension(extension string) bool {
	switch extension {
	case ".txt", ".md", ".csv", ".tsv", ".json", ".pdf", ".docx", ".xlsx",
		".png", ".jpg", ".jpeg", ".webp":
		return true
	default:
		return false
	}
}

func safeKnowledgeFilename(header *multipart.FileHeader) string {
	name := strings.TrimSpace(filepath.Base(header.Filename))
	if name == "" {
		return "uploaded-file"
	}
	if len([]rune(name)) > 255 {
		name = string([]rune(name)[:255])
	}
	return name
}

func knowledgeStorageRoot() (string, error) {
	root := strings.TrimSpace(os.Getenv("KNOWLEDGE_DATA_PATH"))
	if root == "" {
		root = "/app/data/knowledge"
	}
	return filepath.Abs(root)
}

func removeKnowledgeBlobFromRoot(root *os.Root, relativePath string) error {
	if err := root.Remove(relativePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func removeKnowledgeBlob(blobPath string) error {
	blobPath = strings.TrimSpace(blobPath)
	if blobPath == "" {
		return nil
	}
	absoluteRoot, err := knowledgeStorageRoot()
	if err != nil {
		return err
	}
	absoluteBlob, err := filepath.Abs(blobPath)
	if err != nil {
		return err
	}
	relativeBlob, err := filepath.Rel(absoluteRoot, absoluteBlob)
	if err != nil {
		return err
	}
	parentPrefix := ".." + string(os.PathSeparator)
	if relativeBlob == "." || relativeBlob == ".." || filepath.IsAbs(relativeBlob) ||
		strings.HasPrefix(relativeBlob, parentPrefix) {
		return errors.New("knowledge blob is outside the configured storage root")
	}
	storageRoot, err := os.OpenRoot(absoluteRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = storageRoot.Close() }()
	return removeKnowledgeBlobFromRoot(storageRoot, relativeBlob)
}

func makeKnowledgeChunks(
	source *models.KnowledgeSource, text string,
) []models.KnowledgeChunk {
	paragraphs := strings.FieldsFunc(strings.ReplaceAll(text, "\r\n", "\n"), func(r rune) bool {
		return r == '\n'
	})
	const maxRunes = 1_400
	chunks := make([]models.KnowledgeChunk, 0)
	var current strings.Builder
	flush := func() {
		content := strings.TrimSpace(current.String())
		if content == "" {
			current.Reset()
			return
		}
		chunks = append(chunks, models.KnowledgeChunk{
			SourceID: source.ID, ProjectID: source.ProjectID,
			Ordinal: len(chunks), Content: content,
			Vector:     localKnowledgeVector(content),
			TokenCount: aicontext.EstimateTokens(content),
		})
		current.Reset()
	}
	for _, paragraph := range paragraphs {
		paragraph = strings.Join(strings.Fields(paragraph), " ")
		if paragraph == "" {
			continue
		}
		runes := []rune(paragraph)
		for len(runes) > maxRunes {
			if current.Len() > 0 {
				flush()
			}
			piece := strings.TrimSpace(string(runes[:maxRunes]))
			chunks = append(chunks, models.KnowledgeChunk{
				SourceID: source.ID, ProjectID: source.ProjectID,
				Ordinal: len(chunks), Content: piece,
				Vector:     localKnowledgeVector(piece),
				TokenCount: aicontext.EstimateTokens(piece),
			})
			runes = runes[maxRunes-160:]
		}
		paragraph = string(runes)
		if len([]rune(current.String()))+len([]rune(paragraph))+1 > maxRunes {
			flush()
		}
		if current.Len() > 0 {
			current.WriteByte('\n')
		}
		current.WriteString(paragraph)
	}
	flush()
	return chunks
}

func localKnowledgeVector(text string) []float64 {
	vector := make([]float64, knowledgeVectorDimensions)
	runes := []rune(strings.ToLower(text))
	add := func(term string, weight float64) {
		hash := fnv.New64a()
		_, _ = hash.Write([]byte(term))
		value := hash.Sum64()
		index := value % uint64(len(vector))
		sign := 1.0
		if value&(1<<63) != 0 {
			sign = -1
		}
		vector[index] += sign * weight
	}
	for _, word := range strings.FieldsFunc(string(runes), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if word != "" {
			add("w:"+word, 2)
		}
	}
	for size := 2; size <= 4; size++ {
		for index := 0; index+size <= len(runes); index++ {
			gram := runes[index : index+size]
			if unicode.IsSpace(gram[0]) {
				continue
			}
			add("g:"+string(gram), 1)
		}
	}
	var norm float64
	for _, value := range vector {
		norm += value * value
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		for index := range vector {
			vector[index] /= norm
		}
	}
	return vector
}

func retrieveKnowledge(
	query string, chunks []models.KnowledgeChunk, topK int,
) []models.KnowledgeChunk {
	if topK <= 0 {
		topK = 6
	}
	queryVector := localKnowledgeVector(query)
	type scored struct {
		chunk models.KnowledgeChunk
		score float64
	}
	values := make([]scored, 0, len(chunks))
	for index := range chunks {
		chunk := &chunks[index]
		var score float64
		for index := 0; index < len(queryVector) && index < len(chunk.Vector); index++ {
			score += queryVector[index] * chunk.Vector[index]
		}
		values = append(values, scored{chunk: *chunk, score: score})
	}
	sort.Slice(values, func(i, j int) bool { return values[i].score > values[j].score })
	if len(values) > topK {
		values = values[:topK]
	}
	result := make([]models.KnowledgeChunk, 0, len(values))
	for index := range values {
		value := &values[index]
		if value.score <= 0 && len(result) > 0 {
			continue
		}
		result = append(result, value.chunk)
	}
	return result
}

func safeIndexError(err error) string {
	message := strings.TrimSpace(err.Error())
	if len([]rune(message)) > 1_000 {
		message = string([]rune(message)[:1_000])
	}
	return message
}
