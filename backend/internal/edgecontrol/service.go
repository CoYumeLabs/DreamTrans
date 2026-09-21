// Package edgecontrol keeps regional node scheduling and billing on the main site.
package edgecontrol

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/dreamtrans/backend/internal/billing"
	"github.com/dreamtrans/backend/internal/edgeprotocol"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var ErrConflict = errors.New("edge session conflict or stale generation")
var ErrUnavailable = errors.New("no eligible edge capacity")
var ErrUnauthorized = errors.New("node identity rejected")

type Service struct {
	DB           *sql.DB
	Billing      *billing.Service
	Key          ed25519.PrivateKey
	mintProvider func(context.Context, bool) (string, error)
}

func New(db *sql.DB, b *billing.Service, encodedKey string) (*Service, error) {
	seed, err := base64.RawStdEncoding.DecodeString(encodedKey)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("EDGE_SIGNING_SEED must be a base64 Ed25519 seed (32 bytes)")
	}
	return &Service{DB: db, Billing: b, Key: ed25519.NewKeyFromSeed(seed), mintProvider: mintSpeechmatics}, nil
}
func (s *Service) PublicKey() string {
	return base64.RawStdEncoding.EncodeToString(s.Key.Public().(ed25519.PublicKey))
}

type Node struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Region         string          `json:"region"`
	Endpoint       string          `json:"endpoint"`
	Mode           string          `json:"mode"`
	Training       bool            `json:"training"`
	MaxConnections int             `json:"max_connections"`
	Version        string          `json:"version"`
	Heartbeat      *time.Time      `json:"heartbeat_at"`
	Metrics        json.RawMessage `json:"metrics"`
	ProtocolMin    int             `json:"protocol_min"`
	ProtocolMax    int             `json:"protocol_max"`
	Active         int             `json:"active"`
}

