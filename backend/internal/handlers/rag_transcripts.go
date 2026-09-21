package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	aicontext "github.com/dreamtrans/backend/internal/ai"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/models"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/google/uuid"
)

type loadedAIContextSegments struct {
	Segments        []aicontext.TranscriptSegment
	StoredTruncated bool
}

type aiContextTranscriptReader interface {
	GetTranscriptsPageBySession(
		context.Context,
		string,
		int,
		*store.TranscriptPageCursor,
	) ([]models.Transcript, bool, error)
	GetTranscriptsPageBySessionDescending(
		context.Context,
		string,
		int,
		*store.TranscriptPageCursor,
	) ([]models.Transcript, bool, error)
	GetLatestCompleteTranscriptEnd(
		context.Context,
		string,
	) (float64, bool, error)
}

func (h *RAGHandler) loadContextSegments(
	r *http.Request,
	sessionID string,
	client []aicontext.TranscriptSegment,
	policy aicontext.ContextPolicy,
) (loadedAIContextSegments, int, error) {
	claims := auth.GetUserClaims(r.Context())
	if claims == nil || h.store == nil {
		return loadedAIContextSegments{
			Segments: coalesceAIContextSegments(client),
		}, http.StatusOK, nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return loadedAIContextSegments{
			Segments: coalesceAIContextSegments(client),
		}, http.StatusOK, nil
	}
	if uuid.Validate(sessionID) != nil {
		return loadedAIContextSegments{}, http.StatusBadRequest, fmt.Errorf("session_id must be a UUID")
	}
	session, err := h.store.GetSessionByID(r.Context(), sessionID)
	if err != nil {
		return loadedAIContextSegments{}, http.StatusInternalServerError, fmt.Errorf("failed to load session")
	}
	if statusCode, accessErr := validateContextSessionAccess(
		session,
		claims,
	); accessErr != nil {
		return loadedAIContextSegments{}, statusCode, accessErr
	}
	normalizedPolicy, err := aicontext.NormalizePolicy(policy)
	if err != nil {
		return loadedAIContextSegments{}, http.StatusBadRequest, err
	}
	return loadAuthorizedAIContextSegments(
		r.Context(),
		h.store,
		sessionID,
		client,
		normalizedPolicy,
	)
}

func loadAuthorizedAIContextSegments(
	ctx context.Context,
	reader aiContextTranscriptReader,
	sessionID string,
	client []aicontext.TranscriptSegment,
	normalizedPolicy aicontext.ContextPolicy,
) (loadedAIContextSegments, int, error) {
	loaded, err := loadPersistedAIContextSegments(
		ctx,
		reader,
		sessionID,
		client,
		normalizedPolicy,
	)
	if err != nil {
		if errors.Is(err, aicontext.ErrContextTooLarge) {
			return loadedAIContextSegments{}, http.StatusUnprocessableEntity, err
		}
		return loadedAIContextSegments{}, http.StatusInternalServerError, err
	}
	return loaded, http.StatusOK, nil
}

func loadPersistedAIContextSegments(
	ctx context.Context,
	reader aiContextTranscriptReader,
	sessionID string,
	client []aicontext.TranscriptSegment,
	normalizedPolicy aicontext.ContextPolicy,
) (loadedAIContextSegments, error) {
	if normalizedPolicy.Mode == "retrieval" {
		return loadedAIContextSegments{
			Segments: coalesceAIContextSegments(client),
		}, nil
	}
	if normalizedPolicy.Mode == "full" {
		return loadFullContextSegments(
			ctx,
			reader,
			sessionID,
			client,
			normalizedPolicy.MaxTokens,
		)
	}
	return loadSmartContextSegments(
		ctx,
		reader,
		sessionID,
		client,
		normalizedPolicy.MaxTokens,
	)
}

const aiContextTranscriptPageSize = 256

