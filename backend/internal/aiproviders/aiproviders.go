// Package aiproviders is the registry of OpenAI-compatible endpoints the
// deployment may talk to. The historical OPENAI_* variables register the
// default provider; AI_PROVIDERS adds more. Models of non-default providers
// are addressed everywhere as "provider::model" so policies, preferences,
// usage records and cost rates can tell two providers' models apart without
// schema changes.
package aiproviders

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	openaiprovider "github.com/dreamtrans/backend/internal/adapters/openai_provider"
)

// Default is the provider registered from OPENAI_API_BASE / OPENAI_API_KEY.
// Its models keep bare ids, which is what every existing database holds.
const Default = "openai-compatible"

// Separator joins a provider name and a model id in a qualified model id. A
// double colon is used because model ids themselves may contain slashes
// (for example "meta-llama/llama-3.3-70b" on gateways).
const Separator = "::"

var nameFormat = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// Provider is one endpoint.
type Provider struct {
	Name    string
	BaseURL string
	APIKey  string
	// UseResponsesAPI and EnablePromptCache default to the official OpenAI
	// behavior for the default provider and to plain chat completions for
	// everything else; AI_PROVIDER_OPTIONS can override per provider.
	UseResponsesAPI   bool
	EnablePromptCache bool
	// FallbackModels are tried in order when the requested model fails; they
	// belong to this provider only.
	FallbackModels []string
	// DefaultModel is used when a caller asks for this provider without a
	// model (only the default provider has one, from OPENAI_MODEL).
	DefaultModel string
}

// Registry is the immutable set of configured providers.
type Registry struct {
	providers map[string]*Provider
	embedding string
}

var (
	mu        sync.RWMutex
	cached    *Registry
	cachedSig string
)

// envVars are the variables the registry is built from; Current reloads
// when any of them changes, so tests that set them see a fresh registry.
var envVars = []string{"OPENAI_API_KEY", "OPENAI_API_BASE", "OPENAI_BASE", "OPENAI_MODEL", "OPENAI_USE_RESPONSES", "OPENAI_PROMPT_CACHE",
	"OPENAI_FALLBACK_MODELS", "AI_PROVIDERS", "AI_PROVIDER_KEYS", "AI_PROVIDER_OPTIONS", "AI_EMBEDDING_PROVIDER"}

func envSignature() string {
	parts := make([]string, 0, len(envVars))
	for _, name := range envVars {
		parts = append(parts, os.Getenv(name))
	}
	return strings.Join(parts, "\x00")
}

// Load parses the environment into a registry. It is safe to call from tests
// after t.Setenv; production callers use Current, which caches the result.
func Load() (*Registry, error) {
	r := &Registry{providers: map[string]*Provider{}}
	if key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); key != "" {
		base := strings.TrimSpace(os.Getenv("OPENAI_API_BASE"))
		if base == "" {
			base = strings.TrimSpace(os.Getenv("OPENAI_BASE"))
		}
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		official := openaiprovider.IsOfficialOpenAIBase(base)
		p := &Provider{Name: Default, BaseURL: base, APIKey: key, UseResponsesAPI: official, EnablePromptCache: official,
			DefaultModel: strings.TrimSpace(os.Getenv("OPENAI_MODEL"))}
		if v := strings.TrimSpace(os.Getenv("OPENAI_USE_RESPONSES")); v != "" {
			p.UseResponsesAPI = v == "1" || strings.EqualFold(v, "true")
		}
		if v := strings.TrimSpace(os.Getenv("OPENAI_PROMPT_CACHE")); v != "" {
			p.EnablePromptCache = v == "1" || strings.EqualFold(v, "true")
		}
		if v := strings.TrimSpace(os.Getenv("OPENAI_FALLBACK_MODELS")); v != "" {
			p.FallbackModels = splitList(v, ",")
		}
		r.providers[Default] = p
	}
	keys := parsePairs(os.Getenv("AI_PROVIDER_KEYS"))
	options := parsePairs(os.Getenv("AI_PROVIDER_OPTIONS"))
	for _, pair := range strings.Split(os.Getenv("AI_PROVIDERS"), ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, base, ok := strings.Cut(pair, "=")
		name, base = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(base)
		if !ok || !nameFormat.MatchString(name) {
			return nil, fmt.Errorf("AI_PROVIDERS: entry %q must be name=base_url with a lowercase name", pair)
		}
		if name == Default || name == "speechmatics" {
			return nil, fmt.Errorf("AI_PROVIDERS: provider name %q is reserved", name)
		}
		if _, dup := r.providers[name]; dup {
			return nil, fmt.Errorf("AI_PROVIDERS: provider %q listed twice", name)
		}
		if err := validBase(base); err != nil {
			return nil, fmt.Errorf("AI_PROVIDERS: provider %q: %w", name, err)
		}
		key := strings.TrimSpace(keys[name])
		if key == "" {
			return nil, fmt.Errorf("AI_PROVIDER_KEYS: provider %q has no key", name)
		}
		p := &Provider{Name: name, BaseURL: base, APIKey: key}
		for _, option := range splitList(options[name], ",") {
			switch strings.ToLower(option) {
			case "chat":
				p.UseResponsesAPI = false
			case "responses":
				p.UseResponsesAPI = true
			case "cache":
				p.EnablePromptCache = true
			default:
				return nil, fmt.Errorf("AI_PROVIDER_OPTIONS: provider %q: unknown option %q (chat, responses, cache)", name, option)
			}
		}
		r.providers[name] = p
	}
	r.embedding = strings.ToLower(strings.TrimSpace(os.Getenv("AI_EMBEDDING_PROVIDER")))
	if r.embedding == "" {
		r.embedding = Default
	}
	if len(r.providers) > 0 {
		if _, ok := r.providers[r.embedding]; !ok {
			return nil, fmt.Errorf("AI_EMBEDDING_PROVIDER: provider %q is not configured", r.embedding)
		}
	}
	return r, nil
}

