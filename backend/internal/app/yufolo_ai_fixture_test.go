package app

import (
	"encoding/json"
	"github.com/gorilla/websocket"
	"net/http"
	"strings"
	"time"
)

type fixtureAI struct {
	projects map[string]yufoloProject
	owners   map[string]string
	sources  map[string][]yufoloSource
	asks     []map[string]any
}

// Strict fixtures for the existing Yufolo workspace/RAG/atomic translator APIs.
func (f *fakeYufolo) handleAI(w http.ResponseWriter, r *http.Request, owner string) bool {
	path := r.URL.Path
	if path == "/ws/translate" {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return true
		}
		defer c.Close()
		var init struct {
			Type   string `json:"type"`
			Mode   string `json:"mode"`
			Config struct {
				Session string `json:"session_id"`
				Target  string `json:"target_language"`
			} `json:"config"`
		}
		if c.ReadJSON(&init) != nil {
			return true
		}
		f.mu.Lock()
		valid := f.sessions[init.Config.Session] == owner
		f.mu.Unlock()
		if !valid || init.Type != "init" || init.Mode != "ai_rolling" || captionLanguages[init.Config.Target] == "" {
			_ = c.WriteJSON(map[string]string{"message": "Error"})
			return true
		}
		_ = c.WriteJSON(map[string]any{"message": "Info", "reason": "translator initialized", "capabilities": map[string]bool{"request_ids": true, "atomic_transcripts": true}})
		var transcript struct {
			Type    string `json:"type"`
			Payload struct {
				ID   string `json:"request_id"`
				Text string `json:"transcript"`
			} `json:"payload"`
		}
		if c.ReadJSON(&transcript) != nil || transcript.Type != "transcript" || transcript.Payload.ID == "" || transcript.Payload.Text == "" {
			return true
		}
		f.translations.Add(1)
		text := "Hello everyone"
		if init.Config.Target == "cmn" {
			text = "你好，你能听到我吗？"
		}
		if init.Config.Target == "ja" {
			text = "皆さん、こんにちは"
		}
		_ = c.WriteJSON(map[string]any{"message": "AddTranslation", "results": []any{map[string]string{"request_id": transcript.Payload.ID, "content": text}}})
		return true
	}
	if path != "/api/system/access" && path != "/api/rag/ask" && !strings.HasPrefix(path, "/api/ai/") {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	if f.ai.projects == nil {
		f.ai.projects = map[string]yufoloProject{}
		f.ai.owners = map[string]string{}
		f.ai.sources = map[string][]yufoloSource{}
	}
	if path == "/api/system/access" {
		reply(map[string]bool{"rag_enabled": true, "rag_stateless_supported": true})
		return true
	}
	if path == "/api/rag/ask" {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		project, _ := in["project_id"].(string)
		if in["stateless"] != true || in["client_request_id"] == nil || (project != "" && f.ai.owners[project] != owner) {
			w.WriteHeader(403)
			return true
		}
		f.ai.asks = append(f.ai.asks, in)
		answer := "通用建议：先明确概念，再举一个例子。"
		sources := []any{}
		if project != "" {
			answer = "根据讲义，设计思维从理解用户开始。"
			for _, d := range f.ai.sources[project] {
				sources = append(sources, map[string]string{"id": d.ID, "label": d.Name, "kind": "document"})
			}
		}
		reply(map[string]any{"answer": answer, "context": map[string]any{"sources": sources, "retrieval_mode": "lexical"}})
		return true
	}
	if path == "/api/ai/projects" {
		if r.Method == "POST" {
			var p yufoloProject
			_ = json.NewDecoder(r.Body).Decode(&p)
			p.ID = uuidToken()
			f.ai.projects[p.ID] = p
			f.ai.owners[p.ID] = owner
			reply(map[string]any{"project": p})
		} else {
			ps := []yufoloProject{}
			for id, p := range f.ai.projects {
				if f.ai.owners[id] == owner {
					ps = append(ps, p)
				}
			}
			reply(map[string]any{"projects": ps})
		}
		return true
	}
	if strings.HasPrefix(path, "/api/ai/index/") {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		id, _ := in["target_id"].(string)
		if f.ai.owners[id] != owner {
			w.WriteHeader(403)
			return true
		}
		if strings.HasSuffix(path, "preview") {
			reply(map[string]any{"requires_indexing": true, "pending_chunks": 1, "estimated_dp": 0.02, "confirmation_token": "fixture-confirm"})
		} else {
			if in["confirmed"] != true || in["confirmation_token"] != "fixture-confirm" || in["client_request_id"] == nil {
				w.WriteHeader(400)
				return true
			}
			for i := range f.ai.sources[id] {
				f.ai.sources[id][i].IndexStatus = "ready"
			}
			reply(map[string]string{"status": "completed"})
		}
		return true
	}
	parts := strings.Split(path, "/")
	if len(parts) < 5 {
		w.WriteHeader(404)
		return true
	}
	id := parts[4]
	if f.ai.owners[id] != owner {
		w.WriteHeader(403)
		return true
	}
	if len(parts) == 5 {
		reply(map[string]any{"project": f.ai.projects[id]})
		return true
	}
	if parts[5] != "sources" {
		w.WriteHeader(404)
		return true
	}
	if len(parts) == 6 {
		if r.Method == "POST" {
			file, header, err := r.FormFile("file")
			if err != nil {
				w.WriteHeader(400)
				return true
			}
			file.Close()
			if r.MultipartForm != nil {
				defer r.MultipartForm.RemoveAll()
			}
			d := yufoloSource{ID: uuidToken(), Name: header.Filename, Status: "ready", ChunkCount: 1, IndexStatus: "pending", CreatedAt: time.Now().UTC()}
			f.ai.sources[id] = append(f.ai.sources[id], d)
			reply(map[string]any{"source": d})
		} else {
			if r.URL.Query().Get("metadata_only") != "true" {
				w.WriteHeader(400)
				return true
			}
			reply(map[string]any{"sources": f.ai.sources[id]})
		}
		return true
	}
	for i, d := range f.ai.sources[id] {
		if d.ID == parts[6] {
			if r.Method == "DELETE" {
				f.ai.sources[id] = append(f.ai.sources[id][:i], f.ai.sources[id][i+1:]...)
			}
			reply(map[string]bool{"success": true})
			return true
		}
	}
	w.WriteHeader(404)
	return true
}
