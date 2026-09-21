package app

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Ported from Yufolo's TranscriptFeedModel/scriptText: retain short same-speaker
// finals together, prefer sentence boundaries, and weight CJK by reading length.
func spacelessCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || (r >= 0x2e80 && r <= 0x303f) || (r >= 0xff00 && r <= 0xffef)
}
func textWeight(text string) int {
	n := 0
	for _, r := range text {
		if spacelessCJK(r) {
			n += 3
		} else {
			n++
		}
	}
	return n
}
func endsSentence(text string) bool {
	v := strings.TrimRight(strings.TrimSpace(text), "\"')]»”’」』）】")
	return strings.ContainsAny(lastRune(v), ".!?。！？…")
}
func lastRune(text string) string {
	r, n := utf8.DecodeLastRuneInString(text)
	if n == 0 {
		return ""
	}
	return string(r)
}

// Match Yufolo scriptText.joinSegmentTexts, including Hangul fragment joins.
func isCJKJoin(r rune) bool {
	return spacelessCJK(r) || (r >= 0x3130 && r <= 0x318f) || (r >= 0xac00 && r <= 0xd7af)
}
func joinSegmentText(left, right string) string {
	a, b := strings.TrimSpace(left), strings.TrimSpace(right)
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	last, _ := utf8.DecodeLastRuneInString(a)
	first, _ := utf8.DecodeRuneInString(b)
	if isCJKJoin(last) || isCJKJoin(first) || strings.ContainsRune(",.;:!?%)]}»”’…、，。；：！？』」）】", first) {
		return a + b
	}
	return a + " " + b
}
func normalizeSegmentText(text string) string {
	runes := []rune(strings.TrimSpace(text))
	var out strings.Builder
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if unicode.IsSpace(r) {
			j := i + 1
			for j < len(runes) && unicode.IsSpace(runes[j]) {
				j++
			}
			if i > 0 && j < len(runes) && ((spacelessCJK(runes[i-1]) && spacelessCJK(runes[j])) || strings.ContainsRune("、，。；：！？』」）】…", runes[j])) {
				i = j - 1
				continue
			}
			out.WriteRune(' ')
			i = j - 1
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}
func canMergeSegment(card, next Segment) bool {
	if card.Speaker != next.Speaker || card.Source != next.Source || next.EndTime+0.25 < card.StartTime {
		return false
	}
	gap := 2.0
	if !endsSentence(card.Text) {
		gap = 3.5
	}
	return next.StartTime-card.EndTime <= gap && card.Parts < 48 && next.EndTime-card.StartTime <= 32 && textWeight(card.Text)+textWeight(next.Text)+1 <= 420 && !(endsSentence(card.Text) && textWeight(card.Text) >= 120)
}