func loadFullContextSegments(
	ctx context.Context,
	reader aiContextTranscriptReader,
	sessionID string,
	client []aicontext.TranscriptSegment,
	maxTokens int,
) (loadedAIContextSegments, error) {
	accumulator := newAIContextSegmentAccumulator(aiContextTranscriptPageSize)
	seen := make(map[string]struct{})
	lastStoredEnd := float64(-1)
	var cursor *store.TranscriptPageCursor
	for {
		transcripts, hasMore, err := reader.GetTranscriptsPageBySession(
			ctx,
			sessionID,
			aiContextTranscriptPageSize,
			cursor,
		)
		if err != nil {
			return loadedAIContextSegments{}, fmt.Errorf("failed to load transcript: %w", err)
		}
		for index := range transcripts {
			segment, ok := aiContextSegmentFromTranscript(&transcripts[index])
			if !ok {
				continue
			}
			if segment.EndTime > lastStoredEnd {
				lastStoredEnd = segment.EndTime
			}
			appendUniqueAIContextSegment(accumulator, seen, segment)
			if accumulator.formattedBytes > maxTokens {
				return loadedAIContextSegments{}, fmt.Errorf(
					"%w: stored transcript exceeds the configured token limit %d",
					aicontext.ErrContextTooLarge,
					maxTokens,
				)
			}
		}
		if !hasMore {
			break
		}
		if len(transcripts) == 0 {
			return loadedAIContextSegments{}, errors.New(
				"transcript pagination made no progress",
			)
		}
		last := transcripts[len(transcripts)-1]
		cursor = &store.TranscriptPageCursor{
			StartTime: last.StartTime,
			ID:        last.ID,
		}
	}
	for _, segment := range client {
		// Display cards aggregate several atomic database rows. Only append
		// cards that start after the newest persisted row, otherwise the model
		// would receive the same speech in atomic and aggregated form.
		if lastStoredEnd >= 0 && segment.StartTime <= lastStoredEnd+0.05 {
			continue
		}
		appendUniqueAIContextSegment(accumulator, seen, segment)
		if accumulator.formattedBytes > maxTokens {
			return loadedAIContextSegments{}, fmt.Errorf(
				"%w: transcript exceeds the configured token limit %d",
				aicontext.ErrContextTooLarge,
				maxTokens,
			)
		}
	}
	return loadedAIContextSegments{Segments: accumulator.segments}, nil
}

func loadSmartContextSegments(
	ctx context.Context,
	reader aiContextTranscriptReader,
	sessionID string,
	client []aicontext.TranscriptSegment,
	maxTokens int,
) (loadedAIContextSegments, error) {
	lastStoredEnd, hasStoredEnd, err := reader.GetLatestCompleteTranscriptEnd(
		ctx,
		sessionID,
	)
	if err != nil {
		return loadedAIContextSegments{}, fmt.Errorf("failed to load transcript: %w", err)
	}
	if !hasStoredEnd {
		lastStoredEnd = -1
	}

	// Client display cards are request-bounded and newer than the persisted
	// watermark. Include their text in the lower bound so a long unsynced tail
	// does not force unnecessary database pages.
	eligibleClient := make([]aicontext.TranscriptSegment, 0, len(client))
	lowerBoundBytes := 0
	lowerBoundParts := 0
	clientSeen := make(map[string]struct{}, len(client))
	for _, segment := range client {
		if lastStoredEnd >= 0 && segment.StartTime <= lastStoredEnd+0.05 {
			continue
		}
		key := aiContextSegmentKey(segment)
		if _, exists := clientSeen[key]; exists {
			continue
		}
		textBytes := normalizedAIContextTextBytes(segment.Text)
		if textBytes == 0 {
			continue
		}
		clientSeen[key] = struct{}{}
		eligibleClient = append(eligibleClient, segment)
		lowerBoundBytes += textBytes
		lowerBoundParts++
	}

	// Pages arrive newest-first. Accumulated rows are reversed once, after the
	// retained content is guaranteed to exceed the maximum possible transcript
	// budget. Because coalescing never removes text and merges at most
	// aiContextHardMaxParts rows, this lower bound cannot omit an older segment
	// that smart suffix selection could have used.
	newestFirst := make([]aicontext.TranscriptSegment, 0, aiContextTranscriptPageSize)
	var cursor *store.TranscriptPageCursor
	storedTruncated := hasStoredEnd
	for !aiContextLowerBoundExceeds(
		lowerBoundBytes,
		lowerBoundParts,
		maxTokens,
	) {
		transcripts, hasMore, pageErr := reader.GetTranscriptsPageBySessionDescending(
			ctx,
			sessionID,
			aiContextTranscriptPageSize,
			cursor,
		)
		if pageErr != nil {
			return loadedAIContextSegments{}, fmt.Errorf("failed to load transcript: %w", pageErr)
		}
		storedTruncated = hasMore
		for index := range transcripts {
			segment, ok := aiContextSegmentFromTranscript(&transcripts[index])
			if !ok {
				continue
			}
			textBytes := normalizedAIContextTextBytes(segment.Text)
			if textBytes == 0 {
				continue
			}
			newestFirst = append(newestFirst, segment)
			lowerBoundBytes += textBytes
			lowerBoundParts++
		}
		if !hasMore {
			break
		}
		if len(transcripts) == 0 {
			return loadedAIContextSegments{}, errors.New(
				"transcript pagination made no progress",
			)
		}
		last := transcripts[len(transcripts)-1]
		cursor = &store.TranscriptPageCursor{
			StartTime: last.StartTime,
			ID:        last.ID,
		}
	}
	for left, right := 0, len(newestFirst)-1; left < right; left, right = left+1, right-1 {
		newestFirst[left], newestFirst[right] = newestFirst[right], newestFirst[left]
	}
	accumulator := newAIContextSegmentAccumulator(len(newestFirst) + len(eligibleClient))
	seen := make(map[string]struct{}, len(newestFirst)+len(eligibleClient))
	for _, segment := range newestFirst {
		appendUniqueAIContextSegment(accumulator, seen, segment)
	}
	for _, segment := range eligibleClient {
		appendUniqueAIContextSegment(accumulator, seen, segment)
	}
	return loadedAIContextSegments{
		Segments:        accumulator.segments,
		StoredTruncated: storedTruncated,
	}, nil
}

