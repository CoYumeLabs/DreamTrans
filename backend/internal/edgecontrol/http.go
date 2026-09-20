package edgecontrol

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/edgeprotocol"
)

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, edgeprotocol.MaxEventBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return false
	}
	return true
}
func respond(w http.ResponseWriter, value any, err error) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		code := 503
		message := "edge service unavailable"
		switch {
		case errors.Is(err, ErrUnauthorized):
			code = 401
			message = "identity rejected"
		case errors.Is(err, ErrConflict):
			code = 409
			message = "stale generation or conflicting event"
		case errors.Is(err, billing.ErrInsufficientBalance):
			code = 402
			message = "insufficient balance"
		case errors.Is(err, ErrUnavailable):
			code = 409
			message = "no healthy node capacity"
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Service) UserHTTP(w http.ResponseWriter, r *http.Request) {
	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		respond(w, nil, ErrUnauthorized)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/edges" {
		nodes, err := s.Nodes(r.Context())
		if err != nil {
			respond(w, nil, err)
			return
		}
		visible := make([]Node, 0)
		for i := range nodes {
			n := &nodes[i]
			if n.Mode == "enabled" {
				visible = append(visible, *n)
			}
		}
		respond(w, visible, nil)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/api/edges/authorize" {
		var req AuthorizeRequest
		if !decode(w, r, &req) {
			return
		}
		req.Origin = r.Header.Get("Origin")
		result, err := s.Authorize(r.Context(), claims.UserID, claims.TenantID, req)
		respond(w, result, err)
		return
	}
	http.NotFound(w, r)
}
func (s *Service) AdminHTTP(w http.ResponseWriter, r *http.Request) {
	claims := auth.GetUserClaims(r.Context())
	if claims == nil || claims.Role != "super_admin" {
		respond(w, nil, ErrUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/edges")
	if strings.HasSuffix(path, "/archive-owner") && r.Method == http.MethodPost {
		var req struct {
			SessionID  string `json:"session_id"`
			Generation int64  `json:"generation"`
			Reason     string `json:"reason"`
		}
		if !decode(w, r, &req) {
			return
		}
		node := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/archive-owner")
		err := s.AttestArchiveOwner(r.Context(), claims.UserID, node, req.SessionID, req.Generation, req.Reason)
		respond(w, map[string]bool{"ok": err == nil}, err)
		return
	}
	if strings.HasSuffix(path, "/tunnel") && r.Method == http.MethodPost {
		var req struct {
			Automatic bool   `json:"automatic"`
			Token     string `json:"token"`
		}
		if !decode(w, r, &req) {
			return
		}
		node := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/tunnel")
		err := s.SetupTunnel(r.Context(), claims.UserID, node, req.Token, req.Automatic)
		respond(w, map[string]bool{"ok": err == nil}, err)
		return
	}
	if path == "/installer" && r.Method == http.MethodGet {
		info, err := s.installerInfo()
		respond(w, info, err)
		return
	}
	if path == "" && r.Method == http.MethodGet {
		nodes, err := s.Nodes(r.Context())
		respond(w, nodes, err)
		return
	}
	if path == "" && r.Method == http.MethodPost {
		var n Node
		if !decode(w, r, &n) {
			return
		}
		id, token, err := s.CreateNode(r.Context(), claims.UserID, &n)
		respond(w, map[string]string{"id": id, "registration_token": token, "expires_in": "900"}, err)
		return
	}
	if strings.HasPrefix(path, "/") && r.Method == http.MethodPost {
		var request struct {
			Mode   string `json:"mode"`
			Image  string `json:"image,omitempty"`
			Rotate bool   `json:"rotate,omitempty"`
		}
		if !decode(w, r, &request) {
			return
		}
		node := strings.TrimPrefix(path, "/")
		if request.Rotate {
			token, err := s.Rotate(r.Context(), claims.UserID, node)
			respond(w, map[string]string{"registration_token": token}, err)
			return
		}
		if request.Image != "" {
			err := s.DesiredImage(r.Context(), claims.UserID, node, request.Image)
			respond(w, map[string]bool{"ok": err == nil}, err)
			return
		}
		err := s.SetNode(r.Context(), claims.UserID, node, request.Mode)
		respond(w, map[string]bool{"ok": err == nil}, err)
		return
	}
	http.NotFound(w, r)
}
func (s *Service) NodeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/edge-control/")
	if path == "register" {
		var req struct {
			Token string `json:"token"`
		}
		if !decode(w, r, &req) {
			return
		}
		value, err := s.Register(r.Context(), req.Token)
		respond(w, value, err)
		return
	}
	identity := strings.TrimPrefix(r.Header.Get("Authorization"), "Edge ")
	node, err := s.Authenticate(r.Context(), identity)
	if err != nil {
		respond(w, nil, err)
		return
	}
	switch path {
	case "deployment":
		value, err := s.NodeDeployment(r.Context(), node)
		respond(w, value, err)
	case "self-mode":
		var req struct {
			Mode string `json:"mode"`
		}
		if !decode(w, r, &req) {
			return
		}
		if req.Mode != "draining" && req.Mode != "revoked" {
			respond(w, nil, ErrUnauthorized)
			return
		}
		err := s.SetNode(r.Context(), "node:"+node, node, req.Mode)
		respond(w, map[string]bool{"ok": err == nil}, err)

	case "heartbeat":
		var h edgeprotocol.Heartbeat
		if !decode(w, r, &h) {
			return
		}
		mode, e := s.Heartbeat(r.Context(), node, &h)
		respond(w, map[string]string{"mode": mode}, e)
	case "connect":
		var req struct {
			TokenID string `json:"token_id"`
		}
		if !decode(w, r, &req) {
			return
		}
		v, e := s.Connect(r.Context(), node, req.TokenID)
		respond(w, v, e)
	case "renew":
		var req struct {
			SessionID  string `json:"session_id"`
			Generation int64  `json:"generation"`
		}
		if !decode(w, r, &req) {
			return
		}
		v, e := s.Renew(r.Context(), node, req.SessionID, req.Generation)
		respond(w, v, e)
	case "archive":
		var event edgeprotocol.Event
		if !decode(w, r, &event) {
			return
		}
		value, err := s.Archive(r.Context(), node, &event)
		respond(w, value, err)
	case "events":
		var event edgeprotocol.Event
		if !decode(w, r, &event) {
			return
		}
		v, e := s.Event(r.Context(), node, &event)
		respond(w, v, e)
	default:
		http.NotFound(w, r)
	}
}
