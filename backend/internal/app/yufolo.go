package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/CoYumeLabs/YuAction/backend/internal/storage"
)

type yufoloClient struct {
	base string
	http *http.Client
}

func newYufoloClient(base string) *yufoloClient {
	return &yufoloClient{strings.TrimRight(base, "/"), &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type upstreamBody struct {
	Data        []byte
	ContentType string
}

type upstreamError struct {
	status  int
	message string
}

func (e upstreamError) Error() string { return e.message }
func (c *yufoloClient) call(ctx context.Context, method, path, access string, body, out any) error {
	var payload []byte
	var err error
	contentType := "application/json"
	if raw, ok := body.(upstreamBody); ok {
		payload, contentType = raw.Data, raw.ContentType
	} else if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	client := *c.http
	if strings.HasPrefix(path, "/api/rag/") {
		client.Timeout = 150 * time.Second
	}
	res, err := client.Do(req)
	if err != nil {
		return fail(502, "暂时无法连接 Yufolo，请稍后重试")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return fail(502, "Yufolo 响应读取失败")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var msg struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &msg)
		if msg.Error == "" || len(msg.Error) > 500 {
			msg.Error = "服务暂时不可用"
		}
		return upstreamError{res.StatusCode, msg.Error}
	}
	if out != nil {
		if err = json.Unmarshal(data, out); err != nil {
			return fail(502, "Yufolo 响应格式不正确")
		}
	}
	return nil
}
func upstreamFailure(err error) error {
	var e upstreamError
	if !errors.As(err, &e) {
		return err
	}
	status := e.status
	if status != 401 && status != 403 && status != 402 && status != 409 && status != 429 {
		status = 502
	}
	return fail(status, "Yufolo："+e.message)
}

type accountUser struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}
type authResponse struct {
	User      accountUser `json:"user"`
	Access    string      `json:"access_token"`
	Refresh   string      `json:"refresh_token"`
	ExpiresIn int         `json:"expires_in"`
}
type loginSession struct {
	mu                sync.Mutex
	user              accountUser
	access, refresh   string
	expires, deadline time.Time
	revoked           bool
}
type accountContextKey struct{}

func currentAccount(r *http.Request) *loginSession {
	a, _ := r.Context().Value(accountContextKey{}).(*loginSession)
	return a
}

const authCookie = "yuaction_session"

