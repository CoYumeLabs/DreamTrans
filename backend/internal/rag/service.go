package rag

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	openaiprovider "github.com/dreamtrans/backend/internal/adapters/openai_provider"
	"github.com/dreamtrans/backend/internal/aiproviders"
	"github.com/dreamtrans/backend/internal/config"
	"github.com/dreamtrans/backend/internal/metrics"
)

const (
	ragAnswerMaxOutputTokens    = 2048
	artifactMaxOutputTokens     = 8192
	ragSummaryMaxOutputTokens   = 512
	reasoningMediumOutputTokens = 16384
	reasoningHighOutputTokens   = 24576
	reasoningMediumTimeout      = 90 * time.Second
	reasoningHighTimeout        = 120 * time.Second
)

// Service coordinates summarization, embedding and retrieval.
type Service struct {
	store     *Store
	embedder  EmbeddingProvider
	chatCfgFn func() (*openaiprovider.Config, error)
	configMu  sync.RWMutex
	// When false, IngestParagraph will not call LLM to summarize the paragraph;
	// it will directly use cleaned text for storage/embedding. Default false.
	ingestSummarizeEnabled bool
	// When false, the running session_summary will not be updated at all.
	// Useful to fully disable any summary output in UI.
	summaryOutputEnabled bool
	// When false, vector embeddings will not be computed or stored; retrieval uses only summary (if enabled).
	embedEnabled bool
	// live cache keeps freshest paragraphs before embeddings land
	liveMu          sync.RWMutex
	live            map[string]*liveBuffer
	liveLastUsed    map[string]time.Time
	liveMaxEntries  int
	liveMaxSessions int
	liveMaxAge      time.Duration
}

type liveEntry struct {
	Speaker   string
	Original  string
	Summary   string
	StartTime float64
	EndTime   float64
	AddedAt   time.Time
}

type liveBuffer struct {
	entries []*liveEntry
}

func (b *liveBuffer) append(entry *liveEntry, maxEntries int, maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	filtered := b.entries[:0]
	for _, it := range b.entries {
		if it.AddedAt.After(cutoff) {
			filtered = append(filtered, it)
		}
	}
	b.entries = filtered
	for i, it := range b.entries {
		sameTime := math.Abs(it.StartTime-entry.StartTime) < 0.05
		sameText := strings.EqualFold(strings.TrimSpace(it.Original), strings.TrimSpace(entry.Original))
		if sameTime && sameText {
			b.entries[i] = entry
			return
		}
	}
	b.entries = append(b.entries, entry)
	if len(b.entries) > maxEntries {
		b.entries = b.entries[len(b.entries)-maxEntries:]
	}
}

// ChatOverrides allows request-scoped chat configuration.
type ChatOverrides struct {
	APIKey          string
	APIBase         string
	Model           string
	Prompt          string
	ReasoningEffort string
}

// IngestResult describes the work actually performed for a paragraph. Callers
// that meter embedding usage must only charge when Embedded is true.
type IngestResult struct {
	Embedded      bool
	Duplicate     bool
	CanonicalText string
	EmbeddedText  string
	// StorageUsage is populated before the first persistent write and is also
	// returned when a later SQLite write fails.
	StorageUsage IngestStorageUsage
}

// NewServiceFromEnv builds a RAG service from environment variables.
func NewServiceFromEnv() (*Service, error) {
	// Validate the provider before opening SQLite. Otherwise a missing API key
	// would leak one database handle on every WebSocket connection attempt.
	emb, err := NewOpenAIEmbeddingFromEnv()
	if err != nil {
		return nil, err
	}
	dbPath := os.Getenv("RAG_DB_PATH")
	if dbPath == "" {
		dbPath = "./rag.db"
	}
	st, err := NewStore(dbPath)
	if err != nil {
		return nil, err
	}
	chatCfg := func() (*openaiprovider.Config, error) {
		// The chat model names its provider; an empty id means the default
		// provider's default model.
		return aiproviders.ConfigFor(config.Get().Models.Chat)
	}
	return &Service{
		store:                  st,
		embedder:               emb,
		chatCfgFn:              chatCfg,
		ingestSummarizeEnabled: false,
		summaryOutputEnabled:   false,
		embedEnabled:           true,
		live:                   make(map[string]*liveBuffer),
		liveLastUsed:           make(map[string]time.Time),
		liveMaxEntries:         18,
		liveMaxSessions:        2048,
		liveMaxAge:             4 * time.Minute,
	}, nil
}

// Close closes underlying store.
func (s *Service) Close() error { return s.store.Close() }

// RecentDocuments exposes recent stored docs for diagnostics/stat endpoints.
func (s *Service) RecentDocuments(sessionID string, limit int) ([]Document, error) {
	return s.store.RecentDocuments(sessionID, limit)
}

func (s *Service) DeleteSession(sessionID string) error {
	s.liveMu.Lock()
	delete(s.live, sessionID)
	delete(s.liveLastUsed, sessionID)
	s.liveMu.Unlock()
	return s.store.DeleteSession(sessionID)
}

// IngestParagraph summarizes the paragraph and stores summary embedding.
func (s *Service) IngestParagraph(ctx context.Context, sessionID, speaker, text string, start, end float64) error {
	_, err := s.IngestParagraphWithResult(ctx, sessionID, speaker, text, start, end)
	return err
}

