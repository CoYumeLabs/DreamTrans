package handlers

import (
	"fmt"
	"strings"
)

// defaultSummaryPrompt compresses rolling context for any language pair. The
// earlier wording hard-coded English input and output, which produced English
// summaries for Chinese or Japanese lectures and degraded every translation
// that read them.
const defaultSummaryPrompt = "You are a precise context compressor. Summarize the transcribed conversation text for downstream translation. Keep names, entities, topics, and unresolved references. Keep it concise and information-dense. Write the summary in the same language as the transcript."

// translateLanguageNames maps the language codes the workspace offers (plus
// common aliases) to the names used inside generated prompts.
var translateLanguageNames = map[string]string{
	"cmn_en": "Mandarin Chinese and English (possibly mixed)",
	"en":     "English",
	"cmn":    "Simplified Chinese",
	"zh":     "Simplified Chinese",
	"yue":    "Cantonese",
	"ja":     "Japanese",
	"ko":     "Korean",
	"es":     "Spanish",
	"fr":     "French",
	"de":     "German",
	"it":     "Italian",
	"pt":     "Portuguese",
	"ru":     "Russian",
	"ar":     "Arabic",
	"hi":     "Hindi",
	"vi":     "Vietnamese",
	"th":     "Thai",
	"id":     "Indonesian",
	"ms":     "Malay",
	"nl":     "Dutch",
	"tr":     "Turkish",
	"pl":     "Polish",
}

// normalizeLanguageCode lower-cases and trims a client language code.
func normalizeLanguageCode(code string) string {
	return strings.ToLower(strings.TrimSpace(code))
}

// validLanguageCode accepts short BCP-47-ish codes such as "en", "cmn" or
// "zh-Hans"; anything else is rejected before it can reach a prompt.
func validLanguageCode(code string) bool {
	if len(code) < 2 || len(code) > 16 {
		return false
	}
	for _, r := range code {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// translateLanguageName returns a human name for a language code, falling back
// to the code itself so an unknown pair still yields a usable prompt.
func translateLanguageName(code string) string {
	code = normalizeLanguageCode(code)
	if name, ok := translateLanguageNames[code]; ok {
		return name
	}
	if base, _, found := strings.Cut(code, "-"); found {
		if name, ok := translateLanguageNames[base]; ok {
			return name
		}
	}
	return code
}

// isChineseCode reports whether code denotes Mandarin written in Simplified
// Chinese, the pair the operator-configured prompt is written for.
func isChineseCode(code string) bool {
	switch normalizeLanguageCode(code) {
	case "cmn", "zh", "zh-cn", "zh-hans":
		return true
	}
	return false
}

// defaultTranslatePrompt picks the system prompt used when the client sends no
// custom prompt. The operator-configured prompt is written for English →
// Simplified Chinese and is kept for that pair (and for clients that do not
// announce a pair); every other pair gets an equivalent generated prompt so
// the model is told which language to write in.
func defaultTranslatePrompt(source, target, configured string) string {
	source = normalizeLanguageCode(source)
	target = normalizeLanguageCode(target)
	if source == "" || target == "" {
		return configured
	}
	if source == "en" && isChineseCode(target) {
		return configured
	}
	return buildTranslatePrompt(source, target)
}

// buildTranslatePrompt writes a simultaneous-interpretation prompt for one
// language pair, including target-script punctuation and register guidance.
func buildTranslatePrompt(source, target string) string {
	sourceName := translateLanguageName(source)
	targetName := translateLanguageName(target)
	var b strings.Builder
	fmt.Fprintf(&b, "You are a professional simultaneous interpreter translating spoken %s into fluent, natural %s. ", sourceName, targetName)
	b.WriteString("The input is an automatic speech transcript: it may contain recognition errors, homophones, dropped words, and missing or misplaced punctuation. ")
	b.WriteString("Use <context> only to understand the situation, resolve references, and repair obvious recognition errors. ")
	fmt.Fprintf(&b, "Translate only the text inside <text>...</text> into %s, then polish it so it reads smoothly while preserving the original meaning and tone: ", targetName)
	b.WriteString("merge incomplete sentences, fix word order, and drop filler words and false starts. ")
	b.WriteString("Keep terminology accurate and keep numbers, units, names, and code identifiers unchanged. ")
	b.WriteString(targetStyleGuidance(target))
	b.WriteString("Do not include anything from <context> in the output. ")
	b.WriteString("Do not add explanations, quotes, speaker labels, timestamps, or language tags. ")
	if source == target {
		fmt.Fprintf(&b, "If the text is already %s, return it cleaned up and punctuated rather than translating it. ", targetName)
	}
	fmt.Fprintf(&b, "Return only the final polished %s sentence(s), nothing else.", targetName)
	return b.String()
}

// targetStyleGuidance returns script-specific punctuation and register rules.
func targetStyleGuidance(target string) string {
	switch {
	case isChineseCode(target), normalizeLanguageCode(target) == "yue":
		return "Use full-width Chinese punctuation (，。？！：；) and do not put spaces between Chinese characters. "
	case normalizeLanguageCode(target) == "ja":
		return "Use full-width Japanese punctuation (、。？！) with no spaces between characters, and write in natural polite register (です・ます) unless the speaker is clearly casual. "
	case normalizeLanguageCode(target) == "ko":
		return "Use standard Korean spacing and punctuation, and write in natural 해요체 unless the speaker is clearly formal or casual. "
	default:
		return fmt.Sprintf("Use standard %s punctuation, capitalization, and spacing. ", translateLanguageName(target))
	}
}