func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Host, r.Host)
}
func needsAccount(r *http.Request) bool {
	p := r.URL.Path
	return p == "/api/auth/me" || p == "/api/my/rooms" || (p == "/api/rooms" && r.Method == "POST") || strings.Contains(p, "/assistant") || strings.HasSuffix(p, "/host") || strings.HasSuffix(p, "/transcription") || strings.HasSuffix(p, "/audio") || r.Method == "PATCH" || r.Method == "DELETE"
}
func (s *Server) hostAllowed(r *http.Request, rec storage.Record) bool {
	if rec.Link.OwnerID != "" {
		a := currentAccount(r)
		return a != nil && a.user.ID == rec.Link.OwnerID
	}
	return equal(digest(bearer(r)), rec.HostHash)
}
func (s *Server) sessionFor(r *http.Request) *loginSession {
	cookie, err := r.Cookie(authCookie)
	if err != nil {
		return nil
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return s.sessions[digest(cookie.Value)]
}

// Refresh is serialized, but slow AI calls must not hold the login mutex and
// block transcript archives, profile checks or logout for the same account.
func (c *yufoloClient) accessToken(ctx context.Context, a *loginSession, rejected string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.revoked || time.Now().After(a.deadline) {
		return "", fail(401, "请重新登录 Yufolo")
	}
	if time.Until(a.expires) < 30*time.Second || (rejected != "" && rejected == a.access) {
		var next authResponse
		if err := c.call(ctx, "POST", "/api/auth/refresh", "", map[string]string{"refresh_token": a.refresh}, &next); err != nil {
			return "", upstreamFailure(err)
		}
		if next.User.ID != a.user.ID || next.Access == "" || next.Refresh == "" {
			return "", fail(502, "Yufolo 登录响应不正确")
		}
		a.access, a.refresh = next.Access, next.Refresh
		a.expires = time.Now().Add(time.Duration(next.ExpiresIn) * time.Second)
	}
	return a.access, nil
}
func (c *yufoloClient) request(ctx context.Context, a *loginSession, method, path string, body, out any) error {
	access, err := c.accessToken(ctx, a, "")
	if err != nil {
		return err
	}
	err = c.call(ctx, method, path, access, body, out)
	var e upstreamError
	if errors.As(err, &e) && e.status == 401 {
		access, err = c.accessToken(ctx, a, access)
		if err != nil {
			return err
		}
		err = c.call(ctx, method, path, access, body, out)
	}
	return upstreamFailure(err)
}
func (s *Server) account(r *http.Request) (*loginSession, error) {
	a := s.sessionFor(r)
	if a == nil {
		return nil, fail(401, "请先登录 Yufolo 账号")
	}
	var profile struct {
		User accountUser `json:"user"`
	}
	if err := s.yufolo.request(r.Context(), a, "GET", "/api/user/profile", nil, &profile); err != nil {
		return nil, err
	}
	if profile.User.ID != a.user.ID {
		return nil, fail(401, "账号状态已变化，请重新登录")
	}
	return a, nil
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.yufolo == nil {
		writeError(w, fail(503, "尚未配置 Yufolo 连接"))
		return
	}
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &in); err != nil {
		writeError(w, err)
		return
	}
	if !validText(in.Email, 254) || len(in.Password) == 0 || len(in.Password) > 1024 {
		writeError(w, fail(400, "请输入邮箱和密码"))
		return
	}
	var result authResponse
	if err := s.yufolo.call(r.Context(), "POST", "/api/auth/login", "", in, &result); err != nil {
		writeError(w, upstreamFailure(err))
		return
	}
	if result.User.ID == "" || result.Access == "" || result.Refresh == "" {
		writeError(w, fail(502, "Yufolo 登录响应不完整"))
		return
	}
	a := &loginSession{user: result.User, access: result.Access, refresh: result.Refresh, expires: time.Now().Add(time.Duration(result.ExpiresIn) * time.Second), deadline: time.Now().Add(7 * 24 * time.Hour)}
	key := token(32)
	s.authMu.Lock()
	for k, v := range s.sessions {
		if time.Now().After(v.deadline) {
			delete(s.sessions, k)
		}
	}
	if len(s.sessions) >= 4096 {
		s.authMu.Unlock()
		writeError(w, fail(503, "登录连接已满，请稍后重试"))
		return
	}
	s.sessions[digest(key)] = a
	s.authMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: authCookie, Value: key, Path: "/api", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil || (s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https"), MaxAge: 7 * 24 * 3600})
	respond(w, 200, result.User)
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	a := s.sessionFor(r)
	if a != nil {
		s.streamMu.Lock()
		for _, v := range s.streams {
			if v.auth == a {
				v.cancel()
			}
		}
		s.streamMu.Unlock()
		a.mu.Lock()
		a.revoked = true
		if s.yufolo != nil {
			_ = s.yufolo.call(r.Context(), "POST", "/api/auth/logout", a.access, map[string]string{"refresh_token": a.refresh}, nil)
		}
		a.mu.Unlock()
	}
	if cookie, err := r.Cookie(authCookie); err == nil {
		s.authMu.Lock()
		delete(s.sessions, digest(cookie.Value))
		s.authMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: authCookie, Value: "", Path: "/api", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	if a := currentAccount(r); a != nil {
		respond(w, 200, a.user)
	} else {
		writeError(w, fail(401, "请先登录 Yufolo 账号"))
	}
}
func (s *Server) myRooms(w http.ResponseWriter, r *http.Request) {
	a := currentAccount(r)
	if a == nil {
		writeError(w, fail(401, "请先登录 Yufolo 账号"))
		return
	}
	records, err := s.store.ListOwned(r.Context(), a.user.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	rooms := []Room{}
	for _, rec := range records {
		var room Room
		if err = json.Unmarshal(rec.Data, &room); err != nil {
			writeError(w, err)
			return
		}
		room.Questions = nil
		room.Segments = nil
		rooms = append(rooms, room)
	}
	respond(w, 200, rooms)
}