// IngestParagraphWithResult summarizes a paragraph and reports whether an
// embedding was actually computed and persisted. The result lets HTTP callers
// avoid charging for filtered, disabled, or already persisted input while the
// legacy error-only method remains available to realtime ingestion.
func (s *Service) IngestParagraphWithResult(ctx context.Context, sessionID, speaker, text string, start, end float64) (IngestResult, error) {
	base := cleanParagraph(text)
	result := IngestResult{CanonicalText: base}
	if strings.TrimSpace(base) == "" {
		return result, nil
	}
	exists, err := s.store.HasDocument(sessionID, base, start, end)
	if err != nil {
		return result, err
	}
	if exists {
		result.Duplicate = true
		return result, nil
	}

	prev, err := s.store.GetSessionSummary(sessionID)
	if err != nil {
		return result, err
	}

	cfg := config.Get()
	paragraphSummary, skip, err := s.computeParagraphSummary(ctx, base)
	if err != nil {
		return result, err
	}
	if skip {
		return result, nil
	}
	result.EmbeddedText = paragraphSummary

	s.configMu.RLock()
	summaryOutputEnabled := s.summaryOutputEnabled
	embedEnabled := s.embedEnabled
	s.configMu.RUnlock()
	updatedSummary := prev
	if summaryOutputEnabled {
		bullet := strings.TrimSpace(paragraphSummary)
		if bullet != "" {
			bullet = "- " + bullet
		}
		updatedSummary = appendBullets(prev, bullet, cfg.Summary.MaxLines)
	}
	var vec []float32
	if embedEnabled {
		// Metering settles before any local summary/document state is written. A
		// billing or quota failure therefore cannot leave a partially successful
		// persisted ingest behind.
		vec, err = s.embedWithMeter(ctx, paragraphSummary, 0)
		if err != nil {
			return result, fmt.Errorf("embed: %w", err)
		}
	}

	var doc *Document
	if embedEnabled {
		doc = &Document{SessionID: sessionID, Speaker: speaker, StartTime: start, EndTime: end, Original: base, Summary: paragraphSummary, CreatedAt: time.Now().UTC()}
	}
	result.StorageUsage, err = EstimateIngestStorageUsage(doc, vec, prev, updatedSummary)
	if err != nil {
		return result, err
	}

	s.recordLiveParagraph(sessionID, speaker, base, paragraphSummary, start, end)
	if summaryOutputEnabled {
		if err := s.store.UpdateSessionSummary(sessionID, updatedSummary); err != nil {
			return result, err
		}
	}
	if !embedEnabled {
		return result, nil
	}

	_, err = s.store.InsertDocumentWithEmbedding(doc, vec)
	if err != nil {
		return result, err
	}
	result.Embedded = true
	return result, nil
}

// QueryTopK returns top K most similar documents and current session summary.
func (s *Service) QueryTopK(ctx context.Context, sessionID, query string, topK, _ int) ([]Document, string, error) {
	if topK <= 0 {
		topK = 5
	}
	s.configMu.RLock()
	embedEnabled := s.embedEnabled
	s.configMu.RUnlock()
	if !embedEnabled {
		sum, _ := s.store.GetSessionSummary(sessionID)
		return nil, sum, nil
	}
	// Search the complete session. Limiting candidates to the newest few
	// hundred paragraphs made old but highly relevant moments unreachable.
	docs, err := s.store.RecentDocuments(sessionID, 0)
	if err != nil {
		return nil, "", err
	}
	ids := make([]int64, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d.ID)
	}
	vecs, err := s.store.LoadEmbeddingsForDocs(ids)
	if err != nil {
		return nil, "", err
	}
	qvec, err := s.embedWithMeter(ctx, query, 0)
	if err != nil {
		return nil, "", err
	}
	type scored struct {
		d     *Document
		score float64
	}
	list := make([]scored, 0, len(docs)+8)
	qnorm := norm(qvec)
	now := time.Now()
	dedup := make(map[string]struct{}, len(docs)+8)
	for idx := range docs {
		d := &docs[idx]
		v, ok := vecs[d.ID]
		if !ok {
			continue
		}
		s := cosine(qvec, v, qnorm)
		s += recencyBoost(now.Sub(d.CreatedAt))
		key := documentKey(d)
		dedup[key] = struct{}{}
		list = append(list, scored{d: d, score: s})
	}
	liveDocs := s.recentLiveDocuments(sessionID, topK*2)
	for idx, ld := range liveDocs {
		key := documentKey(ld)
		if _, ok := dedup[key]; ok {
			continue
		}
		score := lexicalOverlap(query, ld.Original) + recencyBoost(now.Sub(ld.CreatedAt))
		if score == 0 {
			score = 0.01 + float64(idx)*0.0001
		}
		list = append(list, scored{d: ld, score: score})
		dedup[key] = struct{}{}
	}
	if len(list) == 0 {
		sum, _ := s.store.GetSessionSummary(sessionID)
		return nil, sum, nil
	}
	sort.Slice(list, func(i, j int) bool { return list[i].score > list[j].score })
	if len(list) > topK {
		list = list[:topK]
	}
	// Include direct neighbors so a matching atomic paragraph is presented in
	// its surrounding thought instead of as an isolated transcript fragment.
	selected := make(map[int64]Document, topK*3)
	for _, it := range list {
		selected[it.d.ID] = *it.d
		if it.d.ID <= 0 {
			continue
		}
		for index := range docs {
			if docs[index].ID != it.d.ID {
				continue
			}
			if index > 0 {
				selected[docs[index-1].ID] = docs[index-1]
			}
			if index+1 < len(docs) {
				selected[docs[index+1].ID] = docs[index+1]
			}
			break
		}
	}
	out := make([]Document, 0, len(selected))
	for _, document := range selected {
		out = append(out, document)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartTime < out[j].StartTime })
	sum, _ := s.store.GetSessionSummary(sessionID)
	return out, sum, nil
}