func aiContextSegmentFromTranscript(
	transcript *models.Transcript,
) (aicontext.TranscriptSegment, bool) {
	if transcript == nil ||
		transcript.IsPartial ||
		strings.EqualFold(strings.TrimSpace(transcript.Status), "partial") {
		return aicontext.TranscriptSegment{}, false
	}
	endTime := transcript.StartTime
	if transcript.EndTime != nil {
		endTime = *transcript.EndTime
	}
	return aicontext.TranscriptSegment{
		ID:        transcript.ClientSegmentID,
		Speaker:   transcript.Speaker,
		Text:      transcript.Text,
		StartTime: transcript.StartTime,
		EndTime:   endTime,
	}, true
}

func normalizedAIContextTextBytes(text string) int {
	return len(strings.Join(strings.Fields(strings.TrimSpace(text)), " "))
}

func aiContextLowerBoundExceeds(contentBytes, parts, maxTokens int) bool {
	if parts <= 0 {
		return false
	}
	minimumSegments := (parts + aiContextHardMaxParts - 1) /
		aiContextHardMaxParts
	// Every formatted segment has at least a one-byte speaker plus ": ", and
	// separate segments require one newline.
	lowerBound := contentBytes + minimumSegments*3
	if minimumSegments > 1 {
		lowerBound += minimumSegments - 1
	}
	return lowerBound > maxTokens
}

func aiContextSegmentKey(segment aicontext.TranscriptSegment) string {
	if key := strings.TrimSpace(segment.ID); key != "" {
		return key
	}
	return fmt.Sprintf(
		"%.3f|%.3f|%s",
		segment.StartTime,
		segment.EndTime,
		strings.TrimSpace(segment.Text),
	)
}

func appendUniqueAIContextSegment(
	accumulator *aiContextSegmentAccumulator,
	seen map[string]struct{},
	segment aicontext.TranscriptSegment,
) {
	if strings.TrimSpace(segment.Text) == "" {
		return
	}
	key := aiContextSegmentKey(segment)
	if _, exists := seen[key]; exists {
		return
	}
	seen[key] = struct{}{}
	accumulator.append(segment)
}

const (
	aiContextSentenceBreakMinRunes = 120
	aiContextHardMaxRunes          = 420
	aiContextHardMaxSeconds        = 32
	aiContextHardMaxParts          = 48
	aiContextMergeGapSeconds       = 2
	aiContextMidSentenceGapSeconds = 3.5
)

// coalesceAIContextSegments turns provider micro-finals into readable,
// source-attributable paragraphs without changing the persisted transcript.
// Its bounds mirror the transcript feed so AI previews and the UI describe
// approximately the same complete utterances.
func coalesceAIContextSegments(
	segments []aicontext.TranscriptSegment,
) []aicontext.TranscriptSegment {
	accumulator := newAIContextSegmentAccumulator(len(segments))
	for _, segment := range segments {
		accumulator.append(segment)
	}
	return accumulator.segments
}

type aiContextSegmentAccumulator struct {
	segments       []aicontext.TranscriptSegment
	partCounts     []int
	formattedBytes int
}

