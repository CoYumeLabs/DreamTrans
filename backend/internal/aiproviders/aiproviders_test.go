package aiproviders

import (
	"strings"
	"testing"
)

func TestLoadRegistersDefaultAndExtraProviders(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-default")
	t.Setenv("OPENAI_API_BASE", "https://api.openai.com/v1")
	t.Setenv("OPENAI_MODEL", "gpt-5.6-sol")
	t.Setenv("OPENAI_FALLBACK_MODELS", "gpt-5-mini")
	t.Setenv("AI_PROVIDERS", "cerebras=https://api.cerebras.ai/v1; groq=https://api.groq.com/openai/v1")
	t.Setenv("AI_PROVIDER_KEYS", "cerebras=csk-1;groq=gsk-2")
	t.Setenv("AI_PROVIDER_OPTIONS", "groq=responses,cache")
	t.Setenv("AI_EMBEDDING_PROVIDER", "")
	r, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Names(), ","); got != "openai-compatible,cerebras,groq" {
		t.Fatalf("names=%s", got)
	}
	cfg, err := r.ConfigFor("cerebras::llama-3.3-70b")
	if err != nil || cfg.Provider != "cerebras" || cfg.BaseURL != "https://api.cerebras.ai/v1" || cfg.APIKey != "csk-1" || cfg.Model != "llama-3.3-70b" || cfg.UseResponsesAPI || cfg.EnablePromptCache || len(cfg.FallbackModels) != 0 {
		t.Fatalf("cerebras config: %+v %v", cfg, err)
	}
	cfg, err = r.ConfigFor("groq::llama3-8b")
	if err != nil || !cfg.UseResponsesAPI || !cfg.EnablePromptCache {
		t.Fatalf("groq options: %+v %v", cfg, err)
	}
	// Bare ids and empty ids belong to the default provider, which keeps the
	// OPENAI_* behavior: its model default and its own fallbacks.
	cfg, err = r.ConfigFor("")
	if err != nil || cfg.Provider != "openai-compatible" || cfg.Model != "gpt-5.6-sol" || cfg.APIKey != "sk-default" || !cfg.UseResponsesAPI || strings.Join(cfg.FallbackModels, ",") != "gpt-5-mini" {
		t.Fatalf("default config: %+v %v", cfg, err)
	}
	// Gateway model ids contain slashes; only "::" separates a provider.
	if provider, model := Split("meta-llama/llama-3.3-70b"); provider != Default || model != "meta-llama/llama-3.3-70b" {
		t.Fatalf("slash id split: %s %s", provider, model)
	}
	if Qualify("openai-compatible", "gpt-5.6-sol") != "gpt-5.6-sol" || Qualify("cerebras", "x") != "cerebras::x" {
		t.Fatal("qualify")
	}
	emb, err := r.EmbeddingProvider()
	if err != nil || emb.Name != Default {
		t.Fatalf("embedding provider: %+v %v", emb, err)
	}
}

func TestLoadRejectsMisconfiguration(t *testing.T) {
	cases := map[string]map[string]string{
		"missing key":        {"AI_PROVIDERS": "cerebras=https://api.cerebras.ai/v1"},
		"reserved name":      {"AI_PROVIDERS": "openai-compatible=https://x.test/v1", "AI_PROVIDER_KEYS": "openai-compatible=k"},
		"bad name":           {"AI_PROVIDERS": "Cere bras=https://x.test/v1", "AI_PROVIDER_KEYS": "cere bras=k"},
		"bad url":            {"AI_PROVIDERS": "cerebras=ftp://x.test", "AI_PROVIDER_KEYS": "cerebras=k"},
		"unknown option":     {"AI_PROVIDERS": "cerebras=https://x.test/v1", "AI_PROVIDER_KEYS": "cerebras=k", "AI_PROVIDER_OPTIONS": "cerebras=turbo"},
		"unknown embedding":  {"OPENAI_API_KEY": "sk", "AI_EMBEDDING_PROVIDER": "nowhere"},
		"duplicate provider": {"AI_PROVIDERS": "a=https://x.test/v1;a=https://y.test/v1", "AI_PROVIDER_KEYS": "a=k"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			for _, key := range []string{"OPENAI_API_KEY", "AI_PROVIDERS", "AI_PROVIDER_KEYS", "AI_PROVIDER_OPTIONS", "AI_EMBEDDING_PROVIDER"} {
				t.Setenv(key, env[key])
			}
			if _, err := Load(); err == nil {
				t.Fatalf("%s accepted", name)
			}
		})
	}
}

func TestUnknownProviderInModelIDIsAnError(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk")
	t.Setenv("AI_PROVIDERS", "")
	t.Setenv("AI_PROVIDER_KEYS", "")
	t.Setenv("AI_PROVIDER_OPTIONS", "")
	t.Setenv("AI_EMBEDDING_PROVIDER", "")
	r, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ConfigFor("cerebras::llama"); err == nil {
		t.Fatal("unregistered provider accepted")
	}
}

func TestDefaultModelRemainsAvailableWithoutExplicitModelEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("AI_PROVIDERS", "")
	t.Setenv("AI_EMBEDDING_PROVIDER", "")
	r, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := r.ConfigFor("")
	if err != nil || cfg.Model != "gpt-5.6-sol" {
		t.Fatalf("default config=%+v err=%v", cfg, err)
	}
}
