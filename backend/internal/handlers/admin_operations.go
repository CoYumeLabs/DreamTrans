package handlers

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dreamtrans/backend/internal/acquisition"
	"github.com/dreamtrans/backend/internal/auth"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

type redeemBatchInput struct {
	RequestID string    `json:"client_request_id"`
	Quantity  int       `json:"quantity"`
	Amount    float64   `json:"face_value_usd"`
	Days      int       `json:"grant_days"`
	Expires   time.Time `json:"expires_at"`
	Channel   string    `json:"channel"`
	Tags      []string  `json:"tags"`
}

// HandleRedeemCodes serves code generation, browsing and revocation.
func (h *AdminHandler) HandleRedeemCodes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listRedeemCodes(w, r)
	case http.MethodPost:
		h.createRedeemCodes(w, r)
	case http.MethodDelete:
		h.voidRedeemCode(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// createRedeemCodes turns the code-batch form into a code-claimed campaign
// source plus its single-use codes, so the codes share the one attribution
// and reward path. Retrying a client_request_id returns the same codes.
func (h *AdminHandler) createRedeemCodes(w http.ResponseWriter, r *http.Request) {
	var input redeemBatchInput
	if json.NewDecoder(r.Body).Decode(&input) != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}
	input.Channel = strings.TrimSpace(input.Channel)
	if a := consoleAccess(r); a != nil && !a.AllowsChannel(input.Channel) {
		http.Error(w, "Channel forbidden", http.StatusForbidden)
		return
	}
	if _, err := uuid.Parse(input.RequestID); err != nil {
		http.Error(w, `{"error":"client_request_id must be a UUID"}`, http.StatusBadRequest)
		return
	}
	if invalidRedeemBatch(&input) {
		http.Error(w, `{"error":"invalid code quantity, amount, channel or expiry"}`, http.StatusBadRequest)
		return
	}
	for _, tag := range input.Tags {
		if len([]rune(tag)) > 80 {
			http.Error(w, `{"error":"tag is too long"}`, http.StatusBadRequest)
			return
		}
	}
	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	tx, err := h.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "Database unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = tx.Rollback() }()
	tags, _ := json.Marshal(input.Tags)
	if input.Tags == nil {
		tags = []byte("[]")
	}
	if codes, err := existingCodesTx(r.Context(), tx, claims.UserID, input.RequestID, ""); err != nil {
		http.Error(w, "Database unavailable", http.StatusServiceUnavailable)
		return
	} else if len(codes) > 0 {
		// A replayed request returns the original batch only when every term
		// still matches; otherwise the caller must use a new request id.
		var invite string
		if err := tx.QueryRowContext(r.Context(), `SELECT i.id FROM redeem_codes c JOIN promotion_invites i ON i.id=c.invite_id
 WHERE c.created_by=$1 AND c.client_request_id=$2 AND i.grant_usd=$3 AND i.grant_days=$4 AND i.channel=$5 AND i.tags=$6::jsonb AND i.expires_at=$7 AND i.max_registrations=$8 LIMIT 1`,
			claims.UserID, input.RequestID, input.Amount, input.Days, input.Channel, tags, input.Expires, input.Quantity).Scan(&invite); err != nil || len(codes) != input.Quantity {
			http.Error(w, `{"error":"重复请求的参数已改变，请使用新的请求标识"}`, http.StatusConflict)
			return
		}
		WriteJSON(w, map[string]any{"batch_id": input.RequestID, "invite_id": invite, "codes": codes})
		return
	}
	var inviteID string
	name := input.Channel + " 兑换码 " + time.Now().UTC().Format("2006-01-02")
	err = tx.QueryRowContext(r.Context(), `INSERT INTO promotion_invites(code,name,channel,tags,enabled,expires_at,max_registrations,grant_usd,grant_days,plan_days,created_by,kind,claim_mode)
 VALUES('RB-'||upper(replace($1,'-','')),LEFT($2,100),$3,$4,TRUE,$5,$6,$7,$8,30,$9,'campaign','code') RETURNING id`,
		input.RequestID, name, input.Channel, tags, input.Expires, input.Quantity, input.Amount, input.Days, claims.UserID).Scan(&inviteID)
	var pgErr *pq.Error
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// The request id is embedded in the source code, so a different
		// administrator reusing it is a conflict, not an outage.
		http.Error(w, `{"error":"该请求标识已被使用，请使用新的请求标识"}`, http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "Failed to create code source", http.StatusServiceUnavailable)
		return
	}
	codes, err := issueCodesTx(r.Context(), tx, inviteID, claims.UserID, input.RequestID, input.Quantity, nil)
	if err != nil || tx.Commit() != nil {
		http.Error(w, "Failed to create codes", http.StatusServiceUnavailable)
		return
	}
	WriteJSON(w, map[string]any{"batch_id": input.RequestID, "invite_id": inviteID, "codes": codes})
}