func newAIContextSegmentAccumulator(capacity int) *aiContextSegmentAccumulator {
	if capacity < 0 {
		capacity = 0
	}
	return &aiContextSegmentAccumulator{
		segments:   make([]aicontext.TranscriptSegment, 0, capacity),
		partCounts: make([]int, 0, capacity),
	}
}

func (accumulator *aiContextSegmentAccumulator) append(
	raw aicontext.TranscriptSegment,
) {
	segment := raw
	segment.Text = strings.TrimSpace(segment.Text)
	segment.Speaker = strings.TrimSpace(segment.Speaker)
	if segment.Text == "" {
		return
	}
	if segment.Speaker == "" {
		segment.Speaker = "Speaker"
	}
	if segment.EndTime < segment.StartTime {
		segment.EndTime = segment.StartTime
	}
	if len(accumulator.segments) == 0 {
		accumulator.segments = append(accumulator.segments, segment)
		accumulator.partCounts = append(accumulator.partCounts, 1)
		accumulator.formattedBytes = len(aicontext.FormatTranscript(
			[]aicontext.TranscriptSegment{segment},
		))
		return
	}

	lastIndex := len(accumulator.segments) - 1
	current := &accumulator.segments[lastIndex]
	currentParts := accumulator.partCounts[lastIndex]
	gapLimit := float64(aiContextMidSentenceGapSeconds)
	if aiContextEndsSentence(current.Text) {
		gapLimit = aiContextMergeGapSeconds
	}
	gap := segment.StartTime - current.EndTime
	combined := joinAIContextSegmentText(current.Text, segment.Text)
	canMerge := current.Speaker == segment.Speaker &&
		segment.EndTime+2 >= current.StartTime &&
		gap <= gapLimit &&
		currentParts < aiContextHardMaxParts &&
		segment.EndTime-current.StartTime <= aiContextHardMaxSeconds &&
		utf8.RuneCountInString(combined) <= aiContextHardMaxRunes &&
		(!aiContextEndsSentence(current.Text) ||
			utf8.RuneCountInString(current.Text) <
				aiContextSentenceBreakMinRunes)
	if !canMerge {
		accumulator.segments = append(accumulator.segments, segment)
		accumulator.partCounts = append(accumulator.partCounts, 1)
		accumulator.formattedBytes++ // transcript line separator
		accumulator.formattedBytes += len(aicontext.FormatTranscript(
			[]aicontext.TranscriptSegment{segment},
		))
		return
	}
	oldBytes := len(aicontext.FormatTranscript(
		[]aicontext.TranscriptSegment{*current},
	))
	current.Text = combined
	if segment.EndTime > current.EndTime {
		current.EndTime = segment.EndTime
	}
	accumulator.partCounts[lastIndex] = currentParts + 1
	newBytes := len(aicontext.FormatTranscript(
		[]aicontext.TranscriptSegment{*current},
	))
	accumulator.formattedBytes += newBytes - oldBytes
}

func aiContextEndsSentence(text string) bool {
	trimmed := strings.TrimSpace(text)
	for trimmed != "" {
		last, size := utf8.DecodeLastRuneInString(trimmed)
		if strings.ContainsRune("\"')]}»”’", last) {
			trimmed = strings.TrimSpace(trimmed[:len(trimmed)-size])
			continue
		}
		return strings.ContainsRune(".!?。！？…", last)
	}
	return false
}

func joinAIContextSegmentText(left, right string) string {
	head := strings.TrimSpace(left)
	tail := strings.TrimSpace(right)
	if head == "" {
		return tail
	}
	if tail == "" {
		return head
	}
	last, _ := utf8.DecodeLastRuneInString(head)
	first, _ := utf8.DecodeRuneInString(tail)
	if strings.ContainsRune(",.;:!?%)]}»”’…、，。；：！？』」）】", first) ||
		isAIContextCJK(last) || isAIContextCJK(first) {
		return head + tail
	}
	return head + " " + tail
}

func isAIContextCJK(value rune) bool {
	return unicode.In(
		value,
		unicode.Han,
		unicode.Hiragana,
		unicode.Katakana,
		unicode.Hangul,
	)
}

func validateContextSessionAccess(
	session *models.Session,
	claims *auth.UserClaims,
) (int, error) {
	if session == nil {
		return http.StatusNotFound, fmt.Errorf("session not found")
	}
	if claims == nil {
		return http.StatusUnauthorized, fmt.Errorf("authentication required")
	}
	if session.UserID != claims.UserID || session.TenantID != claims.TenantID {
		return http.StatusForbidden, fmt.Errorf("session access denied")
	}
	return http.StatusOK, nil
}