func (s *Service) recordLiveParagraph(sessionID, speaker, original, summary string, start, end float64) {
	original = strings.TrimSpace(original)
	summary = strings.TrimSpace(summary)
	if original == "" {
		return
	}
	if summary == "" {
		summary = original
	}
	if sessionID == "" {
		sessionID = "default"
	}
	entry := &liveEntry{
		Speaker:   speaker,
		Original:  original,
		Summary:   summary,
		StartTime: start,
		EndTime:   end,
		AddedAt:   time.Now().UTC(),
	}
	s.liveMu.Lock()
	s.pruneLiveSessionsLocked(entry.AddedAt, sessionID)
	buf := s.live[sessionID]
	if buf == nil {
		buf = &liveBuffer{}
		s.live[sessionID] = buf
	}
	buf.append(entry, s.liveMaxEntries, s.liveMaxAge)
	s.liveLastUsed[sessionID] = entry.AddedAt
	s.liveMu.Unlock()
}

func (s *Service) pruneLiveSessionsLocked(now time.Time, incomingSessionID string) {
	cutoff := now.Add(-s.liveMaxAge)
	for sessionID, lastUsed := range s.liveLastUsed {
		if lastUsed.Before(cutoff) {
			delete(s.liveLastUsed, sessionID)
			delete(s.live, sessionID)
		}
	}
	if _, exists := s.live[incomingSessionID]; exists {
		return
	}
	for len(s.live) >= s.liveMaxSessions {
		var oldestID string
		var oldest time.Time
		for sessionID, lastUsed := range s.liveLastUsed {
			if oldestID == "" || lastUsed.Before(oldest) {
				oldestID = sessionID
				oldest = lastUsed
			}
		}
		if oldestID == "" {
			break
		}
		delete(s.liveLastUsed, oldestID)
		delete(s.live, oldestID)
	}
}

// RecordLiveParagraph exposes low-latency live context updates for callers outside the service.
func (s *Service) RecordLiveParagraph(sessionID, speaker, original, summary string, start, end float64) {
	s.recordLiveParagraph(sessionID, speaker, original, summary, start, end)
}

func (s *Service) recentLiveDocuments(sessionID string, limit int) []*Document {
	if sessionID == "" {
		sessionID = "default"
	}
	s.liveMu.RLock()
	buf := s.live[sessionID]
	if buf == nil || len(buf.entries) == 0 {
		s.liveMu.RUnlock()
		return nil
	}
	snapshot := append([]*liveEntry(nil), buf.entries...)
	s.liveMu.RUnlock()
	if len(snapshot) == 0 {
		return nil
	}
	cutoff := time.Now().Add(-s.liveMaxAge)
	out := make([]*Document, 0, len(snapshot))
	for i := len(snapshot) - 1; i >= 0 && len(out) < limit; i-- {
		it := snapshot[i]
		if it.AddedAt.Before(cutoff) {
			continue
		}
		doc := &Document{
			ID:        -1,
			SessionID: sessionID,
			Speaker:   it.Speaker,
			StartTime: it.StartTime,
			EndTime:   it.EndTime,
			Original:  it.Original,
			Summary:   it.Summary,
			CreatedAt: it.AddedAt,
			Ephemeral: true,
		}
		out = append(out, doc)
	}
	return out
}

func documentKey(d *Document) string {
	base := strings.TrimSpace(d.Summary)
	if base == "" {
		base = strings.TrimSpace(d.Original)
	}
	return fmt.Sprintf("%.2f|%.2f|%s|%s", d.StartTime, d.EndTime, strings.ToLower(strings.TrimSpace(d.Speaker)), strings.ToLower(base))
}

func recencyBoost(age time.Duration) float64 {
	if age < 0 {
		age = 0
	}
	sec := age.Seconds()
	if sec <= 30 {
		return 0.35
	}
	if sec > 900 {
		return 0
	}
	return 0.35 * math.Exp(-sec/240)
}

func lexicalOverlap(query, text string) float64 {
	terms := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	if len(terms) == 0 {
		return 0
	}
	haystack := strings.ToLower(text)
	matches := 0
	seen := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		if len([]rune(term)) < 2 {
			continue
		}
		if _, exists := seen[term]; exists {
			continue
		}
		seen[term] = struct{}{}
		if strings.Contains(haystack, term) {
			matches++
		}
	}
	return float64(matches) / float64(len(terms))
}

func norm(v []float32) float64 {
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	return math.Sqrt(n)
}
func dot(a, b []float32) float64 {
	if len(a) != len(b) {
		n := imin(len(a), len(b))
		a, b = a[:n], b[:n]
	}
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}
func cosine(a, b []float32, anorm float64) float64 {
	if anorm == 0 {
		anorm = norm(a)
	}
	bn := norm(b)
	if anorm == 0 || bn == 0 {
		return 0
	}
	return dot(a, b) / (anorm * bn)
}