// issueCodes adds single-use codes to an existing source within the caller's
// channel scope. Codes inherit the source's gift and expiry.
func (h *AdminHandler) issueCodes(w http.ResponseWriter, r *http.Request, inviteID, requestID string, quantity int) {
	if _, err := uuid.Parse(requestID); err != nil || quantity < 1 || quantity > 1000 {
		http.Error(w, `{"error":"client_request_id must be a UUID and quantity 1–1000"}`, http.StatusBadRequest)
		return
	}
	claims := auth.GetUserClaims(r.Context())
	if claims == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	tx, err := h.store.DB().BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "Database unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = tx.Rollback() }()
	var kind string
	var expires time.Time
	if err := tx.QueryRowContext(r.Context(), `SELECT kind,expires_at FROM promotion_invites WHERE id=$1 AND (cardinality($2::text[])=0 OR channel=ANY($2::text[])) FOR UPDATE`, inviteID, pq.Array(consoleChannels(r))).Scan(&kind, &expires); err != nil {
		http.Error(w, `{"error":"promotion not found"}`, http.StatusNotFound)
		return
	}
	if kind == acquisition.KindReferral || !expires.After(time.Now()) {
		http.Error(w, `{"error":"该来源不能发放兑换码"}`, http.StatusConflict)
		return
	}
	if codes, err := existingCodesTx(r.Context(), tx, claims.UserID, requestID, inviteID); err != nil {
		http.Error(w, "Database unavailable", http.StatusServiceUnavailable)
		return
	} else if len(codes) > 0 {
		WriteJSON(w, map[string]any{"batch_id": requestID, "invite_id": inviteID, "codes": codes})
		return
	}
	codes, err := issueCodesTx(r.Context(), tx, inviteID, claims.UserID, requestID, quantity, nil)
	if err != nil || tx.Commit() != nil {
		http.Error(w, "Failed to create codes", http.StatusServiceUnavailable)
		return
	}
	WriteJSON(w, map[string]any{"batch_id": requestID, "invite_id": inviteID, "codes": codes})
}

