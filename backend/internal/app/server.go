package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/CoYumeLabs/YuAction/backend/internal/storage"
)

type Config struct {
	Demo       bool
	CreatorKey string
	IngestKey  string
	TrustProxy bool
}
type Server struct {
	store  storage.Store
	cfg    Config
	hub    *hub
	mu     sync.Mutex
	limits map[string]limit
}
type limit struct {
	n     int
	until time.Time
}
type apiError struct {
	status  int
	message string
}

func (e apiError) Error() string            { return e.message }
func fail(status int, message string) error { return apiError{status, message} }

func New(store storage.Store, cfg Config) *Server {
	return &Server{store: store, cfg: cfg, hub: newHub(), limits: make(map[string]limit)}
}
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/health", s.health)
	m.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"demo": s.cfg.Demo, "creatorKeyRequired": s.cfg.CreatorKey != "", "yufoloConnected": false})
	})
	m.HandleFunc("POST /api/rooms", s.create)
	m.HandleFunc("GET /api/rooms/{code}", s.get)
	m.HandleFunc("GET /api/rooms/{code}/events", s.events)
	m.HandleFunc("GET /api/rooms/{code}/host", s.host)
	m.HandleFunc("POST /api/rooms/{code}/questions", s.question)
	m.HandleFunc("PATCH /api/rooms/{code}/questions/{id}", s.questionStatus)
	m.HandleFunc("PATCH /api/rooms/{code}", s.roomStatus)
	m.HandleFunc("POST /api/rooms/{code}/demo-segments", s.demoSegment)
	m.HandleFunc("POST /api/internal/rooms/{code}/segments", s.ingest)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		// No cross-origin API access. Browser traffic goes through the same-origin
		// Vite/production proxy; host and ingestion credentials never enter URLs.
		if r.Method != "GET" && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeError(w, fail(403, "不允许跨站操作"))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/events") && !strings.HasPrefix(r.URL.Path, "/api/internal/") {
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			if s.cfg.TrustProxy {
				if forwarded := net.ParseIP(r.Header.Get("X-Real-IP")); forwarded != nil {
					ip = forwarded.String()
				}
			}
			key := ip + ":read"
			max := 600
			if r.Method != "GET" {
				key = ip + ":write"
				max = 120
			}
			if r.URL.Path == "/api/rooms" && r.Method == "POST" {
				key = ip + ":create"
				max = 10
			}
			if !s.allow(key, max) {
				w.Header().Set("Retry-After", "60")
				writeError(w, fail(429, "操作较频繁，请稍后再试"))
				return
			}
		}
		m.ServeHTTP(w, r)
	})
}
func (s *Server) allow(key string, max int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if len(s.limits) > 10000 {
		for k, v := range s.limits {
			if now.After(v.until) {
				delete(s.limits, k)
			}
		}
		if len(s.limits) > 10000 {
			return false
		}
	}
	v := s.limits[key]
	if now.After(v.until) {
		v = limit{until: now.Add(time.Minute)}
	}
	v.n++
	s.limits[key] = v
	return v.n <= max
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, err error) {
	var ae apiError
	switch {
	case errors.As(err, &ae):
		respond(w, ae.status, map[string]string{"error": ae.message})
	case errors.Is(err, storage.ErrNotFound):
		respond(w, 404, map[string]string{"error": "找不到这个房间，请检查房间码"})
	case errors.Is(err, storage.ErrConflict):
		respond(w, 409, map[string]string{"error": "房间正在更新，请重试"})
	default:
		slog.Error("request failed", "error", err)
		respond(w, 500, map[string]string{"error": "服务暂时不可用，请稍后重试"})
	}
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return fail(415, "请使用 JSON 请求")
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fail(400, "请求内容不正确或过长")
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return fail(400, "请求只能包含一个 JSON 对象")
	}
	return nil
}
func token(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func digest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if !strings.HasPrefix(v, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(v, "Bearer ")
}
func equal(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func roomCode(r *http.Request) string { return strings.ToUpper(r.PathValue("code")) }
func validText(v string, max int) bool {
	return strings.TrimSpace(v) != "" && utf8.ValidString(v) && utf8.RuneCountInString(v) <= max
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeError(w, fail(503, "数据库暂时不可用"))
		return
	}
	respond(w, 200, map[string]string{"status": "ok"})
}
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CreatorKey != "" && !equal(bearer(r), s.cfg.CreatorKey) {
		writeError(w, fail(401, "请输入正确的活动创建密钥"))
		return
	}
	if s.cfg.CreatorKey == "" && !s.cfg.Demo {
		writeError(w, fail(503, "尚未配置活动创建权限"))
		return
	}
	var in struct {
		Title string `json:"title"`
		Kind  string `json:"kind"`
	}
	if err := decode(w, r, &in); err != nil {
		writeError(w, err)
		return
	}
	in.Title = strings.TrimSpace(in.Title)
	if !validText(in.Title, 100) || (in.Kind != "classroom" && in.Kind != "talk") {
		writeError(w, fail(400, "请输入活动名称，并选择课堂或演讲"))
		return
	}
	hostKey := token(32)
	for i := 0; i < 5; i++ {
		room := Room{Code: strings.ToUpper(token(4)), Title: in.Title, Kind: in.Kind, Status: "live", Revision: 1, CreatedAt: time.Now().UTC(), Questions: []Question{}, Segments: []Segment{}}
		data, _ := json.Marshal(room)
		err := s.store.Create(r.Context(), storage.Record{Code: room.Code, HostHash: digest(hostKey), Revision: 1, Data: data})
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		if err != nil {
			writeError(w, err)
			return
		}
		respond(w, 201, map[string]any{"room": room, "hostKey": hostKey})
		return
	}
	writeError(w, storage.ErrConflict)
}
func (s *Server) read(ctx context.Context, code string) (Room, storage.Record, error) {
	rec, err := s.store.Get(ctx, code)
	if err != nil {
		return Room{}, rec, err
	}
	var room Room
	err = json.Unmarshal(rec.Data, &room)
	return room, rec, err
}
func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	room, _, err := s.read(r.Context(), roomCode(r))
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, 200, room.Public())
}
func (s *Server) host(w http.ResponseWriter, r *http.Request) {
	room, rec, err := s.read(r.Context(), roomCode(r))
	if err != nil {
		writeError(w, err)
		return
	}
	if !equal(digest(bearer(r)), rec.HostHash) {
		writeError(w, fail(401, "需要此房间的主持人密钥"))
		return
	}
	respond(w, 200, room.Public())
}
func (s *Server) mutate(r *http.Request, host bool, change func(*Room) error) (Room, error) {
	for i := 0; i < 8; i++ {
		room, rec, err := s.read(r.Context(), roomCode(r))
		if err != nil {
			return Room{}, err
		}
		if host && !equal(digest(bearer(r)), rec.HostHash) {
			return Room{}, fail(401, "需要此房间的主持人密钥")
		}
		if err = change(&room); err != nil {
			return Room{}, err
		}
		old := rec.Revision
		room.Revision = old + 1
		rec.Revision = room.Revision
		rec.Data, err = json.Marshal(room)
		if err != nil {
			return Room{}, err
		}
		err = s.store.Save(r.Context(), old, rec)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		if err != nil {
			return Room{}, err
		}
		s.hub.publish(room.Code)
		return room.Public(), nil
	}
	return Room{}, storage.ErrConflict
}
func (s *Server) question(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content   string `json:"content"`
		SegmentID string `json:"segmentId"`
	}
	if err := decode(w, r, &in); err != nil {
		writeError(w, err)
		return
	}
	in.Content = strings.TrimSpace(in.Content)
	if !validText(in.Content, 1000) {
		writeError(w, fail(400, "问题需要 1–1000 个字符"))
		return
	}
	id := token(12)
	room, err := s.mutate(r, false, func(room *Room) error {
		if room.Status != "live" {
			return fail(409, "活动已结束，暂不接收新问题")
		}
		if len(room.Questions) >= 300 {
			return fail(409, "此预览版每个房间最多接收 300 个问题")
		}
		quotedText := ""
		if in.SegmentID != "" {
			found := false
			for _, seg := range room.Segments {
				if seg.ID == in.SegmentID {
					found = true
					quotedText = seg.Text
					break
				}
			}
			if !found {
				return fail(400, "引用的字幕不存在")
			}
		}
		room.Questions = append(room.Questions, Question{ID: id, Content: in.Content, Status: "pending", SegmentID: in.SegmentID, QuotedText: quotedText, CreatedAt: time.Now().UTC()})
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, 201, room)
}
func (s *Server) questionStatus(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Status string `json:"status"`
	}
	if err := decode(w, r, &in); err != nil {
		writeError(w, err)
		return
	}
	if in.Status != "pending" && in.Status != "showing" && in.Status != "answered" {
		writeError(w, fail(400, "问题状态不正确"))
		return
	}
	room, err := s.mutate(r, true, func(room *Room) error {
		if room.Status != "live" {
			return fail(409, "请先重新开启活动")
		}
		idx := -1
		for i, q := range room.Questions {
			if q.ID == r.PathValue("id") {
				idx = i
				break
			}
		}
		if idx < 0 {
			return storage.ErrNotFound
		}
		if in.Status == "showing" {
			for i := range room.Questions {
				if room.Questions[i].Status == "showing" {
					room.Questions[i].Status = "pending"
				}
			}
		}
		room.Questions[idx].Status = in.Status
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, 200, room)
}
func (s *Server) roomStatus(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Status string `json:"status"`
	}
	if err := decode(w, r, &in); err != nil {
		writeError(w, err)
		return
	}
	if in.Status != "live" && in.Status != "ended" {
		writeError(w, fail(400, "活动状态不正确"))
		return
	}
	room, err := s.mutate(r, true, func(room *Room) error { room.Status = in.Status; return nil })
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, 200, room)
}
func (s *Server) demoSegment(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Demo {
		writeError(w, fail(404, "演示字幕未启用"))
		return
	}
	s.addSegment(w, r, true, "demo")
}
func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	if !equal(bearer(r), s.cfg.IngestKey) {
		writeError(w, fail(401, "字幕接入凭证无效"))
		return
	}
	s.addSegment(w, r, false, "yufolo")
}
func (s *Server) addSegment(w http.ResponseWriter, r *http.Request, host bool, source string) {
	var in struct {
		ID          string `json:"id"`
		Text        string `json:"text"`
		Translation string `json:"translation"`
	}
	if err := decode(w, r, &in); err != nil {
		writeError(w, err)
		return
	}
	in.Text = strings.TrimSpace(in.Text)
	in.Translation = strings.TrimSpace(in.Translation)
	if !validText(in.ID, 100) || !validText(in.Text, 2000) || utf8.RuneCountInString(in.Translation) > 2000 {
		writeError(w, fail(400, "字幕 ID 或内容不正确"))
		return
	}
	room, err := s.mutate(r, host, func(room *Room) error {
		for _, seg := range room.Segments {
			if seg.ID == in.ID {
				if seg.Text != in.Text || seg.Translation != in.Translation || seg.Source != source {
					return fail(409, "字幕 ID 已存在且内容不同")
				}
				return nil
			}
		}
		if room.Status != "live" {
			return fail(409, "活动已结束，不接收新字幕")
		}
		if len(room.Segments) >= 5000 {
			return fail(409, "此预览版已达到字幕容量上限")
		}
		room.Segments = append(room.Segments, Segment{ID: in.ID, Text: in.Text, Translation: in.Translation, Source: source, CreatedAt: time.Now().UTC()})
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	respond(w, 200, room)
}
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	code := roomCode(r)
	if _, _, err := s.read(r.Context(), code); err != nil {
		writeError(w, err)
		return
	}
	ch, unsubscribe, ok := s.hub.subscribe(code)
	if !ok {
		writeError(w, fail(429, "此预览版房间连接已满"))
		return
	}
	defer unsubscribe()
	f, ok := w.(http.Flusher)
	if !ok {
		writeError(w, fail(500, "无法建立实时连接"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	controller := http.NewResponseController(w)
	last := int64(0)
	send := func() bool {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		room, _, err := s.read(ctx, code)
		if err != nil {
			return false
		}
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if room.Revision != last {
			data, err := json.Marshal(room.Public())
			if err != nil {
				return false
			}
			if _, err = fmt.Fprintf(w, "id: %d\nevent: room\ndata: %s\n\n", room.Revision, data); err != nil {
				return false
			}
			last = room.Revision
		} else {
			if _, err = fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return false
			}
		}
		f.Flush()
		return true
	}
	if !send() {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if !send() {
				return
			}
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}
