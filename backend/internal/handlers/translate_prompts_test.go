package handlers

import (
	"strings"
	"testing"
)

func TestDefaultTranslatePromptKeepsConfiguredPromptForEnglishToChinese(t *testing.T) {
	configured := "operator prompt"
	for _, pair := range [][2]string{{"en", "cmn"}, {"EN", "zh"}, {"en", "zh-Hans"}, {"", ""}, {"en", ""}, {"", "cmn"}} {
		if got := defaultTranslatePrompt(pair[0], pair[1], configured); got != configured {
			t.Fatalf("pair %v: expected configured prompt, got %q", pair, got)
		}
	}
}

func TestDefaultTranslatePromptNamesBothLanguagesForOtherPairs(t *testing.T) {
	cases := []struct {
		source, target string
		wantSource     string
		wantTarget     string
		wantStyle      string
	}{
		{"cmn", "en", "Simplified Chinese", "English", "Use standard English punctuation"},
		{"cmn_en", "en", "Mandarin Chinese and English (possibly mixed)", "English", "Use standard English punctuation"},
		{"ja", "cmn", "Japanese", "Simplified Chinese", "full-width Chinese punctuation"},
		{"en", "ja", "English", "Japanese", "です・ます"},
		{"en", "ko", "English", "Korean", "해요체"},
		{"es", "de", "Spanish", "German", "Use standard German punctuation"},
		{"xx", "yy", "xx", "yy", "Use standard yy punctuation"},
	}
	for _, tc := range cases {
		got := defaultTranslatePrompt(tc.source, tc.target, "operator prompt")
		if got == "operator prompt" {
			t.Fatalf("%s→%s: configured English→Chinese prompt must not be reused", tc.source, tc.target)
		}
		for _, want := range []string{
			"translating spoken " + tc.wantSource + " into fluent, natural " + tc.wantTarget,
			"into " + tc.wantTarget + ", then polish",
			tc.wantStyle,
			"Return only the final polished " + tc.wantTarget,
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s→%s prompt missing %q:\n%s", tc.source, tc.target, want, got)
			}
		}
	}
}

func TestDefaultTranslatePromptSameLanguageAsksForCleanup(t *testing.T) {
	got := defaultTranslatePrompt("en", "en", "operator prompt")
	if !strings.Contains(got, "already English, return it cleaned up") {
		t.Fatalf("same-language prompt should ask for cleanup:\n%s", got)
	}
}

func TestApplyPromptConfigSelectsPromptByLanguagePair(t *testing.T) {
	state := defaultConnState()
	state.configuredTranslatePrompt = "operator en→zh prompt"
	state.translatePrompt = state.configuredTranslatePrompt

	state.applyConfig(&clientConfig{SourceLanguage: "cmn", TargetLanguage: "en"})
	if !strings.Contains(state.translatePrompt, "Simplified Chinese into fluent, natural English") {
		t.Fatalf("cmn→en should generate a prompt, got %q", state.translatePrompt)
	}

	state.applyConfig(&clientConfig{SourceLanguage: "en", TargetLanguage: "cmn"})
	if state.translatePrompt != "operator en→zh prompt" {
		t.Fatalf("en→cmn should restore the configured prompt, got %q", state.translatePrompt)
	}

	state.applyConfig(&clientConfig{SourceLanguage: "ja", TargetLanguage: "ko", TranslatePrompt: "custom"})
	if state.translatePrompt != "custom" {
		t.Fatalf("custom prompt must win, got %q", state.translatePrompt)
	}

	// A legacy client that announces no pair keeps the configured prompt.
	legacy := defaultConnState()
	legacy.configuredTranslatePrompt = "operator en→zh prompt"
	legacy.applyConfig(&clientConfig{MinChunkChars: 8})
	if legacy.translatePrompt != "operator en→zh prompt" {
		t.Fatalf("legacy client should keep configured prompt, got %q", legacy.translatePrompt)
	}
}

func TestValidateClientConfigRejectsMalformedLanguageCodes(t *testing.T) {
	for _, code := range []string{"e", "en cmn", "zh;drop", strings.Repeat("a", 17)} {
		if err := validateClientConfig(&clientConfig{SourceLanguage: code}); err == nil {
			t.Fatalf("source_language %q should be rejected", code)
		}
		if err := validateClientConfig(&clientConfig{TargetLanguage: code}); err == nil {
			t.Fatalf("target_language %q should be rejected", code)
		}
	}
	for _, code := range []string{"en", "cmn", "zh-Hans", "pt_BR", ""} {
		if err := validateClientConfig(&clientConfig{SourceLanguage: code, TargetLanguage: code}); err != nil {
			t.Fatalf("language %q should be accepted: %v", code, err)
		}
	}
}