// Current returns the process-wide registry, loading it on first use. A
// configuration error is returned every time so callers fail loudly rather
// than silently running single-provider.
func Current() (*Registry, error) {
	sig := envSignature()
	mu.RLock()
	r, ok := cached, cachedSig == sig
	mu.RUnlock()
	if r != nil && ok {
		return r, nil
	}
	loaded, err := Load()
	if err != nil {
		return nil, err
	}
	mu.Lock()
	cached, cachedSig = loaded, sig
	mu.Unlock()
	return loaded, nil
}

// Reset drops the cached registry; Current reloads on the next call. It is
// rarely needed because Current already notices changed variables.
func Reset() {
	mu.Lock()
	cached, cachedSig = nil, ""
	mu.Unlock()
}

// Configured reports whether at least one provider has a key.
func Configured() bool {
	r, err := Current()
	return err == nil && len(r.providers) > 0
}

// Names lists providers, default first.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		if name != Default {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if _, ok := r.providers[Default]; ok {
		names = append([]string{Default}, names...)
	}
	return names
}

// Get returns a provider by name.
func (r *Registry) Get(name string) (*Provider, bool) {
	p, ok := r.providers[strings.ToLower(strings.TrimSpace(name))]
	return p, ok
}

// EmbeddingProvider is the provider that serves embeddings.
func (r *Registry) EmbeddingProvider() (*Provider, error) {
	p, ok := r.providers[r.embedding]
	if !ok {
		return nil, errors.New("no embedding provider is configured")
	}
	return p, nil
}

// Split separates a qualified model id into provider and bare model id. Ids
// without a separator belong to the default provider.
func Split(qualified string) (provider, model string) {
	qualified = strings.TrimSpace(qualified)
	if name, rest, ok := strings.Cut(qualified, Separator); ok && nameFormat.MatchString(name) {
		return name, rest
	}
	return Default, qualified
}

// ProviderOf is the provider name a qualified id belongs to.
func ProviderOf(qualified string) string {
	name, _ := Split(qualified)
	return name
}

// Qualify builds the id stored and displayed for a provider's model.
func Qualify(provider, model string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" || provider == Default {
		return model
	}
	return provider + Separator + model
}

// ConfigFor builds the adapter configuration for a qualified model id. An
// empty id selects the default provider's default model.
func ConfigFor(qualified string) (*openaiprovider.Config, error) {
	r, err := Current()
	if err != nil {
		return nil, err
	}
	return r.ConfigFor(qualified)
}

// ConfigFor is ConfigFor on an explicit registry.
func (r *Registry) ConfigFor(qualified string) (*openaiprovider.Config, error) {
	name, model := Split(qualified)
	p, ok := r.providers[name]
	if !ok {
		if len(r.providers) == 0 {
			return nil, errors.New("OPENAI_API_KEY not set")
		}
		return nil, fmt.Errorf("model %q names provider %q, which is not configured", qualified, name)
	}
	if model == "" {
		model = p.DefaultModel
	}
	cfg, err := openaiprovider.NewConfigFromEnv()
	if err != nil && name == Default {
		return nil, err
	}
	if cfg == nil {
		cfg = &openaiprovider.Config{}
	}
	cfg.Provider = p.Name
	cfg.BaseURL = p.BaseURL
	cfg.APIKey = p.APIKey
	cfg.Model = model
	cfg.UseResponsesAPI = p.UseResponsesAPI
	cfg.EnablePromptCache = p.EnablePromptCache
	cfg.FallbackModels = append([]string(nil), p.FallbackModels...)
	return cfg, nil
}

func parsePairs(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok {
			out[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
		}
	}
	return out
}

func splitList(raw, sep string) []string {
	var out []string
	for _, item := range strings.Split(raw, sep) {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func validBase(base string) error {
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("base_url must be an http(s) URL without credentials, query or fragment")
	}
	return nil
}
