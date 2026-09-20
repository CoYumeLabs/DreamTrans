// Package config manages DreamTrans's persisted server configuration.
package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Config is the centralized server configuration. It provides backend defaults
// for prompts, models and algorithm thresholds. Client-side overrides may still
// exist per session, but server defaults come from here.
type Config struct {
	Prompts struct {
		Chat      string `json:"chat,omitempty"`
		Translate string `json:"translate,omitempty"`
		Summary   string `json:"summary,omitempty"`
	} `json:"prompts,omitempty"`
	Models struct {
		Translate string `json:"translate_model,omitempty"`
		Summary   string `json:"summary_model,omitempty"`
		Chat      string `json:"chat_model,omitempty"`
	} `json:"models,omitempty"`
	Translation struct {
		MinChunkChars      int     `json:"min_chunk_chars,omitempty"`
		FlushGapSeconds    float64 `json:"flush_gap_seconds,omitempty"`
		KeepLastTranslated int     `json:"keep_last_translated_segments,omitempty"`
	} `json:"translation,omitempty"`
	Summary struct {
		MinIntervalSeconds float64 `json:"min_interval_seconds,omitempty"`
		MinChars           int     `json:"min_chars,omitempty"`
		MaxBacklogChars    int     `json:"max_backlog_chars,omitempty"`
		MaxLines           int     `json:"max_lines,omitempty"`
		ParMinChars        int     `json:"par_min_chars,omitempty"`
	} `json:"summary,omitempty"`
}

var (
	current Config
	mu      sync.RWMutex
	path    string
)

// defaults used when file is missing or fields empty.
func setDefaults(c *Config) {
	if c.Prompts.Chat == "" {
		c.Prompts.Chat = "You are a helpful learning assistant. Answer in Chinese, structured and easy to skim. If context is insufficient, say you are unsure. Format rules: - Use short paragraphs and bullet points. - Start bullets with '- ' and put each on a new line. - Preserve line breaks for readability."
	}
	if c.Prompts.Translate == "" {
		c.Prompts.Translate = "您是一位专业的同声传译翻译，你正在把英文的口语内容翻译成中文易于理解的话，请使用 <context> 来帮助你理解上下文和当前场景并作出适当的纠错和润色。请仅翻译 <text>...</text> 里的文本变成中文，然后对中文进行润色，使其流畅、自然、易读，同时保留原文含义和语气。请尽量使用简洁、地道的措辞；根据需要合并不完整的句子；修改不合适的词序；删除填充词。请保持专业术语的准确性；保留数字/单位；并在适当的情况下将标点符号标准化为中文格式。请勿在输出中包含 <context> 中的任何内容。请勿添加解释、引述、说话者标签、时间戳或语言标签。仅返回最终润色后的中文句子，其他内容请勿返回。"
	}
	if c.Prompts.Summary == "" {
		c.Prompts.Summary = "You are a precise context compressor. Summarize the transcribed conversation text for downstream translation. Keep names, entities, topics, and unresolved references. Keep it concise and information-dense. Write the summary in the same language as the transcript."
	}
	if c.Translation.MinChunkChars == 0 {
		c.Translation.MinChunkChars = 16
	}
	if c.Translation.FlushGapSeconds == 0 {
		c.Translation.FlushGapSeconds = 0.9
	}
	if c.Translation.KeepLastTranslated == 0 {
		c.Translation.KeepLastTranslated = 3
	}
	if c.Summary.MinIntervalSeconds == 0 {
		c.Summary.MinIntervalSeconds = 45
	}
	if c.Summary.MinChars == 0 {
		c.Summary.MinChars = 800
	}
	if c.Summary.MaxBacklogChars == 0 {
		c.Summary.MaxBacklogChars = 1200
	}
	if c.Summary.MaxLines == 0 {
		c.Summary.MaxLines = 30
	}
	if c.Summary.ParMinChars == 0 {
		c.Summary.ParMinChars = 240
	}
	// Default models. A value matching a superseded shipped default is
	// upgraded in place, so deployments that never customized their config
	// follow new releases; operator-chosen models are left untouched.
	if c.Models.Translate == "" || legacyDefaultModels[c.Models.Translate] {
		c.Models.Translate = "gpt-5.6-luna"
	}
	if c.Models.Summary == "" || legacyDefaultModels[c.Models.Summary] {
		c.Models.Summary = "gpt-5.6-sol"
	}
	if c.Models.Chat == "" || legacyDefaultModels[c.Models.Chat] {
		c.Models.Chat = "gpt-5.6-sol"
	}
}

