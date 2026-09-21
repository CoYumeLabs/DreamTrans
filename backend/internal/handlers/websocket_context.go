package handlers

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	openai "github.com/dreamtrans/backend/internal/adapters/openai_provider"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/metrics"
	"github.com/dreamtrans/backend/internal/rag"
)

func (st *connState) addSegmentEN(seg string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.recentSegments = append(st.recentSegments, seg)
	if len(st.recentSegments) > maxRecentContextSegments {
		st.recentSegments = append(
			[]string(nil),
			st.recentSegments[len(st.recentSegments)-maxRecentContextSegments:]...,
		)
	}
	// Update rolling buffer
	st.recentBuffer += "\n" + seg
	if utf8.RuneCountInString(st.recentBuffer) > st.rollingWindowChars {
		st.recentBuffer = tailRunes(st.recentBuffer, st.rollingWindowChars)
	}
}

func (st *connState) contextForCompressedLocked() string {
	// Build context from summary + last K segments
	var builder strings.Builder
	if st.summary != "" {
		builder.WriteString("Summary:\n")
		builder.WriteString(st.summary)
		builder.WriteString("\n---\n")
	}
	builder.WriteString("Recent:\n")
	start := 0
	if len(st.recentSegments) > st.keepLastSegments {
		start = len(st.recentSegments) - st.keepLastSegments
	}
	for i := start; i < len(st.recentSegments); i++ {
		builder.WriteString("- ")
		builder.WriteString(st.recentSegments[i])
		builder.WriteString("\n")
	}
	if len(st.recentTranslated) > 0 {
		builder.WriteString("---\nRecentTranslated:\n")
		tzStart := 0
		if len(st.recentTranslated) > st.keepLastTranslated {
			tzStart = len(st.recentTranslated) - st.keepLastTranslated
		}
		for i := tzStart; i < len(st.recentTranslated); i++ {
			builder.WriteString("- ")
			builder.WriteString(st.recentTranslated[i])
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

func (st *connState) translationContext() (active bool, contextText string) {
	st.mu.Lock()
	defer st.mu.Unlock()

	switch st.mode {
	case modeAIRolling:
		if st.experimentalSmart {
			return true, st.contextForCompressedLocked()
		}
		return true, st.recentBuffer
	case modeAICompressed:
		return true, st.contextForCompressedLocked()
	default:
		return false, ""
	}
}

type ragRuntimeState struct {
	service              *rag.Service
	sessionID            string
	summarizationEnabled bool
	shouldUpdateSummary  bool
}

func (st *connState) ragRuntime(tenantID, userID string) ragRuntimeState {
	st.mu.Lock()
	defer st.mu.Unlock()

	return ragRuntimeState{
		service:              st.ragSvc,
		sessionID:            namespacedRAGSessionID(tenantID, userID, st.sessionID),
		summarizationEnabled: st.summarizationEnabled,
		shouldUpdateSummary: st.summarizationEnabled &&
			(st.mode == modeAICompressed || (st.mode == modeAIRolling && st.experimentalSmart)),
	}
}

// namespacedRAGSessionID prevents two users choosing the same client-side
// session id from sharing retrieval context.
func namespacedRAGSessionID(tenantID, userID, sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		sessionID = "default"
	}
	if strings.TrimSpace(userID) == "" {
		return "anonymous/session/" + sessionID
	}
	if strings.TrimSpace(tenantID) == "" {
		tenantID = "default"
	}
	return "tenant/" + tenantID + "/user/" + userID + "/session/" + sessionID
}

// updateSummaryIncremental merges the previous summary with a small new paragraph chunk.
// Append new paragraph into backlog and maybe update summary according to rate limits.
//
//nolint:gocyclo // Summary, metrics, billing, and backlog recovery form one transaction-like flow.
func (st *connState) updateSummaryIncremental(
	ctx context.Context,
	para string,
	billingSvc websocketBillingService,
	userID, tenantID string,
	failClosePaidFlow func(error),
) error {
	// Multiple RAG flushes can arrive close together. Serialize summary updates
	// so they all build on the most recently committed summary.
	st.summaryMu.Lock()
	defer st.summaryMu.Unlock()

	st.mu.Lock()
	if !st.summarizationEnabled {
		st.mu.Unlock()
		return nil
	}
	// append into backlog
	if para != "" {
		if st.summaryBacklog.Len() > 0 {
			st.summaryBacklog.WriteString("\n")
		}
		st.summaryBacklog.WriteString(para)
		// cap backlog size
		s := st.summaryBacklog.String()
		if utf8.RuneCountInString(s) > st.summaryMaxBacklogChars {
			s = tailRunes(s, st.summaryMaxBacklogChars)
			st.summaryBacklog.Reset()
			st.summaryBacklog.WriteString(s)
		}
	}
	// check rate limit
	dueByTime := time.Since(st.lastSummaryAt) >= time.Duration(st.summaryMinIntervalSec*float64(time.Second))
	dueBySize := utf8.RuneCountInString(st.summaryBacklog.String()) >= st.summaryMinChars
	backlog := st.summaryBacklog.String()
	prev := st.summary
	if !dueByTime && !dueBySize {
		st.mu.Unlock()
		return nil
	}
	// we will flush backlog now
	st.summaryBacklog.Reset()
	st.mu.Unlock()

	translator, summaryPrompt, summaryModel, err := st.summaryRuntime()
	if err != nil {
		log.Printf("summarize init error: %v", err)
		st.restoreSummaryBacklog(backlog)
		return fmt.Errorf("summary initialization failed: %w", err)
	}
	effectivePrompt := summaryPrompt
	if strings.TrimSpace(effectivePrompt) == "" {
		effectivePrompt = defaultSummaryPrompt
	}

	st.mu.Lock()
	sessionID := billingSessionReference(st.sessionID)
	st.mu.Unlock()
	var reservation *realtimeUsageReservation
	if billingSvc != nil && userID != "" {
		reservation, err = reserveRealtimeUsage(ctx, billingSvc, "ws-summary:", &billing.UsageRecord{
			UserID: userID, TenantID: tenantID, SessionID: sessionID,
			Action: "summarize", Model: summaryModel,
			InputTokens:  realtimeInputReservationTokens(effectivePrompt, prev, backlog),
			OutputTokens: realtimeOutputReservationTokens(backlog),
		})
		if err != nil {
			st.restoreSummaryBacklog(backlog)
			return wrapWebSocketAccountingError(
				classifyBillingAccountingFailure(err),
				fmt.Errorf("summary usage reservation failed: %w", err),
			)
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	var (
		out     string
		u       *openai.Usage
		callErr error
	)
	out, u, callErr = translator.SummarizeWithSystemPromptUsageRetry(
		cctx,
		prev,
		backlog,
		effectivePrompt,
		3,
	)
	if callErr != nil {
		log.Printf("incremental summarize error: %v", callErr)
		if refundErr := reservation.refund("WebSocket summary request failed"); refundErr != nil {
			failClosePaidFlow(refundErr)
			callErr = fmt.Errorf("%w; usage refund failed: %v", callErr, refundErr)
		}
		st.restoreSummaryBacklog(backlog)
		return fmt.Errorf("summary request failed: %w", callErr)
	}
	dur := time.Since(start).Milliseconds()
	if u != nil {
		metrics.RecordSummarize(&metrics.Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens, CachedTokens: u.CachedTokens, CacheWriteTokens: u.CacheWriteTokens, Model: u.Model}, dur)
		if os.Getenv("OPENAI_DEBUG") == "1" {
			log.Printf("metrics.summarize model=%s tokens p=%d c=%d t=%d latency=%dms", u.Model, u.PromptTokens, u.CompletionTokens, u.TotalTokens, dur)
		}
	} else {
		metrics.RecordSummarizeNoUsage(summaryModel, dur)
		if os.Getenv("OPENAI_DEBUG") == "1" {
			log.Printf("metrics.summarize usage missing; model=%s latency=%dms", summaryModel, dur)
		}
	}
	if billingSvc != nil && userID != "" {
		inputTokens := max(1, utf8.RuneCountInString(effectivePrompt+prev+backlog)/4)
		outputTokens := max(1, utf8.RuneCountInString(out)/4)
		cachedInputTokens := 0
		cacheWriteTokens := 0
		model := summaryModel
		if u != nil {
			inputTokens = u.PromptTokens
			cachedInputTokens = u.CachedTokens
			cacheWriteTokens = u.CacheWriteTokens
			outputTokens = u.CompletionTokens
			model = u.Model
		}
		if _, billingErr := reservation.settle(&billing.UsageRecord{
			UserID: userID, TenantID: tenantID, SessionID: sessionID,
			Action: "summarize", Model: model,
			InputTokens: inputTokens, CachedInputTokens: cachedInputTokens,
			CacheWriteTokens: cacheWriteTokens, OutputTokens: outputTokens,
		}); billingErr != nil {
			log.Printf("summary usage settlement failed: %v", billingErr)
			st.restoreSummaryBacklog(backlog)
			failClosePaidFlow(billingErr)
			return fmt.Errorf("summary usage settlement failed: %w", billingErr)
		}
	}
	st.mu.Lock()
	st.summary = out
	st.lastSummaryAt = time.Now()
	st.mu.Unlock()
	return nil
}

func (st *connState) restoreSummaryBacklog(backlog string) {
	if strings.TrimSpace(backlog) == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	current := st.summaryBacklog.String()
	st.summaryBacklog.Reset()
	st.summaryBacklog.WriteString(backlog)
	if current != "" {
		st.summaryBacklog.WriteString("\n")
		st.summaryBacklog.WriteString(current)
	}
	value := st.summaryBacklog.String()
	if utf8.RuneCountInString(value) > st.summaryMaxBacklogChars {
		value = tailRunes(value, st.summaryMaxBacklogChars)
		st.summaryBacklog.Reset()
		st.summaryBacklog.WriteString(value)
	}
}

func tailRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[len(runes)-limit:])
}

// --------- Noise filtering for incremental summary & RAG ingestion ---------
// filterLowInfoText removes filler/disfluency and very short/repetitive fragments to avoid
// polluting incremental summaries and RAG store. Keeps only lines with minimal signal.
func filterLowInfoText(s string) string {
	// quick path
	t := strings.TrimSpace(s)
	if t == "" {
		return ""
	}
	lower := strings.ToLower(t)
	// remove common disfluencies
	repl := []string{" ah ", " uh ", " um ", " hmm ", " okay ", " ok ", " huh ", " ah.", " uh.", " um.", " okay.", " ok.", " hmm.", " huh."}
	for _, r := range repl {
		lower = strings.ReplaceAll(lower, r, " ")
	}
	// normalize punctuation into sentence breaks
	norm := strings.NewReplacer("?", ".", "!", ".", "。", ".", "？", ".", "！", ".", "\n", ". ")
	lower = norm.Replace(lower)
	// split into candidate sentences by period
	parts := strings.Split(lower, ".")
	seen := make(map[string]struct{})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		L := strings.TrimSpace(p)
		if L == "" {
			continue
		}
		// collapse spaces
		L = strings.Join(strings.Fields(L), " ")
		// skip very short lines without numbers
		if textWeight(L) < 12 && !strings.ContainsAny(L, "0123456789$") {
			continue
		}
		// skip if dominated by repeats of 'how much' etc.
		if strings.Count(L, "how much") >= 2 || strings.Count(L, "how many") >= 2 {
			L = "price inquiry"
		}
		key := L
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, L)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "; ")
}
