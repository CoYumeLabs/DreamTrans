package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/store"
	"github.com/google/uuid"
)

type consoleAccessKey struct{}

// ConsoleAccess is reloaded for every request, so revocation does not wait for
// a token refresh. Dynamic console roles never elevate the JWT's base role.
type ConsoleAccess struct {
	UserID       string   `json:"-"`
	TenantID     string   `json:"-"`
	RoleID       string   `json:"role_id"`
	RoleKey      string   `json:"role_key"`
	Name         string   `json:"name"`
	Super        bool     `json:"super"`
	LegacyTenant bool     `json:"tenant_admin"`
	Permissions  []string `json:"permissions"`
	Channels     []string `json:"channels"`
	Allowed      bool     `json:"allowed"`
}

func (a *ConsoleAccess) Has(permission string) bool {
	if a == nil {
		return false
	}
	if a.Super {
		return true
	}
	for _, p := range a.Permissions {
		if p == permission {
			return true
		}
	}
	return false
}
func (a *ConsoleAccess) AllowsChannel(channel string) bool {
	if a == nil || len(a.Channels) == 0 {
		return true
	}
	for _, c := range a.Channels {
		if c == channel {
			return true
		}
	}
	return false
}
func consoleAccess(r *http.Request) *ConsoleAccess {
	access, _ := r.Context().Value(consoleAccessKey{}).(*ConsoleAccess)
	return access
}
func consolePermission(r *http.Request, permission string) bool {
	if access := consoleAccess(r); access != nil {
		return access.Has(permission)
	}
	claims := auth.GetUserClaims(r.Context())
	return claims != nil && claims.Role == "super_admin"
}
func consoleChannels(r *http.Request) []string {
	if access := consoleAccess(r); access != nil && access.Channels != nil {
		return access.Channels
	}
	return []string{}
}
func (h *AdminHandler) resolveConsoleAccess(ctx context.Context, userID string) (*ConsoleAccess, error) {
	access := &ConsoleAccess{UserID: userID, Permissions: []string{}, Channels: []string{}}
	var baseRole string
	var permissions, channels []byte
	err := h.store.DB().QueryRowContext(ctx, `SELECT u.tenant_id,u.role,COALESCE(a.id::text,''),COALESCE(a.key,''),COALESCE(a.name,''),COALESCE(a.permissions,'[]'),COALESCE(a.channels,'[]') FROM users u LEFT JOIN admin_roles a ON a.id=u.admin_role_id WHERE u.id=$1 AND u.is_active`, userID).Scan(&access.TenantID, &baseRole, &access.RoleID, &access.RoleKey, &access.Name, &permissions, &channels)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(permissions, &access.Permissions); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(channels, &access.Channels); err != nil {
		return nil, err
	}
	if baseRole == "super_admin" {
		access.Super = true
		access.RoleKey = "super_admin"
		access.Name = "超级管理员"
		access.Permissions = []string{"*"}
		access.Channels = []string{}
	}
	if baseRole == "admin" && access.RoleID == "" {
		access.LegacyTenant = true
		access.RoleKey = "tenant_admin"
		access.Name = "组织管理员"
		access.Permissions = []string{"users.read", "users.write"}
	}
	access.Allowed = access.Super || len(access.Permissions) > 0
	return access, nil
}
func (h *AdminHandler) HandleConsoleAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	access, err := h.resolveConsoleAccess(r.Context(), auth.GetUserID(r.Context()))
	if err != nil {
		http.Error(w, "Console access unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, access)
}

func consoleOperation(r *http.Request) string {
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	path := strings.TrimSuffix(r.URL.Path, "/")
	mode := func(view, write string) string {
		if read {
			return view
		}
		return write
	}
	switch {
	case strings.HasPrefix(path, "/api/agent/"):
		return "agent.self"
	case strings.HasPrefix(path, "/api/admin/roles"):
		return mode("roles.read", "roles.write")
	case path == "/api/admin/dashboard":
		return "dashboard.read"
	case path == "/api/admin/audit":
		return "audit.read"
	case strings.HasPrefix(path, "/api/admin/redeem-codes"):
		return mode("codes.read", "codes.write")
	case strings.HasPrefix(path, "/api/admin/promotions") || path == "/api/admin/referrals":
		return mode("promotions.read", "promotions.write")
	case strings.HasPrefix(path, "/api/admin/announcements"):
		return mode("announcements.read", "announcements.write")
	case strings.HasPrefix(path, "/api/admin/signup-risk"):
		return mode("risk.read", "risk.write")
	case strings.HasPrefix(path, "/api/admin/settlements"):
		if strings.HasSuffix(path, "/pay") {
			return "super_admin"
		}
		return mode("finance.read", "settlements.review")
	case strings.HasPrefix(path, "/api/admin/agent-fraud") || strings.HasPrefix(path, "/api/admin/agents"):
		return mode("finance.read", "agents.write")
	case strings.HasPrefix(path, "/api/admin/routing") || path == "/api/admin/training-program" || strings.HasPrefix(path, "/api/admin/live-streams"):
		return mode("routing.read", "routing.write")
	case strings.HasPrefix(path, "/api/admin/models"):
		return mode("models.read", "models.write")
	case strings.HasPrefix(path, "/api/admin/billing/analytics") || strings.HasPrefix(path, "/api/admin/customers"):
		return mode("finance.read", "finance.write")
	case strings.HasPrefix(path, "/api/admin/billing/"):
		return mode("pricing.read", "pricing.write")
	case path == "/api/admin/balance":
		return "finance.write"
	case strings.HasPrefix(path, "/api/admin/users"):
		if strings.HasSuffix(path, "/balance") {
			return "finance.read"
		}
		return mode("users.read", "users.write")
	case strings.HasPrefix(path, "/api/admin/tenants"):
		return mode("routing.read", "routing.write")
	case path == "/api/admin/settings":
		return mode("settings.read", "settings.write")
	default:
		return "super_admin"
	}
}

// ConsoleGate authorizes each endpoint and passes channel restrictions to
// both handlers and storage. Unknown endpoints remain super-admin-only.
func (h *AdminHandler) ConsoleGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		access, err := h.resolveConsoleAccess(r.Context(), auth.GetUserID(r.Context()))
		if err != nil {
			http.Error(w, "Console access unavailable", http.StatusServiceUnavailable)
			return
		}
		permission := consoleOperation(r)
		if access.LegacyTenant && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/admin/users/") && strings.HasSuffix(r.URL.Path, "/balance") {
			permission = "users.read"
		}
		if !access.Has(permission) {
			http.Error(w, `{"error":"当前角色无权执行此操作"}`, http.StatusForbidden)
			return
		}
		if !access.AllowsChannel(r.URL.Query().Get("channel")) && r.URL.Query().Get("channel") != "" {
			http.Error(w, "Channel forbidden", http.StatusForbidden)
			return
		}
		// Legacy global reports and platform-wide settings cannot be safely
		// narrowed. Scoped roles use the channel-aware dashboard and code tools.
		if len(access.Channels) > 0 && !scopedConsolePath(r.URL.Path) {
			http.Error(w, `{"error":"此功能不支持当前渠道范围"}`, http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), consoleAccessKey{}, access)
		ctx = store.WithAdminChannels(ctx, access.Channels)
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func scopedConsolePath(path string) bool {
	for _, prefix := range []string{"/api/admin/dashboard", "/api/admin/promotions", "/api/admin/redeem-codes", "/api/admin/audit"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

var consolePermissions = []string{"dashboard.read", "finance.read", "finance.write", "pricing.read", "pricing.write", "promotions.read", "promotions.write", "codes.read", "codes.write", "announcements.read", "announcements.write", "models.read", "models.write", "routing.read", "routing.write", "metrics.read", "users.read", "users.write", "risk.read", "risk.write", "settings.read", "settings.write", "roles.read", "roles.write", "audit.read", "export", "agent.self", "agents.write", "settlements.review"}

type consoleRole struct {
	ID          string   `json:"id"`
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
	Channels    []string `json:"channels"`
	Builtin     bool     `json:"builtin"`
}

func (h *AdminHandler) HandleConsoleRoles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := h.store.DB().QueryContext(r.Context(), `SELECT id,key,name,permissions,channels,builtin FROM admin_roles ORDER BY builtin DESC,name`)
		if err != nil {
			http.Error(w, "Roles unavailable", http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = rows.Close() }()
		roles := []consoleRole{}
		for rows.Next() {
			var role consoleRole
			var permissions, channels []byte
			if rows.Scan(&role.ID, &role.Key, &role.Name, &permissions, &channels, &role.Builtin) != nil {
				http.Error(w, "Roles unavailable", http.StatusServiceUnavailable)
				return
			}
			if json.Unmarshal(permissions, &role.Permissions) != nil || json.Unmarshal(channels, &role.Channels) != nil {
				http.Error(w, "Invalid role", http.StatusServiceUnavailable)
				return
			}
			roles = append(roles, role)
		}
		if rows.Err() != nil {
			http.Error(w, "Roles unavailable", http.StatusServiceUnavailable)
			return
		}
		WriteJSON(w, map[string]any{"roles": roles, "permissions": consolePermissions})
	case http.MethodPost, http.MethodPut:
		var role consoleRole
		if json.NewDecoder(r.Body).Decode(&role) != nil {
			http.Error(w, "Invalid role", http.StatusBadRequest)
			return
		}
		role.Key = strings.TrimSpace(role.Key)
		role.Name = strings.TrimSpace(role.Name)
		if invalidConsoleRole(&role) {
			http.Error(w, "Invalid role", http.StatusBadRequest)
			return
		}
		for _, channel := range role.Channels {
			if strings.TrimSpace(channel) == "" || len([]rune(channel)) > 100 {
				http.Error(w, "Invalid channel", http.StatusBadRequest)
				return
			}
		}
		allowed := map[string]bool{}
		for _, permission := range consolePermissions {
			allowed[permission] = true
		}
		access := consoleAccess(r)
		for _, permission := range role.Permissions {
			if !allowed[permission] || !access.Has(permission) {
				http.Error(w, `{"error":"不能授予自己没有的权限"}`, http.StatusForbidden)
				return
			}
		}
		permissions, _ := json.Marshal(role.Permissions)
		channels, _ := json.Marshal(role.Channels)
		if role.Permissions == nil {
			permissions = []byte("[]")
		}
		if role.Channels == nil {
			channels = []byte("[]")
		}
		if role.ID == "" {
			role.ID = uuid.NewString()
		} else if _, err := uuid.Parse(role.ID); err != nil {
			http.Error(w, "Invalid role id", http.StatusBadRequest)
			return
		}
		result, err := h.store.DB().ExecContext(r.Context(), `INSERT INTO admin_roles(id,key,name,permissions,channels) VALUES($1,$2,$3,$4,$5) ON CONFLICT(id) DO UPDATE SET name=EXCLUDED.name,permissions=EXCLUDED.permissions,channels=EXCLUDED.channels,updated_at=NOW() WHERE NOT admin_roles.builtin`, role.ID, role.Key, role.Name, permissions, channels)
		if err != nil {
			http.Error(w, `{"error":"角色标识重复或无法保存"}`, http.StatusConflict)
			return
		}
		count, _ := result.RowsAffected()
		if count == 0 {
			http.Error(w, `{"error":"内置角色不可修改，请复制为自定义角色"}`, http.StatusConflict)
			return
		}
		WriteJSON(w, role)
	case http.MethodDelete:
		id := strings.TrimPrefix(r.URL.Path, "/api/admin/roles/")
		if _, err := uuid.Parse(id); err != nil {
			http.Error(w, "Invalid role id", http.StatusBadRequest)
			return
		}
		result, err := h.store.DB().ExecContext(r.Context(), `DELETE FROM admin_roles WHERE id=$1 AND NOT builtin AND NOT EXISTS(SELECT 1 FROM users WHERE admin_role_id=$1)`, id)
		if err != nil {
			http.Error(w, "Cannot delete role", http.StatusConflict)
			return
		}
		count, _ := result.RowsAffected()
		if count == 0 {
			http.Error(w, `{"error":"角色正在使用、为内置角色或不存在"}`, http.StatusConflict)
			return
		}
		WriteJSON(w, map[string]bool{"success": true})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
func (h *AdminHandler) HandleAssignConsoleRole(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		UserID string `json:"user_id"`
		RoleID string `json:"role_id"`
	}
	if json.NewDecoder(r.Body).Decode(&input) != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	if _, err := uuid.Parse(input.UserID); err != nil {
		http.Error(w, "Invalid user id", http.StatusBadRequest)
		return
	}
	tx, err := h.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "Role assignment unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = tx.Rollback() }()
	if input.RoleID != "" {
		if _, err := uuid.Parse(input.RoleID); err != nil {
			http.Error(w, "Invalid role id", http.StatusBadRequest)
			return
		}
		var permissions []byte
		var key string
		if err := tx.QueryRowContext(r.Context(), `SELECT key,permissions FROM admin_roles WHERE id=$1 FOR SHARE`, input.RoleID).Scan(&key, &permissions); err != nil {
			http.Error(w, "Role not found", http.StatusNotFound)
			return
		}
		if key == "super_admin" {
			http.Error(w, `{"error":"超级管理员只能通过基础账户角色管理"}`, http.StatusForbidden)
			return
		}
		var values []string
		if json.Unmarshal(permissions, &values) != nil {
			http.Error(w, "Invalid role", http.StatusServiceUnavailable)
			return
		}
		for _, permission := range values {
			if !consoleAccess(r).Has(permission) {
				http.Error(w, "Cannot grant this role", http.StatusForbidden)
				return
			}
		}
	}
	result, err := tx.ExecContext(r.Context(), `UPDATE users SET admin_role_id=NULLIF($2,'')::uuid,updated_at=NOW() WHERE id=$1 AND role<>'super_admin'`, input.UserID, input.RoleID)
	if err != nil {
		http.Error(w, "Role assignment failed", http.StatusServiceUnavailable)
		return
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		http.Error(w, "User not found or is a super administrator", http.StatusConflict)
		return
	}
	if tx.Commit() != nil {
		http.Error(w, "Role assignment failed", http.StatusServiceUnavailable)
		return
	}
	WriteJSON(w, map[string]bool{"success": true})
}

func platformConsoleUsers(r *http.Request) bool {
	a := consoleAccess(r)
	return a != nil && !a.LegacyTenant && (a.Has("users.read") || a.Has("users.write"))
}

func invalidConsoleRole(role *consoleRole) bool {
	return role.Key == "" || len(role.Key) > 60 || role.Name == "" || len([]rune(role.Name)) > 100 || len(role.Channels) > 100 || len(role.Permissions) > len(consolePermissions)
}