func (s *Service) Nodes(ctx context.Context) ([]Node, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT n.id,n.name,n.region,n.endpoint,n.mode,n.training,n.max_connections,n.version,n.heartbeat_at,n.metrics,n.protocol_min,n.protocol_max,(SELECT count(*) FROM edge_sessions e WHERE e.node_id=n.id AND e.status<>'closed' AND e.lease_until>now()) FROM edge_nodes n ORDER BY region,name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	nodes := make([]Node, 0)
	for rows.Next() {
		var n Node
		if err := rows.Scan(&n.ID, &n.Name, &n.Region, &n.Endpoint, &n.Mode, &n.Training, &n.MaxConnections, &n.Version, &n.Heartbeat, &n.Metrics, &n.ProtocolMin, &n.ProtocolMax, &n.Active); err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}
func validEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}
func (s *Service) CreateNode(ctx context.Context, actor string, n *Node) (string, string, error) {
	if len(n.Name) < 1 || len(n.Name) > 120 || len(n.Region) < 1 || len(n.Region) > 64 || !validEndpoint(n.Endpoint) || n.MaxConnections < 1 || n.MaxConnections > 4096 {
		return "", "", errors.New("invalid node configuration")
	}
	id := uuid.NewString()
	secret, err := edgeprotocol.Secret()
	if err != nil {
		return "", "", err
	}
	token := id + "." + secret
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO edge_nodes(id,name,region,endpoint,training,max_connections,registration_hash,registration_until) VALUES($1,$2,$3,$4,$5,$6,$7,now()+interval '15 minutes')`, id, n.Name, n.Region, strings.TrimRight(n.Endpoint, "/"), n.Training, n.MaxConnections, edgeprotocol.Hash(token))
	if err != nil {
		return "", "", err
	}
	if err := audit(ctx, tx, id, actor, "created", `{}`); err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return id, token, nil
}
func audit(ctx context.Context, tx *sql.Tx, node, actor, action, details string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO edge_audit(node_id,actor,action,details) VALUES($1,$2,$3,$4)`, node, actor, action, details)
	return err
}
func (s *Service) SetNode(ctx context.Context, actor, id, mode string) error {
	if mode != "enabled" && mode != "disabled" && mode != "draining" && mode != "revoked" {
		return errors.New("invalid node mode")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE edge_nodes SET mode=$2, identity_hash=CASE WHEN $2='revoked' THEN NULL ELSE identity_hash END,registration_hash=CASE WHEN $2='revoked' THEN NULL ELSE registration_hash END WHERE id=$1 AND (mode<>'revoked' OR $2='revoked')`, id, mode)
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return sql.ErrNoRows
	}
	if err := audit(ctx, tx, id, actor, mode, `{}`); err != nil {
		return err
	}
	return tx.Commit()
}

type Registration struct {
	ProviderAuth string `json:"provider_auth"`
	Maximum      int    `json:"maximum"`
	Training     bool   `json:"training"`
	TunnelToken  string `json:"tunnel_token,omitempty"`
	NodeID       string `json:"node_id"`
	Identity     string `json:"identity"`
	PublicKey    string `json:"public_key"`
	Endpoint     string `json:"endpoint"`
}

func (s *Service) Register(ctx context.Context, token string) (Registration, error) {
	var result Registration
	parts := strings.Split(token, ".")
	if len(parts) != 2 || len(token) > 160 {
		return result, ErrUnauthorized
	}
	if _, err := uuid.Parse(parts[0]); err != nil {
		return result, ErrUnauthorized
	}
	// Deterministic identity makes a lost registration response retryable until the first authenticated heartbeat consumes the registration.
	identity := parts[0] + "." + edgeprotocol.Hash("edge-identity:"+token)
	err := s.DB.QueryRowContext(ctx, `UPDATE edge_nodes SET identity_hash=$2 WHERE id=$1 AND registration_hash=$3 AND registration_until>now() AND mode<>'revoked' RETURNING endpoint,max_connections,training`, parts[0], edgeprotocol.Hash(identity), edgeprotocol.Hash(token)).Scan(&result.Endpoint, &result.Maximum, &result.Training)
	if err != nil {
		return result, ErrUnauthorized
	}
	result.NodeID = parts[0]
	result.Identity = identity
	result.PublicKey = s.PublicKey()
	result.ProviderAuth = "manual"
	var sealed string
	if err := s.DB.QueryRowContext(ctx, `SELECT tunnel_token FROM edge_nodes WHERE id=$1`, result.NodeID).Scan(&sealed); err != nil {
		return result, err
	}
	result.TunnelToken, err = s.openTunnel(result.NodeID, sealed)
	if err != nil {
		return result, err
	}
	return result, nil
}
func (s *Service) Authenticate(ctx context.Context, identity string) (string, error) {
	if len(identity) > 180 {
		return "", ErrUnauthorized
	}
	var node string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM edge_nodes WHERE identity_hash=$1 AND mode<>'revoked'`, edgeprotocol.Hash(identity)).Scan(&node)
	if err != nil {
		return "", ErrUnauthorized
	}
	return node, nil
}
func (s *Service) Heartbeat(ctx context.Context, node string, h *edgeprotocol.Heartbeat) (string, error) {
	if h.Connections < 0 || h.QueueBytes < 0 || h.Load < 0 || math.IsNaN(h.Load) || math.IsInf(h.Load, 0) || h.ProviderLatencyMS < 0 || h.ProtocolMin < 1 || h.ProtocolMax < h.ProtocolMin || len(h.Version) > 128 {
		return "", errors.New("invalid heartbeat")
	}
	data, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	var mode string
	if h.Role != "active" {
		err = s.DB.QueryRowContext(ctx, `UPDATE edge_nodes SET registration_hash=NULL WHERE id=$1 AND mode<>'revoked' RETURNING mode`, node).Scan(&mode)
		return mode, err
	}
	err = s.DB.QueryRowContext(ctx, `UPDATE edge_nodes SET heartbeat_at=now(),metrics=$2,version=$3,protocol_min=$4,protocol_max=$5,registration_hash=NULL WHERE id=$1 AND mode<>'revoked' RETURNING mode`, node, string(data), h.Version, h.ProtocolMin, h.ProtocolMax).Scan(&mode)
	return mode, err
}