func (s *Service) computeParagraphSummary(ctx context.Context, base string) (summary string, skip bool, err error) {
	cfg := config.Get()
	summaryModel := os.Getenv("OPENAI_SUMMARY_MODEL")
	if m2 := cfg.Models.Summary; m2 != "" {
		summaryModel = m2
	}
	sumCfg, err := aiproviders.ConfigFor(summaryModel)
	if err != nil {
		return "", false, err
	}
	sumCfg.MaxOutputTokens = ragSummaryMaxOutputTokens
	modelName := sumCfg.Model

	s.configMu.RLock()
	ingestSummarizeEnabled := s.ingestSummarizeEnabled
	s.configMu.RUnlock()
	if !ingestSummarizeEnabled || len(base) < cfg.Summary.ParMinChars {
		if charCountAlphaNum(base) < 8 {
			return "", true, nil
		}
		metrics.RecordSummarizeNoUsage(modelName, 0)
		return base, false, nil
	}

	translator := openaiprovider.NewTranslator(sumCfg)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	const systemPrompt = "You are a precise context compressor. Summarize English conversation while REMOVING filler/disfluencies, repeated questions, small talk, jokes, and ads. Keep only key facts, decisions, numbers, and topics. Be concise and information-dense. Output in English."
	reservation, err := reserveProviderUsage(ctx, &ProviderUsage{
		Action:       "summarize",
		Model:        modelName,
		InputTokens:  conservativeProviderTokens(systemPrompt, base),
		OutputTokens: ragSummaryMaxOutputTokens,
	})
	if err != nil {
		return "", false, err
	}
	start := time.Now()
	out, usage, summaryErr := translator.SummarizeWithSystemPromptUsageRetry(cctx, "", base, systemPrompt, 3)
	duration := time.Since(start).Milliseconds()

	trimmed := strings.TrimSpace(out)
	if summaryErr != nil {
		if reservation != nil {
			if refundErr := reservation.Refund("RAG paragraph summary provider request failed"); refundErr != nil {
				return "", false, fmt.Errorf(
					"summarize provider request failed: %v; refund provider usage: %w",
					summaryErr,
					refundErr,
				)
			}
		}
		metrics.RecordSummarizeNoUsage(modelName, duration)
		return base, false, nil
	}

	actual := ProviderUsage{
		Action:       "summarize",
		Model:        modelName,
		InputTokens:  conservativeProviderTokens(systemPrompt, base),
		OutputTokens: ragSummaryMaxOutputTokens,
	}
	if usage != nil {
		actual.Model = usage.Model
		actual.InputTokens = usage.PromptTokens
		actual.CachedInputTokens = usage.CachedTokens
		actual.CacheWriteTokens = usage.CacheWriteTokens
		actual.OutputTokens = usage.CompletionTokens
		metrics.RecordSummarize(&metrics.Usage{PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens, CachedTokens: usage.CachedTokens, CacheWriteTokens: usage.CacheWriteTokens, Model: usage.Model}, duration)
	} else {
		metrics.RecordSummarizeNoUsage(modelName, duration)
	}
	if err := settleProviderUsage(ctx, reservation, &actual); err != nil {
		return "", false, err
	}
	if trimmed == "" {
		return base, false, nil
	}

	return trimmed, false, nil
}

// BuildAnswer uses retrieved docs and summary to compose an answer via LLM.
func (s *Service) BuildAnswer(ctx context.Context, sessionID, userQuery string, topK int) (string, error) {
	docs, summary, err := s.QueryTopK(ctx, sessionID, userQuery, topK, 300)
	if err != nil {
		return "", err
	}
	cfg, err := s.chatConfig()
	if err != nil {
		return "", err
	}
	tr := openaiprovider.NewTranslator(cfg)
	// Build prompt
	var ctxParts string
	if summary != "" {
		ctxParts += "[Session Summary]\n" + summary + "\n\n"
	}
	if len(docs) > 0 {
		ctxParts += "[Top Contexts]\n"
		for i, d := range docs {
			label := fmt.Sprintf("(%d)", i+1)
			if d.Ephemeral {
				label = fmt.Sprintf("(%d) [LIVE]", i+1)
			}
			summaryText := strings.TrimSpace(d.Summary)
			if summaryText == "" {
				summaryText = strings.TrimSpace(d.Original)
			}
			ctxParts += fmt.Sprintf("%s Speaker %s [%.1f-%.1f]: %s\n", label, safe(d.Speaker), d.StartTime, d.EndTime, summaryText)
		}
	}
	sys := strings.Join([]string{
		"You are a helpful learning assistant.",
		"Answer in Chinese, structured and easy to skim.",
		"If context is insufficient, say you are unsure.",
		"Format rules:",
		"- Use short paragraphs and bullet points.",
		"- Start bullets with '- ' and put each on a new line.",
		"- Preserve line breaks for readability.",
	}, " ")
	user := ctxParts + "\n[Question]\n" + userQuery + "\n[Format]\n- 简短概括\n- 关键要点（每点一行）\n- 必要时给出下一步建议"
	msgs := []map[string]string{{"role": "system", "content": sys}, {"role": "user", "content": user}}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := tr.Chat(cctx, msgs)
	if err != nil {
		return "", wrapProviderRequest("chat", err)
	}
	return out, nil
}

