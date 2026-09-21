package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type AIConfig struct {
	Key, ChatURL, Model, EmbeddingKey, EmbeddingURL, EmbeddingModel string
	GenericPrompt, KBPrompt                                         string
}

// Keep AnyQA's existing env names, including its historical API_KEYs spelling.
// Embeddings use the same API base by default, not a hard-coded second provider.
func AIConfigFromEnv() AIConfig {
	first := func(names ...string) string {
		for _, name := range names {
			if v := strings.TrimSpace(os.Getenv(name)); v != "" {
				return v
			}
		}
		return ""
	}
	base := strings.TrimRight(first("OPENAI_API_BASE"), "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	chat := first("OPENAI_API_URL")
	if chat == "" {
		chat = base + "/chat/completions"
	} else if strings.HasSuffix(chat, "/chat/completions") {
		base = strings.TrimSuffix(chat, "/chat/completions")
	}
	embed := first("OPENAI_EMBEDDING_API_URL")
	if embed == "" {
		embed = base + "/embeddings"
	}
	model := first("OPENAI_EMBEDDING_MODEL")
	if model == "" {
		model = "text-embedding-3-small"
	}
	key := first("OPENAI_API_KEY", "OPENAI_API_KEYs")
	embeddingKey := first("OPENAI_EMBEDDING_API_KEY")
	if embeddingKey == "" {
		embeddingKey = key
	}
	return AIConfig{Key: key, ChatURL: chat, Model: first("OPENAI_MODEL"), EmbeddingKey: embeddingKey, EmbeddingURL: embed, EmbeddingModel: model, GenericPrompt: first("GENERIC_SYSTEM_PROMPT"), KBPrompt: first("KB_SYSTEM_PROMPT")}
}

func validAIURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func (c AIConfig) chatReady() bool { return c.Key != "" && c.Model != "" && validAIURL(c.ChatURL) }
func (c AIConfig) embeddingReady() bool {
	return c.EmbeddingKey != "" && c.EmbeddingModel != "" && validAIURL(c.EmbeddingURL)
}
func (c AIConfig) embeddingVersion() string { return digest(c.EmbeddingURL + "\n" + c.EmbeddingModel) }

func aiRequest(ctx context.Context, endpoint, key string, input, output any) error {
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("AI 接口地址无效")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("AI 服务连接失败或超时，请稍后重试")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("AI 服务返回 HTTP %d，请检查接口、模型、密钥和额度", res.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(res.Body, (8<<20)+1))
	if err != nil || len(data) > 8<<20 {
		return fmt.Errorf("AI 返回内容过大或不完整")
	}
	if json.Unmarshal(data, output) != nil {
		return fmt.Errorf("AI 服务返回了无法解析的内容")
	}
	return nil
}
func (c AIConfig) chat(ctx context.Context, prompt, question string) (string, error) {
	if !c.chatReady() {
		return "", fmt.Errorf("尚未配置 AI 聊天接口、密钥和模型")
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	err := aiRequest(ctx, c.ChatURL, c.Key, map[string]any{"model": c.Model, "messages": []map[string]string{{"role": "system", "content": prompt}, {"role": "user", "content": question}}}, &response)
	if err != nil {
		return "", err
	}
	if len(response.Choices) == 0 || strings.TrimSpace(response.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("AI 未返回回答内容")
	}
	answer := response.Choices[0].Message.Content
	if len(answer) > 100000 {
		return "", fmt.Errorf("AI 回答过长，请调整提示词后重试")
	}
	return answer, nil
}
func (c AIConfig) embed(ctx context.Context, texts []string) ([][]float32, error) {
	if !c.embeddingReady() {
		return nil, fmt.Errorf("尚未配置知识库向量模型")
	}
	result := make([][]float32, 0, len(texts))
	dimensions := 0
	for start := 0; start < len(texts); start += 16 {
		batch := texts[start:min(start+16, len(texts))]
		var response struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := aiRequest(ctx, c.EmbeddingURL, c.EmbeddingKey, map[string]any{"model": c.EmbeddingModel, "input": batch, "encoding_format": "float"}, &response); err != nil {
			return nil, err
		}
		if len(response.Data) != len(batch) {
			return nil, fmt.Errorf("向量数量与文本数量不一致")
		}
		vectors := make([][]float32, len(batch))
		for _, v := range response.Data {
			if v.Index < 0 || v.Index >= len(batch) || vectors[v.Index] != nil || len(v.Embedding) == 0 || len(v.Embedding) > 8192 {
				return nil, fmt.Errorf("向量服务返回了无效索引或维度")
			}
			if dimensions == 0 {
				dimensions = len(v.Embedding)
			}
			if len(v.Embedding) != dimensions {
				return nil, fmt.Errorf("向量维度不一致")
			}
			if _, ok := cosine(v.Embedding, v.Embedding); !ok {
				return nil, fmt.Errorf("向量服务返回了无效向量")
			}
			vectors[v.Index] = v.Embedding
		}
		result = append(result, vectors...)
	}
	return result, nil
}
func cosine(a, b []float32) (float64, bool) {
	if len(a) == 0 || len(a) != len(b) {
		return 0, false
	}
	var dot, aa, bb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		aa += x * x
		bb += y * y
	}
	if aa == 0 || bb == 0 || math.IsNaN(dot) || math.IsInf(dot, 0) {
		return 0, false
	}
	return dot / math.Sqrt(aa*bb), true
}