type AuthorizeRequest struct {
	Protocol   int                `json:"protocol"`
	SessionID  string             `json:"session_id"`
	Region     string             `json:"region"`
	Latencies  map[string]float64 `json:"latencies"`
	SampleRate int                `json:"sample_rate"`
	Origin     string             `json:"-"`
}
type session struct {
	Protocol                                                     int
	DurableSeq, DurableSamples, ResumeSamples                    int64
	Offset                                                       float64
	PreviousGeneration, PreviousAudioSeq                         int64
	ID, User, Tenant, Node, Token, Status, Origin                string
	Generation, Approved, Consumed, Provider, AudioSeq, EventSeq int64
	Rate                                                         int
	Until                                                        time.Time
	Training                                                     bool
	Route                                                        billing.RouteDecision
}

const sessionColumns = `id,user_id,tenant_id,node_id,token_id,status,origin,generation,approved_samples,consumed_samples,provider_samples,last_audio_seq,last_event_seq,sample_rate,lease_until,training,route,timeline_offset,previous_generation,previous_audio_seq,durable_audio_seq,durable_samples,resume_samples,protocol`

func scanSession(row *sql.Row) (session, error) {
	var v session
	var route []byte
	err := row.Scan(&v.ID, &v.User, &v.Tenant, &v.Node, &v.Token, &v.Status, &v.Origin, &v.Generation, &v.Approved, &v.Consumed, &v.Provider, &v.AudioSeq, &v.EventSeq, &v.Rate, &v.Until, &v.Training, &route, &v.Offset, &v.PreviousGeneration, &v.PreviousAudioSeq, &v.DurableSeq, &v.DurableSamples, &v.ResumeSamples, &v.Protocol)
	if err == nil {
		err = json.Unmarshal(route, &v.Route)
	}
	return v, err
}
func userLock(ctx context.Context, tx *sql.Tx, user string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,5401))`, user)
	return err
}
func (s *Service) grant(v *session, endpoint string) (edgeprotocol.Authorization, error) {
	g := edgeprotocol.Grant{DurableAudioSequence: v.DurableSeq, DurableSamples: v.DurableSamples, ResumeSamples: v.ResumeSamples, TimelineOffset: v.Offset, PreviousGeneration: v.PreviousGeneration, PreviousAudioSequence: v.PreviousAudioSeq, RegisteredClaims: jwt.RegisteredClaims{Issuer: "dreamtrans-edge", Audience: jwt.ClaimStrings{v.Node}, Subject: v.User, ID: v.Token, IssuedAt: jwt.NewNumericDate(time.Now()), ExpiresAt: jwt.NewNumericDate(v.Until)}, NodeID: v.Node, SessionID: v.ID, UserID: v.User, Generation: v.Generation, Provider: "speechmatics", Origin: v.Origin, Training: v.Training, SampleRate: v.Rate, ApprovedSamples: v.Approved, AudioSequence: v.AudioSeq, Protocol: v.Protocol}
	token, err := edgeprotocol.Sign(s.Key, &g)
	return edgeprotocol.Authorization{Endpoint: endpoint, Token: token, Grant: g}, err
}
func (s *Service) reserve(ctx context.Context, tx *sql.Tx, v *session) error {
	samples := int64(v.Rate * edgeprotocol.BudgetSeconds)
	window := v.Approved/samples + 1
	key := fmt.Sprintf("edge:%s:%d:%d", v.ID, v.Generation, window)
	_, err := s.Billing.ReserveUsageTx(ctx, tx, []*billing.UsageRecord{{UserID: v.User, TenantID: v.Tenant, SessionID: &v.ID, Action: "transcription", Provider: "speechmatics", Model: "speechmatics-realtime-enhanced", Quantity: float64(edgeprotocol.BudgetSeconds) / 60, IdempotencyKey: key, Route: &v.Route, StrictBudget: true}})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO edge_budgets(session_id,generation,window_number,usage_key,samples) VALUES($1,$2,$3,$4,$5)`, v.ID, v.Generation, window, key, samples); err != nil {
		return err
	}
	v.Approved += samples
	_, err = tx.ExecContext(ctx, `UPDATE edge_sessions SET approved_samples=$2 WHERE id=$1`, v.ID, v.Approved)
	return err
}

