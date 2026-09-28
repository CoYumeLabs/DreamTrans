package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/store"
)

// Moodle Sync: the browser extension pulls course materials inside the
// user's browser, extracts them there, and posts only the derived text here:
//
//	GET  /api/ai/projects/{id}/sources/derived — what this course already holds
//	POST /api/ai/projects/{id}/sources/derived — one material, per-page text
//	                                              plus renders of figure pages
//
//	PUT  /api/ai/projects/{id}/sources/{sourceId}/original — opt-in: the file
//	                                              itself, for a synced material
//	GET  /api/ai/projects/{id}/sources/{sourceId}/original — download any
//	                                              source whose file is kept
//
// Figure-page renders are OCR'd on arrival and discarded; the server never
// keeps an image of a slide. The original file stays in the browser unless
// the user turns on 保存原文件, and then it is stored like a manual upload.

const (
	derivedMaxPages         = 2000
	derivedMaxTextRunes     = 1_000_000
	derivedMaxFigures       = 120
	derivedMaxFigureBytes   = 2 << 20 // decoded PNG
	derivedMaxNameRunes     = 255
	derivedMaxLMSFieldRunes = 300
)

var derivedSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type derivedFigure struct {
	// PNG render of a page region (or the whole page), base64 without a
	// data: prefix. Only OCR text survives on the server.
	PNGBase64 string    `json:"png_base64"`
	BBox      []float64 `json:"bbox,omitempty"`
}

type derivedPage struct {
	N       int             `json:"n"`
	Text    string          `json:"text"`
	Figures []derivedFigure `json:"figures,omitempty"`
}

// derivedLMS is the provenance stored with the source. Field names are
// stable: the extension reads them back from GET to decide what changed.
type derivedLMS struct {
	Host            string `json:"host"`
	CourseID        int64  `json:"course_id"`
	CourseShortname string `json:"course_shortname"`
	CourseName      string `json:"course_name,omitempty"`
	Section         string `json:"section"`
	SectionOrder    int    `json:"section_order"`
	CMID            int64  `json:"cmid"`
	ModType         string `json:"modtype"`
	ModuleName      string `json:"module_name"`
	URL             string `json:"url,omitempty"`
	TimeModified    int64  `json:"timemodified"`
	Extractor       string `json:"extractor,omitempty"`
}

type derivedSourceRequest struct {
	SHA256    string        `json:"sha256"`
	Filename  string        `json:"filename"`
	MediaType string        `json:"media_type"`
	SizeBytes int64         `json:"size_bytes"`
	PageCount int           `json:"page_count"`
	Pages     []derivedPage `json:"pages"`
	LMS       derivedLMS    `json:"lms"`
}

// figureOCR turns one figure render into text, or "" when OCR is not
// available. It is injected so tests never shell out.
type figureOCR func(ctx context.Context, png []byte, languages []string) string