func safe(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func imin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// StoreSummary returns the current summary for a session.
func (s *Service) StoreSummary(sessionID string) (string, error) {
	return s.store.GetSessionSummary(sessionID)
}

// StoreGetTitle returns cached session title (if any).
func (s *Service) StoreGetTitle(sessionID string) (string, error) {
	return s.store.GetSessionTitle(sessionID)
}

// StoreSetTitle caches the session title (best-effort).
func (s *Service) StoreSetTitle(sessionID, title string) error {
	if strings.TrimSpace(title) == "" {
		return nil
	}
	return s.store.UpdateSessionTitle(sessionID, title)
}

// appendBullets merges new bullet lines into previous bullet list, deduplicates, and trims to maxLines.
func appendBullets(prev, added string, maxLines int) string {
	toLines := func(s string) []string {
		var out []string
		for _, ln := range strings.Split(s, "\n") {
			L := strings.TrimSpace(ln)
			if L == "" {
				continue
			}
			if !strings.HasPrefix(L, "- ") {
				L = "- " + L
			}
			out = append(out, L)
		}
		return out
	}
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(s, "- "))) }
	prevLines := toLines(prev)
	seen := make(map[string]struct{}, len(prevLines))
	for _, l := range prevLines {
		seen[norm(l)] = struct{}{}
	}
	addLines := toLines(added)
	for _, l := range addLines {
		k := norm(l)
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		prevLines = append(prevLines, l)
		seen[k] = struct{}{}
	}
	if maxLines > 0 && len(prevLines) > maxLines {
		prevLines = prevLines[len(prevLines)-maxLines:]
	}
	return strings.Join(prevLines, "\n")
}

// ------ helpers: clean noisy paragraphs ------
func charCountAlphaNum(s string) int {
	c := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			c++
		}
	}
	return c
}

func cleanParagraph(s string) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return ""
	}
	// Preserve the speaker's casing, punctuation and language. The old cleaner
	// lower-cased everything, rewrote questions as statements and discarded
	// short non-Latin sentences, which visibly corrupted quoted source text.
	return strings.Join(strings.Fields(t), " ")
}

// SetChatConfigProvider allows overriding the config provider (e.g., to enforce a per-session model from WS).
func (s *Service) SetChatConfigProvider(fn func() (*openaiprovider.Config, error)) {
	if fn != nil {
		s.configMu.Lock()
		defer s.configMu.Unlock()
		s.chatCfgFn = fn
	}
}

// SetIngestSummarizeEnabled toggles whether IngestParagraph calls the LLM to summarize.
// When disabled, cleaned text is used directly without LLM calls.
func (s *Service) SetIngestSummarizeEnabled(enabled bool) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.ingestSummarizeEnabled = enabled
}

// SetSummaryOutputEnabled toggles whether to update session_summary at all.
func (s *Service) SetSummaryOutputEnabled(enabled bool) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.summaryOutputEnabled = enabled
}

// SetEmbedEnabled toggles embeddings compute/store and retrieval.
func (s *Service) SetEmbedEnabled(enabled bool) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.embedEnabled = enabled
}

func (s *Service) chatConfig() (*openaiprovider.Config, error) {
	s.configMu.RLock()
	provider := s.chatCfgFn
	s.configMu.RUnlock()
	if provider == nil {
		return nil, fmt.Errorf("chat configuration provider is unavailable")
	}
	return provider()
}

func applyChatOverrides(base *openaiprovider.Config, overrides *ChatOverrides) (*openaiprovider.Config, error) {
	if base == nil {
		return nil, fmt.Errorf("chat configuration is unavailable")
	}
	// Never mutate a provider-owned config pointer across requests.
	configCopy := *base
	if overrides == nil {
		return &configCopy, nil
	}
	if overrides.APIBase != "" && overrides.APIKey == "" {
		return nil, fmt.Errorf("request-scoped API base requires a request-scoped API key")
	}
	if overrides.APIKey != "" {
		configCopy.APIKey = overrides.APIKey
	}
	if overrides.APIBase != "" {
		configCopy.BaseURL = overrides.APIBase
		configCopy.UseResponsesAPI = openaiprovider.IsOfficialOpenAIBase(overrides.APIBase)
		configCopy.EnablePromptCache = configCopy.UseResponsesAPI
		configCopy.Provider = ""
		configCopy.FallbackModels = []string{}
	}
	if overrides.Model != "" {
		if overrides.APIBase != "" {
			// A caller-supplied endpoint hosts bare model ids; the provider
			// prefix only routes within our own registry.
			_, configCopy.Model = aiproviders.Split(overrides.Model)
		} else {
			provided, err := aiproviders.ConfigFor(overrides.Model)
			if err != nil {
				return nil, err
			}
			configCopy.Provider, configCopy.BaseURL, configCopy.Model = provided.Provider, provided.BaseURL, provided.Model
			configCopy.UseResponsesAPI, configCopy.EnablePromptCache, configCopy.FallbackModels = provided.UseResponsesAPI, provided.EnablePromptCache, provided.FallbackModels
			if overrides.APIKey == "" {
				configCopy.APIKey = provided.APIKey
			}
		}
	}
	return &configCopy, nil
}

// NormalizeReasoningEffort keeps the user-facing reasoning control deliberately
// small. The provider supports more expert-only levels, but exposing them here
// would make cost and latency much harder to predict for ordinary assistant use.
func NormalizeReasoningEffort(value string) (string, bool) {
	switch normalized := strings.ToLower(strings.TrimSpace(value)); normalized {
	case "", "low", "medium", "high":
		return normalized, true
	default:
		return "", false
	}
}

func outputBudgetForReasoning(fallback int, effort string) int {
	switch effort {
	case "low":
		if fallback < artifactMaxOutputTokens {
			return artifactMaxOutputTokens
		}
	case "medium":
		return reasoningMediumOutputTokens
	case "high":
		return reasoningHighOutputTokens
	}
	return fallback
}

