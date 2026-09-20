package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dreamtrans/backend/internal/deployment"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	openai "github.com/dreamtrans/backend/internal/adapters/openai_provider"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/config"
	"github.com/dreamtrans/backend/internal/metrics"
	"github.com/dreamtrans/backend/internal/modelcatalog"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/rag"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/google/uuid"
)

// 学习模式 practice endpoints under /api/ai/projects/{id}/study/:
//
//	GET  study/state    — every skill's learner state (lights the map)
//	GET  study/lesson   — the skill's frozen 讲解卡, generated on first call
//	POST study/next     — serve a scenario, generating rubric+bank on demand
//	POST study/attempts — grade one answer against the frozen rubric
//	POST study/reveal   — show answer + explanation without grading
//	GET  study/costs    — what this course has cost
//
// Generation and grading run through the shared AI generation pipeline for
// idempotency and reserve→settle billing, on the summary-purpose model.

const (
	studyNextRequestKind  = "study_next"
	studyGradeRequestKind = "study_grade"
)

func (h *RAGHandler) handleProjectStudy(
	w http.ResponseWriter, r *http.Request, project *models.AIProject, action string,
) {
	if (action == "lesson" && r.Method == http.MethodGet) || (action == "next" && r.Method == http.MethodPost) {
		ctx, err := h.studyVersionContext(r.Context(), project)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		r = r.WithContext(ctx)
	}
	switch {
	case action == "state" && r.Method == http.MethodGet:
		h.handleStudyState(w, r, project)
	case action == "lesson" && r.Method == http.MethodGet:
		h.handleStudyLesson(w, r, project)
	case action == "next" && r.Method == http.MethodPost:
		h.handleStudyNext(w, r, project)
	case action == "attempts" && r.Method == http.MethodPost:
		h.handleStudyAttempt(w, r, project)
	case action == "attempts" && r.Method == http.MethodGet:
		h.handleStudyHistory(w, r, project)
	case action == "reveal" && r.Method == http.MethodPost:
		h.handleStudyReveal(w, r, project)
	case action == "costs" && r.Method == http.MethodGet:
		h.handleStudyCosts(w, r, project)
	case action == "weeks" && r.Method == http.MethodGet:
		h.handleStudyWeeks(w, r, project)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *RAGHandler) handleStudyState(
	w http.ResponseWriter, r *http.Request, project *models.AIProject,
) {
	states, err := h.store.ListStudySkillStates(r.Context(), project.UserID, project.ID)
	if err != nil {
		http.Error(w, "failed to load study state", http.StatusInternalServerError)
		return
	}
	var recommended *studyContinue
	if artifact, artErr := h.store.GetLatestAIArtifactByProject(
		r.Context(), project.UserID, project.ID, skillMapArtifactType,
	); artErr == nil && artifact != nil {
		recommended = recommendStudySkill(parseStoredSkillMap(artifact.Content), states)
	}
	WriteJSON(w, map[string]any{"states": states, "continue": recommended})
}

// Billing attribution for every 学习模式 charge.
const (
	studyFeatureSkillMap = "skill_map"
	studyFeatureBank     = "study_bank"
	studyFeatureGrade    = "study_grade"
	studyFeatureLesson   = "study_lesson"
)

// ragProjectCostReader is the optional billing capability behind
// GET study/costs; the meter interface stays minimal for tests.
type ragProjectCostReader interface {
	GetProjectCostSummary(ctx context.Context, userID, projectID string) (*billing.ProjectCostSummary, error)
	GetProjectUsage(ctx context.Context, userID, projectID string, limit int) ([]billing.UserUsageItem, error)
}

// handleStudyCosts answers "what has this course cost me": totals by
// feature plus the most recent charges, all from the ledger.
func (h *RAGHandler) handleStudyCosts(
	w http.ResponseWriter, r *http.Request, project *models.AIProject,
) {
	reader, ok := h.billing.(ragProjectCostReader)
	if !ok || h.billing == nil {
		WriteJSON(w, map[string]any{
			"billing_enabled": false,
			"summary": billing.ProjectCostSummary{
				ProjectID: project.ID, ByFeature: map[string]float64{},
			},
			"items": []billing.UserUsageItem{},
		})
		return
	}
	summary, err := reader.GetProjectCostSummary(r.Context(), project.UserID, project.ID)
	if err != nil {
		http.Error(w, "failed to load course costs", http.StatusInternalServerError)
		return
	}
	items, err := reader.GetProjectUsage(r.Context(), project.UserID, project.ID, 40)
	if err != nil {
		http.Error(w, "failed to load course usage", http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]any{
		"billing_enabled": true,
		"summary":         summary,
		"items":           items,
	})
}

// One lesson generation per (learner, course, skill) at a time.
var studyLessonLocks sync.Map

func studyLessonLock(key string) *sync.Mutex {
	lock, _ := studyLessonLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// handleStudyLesson returns the skill's 讲解卡, generating and freezing it on
// the first request. Reading an existing lesson is free.
func (h *RAGHandler) handleStudyLesson(
	w http.ResponseWriter, r *http.Request, project *models.AIProject,
) {
	skillLabel := clampStudyText(r.URL.Query().Get("skill_label"), skillMapMaxLabelRunes)
	skillKey := skillMapLabelKey(skillLabel)
	if skillKey == "" {
		http.Error(w, "skill_label is required", http.StatusBadRequest)
		return
	}
	lesson, err := h.store.GetStudyLesson(r.Context(), project.UserID, project.ID, skillKey)
	if err != nil {
		http.Error(w, "failed to load lesson", http.StatusInternalServerError)
		return
	}
	if lesson != nil {
		WriteJSON(w, map[string]any{"lesson": lesson, "generated": false, "cost_usd": 0})
		return
	}
	if h.svc == nil {
		http.Error(w, "AI service is unavailable", http.StatusServiceUnavailable)
		return
	}
	skill := h.findSkillMapSkill(r.Context(), project, skillKey)
	if skill == nil {
		http.Error(w, "skill is not in this course's skill map; regenerate the map first", http.StatusUnprocessableEntity)
		return
	}
	model, modelErr := h.studyModel(r.Context(), project.UserID)
	if modelErr != nil {
		writeArtifactModelResolutionError(w, modelErr)
		return
	}
	lockKey := project.UserID + "|" + project.ID + "|" + skillKey + "|" + store.StudyContentVersion(r.Context())
	lock := studyLessonLock(lockKey)
	lock.Lock()
	defer lock.Unlock()
	// A concurrent request may have frozen it while we waited.
	if lesson, err = h.store.GetStudyLesson(r.Context(), project.UserID, project.ID, skillKey); err == nil && lesson != nil {
		WriteJSON(w, map[string]any{"lesson": lesson, "generated": false, "cost_usd": 0})
		return
	}
	rubric, err := h.store.GetStudyRubric(r.Context(), project.UserID, project.ID, skillKey)
	if err != nil {
		http.Error(w, "failed to load rubric", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	grounding, err := h.groundedStudyContext(ctx, project, skill, studyLessonContext(skill, rubric))
	if err != nil {
		http.Error(w, "failed to retrieve course materials", http.StatusInternalServerError)
		return
	}
	identity := project.ID + "|" + skillKey + "|" + store.StudyContentVersion(ctx)
	meter, ctx := h.newRAGMeter(ctx, "", "study-lesson:"+identity, studyFeatureLesson, project.ID)
	ctx = rag.WithProviderOperationID(ctx, stableProviderOperationID("study-lesson", identity))
	content, usage, duration, err := h.svc.BuildArtifactFromContextWithConfigUsage(
		ctx,
		"project/"+project.ID+"/study_lesson",
		studyLessonInstruction(),
		grounding,
		"",
		&rag.ChatOverrides{Model: model, ReasoningEffort: "low"},
	)
	if err != nil {
		if h.isRAGAccountingError(err) {
			h.writeRAGAccountingError(w, err)
			return
		}
		log.Printf("generate study lesson: %v", err)
		http.Error(w, "lesson generation failed", ragServiceErrorStatus(err))
		return
	}
	if usage != nil {
		metrics.RecordSummarize(&metrics.Usage{
			PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
			TotalTokens: usage.TotalTokens, CachedTokens: usage.CachedTokens,
			CacheWriteTokens: usage.CacheWriteTokens, Model: usage.Model,
		}, duration.Milliseconds())
	}
	var raw studyLessonDocument
	if err := studyExtractJSON(content, &raw); err != nil {
		http.Error(w, "lesson generation returned an invalid result; please retry", http.StatusBadGateway)
		return
	}
	encoded, err := validateStudyLesson(&raw)
	if err != nil {
		log.Printf("validate study lesson: %v", err)
		http.Error(w, "lesson generation returned an invalid result; please retry", http.StatusBadGateway)
		return
	}
	claims := auth.GetUserClaims(r.Context())
	newLesson := &models.StudyLesson{
		TenantID: claims.TenantID, UserID: project.UserID, ProjectID: project.ID,
		SkillKey: skillKey, SkillLabel: firstNonEmpty(skill.Label, skillLabel), Content: encoded,
	}
	if usage != nil {
		newLesson.Model = usage.Model
	}
	if err := h.store.CreateStudyLesson(r.Context(), newLesson); err != nil {
		log.Printf("save study lesson: %v", err)
		http.Error(w, "failed to save lesson", http.StatusInternalServerError)
		return
	}
	stored, err := h.store.GetStudyLesson(r.Context(), project.UserID, project.ID, skillKey)
	if err != nil || stored == nil {
		http.Error(w, "failed to load lesson", http.StatusInternalServerError)
		return
	}
	WriteJSON(w, map[string]any{"lesson": stored, "generated": true, "cost_usd": meter.ChargedUSD()})
}

type studyNextRequest struct {
	SkillLabel        string `json:"skill_label"`
	ClientRequestID   string `json:"client_request_id"`
	PracticeSessionID string `json:"practice_session_id"`
}

// studyServe is the wire shape of one served scenario.
type studyServe struct {
	ScenarioID string               `json:"scenario_id"`
	Difficulty int                  `json:"difficulty"`
	Level      string               `json:"level"`
	Generated  bool                 `json:"generated"`
	Scenario   studyScenarioContent `json:"scenario"`
	Scaffold   studyScaffold        `json:"scaffold"`
	CoachLine  string               `json:"coach_line,omitempty"`
	// What generating this item cost (0 when served from the bank).
	CostUSD float64 `json:"cost_usd"`
}

//nolint:gocyclo // Coordinates bank lookup, on-demand generation, and idempotency.
func (h *RAGHandler) handleStudyNext(
	w http.ResponseWriter, r *http.Request, project *models.AIProject,
) {
	if h.svc == nil {
		http.Error(w, "AI service is unavailable", http.StatusServiceUnavailable)
		return
	}
	var req studyNextRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	skillLabel := clampStudyText(req.SkillLabel, skillMapMaxLabelRunes)
	skillKey := skillMapLabelKey(skillLabel)
	if skillKey == "" {
		http.Error(w, "skill_label is required", http.StatusBadRequest)
		return
	}
	req.ClientRequestID = strings.TrimSpace(req.ClientRequestID)
	if req.ClientRequestID == "" || len(req.ClientRequestID) > 128 {
		http.Error(w, "client_request_id is required and must be at most 128 characters", http.StatusBadRequest)
		return
	}
	req.PracticeSessionID = strings.TrimSpace(req.PracticeSessionID)
	if len(req.PracticeSessionID) > 128 {
		http.Error(w, "practice_session_id must be at most 128 characters", http.StatusBadRequest)
		return
	}
	state, err := h.store.GetStudySkillState(
		r.Context(), project.UserID, project.ID, skillKey,
	)
	if err != nil {
		http.Error(w, "failed to load study state", http.StatusInternalServerError)
		return
	}
	level := "learner"
	if state != nil {
		level = state.Level
	}
	difficulty := studyDifficultyForLevel(level)
	last, err := h.store.GetLastStudyAttempt(r.Context(), project.UserID, project.ID, skillKey)
	if err != nil {
		http.Error(w, "failed to load study history", http.StatusInternalServerError)
		return
	}
	scaffold := studyScaffoldFor(level, last)
	exclude, err := h.store.ListPracticeSessionScenarioIDs(
		r.Context(), project.UserID, project.ID, req.PracticeSessionID,
	)
	if err != nil {
		http.Error(w, "failed to load practice session", http.StatusInternalServerError)
		return
	}
	minUses, err := h.store.MinStudyScenarioUses(
		r.Context(), project.UserID, project.ID, skillKey, difficulty,
	)
	if err != nil {
		http.Error(w, "failed to inspect scenario bank", http.StatusInternalServerError)
		return
	}
	count, err := h.store.CountStudyScenarios(
		r.Context(), project.UserID, project.ID, skillKey, difficulty,
	)
	if err != nil {
		http.Error(w, "failed to inspect scenario bank", http.StatusInternalServerError)
		return
	}
	claims := auth.GetUserClaims(r.Context())
	if count > 0 {
		scenario, pickErr := h.store.PickStudyScenario(
			r.Context(), project.UserID, project.ID, skillKey, difficulty, exclude,
		)
		if pickErr != nil {
			http.Error(w, "failed to pick a scenario", http.StatusInternalServerError)
			return
		}
		if scenario != nil {
			if minUses >= studyScenarioRefillAt {
				// Worn bank: serve what exists now, top up behind the learner.
				h.refillStudyBankAsync(project, claims.TenantID, skillKey, skillLabel, difficulty, last, store.StudyContentVersion(r.Context()))
			}
			if h.writeServedScenario(r.Context(), w, scenario, level, scaffold, last, false) {
				return
			}
			log.Printf("study scenario %s content is unreadable; regenerating bank", scenario.ID)
		}
	}

	// Bank is empty at this difficulty or this session already used every
	// item: the learner is waiting, so generate the rubric (once) and a small
	// batch synchronously.
	skill := h.findSkillMapSkill(r.Context(), project, skillKey)
	if skill == nil {
		http.Error(w, "skill is not in this course's skill map; regenerate the map first", http.StatusUnprocessableEntity)
		return
	}
	rubric, err := h.store.GetStudyRubric(r.Context(), project.UserID, project.ID, skillKey)
	if err != nil {
		http.Error(w, "failed to load rubric", http.StatusInternalServerError)
		return
	}
	model, modelErr := h.studyModel(r.Context(), project.UserID)
	if modelErr != nil {
		writeArtifactModelResolutionError(w, modelErr)
		return
	}
	existingCount, err := h.store.CountStudyScenarios(
		r.Context(), project.UserID, project.ID, skillKey, 0,
	)
	if err != nil {
		http.Error(w, "failed to inspect scenario bank", http.StatusInternalServerError)
		return
	}
	requestHash, err := hashAIGenerationPayload(struct {
		RequestKind    string `json:"request_kind"`
		ProjectID      string `json:"project_id"`
		SkillKey       string `json:"skill_key"`
		Difficulty     int    `json:"difficulty"`
		ExistingCount  int    `json:"existing_count"`
		RefillSequence int    `json:"refill_sequence"`
		ContentVersion string `json:"content_version"`
	}{studyNextRequestKind, project.ID, skillKey, difficulty, existingCount, existingCount / studyScenarioColdBatch, store.StudyContentVersion(r.Context())})
	if err != nil {
		http.Error(w, "failed to identify AI request", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	generationClaim, replay, err := h.beginAIGeneration(
		ctx, req.ClientRequestID, studyNextRequestKind, requestHash, "",
	)
	if err != nil {
		writeAIGenerationBeginError(w, err)
		return
	}
	if replay != nil {
		if err := writeAIGenerationReplay(w, replay); err != nil {
			log.Printf("write replayed study scenario: %v", err)
		}
		return
	}
	generationCompleted := false
	defer func() {
		if !generationCompleted {
			h.failAIGeneration(generationClaim, "study bank generation did not complete")
		}
	}()
	meter, ctx := h.newRAGMeter(
		ctx, "", aiGenerationBillingNamespace(generationClaim), studyFeatureBank, project.ID,
	)
	generationCtx := rag.WithProviderOperationID(
		ctx, stableProviderOperationID("study-bank", requestHash),
	)
	bank, err := h.generateStudyBank(generationCtx, &studyBankRequest{
		project: project, tenantID: claims.TenantID,
		skillKey: skillKey, skillLabel: skillLabel, skill: skill,
		rubric: rubric, model: model, last: last,
		batch: studyScenarioColdBatch, difficulty: difficulty,
	})
	if err != nil {
		h.writeStudyBankError(w, err)
		return
	}
	served := bank.scenarios[0]
	for _, candidate := range bank.scenarios[1:] {
		if absInt(candidate.Difficulty-difficulty) < absInt(served.Difficulty-difficulty) {
			served = candidate
		}
	}
	response, ok := h.buildServedScenario(r.Context(), served, level, scaffold, last, true)
	if !ok {
		http.Error(w, "failed to serve generated scenario", http.StatusInternalServerError)
		return
	}
	response.CostUSD = meter.ChargedUSD()
	completeCtx, completeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer completeCancel()
	if err := h.completeAIGeneration(completeCtx, generationClaim, response); err != nil {
		log.Printf("complete study bank generation: %v", err)
		http.Error(w, "practice response could not be committed", http.StatusInternalServerError)
		return
	}
	generationCompleted = true
	WriteJSON(w, response)
}

type studyBankRequest struct {
	project    *models.AIProject
	tenantID   string
	skillKey   string
	skillLabel string
	skill      *skillMapSkill
	rubric     *models.StudyRubric
	model      string
	last       *models.StudyAttempt
	batch      int
	difficulty int
}

type studyBankResult struct {
	scenarios []*models.StudyScenario
	usage     *openai.Usage
}

var (
	errStudyInvalidRubric = errors.New("practice generation returned an invalid rubric")
	errStudyNoScenarios   = errors.New("practice generation returned no usable scenarios")
)

// studyStoreError marks a persistence failure so the HTTP layer answers 500
// instead of blaming the model.
type studyStoreError struct{ err error }

func (e *studyStoreError) Error() string { return e.err.Error() }
func (e *studyStoreError) Unwrap() error { return e.err }

// generateStudyBank asks the model for a batch of scenarios at one difficulty
// (plus the frozen rubric when the skill has none yet) and persists them.
// Billing and idempotency come from ctx; both the synchronous cold start and
// the background refill go through here.
func (h *RAGHandler) generateStudyBank(
	ctx context.Context, req *studyBankRequest,
) (*studyBankResult, error) {
	difficulty := clampStudyDifficulty(req.difficulty)
	base := studyBankContext(req.skill, req.last)
	if req.rubric != nil {
		base += "\n固定评分标准：\n" + string(req.rubric.Rubric)
	}
	grounding, err := h.groundedStudyContext(ctx, req.project, req.skill, base)
	if err != nil {
		return nil, err
	}
	content, usage, duration, err := h.svc.BuildArtifactFromContextWithConfigUsage(
		ctx,
		"project/"+req.project.ID+"/study_bank",
		studyBankInstruction(req.rubric == nil, req.batch, difficulty),
		grounding,
		"",
		&rag.ChatOverrides{Model: req.model, ReasoningEffort: "low"},
	)
	if err != nil {
		return nil, err
	}
	if usage != nil {
		metrics.RecordSummarize(&metrics.Usage{
			PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
			TotalTokens: usage.TotalTokens, CachedTokens: usage.CachedTokens,
			CacheWriteTokens: usage.CacheWriteTokens, Model: usage.Model,
		}, duration.Milliseconds())
	}
	var raw studyBankLLMOutput
	if err := studyExtractJSON(content, &raw); err != nil {
		return nil, errStudyInvalidJSON
	}
	if req.rubric == nil {
		rubricJSON, rubricErr := validateStudyRubric(raw.Rubric)
		if rubricErr != nil {
			log.Printf("generate study rubric: %v", rubricErr)
			return nil, errStudyInvalidRubric
		}
		newRubric := &models.StudyRubric{
			TenantID: req.tenantID, UserID: req.project.UserID, ProjectID: req.project.ID,
			SkillKey: req.skillKey, SkillLabel: req.skillLabel, Rubric: rubricJSON,
		}
		if usage != nil {
			newRubric.Model = usage.Model
		}
		if err := h.store.CreateStudyRubric(ctx, newRubric); err != nil {
			return nil, &studyStoreError{err: fmt.Errorf("save rubric: %w", err)}
		}
	}
	contents := validateStudyScenarios(&raw)
	if len(contents) == 0 {
		return nil, errStudyNoScenarios
	}
	scenarios := make([]*models.StudyScenario, 0, len(contents))
	for index := range contents {
		// The batch was requested at one difficulty; the format decides how
		// far a self-reported difficulty may drift from it.
		entryDifficulty := clampStudyDifficultyForFormat(difficulty, contents[index].Format)
		contents[index].Lang = studyLangForDifficulty(entryDifficulty)
		encoded, encodeErr := json.Marshal(&contents[index])
		if encodeErr != nil {
			return nil, &studyStoreError{err: fmt.Errorf("serialize scenario: %w", encodeErr)}
		}
		row := &models.StudyScenario{
			TenantID: req.tenantID, UserID: req.project.UserID, ProjectID: req.project.ID,
			SkillKey: req.skillKey, SkillLabel: req.skillLabel,
			Difficulty: entryDifficulty, Content: encoded,
		}
		if usage != nil {
			row.Model = usage.Model
		}
		scenarios = append(scenarios, row)
	}
	if err := h.store.CreateStudyScenarios(ctx, scenarios); err != nil {
		return nil, &studyStoreError{err: fmt.Errorf("save scenarios: %w", err)}
	}
	return &studyBankResult{scenarios: scenarios, usage: usage}, nil
}

func (h *RAGHandler) writeStudyBankError(w http.ResponseWriter, err error) {
	var storeErr *studyStoreError
	switch {
	case h.isRAGAccountingError(err):
		h.writeRAGAccountingError(w, err)
	case errors.As(err, &storeErr):
		log.Printf("persist study bank: %v", err)
		http.Error(w, "failed to save practice material", http.StatusInternalServerError)
	case errors.Is(err, errStudyInvalidJSON):
		http.Error(w, "practice generation returned an invalid result; please retry", http.StatusBadGateway)
	case errors.Is(err, errStudyInvalidRubric):
		http.Error(w, "practice generation returned an invalid rubric; please retry", http.StatusBadGateway)
	case errors.Is(err, errStudyNoScenarios):
		http.Error(w, "practice generation returned no usable scenarios; please retry", http.StatusBadGateway)
	default:
		log.Printf("generate study bank: %v", err)
		http.Error(w, "practice generation failed", ragServiceErrorStatus(err))
	}
}

// One refill per (learner, course, skill) at a time.
var studyRefillInFlight sync.Map

// refillStudyBankAsync tops up a worn bank without making the learner wait.
// The next request finds fresh items; a failure only shows up in the log.
func (h *RAGHandler) refillStudyBankAsync(
	project *models.AIProject, tenantID, skillKey, skillLabel string, difficulty int,
	last *models.StudyAttempt, version string,
) {
	if h.svc == nil || h.store == nil {
		return
	}
	done, allowed := deployment.Default.BeginTask()
	if !allowed {
		return
	}
	key := project.UserID + "|" + project.ID + "|" + skillKey + "|" + version
	if _, loaded := studyRefillInFlight.LoadOrStore(key, struct{}{}); loaded {
		done()
		return
	}
	go func() {
		defer done()
		defer studyRefillInFlight.Delete(key)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		defer cancel()
		ctx, err := h.studyVersionContext(ctx, project)
		if err != nil || store.StudyContentVersion(ctx) != version {
			return
		}
		skill := h.findSkillMapSkill(ctx, project, skillKey)
		if skill == nil {
			return
		}
		rubric, err := h.store.GetStudyRubric(ctx, project.UserID, project.ID, skillKey)
		if err != nil || rubric == nil {
			log.Printf("refill study bank %s: rubric unavailable: %v", skillKey, err)
			return
		}
		model, err := h.studyModel(ctx, project.UserID)
		if err != nil {
			log.Printf("refill study bank %s: %v", skillKey, err)
			return
		}
		count, err := h.store.CountStudyScenarios(ctx, project.UserID, project.ID, skillKey, 0)
		if err != nil {
			log.Printf("refill study bank %s: %v", skillKey, err)
			return
		}
		identity := fmt.Sprintf("%s|%s|%d|%d|%s", project.ID, skillKey, difficulty, count, version)
		refillProjectID := project.ID
		meter := &ragHTTPUsageMeter{
			billing:         h.billing,
			userID:          project.UserID,
			tenantID:        tenantID,
			stableNamespace: "study-refill:" + identity,
			feature:         studyFeatureBank,
			projectID:       &refillProjectID,
		}
		ctx = rag.WithProviderUsageMeter(ctx, meter)
		ctx = rag.WithProviderOperationID(ctx, stableProviderOperationID("study-refill", identity))
		if _, err := h.generateStudyBank(ctx, &studyBankRequest{
			project: project, tenantID: tenantID,
			skillKey: skillKey, skillLabel: skillLabel, skill: skill,
			rubric: rubric, model: model, last: last,
			batch: studyScenarioBatchSize, difficulty: difficulty,
		}); err != nil {
			log.Printf("refill study bank %s: %v", skillKey, err)
		}
	}()
}

func (h *RAGHandler) buildServedScenario(
	ctx context.Context,
	scenario *models.StudyScenario,
	level string,
	scaffold studyScaffold,
	last *models.StudyAttempt,
	generated bool,
) (studyServe, bool) {
	var content studyScenarioContent
	if scenario == nil || json.Unmarshal(scenario.Content, &content) != nil || content.Question == "" {
		return studyServe{}, false
	}
	if touchErr := h.store.TouchStudyScenarioUse(ctx, scenario.ID); touchErr != nil {
		log.Printf("touch study scenario use: %v", touchErr)
	}
	if content.Lang == "" {
		content.Lang = studyLangForDifficulty(scenario.Difficulty)
	}
	return studyServe{
		ScenarioID: scenario.ID,
		Difficulty: scenario.Difficulty,
		Level:      level,
		Generated:  generated,
		Scenario:   publicStudyScenario(&content, scaffold),
		Scaffold:   scaffold,
		CoachLine:  studyCoachLine(scaffold, last),
	}, true
}

func (h *RAGHandler) writeServedScenario(
	ctx context.Context,
	w http.ResponseWriter,
	scenario *models.StudyScenario,
	level string,
	scaffold studyScaffold,
	last *models.StudyAttempt,
	generated bool,
) bool {
	response, ok := h.buildServedScenario(ctx, scenario, level, scaffold, last, generated)
	if !ok {
		return false
	}
	WriteJSON(w, response)
	return true
}

type studyRevealRequest struct {
	ScenarioID string `json:"scenario_id"`
}

// handleStudyReveal shows the answer and explanation without grading: "不会，
// 直接看解析". Nothing is recorded against the learner. Legacy items that were
// generated without a teaching layer get one filled in (charged once).
func (h *RAGHandler) handleStudyReveal(
	w http.ResponseWriter, r *http.Request, project *models.AIProject,
) {
	var req studyRevealRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if uuid.Validate(strings.TrimSpace(req.ScenarioID)) != nil {
		http.Error(w, "scenario_id must be a UUID", http.StatusBadRequest)
		return
	}
	scenario, err := h.store.GetStudyScenario(
		r.Context(), project.UserID, project.ID, strings.TrimSpace(req.ScenarioID),
	)
	if err != nil {
		http.Error(w, "failed to load scenario", http.StatusInternalServerError)
		return
	}
	if scenario == nil {
		http.Error(w, "scenario not found", http.StatusNotFound)
		return
	}
	var content studyScenarioContent
	if err := json.Unmarshal(scenario.Content, &content); err != nil || content.Question == "" {
		http.Error(w, "scenario content is unreadable", http.StatusInternalServerError)
		return
	}
	cost := 0.0
	if !studyHasTeaching(&content) && h.svc != nil {
		charged, fillErr := h.fillStudyTeaching(r.Context(), project, scenario, &content)
		if fillErr != nil {
			if h.isRAGAccountingError(fillErr) {
				h.writeRAGAccountingError(w, fillErr)
				return
			}
			log.Printf("fill study teaching for %s: %v", scenario.ID, fillErr)
		}
		cost = charged
	}
	WriteJSON(w, map[string]any{
		"scenario_id": scenario.ID,
		"reveal":      studyRevealFor(&content),
		"cost_usd":    cost,
	})
}

// fillStudyTeaching writes an explanation and model answer into a legacy
// bank item so the learner never meets a question without one.
func (h *RAGHandler) fillStudyTeaching(
	ctx context.Context, project *models.AIProject,
	scenario *models.StudyScenario, content *studyScenarioContent,
) (float64, error) {
	ctx = store.WithStudyContentVersion(ctx, scenario.ContentVersion)
	model, err := h.studyModel(ctx, project.UserID)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	meter, ctx := h.newRAGMeter(ctx, "", "study-teach:"+scenario.ID, studyFeatureBank, project.ID)
	ctx = rag.WithProviderOperationID(ctx, stableProviderOperationID("study-teach", scenario.ID))
	rubric, err := h.store.GetStudyRubric(ctx, project.UserID, project.ID, scenario.SkillKey)
	if err != nil {
		return 0, err
	}
	rubricJSON := json.RawMessage(`{}`)
	if rubric != nil {
		rubricJSON = rubric.Rubric
	}
	sub := &studySubmission{Format: firstNonEmpty(content.Format, studyFormatOpen), Reason: "（学生选择直接看解析，没有作答）"}
	raw, _, err := h.completeStudyGrade(ctx, project, rubricJSON, content, sub, scenario.Difficulty, false, false, model, true)
	if err != nil {
		return meter.ChargedUSD(), err
	}
	if raw.Explanation == "" && raw.ModelAnswer == "" {
		return meter.ChargedUSD(), nil
	}
	h.persistStudyTeaching(ctx, project, scenario, content, raw)
	return meter.ChargedUSD(), nil
}

// persistStudyTeaching merges grader-written teaching fields into the item.
func (h *RAGHandler) persistStudyTeaching(
	ctx context.Context, project *models.AIProject, scenario *models.StudyScenario,
	content *studyScenarioContent, raw *studyGradeLLMOutput,
) {
	if raw == nil || (raw.Explanation == "" && raw.ModelAnswer == "") {
		return
	}
	if content.Explanation == "" {
		content.Explanation = raw.Explanation
	}
	if content.ModelAnswer == "" {
		content.ModelAnswer = raw.ModelAnswer
	}
	if len(content.Targets) == 0 {
		content.Targets = raw.Targets
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return
	}
	if err := h.store.UpdateStudyScenarioContent(ctx, project.UserID, project.ID, scenario.ID, encoded); err != nil {
		log.Printf("persist study teaching for %s: %v", scenario.ID, err)
	}
}

// completeStudyGrade runs one grading call and validates its output.
func (h *RAGHandler) completeStudyGrade(
	ctx context.Context, project *models.AIProject, rubricJSON json.RawMessage,
	content *studyScenarioContent, sub *studySubmission, difficulty int,
	usedHint, usedZH bool, model string, needsTeaching bool,
) (*studyGradeLLMOutput, *openai.Usage, error) {
	rawContent, usage, duration, err := h.svc.BuildArtifactFromContextWithConfigUsage(
		ctx,
		"project/"+project.ID+"/study_grade",
		studyGradeInstruction(needsTeaching),
		studyGradeContext(rubricJSON, content, sub, difficulty, usedHint, usedZH),
		"",
		&rag.ChatOverrides{Model: model, ReasoningEffort: "low"},
	)
	if err != nil {
		return nil, usage, err
	}
	if usage != nil {
		metrics.RecordSummarize(&metrics.Usage{
			PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
			TotalTokens: usage.TotalTokens, CachedTokens: usage.CachedTokens,
			CacheWriteTokens: usage.CacheWriteTokens, Model: usage.Model,
		}, duration.Milliseconds())
	}
	var rawGrade studyGradeLLMOutput
	if err := studyExtractJSON(rawContent, &rawGrade); err != nil {
		return nil, usage, errStudyInvalidJSON
	}
	graded, err := validateStudyGrade(&rawGrade)
	if err != nil {
		return nil, usage, fmt.Errorf("%w: %v", errStudyInvalidJSON, err)
	}
	return graded, usage, nil
}

type studyAttemptRequest struct {
	ScenarioID string `json:"scenario_id"`
	// Open: the answer. Cloze: the term for the blank.
	Answer string `json:"answer"`
	// Single / multi: selected option indexes.
	Choices []int `json:"choices"`
	// True/false: the judgment.
	AnswerBool *bool `json:"answer_bool"`
	// Choice, tf and cloze formats: the learner's explanation (optional).
	Reason            string `json:"reason"`
	UsedHint          bool   `json:"used_hint"`
	UsedZH            bool   `json:"used_zh"`
	PracticeSessionID string `json:"practice_session_id"`
	ClientRequestID   string `json:"client_request_id"`
}

//nolint:gocyclo // Coordinates rubric-anchored grading, XP, progression and the reveal.
func (h *RAGHandler) handleStudyAttempt(
	w http.ResponseWriter, r *http.Request, project *models.AIProject,
) {
	if h.svc == nil {
		http.Error(w, "AI service is unavailable", http.StatusServiceUnavailable)
		return
	}
	var req studyAttemptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if uuid.Validate(strings.TrimSpace(req.ScenarioID)) != nil {
		http.Error(w, "scenario_id must be a UUID", http.StatusBadRequest)
		return
	}
	req.ClientRequestID = strings.TrimSpace(req.ClientRequestID)
	if req.ClientRequestID == "" || len(req.ClientRequestID) > 128 {
		http.Error(w, "client_request_id is required and must be at most 128 characters", http.StatusBadRequest)
		return
	}
	req.PracticeSessionID = strings.TrimSpace(req.PracticeSessionID)
	if len(req.PracticeSessionID) > 128 {
		http.Error(w, "practice_session_id must be at most 128 characters", http.StatusBadRequest)
		return
	}
	scenario, err := h.store.GetStudyScenario(
		r.Context(), project.UserID, project.ID, strings.TrimSpace(req.ScenarioID),
	)
	if err != nil {
		http.Error(w, "failed to load scenario", http.StatusInternalServerError)
		return
	}
	if scenario == nil {
		http.Error(w, "scenario not found", http.StatusNotFound)
		return
	}
	var content studyScenarioContent
	if err := json.Unmarshal(scenario.Content, &content); err != nil || content.Question == "" {
		http.Error(w, "scenario content is unreadable", http.StatusInternalServerError)
		return
	}
	sub, err := buildStudySubmission(&content, &req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	answer := studySubmissionText(sub, &content)
	r = r.WithContext(store.WithStudyContentVersion(r.Context(), scenario.ContentVersion))
	rubric, err := h.store.GetStudyRubric(
		r.Context(), project.UserID, project.ID, scenario.SkillKey,
	)
	if err != nil {
		http.Error(w, "failed to load rubric", http.StatusInternalServerError)
		return
	}
	if rubric == nil {
		http.Error(w, "this skill has no rubric yet; request a scenario first", http.StatusConflict)
		return
	}
	claims := auth.GetUserClaims(r.Context())
	model, modelErr := h.studyModel(r.Context(), project.UserID)
	if modelErr != nil {
		writeArtifactModelResolutionError(w, modelErr)
		return
	}
	requestHash, err := hashAIGenerationPayload(struct {
		RequestKind string `json:"request_kind"`
		ScenarioID  string `json:"scenario_id"`
		Answer      string `json:"answer"`
		UsedHint    bool   `json:"used_hint"`
		UsedZH      bool   `json:"used_zh"`
	}{studyGradeRequestKind, scenario.ID, answer, req.UsedHint, req.UsedZH})
	if err != nil {
		http.Error(w, "failed to identify AI request", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	generationClaim, replay, err := h.beginAIGeneration(
		ctx, req.ClientRequestID, studyGradeRequestKind, requestHash, "",
	)
	if err != nil {
		writeAIGenerationBeginError(w, err)
		return
	}
	if replay != nil {
		if err := writeAIGenerationReplay(w, replay); err != nil {
			log.Printf("write replayed study grade: %v", err)
		}
		return
	}
	generationCompleted := false
	defer func() {
		if !generationCompleted {
			h.failAIGeneration(generationClaim, "study grading did not complete")
		}
	}()
	meter, ctx := h.newRAGMeter(
		ctx, "", aiGenerationBillingNamespace(generationClaim), studyFeatureGrade, project.ID,
	)
	generationCtx := rag.WithProviderOperationID(
		ctx, stableProviderOperationID("study-grade", requestHash),
	)
	needsTeaching := !studyHasTeaching(&content)
	graded, usage, err := h.completeStudyGrade(
		generationCtx, project, rubric.Rubric, &content, sub, scenario.Difficulty,
		req.UsedHint, req.UsedZH, model, needsTeaching,
	)
	if err != nil {
		if h.isRAGAccountingError(err) {
			h.writeRAGAccountingError(w, err)
			return
		}
		if errors.Is(err, errStudyInvalidJSON) {
			log.Printf("grade study attempt: %v", err)
			http.Error(w, "grading returned an invalid result; please retry", http.StatusBadGateway)
			return
		}
		log.Printf("grade study attempt: %v", err)
		http.Error(w, "grading failed", ragServiceErrorStatus(err))
		return
	}
	if needsTeaching {
		h.persistStudyTeaching(r.Context(), project, scenario, &content, graded)
	}
	// The answer key outranks the model: wrong choices never pass.
	graded.Grade = capStudyGrade(graded.Grade, sub, graded.AnswerCorrect)
	answerCorrect := true
	switch sub.Format {
	case studyFormatSingle, studyFormatMulti, studyFormatTF:
		answerCorrect = sub.Correct != nil && *sub.Correct
	case studyFormatCloze:
		answerCorrect = graded.AnswerCorrect == nil || *graded.AnswerCorrect
	}
	previous, err := h.store.GetStudySkillState(
		r.Context(), project.UserID, project.ID, scenario.SkillKey,
	)
	if err != nil {
		http.Error(w, "failed to load study state", http.StatusInternalServerError)
		return
	}
	state := models.StudySkillState{
		UserID: project.UserID, ProjectID: project.ID, SkillKey: scenario.SkillKey,
		TenantID: claims.TenantID, SkillLabel: scenario.SkillLabel, Level: "learner",
	}
	if previous != nil {
		state = *previous
	}
	priorAttempts, countErr := h.store.CountScenarioAttempts(
		r.Context(), project.UserID, project.ID, scenario.ID,
	)
	if countErr != nil {
		http.Error(w, "failed to load scenario history", http.StatusInternalServerError)
		return
	}
	firstTry := priorAttempts == 0
	lastOnScenario, lastErr := h.store.GetLastScenarioAttempt(
		r.Context(), project.UserID, project.ID, scenario.ID,
	)
	if lastErr != nil {
		http.Error(w, "failed to load scenario history", http.StatusInternalServerError)
		return
	}
	passed := studyGradeRank(graded.Grade) >= studyGradeRank(studyGradeC)
	selfCorrected := passed && lastOnScenario != nil &&
		studyGradeRank(lastOnScenario.Grade) < studyGradeRank(studyGradeC)
	sessionLast, sessionErr := h.store.GetLastPracticeSessionAttempt(
		r.Context(), project.UserID, project.ID, req.PracticeSessionID,
	)
	if sessionErr != nil {
		http.Error(w, "failed to load practice session", http.StatusInternalServerError)
		return
	}
	prevCombo := 0
	if sessionLast != nil {
		prevCombo = sessionLast.Combo
	}
	combo := nextStudyCombo(prevCombo, graded.Grade, req.UsedHint)
	if studyLanguageIndependence(&state, req.UsedZH, graded.Grade) {
		graded.Bonuses = append(graded.Bonuses, studyBonusLanguageIndependence)
	}
	xp, bonuses := studyAttemptXP(graded.Grade, req.UsedHint, firstTry, selfCorrected, graded.Bonuses)
	xp = applyStudyDifficultyXP(xp, scenario.Difficulty)
	xp = applyStudyComboXP(xp, combo)
	leveledUp := false
	if !graded.LanguageIssue || passed {
		var up bool
		state.Level, state.CleanStreak, up = advanceStudyState(
			state.Level, state.CleanStreak, graded.Grade, req.UsedHint,
		)
		leveledUp = up
	}
	if !req.UsedZH && passed {
		state.EnSuccessStreak++
	} else if !req.UsedZH {
		state.EnSuccessStreak = 0
	}
	events := studyEvents(
		graded.Grade, scenario.Difficulty, req.UsedHint, req.UsedZH, firstTry,
		bonuses, state.LastErrorPattern, graded.ErrorPattern, sessionLast,
	)
	for _, event := range events {
		if event == studyEventLanguageSave {
			state.LanguageSaves++
		}
		if event == studyEventMisconceptionBroken {
			state.LastErrorPattern = ""
		}
	}
	if graded.ErrorPattern != "" {
		state.LastErrorPattern = graded.ErrorPattern
	}
	state.AttemptsCount++
	state.XPTotal += int64(xp)
	state.LastGrade = graded.Grade
	state.SkillLabel = scenario.SkillLabel
	bonusesJSON, err := json.Marshal(bonuses)
	if err != nil {
		http.Error(w, "failed to serialize attempt", http.StatusInternalServerError)
		return
	}
	eventsJSON, err := json.Marshal(events)
	if err != nil {
		http.Error(w, "failed to serialize attempt", http.StatusInternalServerError)
		return
	}
	scenarioID := scenario.ID
	attempt := models.StudyAttempt{
		TenantID: claims.TenantID, UserID: claims.UserID, ProjectID: project.ID,
		ScenarioID: &scenarioID, SkillKey: scenario.SkillKey, Answer: answer,
		Grade: graded.Grade, Feedback: graded.Feedback, NextStep: graded.NextStep,
		Bonuses: bonusesJSON, XP: xp, UsedHint: req.UsedHint, UsedZH: req.UsedZH,
		PracticeSessionID: req.PracticeSessionID, Combo: combo, Events: eventsJSON,
		ErrorPattern: graded.ErrorPattern, ClientRequestID: req.ClientRequestID,
	}
	if usage != nil {
		attempt.Model = usage.Model
	}
	if err := h.store.CreateStudyAttempt(r.Context(), &attempt); err != nil {
		http.Error(w, "failed to save attempt", http.StatusInternalServerError)
		return
	}
	if err := h.store.UpsertStudySkillState(r.Context(), &state); err != nil {
		http.Error(w, "failed to save study state", http.StatusInternalServerError)
		return
	}
	response := map[string]any{
		"grade":      graded.Grade,
		"feedback":   graded.Feedback,
		"next_step":  graded.NextStep,
		"bonuses":    bonuses,
		"xp":         xp,
		"combo":      combo,
		"events":     events,
		"used_hint":  req.UsedHint,
		"used_zh":    req.UsedZH,
		"leveled_up": leveledUp,
		"state":      state,

		"format":         sub.Format,
		"answer_correct": answerCorrect,
		"language_tip":   graded.LanguageTip,
		"cost_usd":       meter.ChargedUSD(),

		"difficulty":            scenario.Difficulty,
		"difficulty_multiplier": studyDifficultyMultiplier(scenario.Difficulty),
		"reveal":                studyRevealFor(&content),
		"retry_allowed":         !passed,
		"self_corrected":        selfCorrected,
	}
	completeCtx, completeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer completeCancel()
	if err := h.completeAIGeneration(completeCtx, generationClaim, response); err != nil {
		log.Printf("complete study grading: %v", err)
		http.Error(w, "grading response could not be committed", http.StatusInternalServerError)
		return
	}
	generationCompleted = true
	WriteJSON(w, response)
}

// findSkillMapSkill resolves one skill from the course's latest skill map.
func (h *RAGHandler) findSkillMapSkill(
	ctx context.Context, project *models.AIProject, skillKey string,
) *skillMapSkill {
	if doc, ok := ctx.Value(studyMapContextKey{}).(*skillMapDocument); ok {
		for index := range doc.Skills {
			if skillMapLabelKey(doc.Skills[index].Label) == skillKey {
				return &doc.Skills[index]
			}
		}
		return nil
	}
	artifact, err := h.store.GetLatestAIArtifactByProject(
		ctx, project.UserID, project.ID, skillMapArtifactType,
	)
	if err != nil || artifact == nil {
		return nil
	}
	doc := parseStoredSkillMap(artifact.Content)
	if doc == nil {
		return nil
	}
	for index := range doc.Skills {
		if skillMapLabelKey(doc.Skills[index].Label) == skillKey {
			return &doc.Skills[index]
		}
	}
	return nil
}

// studyModel resolves the model practice runs on (summary purpose).
func (h *RAGHandler) studyModel(ctx context.Context, userID string) (string, error) {
	if h.modelCatalog == nil {
		return config.Get().Models.Summary, nil
	}
	return h.modelCatalog.EffectiveModel(ctx, userID, modelcatalog.PurposeSummary)
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// buildStudySubmission validates the request against the scenario's format.
func buildStudySubmission(
	content *studyScenarioContent, req *studyAttemptRequest,
) (*studySubmission, error) {
	format := content.Format
	if format == "" {
		format = studyFormatOpen
	}
	sub := &studySubmission{Format: format}
	switch format {
	case studyFormatSingle, studyFormatMulti:
		if len(req.Choices) == 0 {
			return nil, errors.New("choices are required for this question")
		}
		if format == studyFormatSingle && len(req.Choices) != 1 {
			return nil, errors.New("pick exactly one option")
		}
		seen := map[int]bool{}
		for _, choice := range req.Choices {
			if choice < 0 || choice >= len(content.Options) || seen[choice] {
				return nil, errors.New("choices must be distinct option indexes")
			}
			seen[choice] = true
			sub.Choices = append(sub.Choices, choice)
		}
		sub.Reason = clampStudyText(firstNonEmpty(req.Reason, req.Answer), studyMaxAnswerRunes)
		correct := evaluateStudyChoices(content, sub.Choices)
		sub.Correct = &correct
	case studyFormatTF:
		if req.AnswerBool == nil {
			return nil, errors.New("answer_bool is required for this question")
		}
		value := *req.AnswerBool
		sub.Bool = &value
		sub.Reason = clampStudyText(firstNonEmpty(req.Reason, req.Answer), studyMaxAnswerRunes)
		correct := content.AnswerBool != nil && *content.AnswerBool == value
		sub.Correct = &correct
	case studyFormatCloze:
		sub.Fill = clampStudyText(req.Answer, studyMaxOptionRunes)
		if sub.Fill == "" {
			return nil, errors.New("answer is required")
		}
		sub.Reason = clampStudyText(req.Reason, studyMaxAnswerRunes)
	default:
		sub.Reason = clampStudyText(firstNonEmpty(req.Answer, req.Reason), studyMaxAnswerRunes)
		if sub.Reason == "" {
			return nil, errors.New("answer is required")
		}
	}
	return sub, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