// Authorize serializes user concurrency, node capacity and the budget debit before returning a grant.
func (s *Service) Authorize(ctx context.Context, user, tenant string, req AuthorizeRequest) (edgeprotocol.Authorization, error) {
	return s.authorize(ctx, user, tenant, req, true)
}

//nolint:gocyclo // Keep admission, concurrency and budget checks in one transaction.
func (s *Service) authorize(ctx context.Context, user, tenant string, req AuthorizeRequest, allowNew bool) (edgeprotocol.Authorization, error) {
	var empty edgeprotocol.Authorization
	if req.Protocol == 0 {
		req.Protocol = 1
	}
	if req.Protocol < edgeprotocol.MinVersion || req.Protocol > edgeprotocol.Version {
		return empty, errors.New("unsupported edge protocol")
	}
	if _, err := uuid.Parse(req.SessionID); err != nil {
		return empty, err
	}
	if !edgeprotocol.ValidSampleRate(req.SampleRate) {
		return empty, errors.New("unsupported sample rate")
	}
	if !validEndpoint(req.Origin) {
		return empty, errors.New("HTTPS origin required")
	}
	limit, err := s.Billing.SessionLimitForUser(ctx, user)
	if err != nil {
		return empty, err
	}
	route, err := s.Billing.RouteForUser(ctx, user)
	if err != nil {
		return empty, err
	}
	nodes, err := s.Nodes(ctx)
	if err != nil {
		return empty, err
	}
	sort.SliceStable(nodes, func(i, j int) bool { return nodeScore(&nodes[i], req) < nodeScore(&nodes[j], req) })
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := userLock(ctx, tx, user); err != nil {
		return empty, err
	}
	var owns bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id=$1 AND user_id=$2 AND tenant_id=$3)`, req.SessionID, user, tenant).Scan(&owns); err != nil || !owns {
		return empty, ErrUnauthorized
	}
	if !allowNew {
		var existing bool
		// Account locking serializes close/settlement and authorization. A
		// recently finalized handoff can recover without admitting new sessions.
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM edge_sessions WHERE id=$1 AND user_id=$2 AND tenant_id=$3 AND (status<>'closed' OR updated_at>now()-interval '2 minutes'))`, req.SessionID, user, tenant).Scan(&existing)
		if err != nil {
			return empty, err
		}
		if !existing {
			return empty, ErrUnavailable
		}
	}
	old, oldErr := scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM edge_sessions WHERE id=$1 FOR UPDATE`, req.SessionID))
	generation := int64(1)
	if oldErr == nil {
		req.Protocol = old.Protocol // Session semantics are fixed across generations.
		if old.Status != "closed" && old.Until.After(time.Now()) {
			if old.Status == "authorized" && old.Origin == req.Origin && old.Rate == req.SampleRate {
				var endpoint string
				if err := tx.QueryRowContext(ctx, `SELECT endpoint FROM edge_nodes WHERE id=$1 AND mode='enabled'`, old.Node).Scan(&endpoint); err != nil {
					return empty, ErrUnavailable
				}
				return s.grant(&old, endpoint)
			}
			return empty, ErrConflict
		}
		// Wait through the old grant's expiry + clock allowance before handing off.
		if old.Status != "closed" && old.Until.Add(3*time.Second).After(time.Now()) {
			return empty, ErrConflict
		}
		if err := s.settle(ctx, tx, &old, "lease_expired_or_replaced"); err != nil {
			return empty, err
		}
		generation = old.Generation + 1
	} else if !errors.Is(oldErr, sql.ErrNoRows) {
		return empty, oldErr
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM edge_sessions WHERE user_id=$1 AND status<>'closed' AND lease_until>now()`, user).Scan(&count); err != nil {
		return empty, err
	}
	if limit >= 0 && count >= limit {
		return empty, ErrUnavailable
	}
	selected, err := selectNode(ctx, tx, nodes, req, route.Training)
	if err != nil {
		return empty, err
	}
	v := session{Protocol: req.Protocol, ID: req.SessionID, User: user, Tenant: tenant, Node: selected.ID, Token: uuid.NewString(), Status: "authorized", Origin: req.Origin, Generation: generation, Rate: req.SampleRate, Until: time.Now().Add(edgeprotocol.LeaseSeconds * time.Second), Training: route.Training, Route: route}
	if oldErr == nil {
		if v.Rate != old.Rate {
			return empty, errors.New("resuming audio requires the original sample rate")
		}
		v.Offset = old.Offset + float64(old.DurableSamples-old.ResumeSamples)/float64(old.Rate)
		v.DurableSeq, v.DurableSamples, v.ResumeSamples = old.DurableSeq, old.DurableSamples, old.DurableSamples
		v.AudioSeq = old.DurableSeq
		v.PreviousGeneration = old.Generation
		v.PreviousAudioSeq = old.DurableSeq
		if v.Protocol == 1 {
			v.Offset = old.Offset + float64(old.Consumed)/float64(old.Rate)
			v.PreviousAudioSeq = old.AudioSeq
			v.DurableSeq, v.DurableSamples, v.ResumeSamples, v.AudioSeq = 0, 0, 0, 0
		}
	} else if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(end_time),0) FROM transcripts WHERE session_id=$1`, v.ID).Scan(&v.Offset); err != nil {
		return empty, err
	}
	data, err := json.Marshal(route)
	if err != nil {
		return empty, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO edge_sessions(id,user_id,tenant_id,node_id,token_id,status,origin,generation,sample_rate,lease_until,training,route) VALUES($1,$2,$3,$4,$5,'authorized',$6,$7,$8,$9,$10,$11) ON CONFLICT(id) DO UPDATE SET node_id=excluded.node_id,token_id=excluded.token_id,status=excluded.status,origin=excluded.origin,generation=excluded.generation,sample_rate=excluded.sample_rate,lease_until=excluded.lease_until,training=excluded.training,route=excluded.route,approved_samples=0,consumed_samples=0,provider_samples=0,last_audio_seq=0,last_event_seq=0,updated_at=now()`, v.ID, v.User, v.Tenant, v.Node, v.Token, v.Origin, v.Generation, v.Rate, v.Until, v.Training, string(data))
	if err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE edge_sessions SET timeline_offset=$2,previous_generation=$3,previous_audio_seq=$4,durable_audio_seq=$5,durable_samples=$6,resume_samples=$7,last_audio_seq=$5,protocol=$8 WHERE id=$1`, v.ID, v.Offset, v.PreviousGeneration, v.PreviousAudioSeq, v.DurableSeq, v.DurableSamples, v.ResumeSamples, v.Protocol); err != nil {
		return empty, err
	}
	if err := s.reserve(ctx, tx, &v); err != nil {
		return empty, err
	}
	if err := tx.Commit(); err != nil {
		return empty, err
	}
	return s.grant(&v, selected.Endpoint)
}
func nodeScore(n *Node, r AuthorizeRequest) float64 {
	latency := 1000.0
	if x, ok := r.Latencies[n.ID]; ok && x >= 0 && x <= 10000 {
		latency = x
	}
	var h edgeprotocol.Heartbeat
	_ = json.Unmarshal(n.Metrics, &h)
	return latency + h.ProviderLatencyMS + float64(n.Active)/float64(n.MaxConnections)*500 + h.Load*100
}
func selectNode(ctx context.Context, tx *sql.Tx, nodes []Node, req AuthorizeRequest, training bool) (Node, error) {
	for i := range nodes {
		n := &nodes[i]
		if req.Region != "" && req.Region != "auto" && req.Region != n.Region {
			continue
		}
		var eligible bool
		err := tx.QueryRowContext(ctx, `SELECT mode='enabled' AND training=$2 AND heartbeat_at>now()-interval '30 seconds' AND protocol_min<=$3 AND protocol_max>=$3 AND coalesce((metrics->>'healthy')::boolean,false) AND coalesce((metrics->>'load')::float,100)<0.95 AND (SELECT count(*) FROM edge_sessions WHERE node_id=edge_nodes.id AND status<>'closed' AND lease_until>now())<max_connections FROM edge_nodes WHERE id=$1 FOR UPDATE SKIP LOCKED`, n.ID, training, req.Protocol).Scan(&eligible)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return Node{}, err
		}
		if eligible {
			return *n, nil
		}
	}
	return Node{}, ErrUnavailable
}