// GenerationTimeoutForReasoning keeps the request deadline aligned with the
// extra work requested by the user without extending fast/default requests.
func GenerationTimeoutForReasoning(fallback time.Duration, effort string) time.Duration {
	minimum := time.Duration(0)
	switch effort {
	case "medium":
		minimum = reasoningMediumTimeout
	case "high":
		minimum = reasoningHighTimeout
	}
	if fallback < minimum {
		return minimum
	}
	return fallback
}

// BuildAnswerWithUsage returns answer and usage/latency using current env config.
func (s *Service) BuildAnswerWithUsage(ctx context.Context, sessionID, userQuery string, topK int) (string, *openaiprovider.Usage, time.Duration, error) {
	docs, summary, err := s.QueryTopK(ctx, sessionID, userQuery, topK, 300)
	if err != nil {
		return "", nil, 0, err
	}
	cfg, err := s.chatConfig()
	if err != nil {
		return "", nil, 0, err
	}
	tr := openaiprovider.NewTranslator(cfg)
	var ctxParts string
	if summary != "" {
		ctxParts += "[Session Summary]\n" + summary + "\n\n"
	}
	if len(docs) > 0 {
		ctxParts += "[Top Contexts]\n"
		for i, d := range docs {
			label := fmt.Sprintf("(%d)", i+1)
			if d.Ephemeral {
				label = fmt.Sprintf("(%d) [LIVE]", i+1)
			}
			summaryText := strings.TrimSpace(d.Summary)
			if summaryText == "" {
				summaryText = strings.TrimSpace(d.Original)
			}
			ctxParts += fmt.Sprintf("%s Speaker %s [%.1f-%.1f]: %s\n", label, safe(d.Speaker), d.StartTime, d.EndTime, summaryText)
		}
	}
	sys := strings.Join([]string{
		"You are a helpful learning assistant.",
		"Answer in Chinese, structured and easy to skim.",
		"If context is insufficient, say you are unsure.",
		"Format rules:",
		"- Use short paragraphs and bullet points.",
		"- Start bullets with '- ' and put each on a new line.",
		"- Preserve line breaks for readability.",
	}, " ")
	user := ctxParts + "\n[Question]\n" + userQuery + "\n[Format]\n- 简短概括\n- 关键要点（每点一行）\n- 必要时给出下一步建议"
	msgs := []map[string]string{{"role": "system", "content": sys}, {"role": "user", "content": user}}
	start := time.Now()
	out, usage, err := tr.ChatWithUsageRetry(ctx, msgs, 3)
	dur := time.Since(start)
	if err != nil {
		return "", nil, dur, wrapProviderRequest("chat", err)
	}
	return out, usage, dur, nil
}

// BuildAnswerWithConfigUsage returns answer and usage/latency using overrides.
func (s *Service) BuildAnswerWithConfigUsage(ctx context.Context, sessionID, userQuery string, topK int, ov *ChatOverrides) (string, *openaiprovider.Usage, time.Duration, error) {
	docs, summary, err := s.QueryTopK(ctx, sessionID, userQuery, topK, 300)
	if err != nil {
		return "", nil, 0, err
	}
	baseCfg, err := s.chatConfig()
	if err != nil {
		return "", nil, 0, err
	}
	baseCfg, err = applyChatOverrides(baseCfg, ov)
	if err != nil {
		return "", nil, 0, err
	}
	baseCfg.MaxOutputTokens = ragAnswerMaxOutputTokens
	tr := openaiprovider.NewTranslator(baseCfg)
	var ctxParts string
	if summary != "" {
		ctxParts += "[Session Summary]\n" + summary + "\n\n"
	}
	if len(docs) > 0 {
		ctxParts += "[Top Contexts]\n"
		for i, d := range docs {
			ctxParts += fmt.Sprintf("(%d) Speaker %s [%.1f-%.1f]: %s\n", i+1, safe(d.Speaker), d.StartTime, d.EndTime, d.Summary)
		}
	}
	sys := strings.Join([]string{
		"You are a helpful learning assistant.",
		"Answer in Chinese, structured and easy to skim.",
		"If context is insufficient, say you are unsure.",
		"Format rules:",
		"- Use short paragraphs and bullet points.",
		"- Start bullets with '- ' and put each on a new line.",
		"- Preserve line breaks for readability.",
	}, " ")
	if ov != nil && ov.Prompt != "" {
		sys = sys + " Additional guidance: " + ov.Prompt
	}
	user := ctxParts + "\n[Question]\n" + userQuery + "\n[Format]\n- 简短概括\n- 关键要点（每点一行）\n- 必要时给出下一步建议"
	msgs := []map[string]string{{"role": "system", "content": sys}, {"role": "user", "content": user}}
	start := time.Now()
	out, usage, err := tr.ChatWithUsageRetry(ctx, msgs, 3)
	dur := time.Since(start)
	if err != nil {
		return "", nil, dur, wrapProviderRequest("chat", err)
	}
	return out, usage, dur, nil
}

