package handlers

import (
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

func isSentenceEnding(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	// A sentence may close with quotes or brackets after its terminal mark:
	// `He said "stop."` / 「終わりました。」
	s = strings.TrimRight(s, "\"')]»”’」』）】")
	rs := []rune(s)
	if len(rs) == 0 {
		return false
	}
	last := rs[len(rs)-1]
	switch last {
	case '.', '?', '!', ';', '\n', '\u3002', '\uFF1F', '\uFF01', '\uFF1B', '\u2026':
		return true
	}
	return false
}

// handleAggregation appends the segment to speaker buffer and decides whether to flush.
// Returns: (flushed, text, start, end)
func (st *connState) handleAggregation(speaker, seg string, start, end float64) (flushed bool, text string, s, e float64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	trimmed := strings.TrimSpace(seg)

	a := st.speakers[speaker]
	if a == nil {
		a = &aggState{}
		st.speakers[speaker] = a
	}

	// If there is a long gap between previous end and current start, flush first
	if a.buffer != "" && a.lastEnd > 0 && (start-a.lastEnd) > st.flushGapSeconds {
		text = strings.TrimSpace(a.buffer)
		s := a.startTime
		e := a.lastEnd
		// reset and start new with current
		a.buffer = ""
		a.startTime = 0
		a.lastEnd = 0

		// initialize with current seg after releasing flush
		a.buffer = trimmed
		a.startTime = start
		a.lastEnd = end
		a.updatedAt = now
		if text != "" {
			return true, text, s, e
		}
		// if empty, fallthrough
		return false, "", 0, 0
	}

	separatorRunes := 0
	if a.buffer != "" && !strings.HasSuffix(a.buffer, " ") && trimmed != "" {
		separatorRunes = 1
	}
	if a.buffer != "" &&
		utf8.RuneCountInString(a.buffer)+separatorRunes+utf8.RuneCountInString(trimmed) >
			maxAggregationBufferRunes {
		text = strings.TrimSpace(a.buffer)
		s = a.startTime
		e = a.lastEnd
		a.buffer = trimmed
		a.startTime = start
		a.lastEnd = end
		a.updatedAt = now
		if text != "" {
			return true, text, s, e
		}
		return false, "", 0, 0
	}

	// Normal append
	if a.buffer == "" {
		a.startTime = start
		a.buffer = trimmed
		a.lastEnd = end
		a.updatedAt = now
	} else {
		// Script-aware join: a space between Latin words, none inside CJK.
		a.buffer = joinFragments(a.buffer, trimmed)
		a.lastEnd = end
		a.updatedAt = now
	}

	// Decide flush
	// Flush on sentence ending regardless of minChunkChars to avoid missing short utterances
	if isSentenceEnding(seg) {
		text = strings.TrimSpace(a.buffer)
		s := a.startTime
		e := a.lastEnd
		// reset
		a.buffer = ""
		a.startTime = 0
		a.lastEnd = 0
		a.updatedAt = time.Time{}
		if text != "" {
			return true, text, s, e
		}
	}
	return false, "", 0, 0
}

// handleRAGAggregation manages a dedicated buffer for RAG ingestion so that
// translation batching can remain conservative while chat retrieval gets fresher context.
func (st *connState) handleRAGAggregation(speaker, seg string, start, end float64) (flushed bool, text string, s, e float64) {
	trimmed := strings.TrimSpace(seg)
	if trimmed == "" {
		return false, "", 0, 0
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()

	rs := st.ragBuffers[speaker]
	if rs == nil {
		rs = &ragState{}
		st.ragBuffers[speaker] = rs
	}

	if rs.buffer == "" {
		rs.startTime = start
	} else if start-rs.lastEnd >= st.ragFlushGapSeconds {
		text = strings.TrimSpace(rs.buffer)
		s = rs.startTime
		e = rs.lastEnd
		rs.buffer = trimmed
		rs.startTime = start
		rs.lastEnd = end
		rs.charCount = utf8.RuneCountInString(trimmed)
		rs.updatedAt = now
		if text != "" {
			return true, text, s, e
		}
		return false, "", 0, 0
	}

	separatorRunes := 0
	if rs.buffer != "" && !strings.HasSuffix(rs.buffer, " ") {
		separatorRunes = 1
	}
	incomingRunes := utf8.RuneCountInString(trimmed)
	if rs.buffer != "" &&
		rs.charCount+separatorRunes+incomingRunes > maxAggregationBufferRunes {
		text = strings.TrimSpace(rs.buffer)
		s = rs.startTime
		e = rs.lastEnd
		rs.buffer = trimmed
		rs.startTime = start
		rs.lastEnd = end
		rs.charCount = incomingRunes
		rs.updatedAt = now
		if text != "" {
			return true, text, s, e
		}
		return false, "", 0, 0
	}

	rs.buffer = joinFragments(rs.buffer, trimmed)
	rs.lastEnd = end
	rs.charCount = utf8.RuneCountInString(rs.buffer)
	rs.updatedAt = now

	if isSentenceEnding(seg) {
		text = strings.TrimSpace(rs.buffer)
		s = rs.startTime
		e = rs.lastEnd
		rs.buffer = ""
		rs.startTime = 0
		rs.lastEnd = 0
		rs.charCount = 0
		rs.updatedAt = time.Time{}
		if text != "" {
			return true, text, s, e
		}
		return false, "", 0, 0
	}

	span := end - rs.startTime
	if rs.charCount >= st.ragMinChars && span >= st.ragMinSpanSeconds {
		text = strings.TrimSpace(rs.buffer)
		s = rs.startTime
		e = rs.lastEnd
		rs.buffer = ""
		rs.startTime = 0
		rs.lastEnd = 0
		rs.charCount = 0
		rs.updatedAt = time.Time{}
		if text != "" {
			return true, text, s, e
		}
	}
	return false, "", 0, 0
}

// enqueueSentence adds a completed sentence to a paragraph batch and decides whether to flush.
// Returns (flushed, text, start, end)
func (st *connState) enqueueSentence(speaker, text string, start, end float64) (flushed bool, combined string, s, e float64) {
	st.mu.Lock()
	defer st.mu.Unlock()

	ps := st.paragraphs[speaker]
	if ps == nil {
		ps = &paraState{}
		st.paragraphs[speaker] = ps
	}

	if len(ps.list) == 0 {
		ps.firstTime = start
	}
	ps.list = append(ps.list, sentence{text: strings.TrimSpace(text), startTime: start, endTime: end})
	ps.lastTime = end
	ps.updatedAt = time.Now()

	// Flush on max sentences
	if len(ps.list) >= st.maxSentences {
		combined, s, e = combineSentences(ps.list)
		ps.list = nil
		ps.updatedAt = time.Time{}
		return true, combined, s, e
	}
	// Flush if window exceeded
	if (ps.lastTime - ps.firstTime) >= st.paragraphWindowSeconds {
		combined, s, e = combineSentences(ps.list)
		ps.list = nil
		ps.updatedAt = time.Time{}
		return true, combined, s, e
	}
	return false, "", 0, 0
}

// nolint:gocritic // return names are unnecessary here; keep concise signature
func combineSentences(list []sentence) (string, float64, float64) {
	if len(list) == 0 {
		return "", 0, 0
	}
	combined := ""
	for _, s := range list {
		combined = joinFragments(combined, s.text)
	}
	return combined, list[0].startTime, list[len(list)-1].endTime
}

// flushPending drains buffers after wall-clock silence. Speech timestamps only
// advance when a new segment arrives, so relying on the next segment to notice a
// gap loses the final utterance of a session.
//
//nolint:gocyclo // The drain handles aggregation, paragraph, and RAG buffers atomically.
func (st *connState) flushPending(now time.Time, force bool) ([]pendingParagraph, []pendingRAGParagraph) {
	st.mu.Lock()
	defer st.mu.Unlock()

	paragraphs := make([]pendingParagraph, 0)
	ragParagraphs := make([]pendingRAGParagraph, 0)
	silentSpeakers := make(map[string]bool)

	appendSentence := func(speaker string, sent sentence, updatedAt time.Time) {
		ps := st.paragraphs[speaker]
		if ps == nil {
			ps = &paraState{}
			st.paragraphs[speaker] = ps
		}
		if len(ps.list) == 0 {
			ps.firstTime = sent.startTime
		}
		ps.list = append(ps.list, sent)
		ps.lastTime = sent.endTime
		ps.updatedAt = updatedAt
		if len(ps.list) >= st.maxSentences ||
			(ps.lastTime-ps.firstTime) >= st.paragraphWindowSeconds {
			text, start, end := combineSentences(ps.list)
			if text != "" {
				paragraphs = append(paragraphs, pendingParagraph{
					speaker: speaker, text: text, startTime: start, endTime: end,
				})
			}
			ps.list = nil
			ps.updatedAt = time.Time{}
		}
	}

	for speaker, a := range st.speakers {
		if a == nil || strings.TrimSpace(a.buffer) == "" {
			continue
		}
		expired := !a.updatedAt.IsZero() &&
			now.Sub(a.updatedAt) >= time.Duration(st.flushGapSeconds*float64(time.Second))
		if !force && !expired {
			continue
		}
		appendSentence(speaker, sentence{
			text:      strings.TrimSpace(a.buffer),
			startTime: a.startTime,
			endTime:   a.lastEnd,
		}, a.updatedAt)
		a.buffer = ""
		a.startTime = 0
		a.lastEnd = 0
		a.updatedAt = time.Time{}
		// An incomplete utterance has already waited for the aggregation
		// silence threshold; do not make it wait for another paragraph timer.
		silentSpeakers[speaker] = true
	}

	for speaker, ps := range st.paragraphs {
		if ps == nil || len(ps.list) == 0 {
			continue
		}
		expired := !ps.updatedAt.IsZero() &&
			now.Sub(ps.updatedAt) >= time.Duration(st.paragraphWindowSeconds*float64(time.Second))
		if !force && !expired && !silentSpeakers[speaker] {
			continue
		}
		text, start, end := combineSentences(ps.list)
		if text != "" {
			paragraphs = append(paragraphs, pendingParagraph{
				speaker: speaker, text: text, startTime: start, endTime: end,
			})
		}
		ps.list = nil
		ps.updatedAt = time.Time{}
	}

	for speaker, rs := range st.ragBuffers {
		if rs == nil || strings.TrimSpace(rs.buffer) == "" {
			continue
		}
		expired := !rs.updatedAt.IsZero() &&
			now.Sub(rs.updatedAt) >= time.Duration(st.ragFlushGapSeconds*float64(time.Second))
		if !force && !expired {
			continue
		}
		ragParagraphs = append(ragParagraphs, pendingRAGParagraph{
			speaker:   speaker,
			text:      strings.TrimSpace(rs.buffer),
			startTime: rs.startTime,
			endTime:   rs.lastEnd,
		})
		rs.buffer = ""
		rs.startTime = 0
		rs.lastEnd = 0
		rs.charCount = 0
		rs.updatedAt = time.Time{}
	}

	sort.SliceStable(paragraphs, func(i, j int) bool {
		return paragraphs[i].startTime < paragraphs[j].startTime
	})
	sort.SliceStable(ragParagraphs, func(i, j int) bool {
		return ragParagraphs[i].startTime < ragParagraphs[j].startTime
	})
	return paragraphs, ragParagraphs
}