func (h *RAGHandler) handleDerivedSources(
	w http.ResponseWriter, r *http.Request, project *models.AIProject,
) {
	switch r.Method {
	case http.MethodGet:
		refs, err := h.store.ListLMSSources(r.Context(), project.ID, project.UserID)
		if err != nil {
			http.Error(w, "failed to list synced materials", http.StatusInternalServerError)
			return
		}
		WriteJSON(w, map[string]any{"sources": refs})
	case http.MethodPost:
		h.handleDerivedSourceUpload(w, r, project)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *RAGHandler) handleDerivedSourceUpload(
	w http.ResponseWriter, r *http.Request, project *models.AIProject,
) {
	if !derivedUploads.acquire(project.UserID) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "material extraction is busy; retry later", http.StatusTooManyRequests)
		return
	}
	defer derivedUploads.release(project.UserID)
	ctx, cancel := context.WithTimeout(r.Context(), knowledgeCommandTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	var req derivedSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := validateDerivedSource(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	existing, err := h.store.GetKnowledgeSourceBySHA256(r.Context(), project.ID, project.UserID, req.SHA256)
	if err != nil {
		http.Error(w, "failed to check existing materials", http.StatusInternalServerError)
		return
	}
	if existing != nil {
		WriteJSON(w, map[string]any{"source": existing, "duplicate": true})
		return
	}
	text := renderDerivedText(r.Context(), &req, tesseractFigureOCR)
	if err := ctx.Err(); err != nil {
		http.Error(w, "material extraction timed out or was cancelled", http.StatusRequestTimeout)
		return
	}
	if strings.TrimSpace(text) == "" {
		http.Error(w, "material has no extractable text", http.StatusUnprocessableEntity)
		return
	}
	lmsJSON, err := json.Marshal(req.LMS)
	if err != nil {
		http.Error(w, "failed to encode provenance", http.StatusInternalServerError)
		return
	}
	source := &models.KnowledgeSource{
		ProjectID: project.ID, TenantID: project.TenantID, UserID: project.UserID,
		SourceType: "lms", Name: derivedSourceName(&req), MediaType: req.MediaType,
		SizeBytes: req.SizeBytes, SHA256: req.SHA256, Content: text, Status: "ready",
		LMS: lmsJSON,
	}
	chunks := makeKnowledgeChunks(source, text)
	cancelledJobIDs, err := h.store.CreateLMSSourceWithChunks(r.Context(), source, chunks)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrStorageQuota):
			http.Error(w, "tenant storage quota exceeded", http.StatusRequestEntityTooLarge)
		case isDuplicateKnowledgeSourceError(err):
			// Lost a race with a concurrent upload of the same file.
			if again, lookupErr := h.store.GetKnowledgeSourceBySHA256(
				r.Context(), project.ID, project.UserID, req.SHA256,
			); lookupErr == nil && again != nil {
				WriteJSON(w, map[string]any{"source": again, "duplicate": true})
				return
			}
			http.Error(w, "material already exists", http.StatusConflict)
		default:
			log.Printf("create lms source: %v", err)
			http.Error(w, "failed to save material", http.StatusInternalServerError)
		}
		return
	}
	cancelActiveAIIndexJobs(cancelledJobIDs)
	source.Content = ""
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	WriteJSON(w, map[string]any{"source": source, "duplicate": false})
}

func clampDerivedText(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > maxRunes {
		return strings.TrimSpace(string(runes[:maxRunes]))
	}
	return value
}

// validateDerivedSource bounds the request; it also normalizes the fields
// it keeps so later steps can trust them.
func validateDerivedSource(req *derivedSourceRequest) error {
	req.SHA256 = strings.ToLower(strings.TrimSpace(req.SHA256))
	if !derivedSHA256Pattern.MatchString(req.SHA256) {
		return errors.New("sha256 must be 64 hex characters")
	}
	req.Filename = clampDerivedText(req.Filename, derivedMaxNameRunes)
	if req.Filename == "" {
		return errors.New("filename is required")
	}
	req.MediaType = strings.ToLower(clampDerivedText(req.MediaType, 120))
	if req.MediaType == "" {
		req.MediaType = "application/octet-stream"
	}
	if req.SizeBytes < 0 {
		return errors.New("size_bytes must not be negative")
	}
	if len(req.Pages) == 0 {
		return errors.New("pages are required")
	}
	if len(req.Pages) > derivedMaxPages {
		return fmt.Errorf("at most %d pages", derivedMaxPages)
	}
	if req.PageCount < len(req.Pages) {
		req.PageCount = len(req.Pages)
	}
	totalRunes := 0
	totalFigures := 0
	for index := range req.Pages {
		page := &req.Pages[index]
		if page.N <= 0 {
			page.N = index + 1
		}
		page.Text = strings.TrimSpace(page.Text)
		totalRunes += len([]rune(page.Text))
		if totalRunes > derivedMaxTextRunes {
			return fmt.Errorf("text exceeds %d characters", derivedMaxTextRunes)
		}
		totalFigures += len(page.Figures)
		if totalFigures > derivedMaxFigures {
			return fmt.Errorf("at most %d figure renders", derivedMaxFigures)
		}
		for figureIndex := range page.Figures {
			figure := &page.Figures[figureIndex]
			figure.PNGBase64 = strings.TrimSpace(figure.PNGBase64)
			if figure.PNGBase64 == "" {
				return errors.New("figure png_base64 is required")
			}
			if len(figure.PNGBase64) > derivedMaxFigureBytes*4/3+4 {
				return errors.New("figure render is too large")
			}
		}
	}
	lms := &req.LMS
	lms.Host = strings.ToLower(clampDerivedText(lms.Host, derivedMaxLMSFieldRunes))
	lms.CourseShortname = clampDerivedText(lms.CourseShortname, derivedMaxLMSFieldRunes)
	lms.CourseName = clampDerivedText(lms.CourseName, derivedMaxLMSFieldRunes)
	lms.Section = clampDerivedText(lms.Section, derivedMaxLMSFieldRunes)
	lms.ModType = strings.ToLower(clampDerivedText(lms.ModType, 40))
	lms.ModuleName = clampDerivedText(lms.ModuleName, derivedMaxLMSFieldRunes)
	lms.URL = clampDerivedText(lms.URL, 2000)
	lms.Extractor = clampDerivedText(lms.Extractor, 60)
	if lms.Host == "" {
		return errors.New("lms.host is required")
	}
	if lms.CMID <= 0 {
		return errors.New("lms.cmid is required")
	}
	if lms.TimeModified < 0 {
		lms.TimeModified = 0
	}
	return nil
}