// BuildAnswerWithHistoryWithConfigUsage is like BuildAnswerWithConfigUsage but includes recent chat history
// to improve coreference resolution and continuity.
func (s *Service) BuildAnswerWithHistoryWithConfigUsage(ctx context.Context, sessionID, userQuery string, topK int, ov *ChatOverrides, history string) (string, *openaiprovider.Usage, time.Duration, error) {
	docs, summary, err := s.QueryTopK(ctx, sessionID, userQuery, topK, 300)
	if err != nil {
		return "", nil, 0, err
	}
	baseCfg, err := s.chatConfig()
	if err != nil {
		return "", nil, 0, err
	}
	baseCfg, err = applyChatOverrides(baseCfg, ov)
	if err != nil {
		return "", nil, 0, err
	}
	baseCfg.MaxOutputTokens = ragAnswerMaxOutputTokens
	tr := openaiprovider.NewTranslator(baseCfg)
	var ctxParts string
	if summary != "" {
		ctxParts += "[Session Summary]\n" + summary + "\n\n"
	}
	if len(docs) > 0 {
		ctxParts += "[Top Contexts]\n"
		for i, d := range docs {
			label := fmt.Sprintf("(%d)", i+1)
			if d.Ephemeral {
				label = fmt.Sprintf("(%d) [LIVE]", i+1)
			}
			summaryText := strings.TrimSpace(d.Summary)
			if summaryText == "" {
				summaryText = strings.TrimSpace(d.Original)
			}
			ctxParts += fmt.Sprintf("%s Speaker %s [%.1f-%.1f]: %s\n", label, safe(d.Speaker), d.StartTime, d.EndTime, summaryText)
		}
		ctxParts += "\n"
	}
	if strings.TrimSpace(history) != "" {
		ctxParts += "[Chat History]\n" + strings.TrimSpace(history) + "\n\n"
	}
	sys := strings.Join([]string{
		"You are a helpful learning assistant.",
		"Answer in Chinese, concise and easy to skim.",
		"Use 'Chat History' to resolve pronouns and follow-ups.",
		"If the referent is still ambiguous, ask a brief clarifying question before answering.",
		"Prefer bullet points; preserve important names and entities.",
	}, " ")
	user := ctxParts + "[Question]\n" + userQuery + "\n[Format]\n- 简短概括\n- 关键要点（每点一行）\n- 必要时先澄清再回答"
	msgs := []map[string]string{{"role": "system", "content": sys}, {"role": "user", "content": user}}
	reservation, err := reserveProviderUsage(ctx, &ProviderUsage{
		Action:         "chat",
		Model:          baseCfg.Model,
		InputTokens:    conservativeProviderTokens(sys, user),
		OutputTokens:   ragAnswerMaxOutputTokens,
		CustomerFunded: ov != nil && strings.TrimSpace(ov.APIKey) != "",
	})
	if err != nil {
		return "", nil, 0, err
	}
	start := time.Now()
	out, usage, err := tr.ChatWithUsageRetry(ctx, msgs, 3)
	dur := time.Since(start)
	if err != nil {
		return "", nil, dur, refundProviderUsage(
			reservation,
			"RAG answer provider request failed",
			wrapProviderRequest("chat", err),
		)
	}
	actual := ProviderUsage{
		Action:         "chat",
		Model:          baseCfg.Model,
		InputTokens:    conservativeProviderTokens(sys, user),
		OutputTokens:   ragAnswerMaxOutputTokens,
		CustomerFunded: ov != nil && strings.TrimSpace(ov.APIKey) != "",
	}
	if usage != nil {
		actual.Model = usage.Model
		actual.InputTokens = usage.PromptTokens
		actual.CachedInputTokens = usage.CachedTokens
		actual.CacheWriteTokens = usage.CacheWriteTokens
		actual.OutputTokens = usage.CompletionTokens
	}
	if err := settleProviderUsage(ctx, reservation, &actual); err != nil {
		return "", nil, dur, err
	}
	return out, usage, dur, nil
}

// BuildAnswerFromContextWithConfigUsage answers against context assembled by
// the HTTP context resolver. It keeps the transcript/question/history fields
// separate and makes the stable transcript prefix eligible for provider cache.
func (s *Service) BuildAnswerFromContextWithConfigUsage(
	ctx context.Context,
	cacheKey, userQuery, contextText, history string,
	ov *ChatOverrides,
) (string, *openaiprovider.Usage, time.Duration, error) {
	return s.buildAnswerFromContextWithConfigUsage(
		ctx,
		cacheKey,
		userQuery,
		contextText,
		history,
		ov,
		ragAnswerMaxOutputTokens,
		"",
	)
}

// BuildArtifactFromContextWithConfigUsage generates a persisted summary,
// notes document, or action list. GPT-5.6's reasoning tokens share the output
// budget with visible text, so artifacts default to a larger low-reasoning
// budget while still honoring an explicit user reasoning choice.
func (s *Service) BuildArtifactFromContextWithConfigUsage(
	ctx context.Context,
	cacheKey, userQuery, contextText, history string,
	ov *ChatOverrides,
) (string, *openaiprovider.Usage, time.Duration, error) {
	return s.buildAnswerFromContextWithConfigUsage(
		ctx,
		cacheKey,
		userQuery,
		contextText,
		history,
		ov,
		artifactMaxOutputTokens,
		"low",
	)
}