// Prior releases' shipped defaults, eligible for automatic upgrade.
var legacyDefaultModels = map[string]bool{
	"gpt-4.1-mini":      true,
	"gpt-5-chat-latest": true,
}

func Load() error {
	mu.Lock()
	defer mu.Unlock()
	// Decide path
	p := os.Getenv("DREAMTRANS_CONFIG_PATH")
	if p == "" {
		p = "data/dreamtrans.config.json"
	}
	path = p
	// Try read
	// DREAMTRANS_CONFIG_PATH is an operator-controlled startup setting.
	//nolint:gosec // G304: reading the configured local config file is intentional.
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// create parent dir and save defaults
			setDefaults(&current)
			// DREAMTRANS_CONFIG_PATH is an operator-controlled startup setting.
			//nolint:gosec // G703: creating its configured parent directory is intentional.
			if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
				return err
			}
			return saveLocked()
		}
		return err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	setDefaults(&cfg)
	current = cfg
	return nil
}

// Save persists current config. Callers that mutated via Get/Set must ensure locking.
func Save() error {
	mu.Lock()
	defer mu.Unlock()
	return saveLocked()
}

func saveLocked() error {
	if sharedDB != nil {
		return saveSharedLocked()
	}
	if path == "" {
		return errors.New("config path empty")
	}
	b, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	configDir := filepath.Dir(path)
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		return err
	}
	tempFile, err := os.CreateTemp(configDir, ".dreamtrans-config-*")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	defer func() {
		_ = tempFile.Close()
		_ = os.Remove(tempPath)
	}()
	if err := tempFile.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tempFile.Write(b); err != nil {
		return err
	}
	if err := tempFile.Sync(); err != nil {
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

// Get returns a copy of the current config for read-only usage.
func Get() Config {
	mu.Lock()
	defer mu.Unlock()
	if sharedDB != nil {
		_ = loadSharedLocked()
	}
	return current
}

// Update merges non-zero/non-empty fields from incoming cfg into current and saves.
func Update(partial *Config) error {
	mu.Lock()
	defer mu.Unlock()
	if partial == nil {
		return nil
	}
	if sharedDB != nil {
		return updateSharedLocked(partial)
	}
	mergeLocked(partial)
	return saveLocked()
}

func mergeLocked(partial *Config) {
	// prompts
	if partial.Prompts.Chat != "" {
		current.Prompts.Chat = partial.Prompts.Chat
	}
	if partial.Prompts.Translate != "" {
		current.Prompts.Translate = partial.Prompts.Translate
	}
	if partial.Prompts.Summary != "" {
		current.Prompts.Summary = partial.Prompts.Summary
	}
	// models
	if partial.Models.Translate != "" {
		current.Models.Translate = partial.Models.Translate
	}
	if partial.Models.Summary != "" {
		current.Models.Summary = partial.Models.Summary
	}
	if partial.Models.Chat != "" {
		current.Models.Chat = partial.Models.Chat
	}
	// translation thresholds
	if partial.Translation.MinChunkChars > 0 {
		current.Translation.MinChunkChars = partial.Translation.MinChunkChars
	}
	if partial.Translation.FlushGapSeconds > 0 {
		current.Translation.FlushGapSeconds = partial.Translation.FlushGapSeconds
	}
	if partial.Translation.KeepLastTranslated > 0 {
		current.Translation.KeepLastTranslated = partial.Translation.KeepLastTranslated
	}
	// summary thresholds
	if partial.Summary.MinIntervalSeconds > 0 {
		current.Summary.MinIntervalSeconds = partial.Summary.MinIntervalSeconds
	}
	if partial.Summary.MinChars > 0 {
		current.Summary.MinChars = partial.Summary.MinChars
	}
	if partial.Summary.MaxBacklogChars > 0 {
		current.Summary.MaxBacklogChars = partial.Summary.MaxBacklogChars
	}
	if partial.Summary.MaxLines > 0 {
		current.Summary.MaxLines = partial.Summary.MaxLines
	}
	if partial.Summary.ParMinChars > 0 {
		current.Summary.ParMinChars = partial.Summary.ParMinChars
	}
}