// derivedSourceName is what the skill map and the materials list show:
// the section first so a course's files read in teaching order.
func derivedSourceName(req *derivedSourceRequest) string {
	name := req.Filename
	if req.LMS.Section != "" {
		name = req.LMS.Section + " · " + name
	}
	return clampDerivedText(name, derivedMaxNameRunes)
}

// renderDerivedText flattens pages into one document. Each page keeps its
// number so transcript ↔ slide alignment can cite it, and figure renders
// contribute only their OCR text.
func renderDerivedText(ctx context.Context, req *derivedSourceRequest, ocr figureOCR) string {
	var builder strings.Builder
	for index := range req.Pages {
		if ctx.Err() != nil {
			return ""
		}
		page := &req.Pages[index]
		var parts []string
		if page.Text != "" {
			parts = append(parts, page.Text)
		}
		for figureIndex := range page.Figures {
			if ctx.Err() != nil {
				return ""
			}
			png, err := base64.StdEncoding.DecodeString(page.Figures[figureIndex].PNGBase64)
			if err != nil || len(png) == 0 || len(png) > derivedMaxFigureBytes || ocr == nil {
				continue
			}
			text := strings.TrimSpace(ocr(ctx, png, []string{"eng", "chi_sim"}))
			// The render is gone as soon as this call returns.
			page.Figures[figureIndex].PNGBase64 = ""
			if text != "" {
				parts = append(parts, fmt.Sprintf("[图 %d] %s", figureIndex+1, text))
			}
		}
		if len(parts) == 0 {
			continue
		}
		fmt.Fprintf(&builder, "## 第 %d 页\n%s\n\n", page.N, strings.Join(parts, "\n"))
	}
	return clampDerivedText(builder.String(), derivedMaxTextRunes)
}

// tesseractFigureOCR runs the same OCR the file pipeline uses on a temp
// file that is removed before returning. Missing tesseract means no text,
// not an error: the page text still lands.
func tesseractFigureOCR(ctx context.Context, png []byte, languages []string) string {
	dir, err := os.MkdirTemp("", "dt-figure-*")
	if err != nil {
		return ""
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "figure.png")
	if err := os.WriteFile(path, png, 0o600); err != nil {
		return ""
	}
	limits := currentKnowledgeExtractionLimits()
	if err := validateKnowledgeImage(path, ".png", limits.maxImagePixels); err != nil {
		return ""
	}
	output, err := runBoundedKnowledgeCommand(
		ctx, limits.maxExtractedBytes, "tesseract", path, "stdout",
		"-l", tesseractLanguageArgument(languages),
	)
	if err != nil {
		return ""
	}
	return string(output)
}

// derivedOriginalExtensions maps the media types the extension may attach
// as originals to the extension the blob is stored and validated under.
var derivedOriginalExtensions = map[string]string{
	"application/pdf": ".pdf",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
}