func (s *Service) buildAnswerFromContextWithConfigUsage(
	ctx context.Context,
	cacheKey, userQuery, contextText, history string,
	ov *ChatOverrides,
	maxOutputTokens int,
	reasoningEffort string,
) (string, *openaiprovider.Usage, time.Duration, error) {
	baseCfg, err := s.chatConfig()
	if err != nil {
		return "", nil, 0, err
	}
	baseCfg, err = applyChatOverrides(baseCfg, ov)
	if err != nil {
		return "", nil, 0, err
	}
	effectiveReasoning := reasoningEffort
	if ov != nil && strings.TrimSpace(ov.ReasoningEffort) != "" {
		effectiveReasoning = ov.ReasoningEffort
	}
	effectiveReasoning, validReasoning := NormalizeReasoningEffort(effectiveReasoning)
	if !validReasoning {
		return "", nil, 0, fmt.Errorf("unsupported reasoning effort")
	}
	baseCfg.MaxOutputTokens = maxOutputTokens
	if strings.HasPrefix(strings.ToLower(baseCfg.Model), "gpt-5.6") {
		baseCfg.ReasoningEffort = effectiveReasoning
		baseCfg.MaxOutputTokens = outputBudgetForReasoning(
			maxOutputTokens,
			effectiveReasoning,
		)
		baseCfg.Timeout = GenerationTimeoutForReasoning(
			baseCfg.Timeout,
			effectiveReasoning,
		)
	}
	systemPrompt := config.Get().Prompts.Chat
	if strings.TrimSpace(systemPrompt) == "" {
		systemPrompt = "You are a helpful assistant. Answer from the supplied context and say when the context is insufficient."
	}
	if ov != nil && strings.TrimSpace(ov.Prompt) != "" {
		systemPrompt += "\n\nAdditional guidance:\n" + strings.TrimSpace(ov.Prompt)
	}
	reservation, err := reserveProviderUsage(ctx, &ProviderUsage{
		Action:       "chat",
		Model:        baseCfg.Model,
		InputTokens:  conservativeProviderTokens(systemPrompt, contextText, history, userQuery),
		OutputTokens: baseCfg.MaxOutputTokens,
		OperationID: exactProviderOperationID(
			ctx,
			"chat",
			baseCfg.Model,
			baseCfg.BaseURL,
			systemPrompt,
			contextText,
			history,
			userQuery,
		),
		CustomerFunded: ov != nil && strings.TrimSpace(ov.APIKey) != "",
	})
	if err != nil {
		return "", nil, 0, err
	}
	translator := openaiprovider.NewTranslator(baseCfg)
	start := time.Now()
	out, usage, err := translator.RespondWithUsageRetry(
		ctx,
		systemPrompt,
		contextText,
		history,
		userQuery,
		cacheKey,
		3,
	)
	duration := time.Since(start)
	actual := ProviderUsage{
		Action:         "chat",
		Model:          baseCfg.Model,
		InputTokens:    conservativeProviderTokens(systemPrompt, contextText, history, userQuery),
		OutputTokens:   baseCfg.MaxOutputTokens,
		CustomerFunded: ov != nil && strings.TrimSpace(ov.APIKey) != "",
	}
	if usage != nil {
		actual.Model = usage.Model
		actual.InputTokens = usage.PromptTokens
		actual.CachedInputTokens = usage.CachedTokens
		actual.CacheWriteTokens = usage.CacheWriteTokens
		actual.OutputTokens = usage.CompletionTokens
	}
	if err != nil {
		if usage == nil {
			return "", nil, duration, refundProviderUsage(
				reservation,
				"AI answer provider request failed",
				wrapProviderRequest("chat", err),
			)
		}
		if settleErr := settleProviderUsage(ctx, reservation, &actual); settleErr != nil {
			return "", usage, duration, fmt.Errorf("%v; %w", err, settleErr)
		}
		return "", usage, duration, wrapProviderRequest("chat", err)
	}
	if err := settleProviderUsage(ctx, reservation, &actual); err != nil {
		return "", nil, duration, err
	}
	return out, usage, duration, nil
}

// BuildAnswerWithConfig is like BuildAnswer but allows overriding API settings and prompt per request.
func (s *Service) BuildAnswerWithConfig(ctx context.Context, sessionID, userQuery string, topK int, ov *ChatOverrides) (string, error) {
	docs, summary, err := s.QueryTopK(ctx, sessionID, userQuery, topK, 300)
	if err != nil {
		return "", err
	}

	baseCfg, err := s.chatConfig()
	if err != nil {
		return "", err
	}
	baseCfg, err = applyChatOverrides(baseCfg, ov)
	if err != nil {
		return "", err
	}
	tr := openaiprovider.NewTranslator(baseCfg)

	var ctxParts string
	if summary != "" {
		ctxParts += "[Session Summary]\n" + summary + "\n\n"
	}
	if len(docs) > 0 {
		ctxParts += "[Top Contexts]\n"
		for i, d := range docs {
			label := fmt.Sprintf("(%d)", i+1)
			if d.Ephemeral {
				label = fmt.Sprintf("(%d) [LIVE]", i+1)
			}
			summaryText := strings.TrimSpace(d.Summary)
			if summaryText == "" {
				summaryText = strings.TrimSpace(d.Original)
			}
			ctxParts += fmt.Sprintf("%s Speaker %s [%.1f-%.1f]: %s\n", label, safe(d.Speaker), d.StartTime, d.EndTime, summaryText)
		}
	}
	sys := strings.Join([]string{
		"You are a helpful learning assistant.",
		"Answer in Chinese, structured and easy to skim.",
		"If context is insufficient, say you are unsure.",
		"Format rules:",
		"- Use short paragraphs and bullet points.",
		"- Start bullets with '- ' and put each on a new line.",
		"- Preserve line breaks for readability.",
	}, " ")
	if ov != nil && ov.Prompt != "" {
		sys = sys + " Additional guidance: " + ov.Prompt
	}
	user := ctxParts + "\n[Question]\n" + userQuery + "\n[Format]\n- 简短概括\n- 关键要点（每点一行）\n- 必要时给出下一步建议"
	msgs := []map[string]string{{"role": "system", "content": sys}, {"role": "user", "content": user}}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := tr.Chat(cctx, msgs)
	if err != nil {
		return "", wrapProviderRequest("chat", err)
	}
	return out, nil
}
