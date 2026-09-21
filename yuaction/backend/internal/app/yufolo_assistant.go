package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/CoYumeLabs/YuAction/backend/internal/storage"
)

type yufoloProject struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}
type yufoloSource struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Error       string    `json:"error_message"`
	ChunkCount  int       `json:"chunk_count"`
	IndexStatus string    `json:"index_status"`
	Model       string    `json:"embedding_model"`
	CreatedAt   time.Time `json:"created_at"`
}

func safeUpstreamID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
func (s *Server) validateYufoloProject(ctx context.Context, a *loginSession, id string) error {
	if !safeUpstreamID(id) {
		return fail(400, "知识库编号无效")
	}
	var response struct {
		Project yufoloProject `json:"project"`
	}
	return s.yufolo.request(ctx, a, "GET", "/api/ai/projects/"+id, nil, &response)
}
func (s *Server) assistantProjects(w http.ResponseWriter, r *http.Request) {
	if _, err := s.privateHost(r); err != nil {
		writeError(w, err)
		return
	}
	if s.yufolo == nil {
		respond(w, 200, map[string]any{"projects": []yufoloProject{}})
		return
	}
	var out struct {
		Projects []yufoloProject `json:"projects"`
	}
	if err := s.yufolo.request(r.Context(), currentAccount(r), "GET", "/api/ai/projects", nil, &out); err != nil {
		writeError(w, err)
		return
	}
	respond(w, 200, out)
}