// existingCodesTx returns the codes an earlier attempt with the same request
// id already issued; with an invite id the match is scoped to that source.
func existingCodesTx(ctx context.Context, tx *sql.Tx, createdBy, requestID, inviteID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT code FROM redeem_codes WHERE created_by=$1 AND client_request_id=$2 AND ($3='' OR invite_id=$3::uuid) ORDER BY code`, createdBy, requestID, inviteID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	codes := make([]string, 0)
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	return codes, rows.Err()
}

func issueCodesTx(ctx context.Context, tx *sql.Tx, inviteID, createdBy, requestID string, quantity int, expires *time.Time) ([]string, error) {
	codes := make([]string, 0, quantity)
	for i := 0; i < quantity; i++ {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		code := strings.ToUpper(hex.EncodeToString(random[:]))
		if _, err := tx.ExecContext(ctx, `INSERT INTO redeem_codes(invite_id,code,created_by,client_request_id,expires_at) VALUES($1,$2,$3,$4,$5)`, inviteID, code, createdBy, requestID, expires); err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes, nil
}

func (h *AdminHandler) listRedeemCodes(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > 100000 {
		http.Error(w, "Page out of range", http.StatusBadRequest)
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if len(search) > 100 {
		http.Error(w, "Search too long", http.StatusBadRequest)
		return
	}
	// /api/admin/promotions/{id}/codes lists one source; the codes page lists all.
	inviteID := ""
	if rest := strings.TrimPrefix(r.URL.Path, "/api/admin/promotions/"); rest != r.URL.Path {
		inviteID, _, _ = strings.Cut(rest, "/")
	}
	rows, err := h.store.DB().QueryContext(r.Context(), `SELECT c.id,c.code,c.invite_id,i.name,i.kind,i.channel,i.grant_usd,LEAST(COALESCE(c.expires_at,i.expires_at),i.expires_at),c.redeemed_at,c.voided_at,COALESCE(c.client_request_id::text,''),COUNT(*) OVER()
 FROM redeem_codes c JOIN promotion_invites i ON i.id=c.invite_id
 WHERE ($1='' OR c.code ILIKE '%' || $1 || '%' OR i.channel ILIKE '%' || $1 || '%' OR i.name ILIKE '%' || $1 || '%') AND ($4='' OR c.invite_id=$4::uuid)
 AND (cardinality($3::text[])=0 OR i.channel=ANY($3::text[])) ORDER BY c.created_at DESC,c.id LIMIT 50 OFFSET $2`, search, (page-1)*50, pq.Array(consoleChannels(r)), inviteID)
	if err != nil {
		http.Error(w, "Failed to read codes", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = rows.Close() }()
	items := make([]map[string]any, 0)
	total := 0
	for rows.Next() {
		var id, code, invite, name, kind, channel, request string
		var amount float64
		var expires time.Time
		var redeemed, voided sql.NullTime
		if rows.Scan(&id, &code, &invite, &name, &kind, &channel, &amount, &expires, &redeemed, &voided, &request, &total) != nil {
			http.Error(w, "Failed to read code", http.StatusServiceUnavailable)
			return
		}
		status := "available"
		if voided.Valid {
			status = "voided"
		} else if redeemed.Valid {
			status = "redeemed"
		} else if !expires.After(time.Now()) {
			status = "expired"
		}
		items = append(items, map[string]any{"id": id, "code": code, "invite_id": invite, "batch_id": request, "source": name, "kind": kind, "channel": channel, "face_value_usd": amount, "expires_at": expires, "status": status})
	}
	if rows.Err() != nil {
		http.Error(w, "Failed to read codes", http.StatusServiceUnavailable)
		return
	}
	WriteJSON(w, map[string]any{"codes": items, "total": total, "page": page})
}

func (h *AdminHandler) voidRedeemCode(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/redeem-codes/")
	if _, err := uuid.Parse(id); err != nil {
		http.Error(w, "Invalid code id", http.StatusBadRequest)
		return
	}
	result, err := h.store.DB().ExecContext(r.Context(), `UPDATE redeem_codes SET voided_at=COALESCE(voided_at,NOW()),voided_by=$2 WHERE id=$1 AND redeemed_at IS NULL AND invite_id IN (SELECT id FROM promotion_invites WHERE cardinality($3::text[])=0 OR channel=ANY($3::text[]))`, id, auth.GetUserID(r.Context()), pq.Array(consoleChannels(r)))
	if err != nil {
		http.Error(w, "Failed to revoke code", http.StatusServiceUnavailable)
		return
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		http.Error(w, `{"error":"兑换码不存在或已被兑换"}`, http.StatusConflict)
		return
	}
	WriteJSON(w, map[string]bool{"success": true})
}

// HandleRedeem consumes a signed-in user's single-use gift code.
func (h *BillingHandler) HandleRedeem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		Code string `json:"code"`
	}
	if json.NewDecoder(r.Body).Decode(&input) != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	result, err := h.billing.RedeemGift(r.Context(), auth.GetUserID(r.Context()), input.Code)
	if err != nil {
		writeBillingAdminError(w, "redeem gift", err)
		return
	}
	// A nil grant means the claim is recorded but the gift is held for
	// signup-risk review; it arrives once the review clears.
	WriteJSON(w, map[string]any{"grant": result, "pending": result == nil})
}

// HandleAudit lists administrator audit records using a stable page boundary.
func (h *AdminHandler) HandleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > 100000 {
		http.Error(w, "Page out of range", http.StatusBadRequest)
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if len(search) > 100 {
		http.Error(w, "Search too long", http.StatusBadRequest)
		return
	}
	rows, err := h.store.DB().QueryContext(r.Context(), `SELECT a.id,a.created_at,COALESCE(u.email,''),a.action,a.target_type,COALESCE(a.target_id,''),a.details,COUNT(*) OVER() FROM admin_audit_logs a LEFT JOIN users u ON u.id=a.actor_user_id WHERE ($1='' OR a.action ILIKE '%'||$1||'%' OR a.target_id ILIKE '%'||$1||'%' OR u.email ILIKE '%'||$1||'%') AND (cardinality($3::text[])=0 OR a.details->>'channel'=ANY($3::text[])) ORDER BY a.created_at DESC,a.id DESC LIMIT 50 OFFSET $2`, search, (page-1)*50, pq.Array(consoleChannels(r)))
	if err != nil {
		http.Error(w, "Audit unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = rows.Close() }()
	items := make([]map[string]any, 0)
	total := 0
	for rows.Next() {
		var id, actor, action, targetType, targetID string
		var at time.Time
		var details json.RawMessage
		if rows.Scan(&id, &at, &actor, &action, &targetType, &targetID, &details, &total) != nil {
			http.Error(w, "Audit unavailable", http.StatusServiceUnavailable)
			return
		}
		items = append(items, map[string]any{"id": id, "created_at": at, "actor": actor, "action": action, "target_type": targetType, "target_id": targetID, "details": details})
	}
	if rows.Err() != nil {
		http.Error(w, "Audit unavailable", http.StatusServiceUnavailable)
		return
	}
	WriteJSON(w, map[string]any{"entries": items, "page": page, "total": total})
}

// ConsoleWrites records intent before mutation and blocks monetary operations
// until the caller explicitly confirms. An audit outage fails closed.
func (h *AdminHandler) ConsoleWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
		if err != nil || len(body) > 1<<20 {
			http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var payload any
		if len(body) > 0 && json.Unmarshal(body, &payload) != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		if action, money := consoleConfirmation(r, payload); money && r.Header.Get("X-Admin-Confirm") != "true" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusPreconditionRequired)
			WriteJSON(w, map[string]string{"error": "此操作将" + action + "，会直接影响客户实际支付的价格、赠送额度或余额。请核对表单后确认。", "code": "confirmation_required", "action": action})
			return
		}
		sanitized := redactAuditPayload(payload)
		encoded, err := json.Marshal(map[string]any{"actor_id": auth.GetUserID(r.Context()), "method": r.Method, "request": sanitized, "state": "started", "channel": h.auditChannel(r, payload)})
		if err != nil {
			http.Error(w, "Invalid audit payload", http.StatusBadRequest)
			return
		}
		var id string
		err = h.store.DB().QueryRowContext(r.Context(), `INSERT INTO admin_audit_logs(actor_user_id,action,target_type,target_id,details) VALUES($1,'request.started','http',$2,$3) RETURNING id`, auth.GetUserID(r.Context()), r.URL.Path, encoded).Scan(&id)
		if err != nil {
			http.Error(w, "Audit unavailable; no changes were made", http.StatusServiceUnavailable)
			return
		}
		response := &adminStatusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(response, r)
		// Intent remains durable even if recording the outcome fails.
		_, _ = h.store.DB().ExecContext(r.Context(), `UPDATE admin_audit_logs SET action=$2,details=details||jsonb_build_object('status',$3::int,'state','completed') WHERE id=$1`, id, fmt.Sprintf("request.%d", response.status), response.status)
	})
}

type adminStatusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *adminStatusWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// consoleConfirmation names the operations that move money or change what a
// customer pays, so only those ask the operator to confirm. Everything else
// (routing estimates, roles, announcements, copy edits) saves directly.
func consoleConfirmation(r *http.Request, payload any) (string, bool) {
	path := r.URL.Path
	switch {
	case path == "/api/admin/balance" || strings.HasSuffix(path, "/balance"):
		return "调整用户钱包余额", true
	case strings.HasPrefix(path, "/api/admin/billing/plans") || strings.HasPrefix(path, "/api/admin/billing/topup-tiers"):
		return "修改对客户生效的套餐或充值档位", true
	case strings.HasPrefix(path, "/api/admin/billing/"):
		return "修改计费目录、加价或成本口径", true
	case strings.HasPrefix(path, "/api/admin/customers/"):
		return "修改该客户的会员、赠送额度或余额", true
	case strings.HasPrefix(path, "/api/admin/redeem-codes"):
		if r.Method == http.MethodDelete {
			return "作废尚未使用的兑换码", true
		}
		return "生成可兑换成赠送额度的兑换码", true
	case strings.HasPrefix(path, "/api/agent/codes"):
		return "生成代理兑换码", true
	case strings.HasPrefix(path, "/api/agent/settlements"):
		return "申请结算代理分成", true
	case strings.HasPrefix(path, "/api/admin/settlements"):
		return "审核或登记支付代理结算", true
	case path == "/api/admin/agents":
		return "修改代理的分成比例、面值或结算门槛", true
	case path == "/api/admin/agent-fraud":
		return "解除分成风控标记或放宽风控规则", true
	case path == "/api/admin/promotions" && r.Method == http.MethodPost:
		return "创建带赠送权益的推广活动", true
	case strings.HasPrefix(path, "/api/admin/promotions/") && strings.HasSuffix(path, "/codes"):
		return "生成可兑换的赠送额度", true
	case strings.HasPrefix(path, "/api/admin/tenants/"):
		return "修改组织的套餐或配额", true
	case strings.HasPrefix(path, "/api/admin/models") || path == "/api/admin/settings":
		// Only the price-bearing fields of these mixed forms move money.
		if moneySettings(payload) {
			return "修改模型定价、折扣或赠送相关设置", true
		}
	}
	return "", false
}

func moneySettings(value any) bool {
	switch object := value.(type) {
	case map[string]any:
		for key, value := range object {
			key = strings.ToLower(key)
			if strings.Contains(key, "usd") || strings.Contains(key, "cents") || strings.Contains(key, "price") || strings.Contains(key, "discount") || strings.Contains(key, "bonus") || strings.Contains(key, "amount") || strings.Contains(key, "credit") || strings.Contains(key, "markup") || moneySettings(value) {
				return true
			}
		}
	case []any:
		for _, item := range object {
			if moneySettings(item) {
				return true
			}
		}
	}
	return false
}
func redactAuditPayload(value any) any {
	switch object := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(object))
		for key, item := range object {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || strings.Contains(lower, "api_key") {
				result[key] = "[redacted]"
			} else {
				result[key] = redactAuditPayload(item)
			}
		}
		return result
	case []any:
		result := make([]any, len(object))
		for i, item := range object {
			result[i] = redactAuditPayload(item)
		}
		return result
	default:
		return value
	}
}

func (w *adminStatusWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}
func (h *AdminHandler) auditChannel(r *http.Request, payload any) string {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 4 {
		id := parts[3]
		if _, err := uuid.Parse(id); err == nil {
			var channel string
			switch parts[2] {
			case "redeem-codes":
				_ = h.store.DB().QueryRowContext(r.Context(), `SELECT i.channel FROM redeem_codes c JOIN promotion_invites i ON i.id=c.invite_id WHERE c.id=$1`, id).Scan(&channel)
			case "promotions":
				_ = h.store.DB().QueryRowContext(r.Context(), `SELECT channel FROM promotion_invites WHERE id=$1`, id).Scan(&channel)
			}
			return channel
		}
	}
	if body, ok := payload.(map[string]any); ok {
		channel, _ := body["channel"].(string)
		return strings.TrimSpace(channel)
	}
	return ""
}

func invalidRedeemBatch(input *redeemBatchInput) bool {
	return input.Quantity < 1 || input.Quantity > 1000 || input.Amount < 0.00000001 || input.Amount > 10000 || input.Days < 1 || input.Days > 3650 || len([]rune(input.Channel)) < 1 || len([]rune(input.Channel)) > 100 || !input.Expires.After(time.Now()) || input.Expires.After(time.Now().AddDate(10, 0, 0)) || len(input.Tags) > 20
}