func (h *RAGHandler) handleKnowledgeSourceOriginal(
	w http.ResponseWriter, r *http.Request, project *models.AIProject, sourceID string,
) {
	switch r.Method {
	case http.MethodGet:
		h.serveKnowledgeSourceOriginal(w, r, project, sourceID)
	case http.MethodPut:
		h.attachDerivedSourceOriginal(w, r, project, sourceID)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// attachDerivedSourceOriginal stores the file a synced material was derived
// from. The body must hash to the sha256 the material was registered with,
// so an original can never be swapped for different content, and its size
// must match the size already counted against the tenant's quota.
func (h *RAGHandler) attachDerivedSourceOriginal(
	w http.ResponseWriter, r *http.Request, project *models.AIProject, sourceID string,
) {
	source, err := h.store.GetKnowledgeSource(r.Context(), sourceID, project.ID, project.TenantID, project.UserID)
	if err != nil {
		http.Error(w, "failed to load material", http.StatusInternalServerError)
		return
	}
	if source == nil || source.SourceType != "lms" {
		http.Error(w, "synced material not found", http.StatusNotFound)
		return
	}
	if source.BlobPath != "" {
		WriteJSON(w, map[string]any{"source": source, "duplicate": true})
		return
	}
	extension, ok := derivedOriginalExtensions[strings.ToLower(source.MediaType)]
	if !ok {
		http.Error(w, "this material type has no original to keep", http.StatusUnsupportedMediaType)
		return
	}
	maxBytes := knowledgeFileLimit()
	if source.SizeBytes <= 0 || source.SizeBytes > maxBytes {
		http.Error(w, "file upload is too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, source.SizeBytes)
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
	destination, err := storageRoot.OpenFile(relativeBlobPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		http.Error(w, "failed to store file", http.StatusInternalServerError)
		return
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(destination, hasher), r.Body)
	closeErr := destination.Close()
	discard := func() { _ = removeKnowledgeBlobFromRoot(storageRoot, relativeBlobPath) }
	if copyErr != nil || closeErr != nil {
		discard()
		http.Error(w, "failed to store file or file exceeds its registered size", http.StatusRequestEntityTooLarge)
		return
	}
	if written != source.SizeBytes || hex.EncodeToString(hasher.Sum(nil)) != source.SHA256 {
		discard()
		http.Error(w, "file does not match the synced material", http.StatusBadRequest)
		return
	}
	blobPath := filepath.Join(root, relativeBlobPath)
	if _, err := validateKnowledgeUpload(blobPath, extension, source.MediaType); err != nil {
		discard()
		http.Error(w, err.Error(), http.StatusUnsupportedMediaType)
		return
	}
	attached, err := h.store.AttachLMSSourceOriginal(
		r.Context(), source.ID, project.ID, project.TenantID, project.UserID, blobPath,
	)
	if err != nil || !attached {
		discard()
		if err != nil {
			http.Error(w, "failed to save original", http.StatusInternalServerError)
			return
		}
		// Lost a race: deleted meanwhile, or another upload attached first.
		http.Error(w, "material changed while uploading; sync again", http.StatusConflict)
		return
	}
	source.BlobPath = blobPath
	source.HasOriginal = true
	source.Content = ""
	WriteJSON(w, map[string]any{"source": source, "duplicate": false})
}

// serveKnowledgeSourceOriginal streams a kept file to its owner. The blob
// path comes from the database but is still resolved inside the storage
// root, so a bad row cannot read outside it.
func (h *RAGHandler) serveKnowledgeSourceOriginal(
	w http.ResponseWriter, r *http.Request, project *models.AIProject, sourceID string,
) {
	source, err := h.store.GetKnowledgeSource(r.Context(), sourceID, project.ID, project.TenantID, project.UserID)
	if err != nil {
		http.Error(w, "failed to load material", http.StatusInternalServerError)
		return
	}
	if source == nil || source.BlobPath == "" {
		http.Error(w, "original file not kept", http.StatusNotFound)
		return
	}
	root, err := knowledgeStorageRoot()
	if err != nil {
		http.Error(w, "invalid knowledge storage path", http.StatusInternalServerError)
		return
	}
	relative, err := filepath.Rel(root, source.BlobPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		http.Error(w, "original file not kept", http.StatusNotFound)
		return
	}
	storageRoot, err := os.OpenRoot(root)
	if err != nil {
		http.Error(w, "failed to open knowledge storage", http.StatusInternalServerError)
		return
	}
	defer func() { _ = storageRoot.Close() }()
	file, err := storageRoot.Open(relative)
	if err != nil {
		http.Error(w, "original file not kept", http.StatusNotFound)
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "original file not kept", http.StatusNotFound)
		return
	}
	filename := originalDownloadName(source.Name, filepath.Ext(source.BlobPath))
	w.Header().Set("Content-Type", source.MediaType)
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(filename))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "", info.ModTime(), file)
}

// originalDownloadName is the material's display name (a synced material
// is named "Section · file.pdf") with the stored extension guaranteed.
func originalDownloadName(name, extension string) string {
	if index := strings.LastIndex(name, " · "); index >= 0 {
		name = name[index+len(" · "):]
	}
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < 0x20 {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	if name == "" {
		name = "material"
	}
	if !strings.EqualFold(filepath.Ext(name), extension) {
		name += extension
	}
	return name
}