// The marker lets a retry recover a project created before a lost response or
// process restart. It includes the room's private identity, not just its code.
func (s *Server) ensureYufoloProject(r *http.Request, room Room) (string, error) {
	s.aiMu.Lock()
	defer s.aiMu.Unlock()
	settings, err := s.assistantSettings(r.Context(), room.Code)
	if err != nil {
		return "", err
	}
	if settings.ProjectID != "" {
		return settings.ProjectID, nil
	}
	_, record, err := s.read(r.Context(), room.Code)
	if err != nil {
		return "", err
	}
	marker := "YuAction:" + digest(record.HostHash)[:32]
	var projects struct {
		Projects []yufoloProject `json:"projects"`
	}
	a := currentAccount(r)
	if err = s.yufolo.request(r.Context(), a, "GET", "/api/ai/projects", nil, &projects); err != nil {
		return "", err
	}
	for _, p := range projects.Projects {
		if p.Description == marker {
			settings.ProjectID = p.ID
			break
		}
	}
	if settings.ProjectID == "" {
		var out struct {
			Project yufoloProject `json:"project"`
		}
		if err = s.yufolo.request(r.Context(), a, "POST", "/api/ai/projects", map[string]any{"name": room.Title + " · YuAction", "description": marker, "context_mode": "retrieval", "max_context_tokens": 16000}, &out); err != nil {
			return "", err
		}
		if !safeUpstreamID(out.Project.ID) {
			return "", fail(502, "Yufolo 知识库响应无效")
		}
		settings.ProjectID = out.Project.ID
	}
	rec, err := s.store.GetPrivate(r.Context(), room.Code, "settings", "default")
	if errors.Is(err, storage.ErrNotFound) {
		rec = storage.PrivateRecord{Code: room.Code, Kind: "settings", ID: "default"}
	} else if err != nil {
		return "", err
	}
	if err = s.savePrivate(r.Context(), rec, settings); err != nil {
		return "", err
	}
	return settings.ProjectID, nil
}
func (s *Server) yufoloSources(ctx context.Context, a *loginSession, project string) ([]knowledgeDocument, error) {
	docs := []knowledgeDocument{}
	if project == "" {
		return docs, nil
	}
	if !safeUpstreamID(project) {
		return nil, fail(400, "知识库编号无效")
	}
	var out struct {
		Sources []yufoloSource `json:"sources"`
	}
	if err := s.yufolo.request(ctx, a, "GET", "/api/ai/projects/"+project+"/sources?metadata_only=true", nil, &out); err != nil {
		return nil, err
	}
	for _, d := range out.Sources {
		docs = append(docs, knowledgeDocument{ID: d.ID, Name: d.Name, Status: d.Status, Error: d.Error, CreatedAt: d.CreatedAt, ChunkCount: d.ChunkCount, Model: d.Model, IndexStatus: d.IndexStatus})
	}
	return docs, nil
}
func (s *Server) yufoloAssistantInfo(w http.ResponseWriter, r *http.Request, room Room, settings assistantSettings) {
	var access struct {
		RAG       bool `json:"rag_enabled"`
		Stateless bool `json:"rag_stateless_supported"`
	}
	err := s.yufolo.request(r.Context(), currentAccount(r), "GET", "/api/system/access", nil, &access)
	if err != nil {
		writeError(w, err)
		return
	}
	docs, err := s.yufoloSources(r.Context(), currentAccount(r), settings.ProjectID)
	if err != nil {
		writeError(w, err)
		return
	}
	s.aiMu.Lock()
	defer s.aiMu.Unlock()
	rows, err := s.store.ListPrivate(r.Context(), room.Code, "answer")
	if err != nil {
		writeError(w, err)
		return
	}
	answers := []answerDraft{}
	known := map[string]bool{}
	for _, q := range room.Questions {
		known[q.ID] = true
	}
	for _, rec := range rows {
		var a answerDraft
		if err = json.Unmarshal(rec.Data, &a); err != nil {
			writeError(w, err)
			return
		}
		if !known[a.QuestionID] {
			continue
		}
		if !s.jobActive(jobKey(room.Code, "answer", a.QuestionID)) {
			for _, part := range []*draftPart{&a.Generic, &a.Knowledge} {
				if part.Status == "processing" {
					part.Status = "interrupted"
					part.Error = "生成中断，请重新登录后重试"
				}
			}
		}
		answers = append(answers, a)
	}
	message := ""
	if !access.RAG {
		message = "Yufolo 尚未启用 AI 服务"
	} else if !access.Stateless {
		message = "请更新 Yufolo，以启用独立问答并避免不同问题串入历史回答"
	}
	respond(w, 200, map[string]any{"provider": "yufolo", "configured": access.RAG && access.Stateless, "embeddingConfigured": access.RAG, "message": message, "settings": settings, "documents": docs, "answers": answers})
}
func (s *Server) uploadYufoloDocument(w http.ResponseWriter, r *http.Request, room Room) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+(1<<20))
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		writeError(w, fail(400, "请选择不超过 10 MB 的资料"))
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, fail(400, "请选择资料文件"))
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxUpload+1))
	if err != nil || len(data) > maxUpload {
		writeError(w, fail(400, "单个资料最多 10 MB"))
		return
	}
	project, err := s.ensureYufoloProject(r, room)
	if err != nil {
		writeError(w, err)
		return
	}
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", header.Filename)
	if err != nil {
		writeError(w, fail(400, "文件名无效"))
		return
	}
	_, _ = part.Write(data)
	_ = writer.Close()
	var out struct {
		Source yufoloSource `json:"source"`
	}
	err = s.yufolo.request(r.Context(), currentAccount(r), "POST", "/api/ai/projects/"+project+"/sources", upstreamBody{Data: buf.Bytes(), ContentType: writer.FormDataContentType()}, &out)
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, 202, map[string]any{"id": out.Source.ID, "status": out.Source.Status})
}
func (s *Server) yufoloDocumentAction(w http.ResponseWriter, r *http.Request, action string) {
	room, err := s.privateHost(r)
	if err != nil {
		writeError(w, err)
		return
	}
	settings, err := s.assistantSettings(r.Context(), room.Code)
	if err != nil {
		writeError(w, err)
		return
	}
	id := r.PathValue("id")
	if !safeUpstreamID(settings.ProjectID) || !safeUpstreamID(id) {
		writeError(w, fail(400, "资料编号无效"))
		return
	}
	method, path := "DELETE", "/api/ai/projects/"+settings.ProjectID+"/sources/"+id
	if action == "retry" {
		method = "POST"
		path += "/retry"
	}
	if action == "read" {
		writeError(w, fail(400, "请在 Yufolo 知识库中查看原始资料"))
		return
	}
	if err = s.yufolo.request(r.Context(), currentAccount(r), method, path, nil, nil); err != nil {
		writeError(w, err)
		return
	}
	if action == "delete" {
		s.aiMu.Lock()
		e := s.invalidateKnowledgeLocked(r.Context(), room.Code)
		s.aiMu.Unlock()
		if e != nil {
			writeError(w, e)
			return
		}
	}
	respond(w, 200, map[string]bool{"success": true})
}
func (s *Server) assistantIndexPreview(w http.ResponseWriter, r *http.Request) {
	room, err := s.privateHost(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.yufolo == nil {
		writeError(w, fail(400, "独立模式会在上传后自动建立索引"))
		return
	}
	settings, err := s.assistantSettings(r.Context(), room.Code)
	if err != nil {
		writeError(w, err)
		return
	}
	if !safeUpstreamID(settings.ProjectID) {
		writeError(w, fail(400, "请先上传资料或关联知识库"))
		return
	}
	var out map[string]any
	err = s.yufolo.request(r.Context(), currentAccount(r), "POST", "/api/ai/index/preview", map[string]string{"target_type": "project", "target_id": settings.ProjectID}, &out)
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, 200, out)
}
func (s *Server) assistantIndex(w http.ResponseWriter, r *http.Request) {
	room, err := s.privateHost(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.yufolo == nil {
		writeError(w, fail(400, "此操作需要 Yufolo"))
		return
	}
	var in struct {
		Token     string `json:"confirmationToken"`
		RequestID string `json:"requestId"`
		Confirmed bool   `json:"confirmed"`
	}
	if err = decode(w, r, &in); err != nil {
		writeError(w, err)
		return
	}
	if !in.Confirmed || len(in.Token) > 4096 || len(in.RequestID) > 128 || in.RequestID == "" {
		writeError(w, fail(400, "请确认索引费用后继续"))
		return
	}
	settings, err := s.assistantSettings(r.Context(), room.Code)
	if err != nil {
		writeError(w, err)
		return
	}
	if !safeUpstreamID(settings.ProjectID) {
		writeError(w, fail(400, "请先关联知识库"))
		return
	}
	var out map[string]any
	err = s.yufolo.request(r.Context(), currentAccount(r), "POST", "/api/ai/index/jobs", map[string]any{"target_type": "project", "target_id": settings.ProjectID, "confirmation_token": in.Token, "confirmed": true, "client_request_id": in.RequestID}, &out)
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, 202, out)
}
func (s *Server) startYufoloAnswer(code, id string) error {
	s.aiMu.Lock()
	defer s.aiMu.Unlock()
	a := s.hostSession(context.Background(), code)
	if a == nil {
		a = s.aiHosts[code]
	}
	if a == nil {
		return fail(401, "主持人需要先登录 Yufolo 并打开工作台")
	}
	key := jobKey(code, "answer", id)
	releaseJob, claimErr := s.claimJob(key)
	if claimErr != nil {
		return claimErr
	}
	launched := false
	defer func() {
		if !launched {
			releaseJob()
		}
	}()
	if s.aiJobs[key] != nil {
		return fail(409, "此问题正在生成")
	}
	if len(s.aiJobs) >= 4 {
		return fail(429, "AI 正忙，请稍后重试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	room, link, err := s.read(ctx, code)
	if err != nil {
		cancel()
		return err
	}
	if link.Link.OwnerID != a.user.ID {
		cancel()
		return fail(403, "活动所属账号已变化，请重新登录")
	}
	var q *Question
	for i := range room.Questions {
		if room.Questions[i].ID == id {
			q = &room.Questions[i]
			break
		}
	}
	if q == nil {
		cancel()
		return storage.ErrNotFound
	}
	settings, err := s.assistantSettings(ctx, code)
	if err != nil {
		cancel()
		return err
	}
	var access struct {
		Stateless bool `json:"rag_stateless_supported"`
	}
	if err = s.yufolo.request(ctx, a, "GET", "/api/system/access", nil, &access); err != nil {
		cancel()
		return err
	}
	if !access.Stateless {
		cancel()
		return fail(503, "请先更新 Yufolo，以支持独立问答")
	}
	rec, err := s.store.GetPrivate(ctx, code, "answer", id)
	if errors.Is(err, storage.ErrNotFound) {
		rec = storage.PrivateRecord{Code: code, Kind: "answer", ID: id}
	} else if err != nil {
		cancel()
		return err
	}
	draft := answerDraft{QuestionID: id, Generic: draftPart{Status: "processing"}, Knowledge: draftPart{Status: "processing"}, UpdatedAt: time.Now().UTC()}
	if err = s.savePrivate(ctx, rec, draft); err != nil {
		cancel()
		return err
	}
	rec.Revision++
	s.aiJobs[key] = cancel
	question := q.Content
	if q.QuotedText != "" {
		question += "\n\n引用的字幕：\n" + q.QuotedText
	}
	requestID := token(16)
	launched = true
	go func() {
		defer releaseJob()
		defer cancel()
		generic := make(chan draftPart, 1)
		go func() {
			generic <- s.yufoloAnswer(ctx, a, question, settings.GenericPrompt, "", "", settings.TopK, "yuaction-"+requestID+"-generic")
		}()
		if settings.ProjectID == "" {
			draft.Knowledge = draftPart{Status: "empty", Error: "上传资料或关联 Yufolo 知识库后，可生成资料回答"}
		} else {
			draft.Knowledge = s.yufoloAnswer(ctx, a, question, strings.ReplaceAll(settings.KBPrompt, "%s", "（使用本次检索附带的资料）"), settings.ProjectID, link.Link.SessionID, settings.TopK, "yuaction-"+requestID+"-knowledge")
		}
		draft.Generic = <-generic
		draft.UpdatedAt = time.Now().UTC()
		s.aiMu.Lock()
		defer s.aiMu.Unlock()
		defer delete(s.aiJobs, key)
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = s.savePrivate(cleanup, rec, draft)
	}()
	return nil
}
func (s *Server) yufoloAnswer(ctx context.Context, a *loginSession, question, prompt, project, session string, topK int, requestID string) draftPart {
	var response struct {
		Answer  string `json:"answer"`
		Context struct {
			Sources []struct {
				ID    string `json:"id"`
				Label string `json:"label"`
				Kind  string `json:"kind"`
			} `json:"sources"`
			RetrievalMode string `json:"retrieval_mode"`
		} `json:"context"`
	}
	body := map[string]any{"question": question, "stateless": true, "client_request_id": requestID, "config": map[string]string{"prompt": prompt}, "context_policy": map[string]any{"mode": "retrieval", "max_tokens": 16000}, "top_k": topK}
	if project != "" {
		body["project_id"] = project
	}
	if session != "" {
		body["session_id"] = session
	}
	err := s.yufolo.request(ctx, a, "POST", "/api/rag/ask", body, &response)
	if err != nil {
		return draftPart{Status: "failed", Error: err.Error()}
	}
	if strings.TrimSpace(response.Answer) == "" {
		return draftPart{Status: "failed", Error: "Yufolo 未返回回答内容"}
	}
	part := draftPart{Status: "ready", Text: response.Answer}
	for _, source := range response.Context.Sources {
		part.Sources = append(part.Sources, answerSource{DocumentID: source.ID, Name: source.Label})
	}
	return part
}

// Caller holds aiMu so old workers cannot overwrite the invalidation.
func (s *Server) invalidateKnowledgeLocked(ctx context.Context, code string) error {
	rows, err := s.store.ListPrivate(ctx, code, "answer")
	if err != nil {
		return err
	}
	for _, rec := range rows {
		var a answerDraft
		if err = json.Unmarshal(rec.Data, &a); err != nil {
			return err
		}
		a.Knowledge = draftPart{Status: "outdated", Error: "资料已变更，请重新生成"}
		if a.Generic.Status == "processing" {
			a.Generic = draftPart{Status: "interrupted", Error: "资料已变更，请重试"}
		}
		if cancel := s.aiJobs[jobKey(code, "answer", a.QuestionID)]; cancel != nil {
			cancel()
		}
		if err = s.savePrivate(ctx, rec, a); err != nil {
			return err
		}
	}
	return nil
}
