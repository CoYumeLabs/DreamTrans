package edgecontrol

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/dreamtrans/backend/internal/auth"
	"github.com/dreamtrans/backend/internal/edgeprotocol"
)

var ErrRateLimited = errors.New("provider credential rate limit")

// Match main-site account routing. A training node requires both independently
// configured accounts; the legacy single-account deployment remains non-training.
func speechmaticsAccount(training bool) string {
	normal := strings.TrimSpace(os.Getenv("SM_API_KEY_NO_TRAINING"))
	primary := strings.TrimSpace(os.Getenv("SM_API_KEY"))
	if training {
		if normal == "" {
			return ""
		}
		return primary
	}
	if normal != "" {
		return normal
	}
	return primary
}
func mintSpeechmatics(ctx context.Context, training bool) (string, error) {
	generator, err := auth.NewTokenGeneratorForKey(speechmaticsAccount(training))
	if err != nil {
		return "", errors.New("provider account unavailable")
	}
	token, err := generator.GenerateTokenTTLContext(ctx, 60)
	if err != nil {
		return "", errors.New("provider temporary credential unavailable")
	}
	return token, nil
}

// ProviderCredential is accessible only with a node identity. It reserves an
// issuance attempt transactionally across blue/green main instances before
// contacting the provider, so retries and provider failures cannot bypass limits.
// No network calls hold database locks. Credentials remain outside the database.
func (s *Service) ProviderCredential(ctx context.Context, node, identity string, req edgeprotocol.ProviderCredentialRequest) (edgeprotocol.ProviderCredential, error) {
	var result edgeprotocol.ProviderCredential
	if (req.Probe && (req.SessionID != "" || req.Generation != 0)) || (!req.Probe && (req.SessionID == "" || req.Generation < 1)) {
		return result, ErrUnauthorized
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	var maximum int
	var training bool
	if err := tx.QueryRowContext(ctx, `SELECT max_connections,training FROM edge_nodes WHERE id=$1 AND identity_hash=$2 AND mode<>'revoked' FOR UPDATE`, node, edgeprotocol.Hash(identity)).Scan(&maximum, &training); err != nil {
		return result, ErrUnauthorized
	}
	if !req.Probe {
		if err := credentialSession(ctx, tx, node, req, training); err != nil {
			return result, err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM edge_audit WHERE node_id=$1 AND action='provider_credential_requested' AND created_at>now()-interval '1 minute'`, node).Scan(&count); err != nil {
		return result, err
	}
	if count >= maximum*4+16 {
		return result, ErrRateLimited
	}
	if req.Probe {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM edge_audit WHERE node_id=$1 AND action='provider_credential_requested' AND details->>'probe'='true' AND created_at>now()-interval '1 minute'`, node).Scan(&count); err != nil {
			return result, err
		}
		// Two colors may each perform a readiness probe while an upstream is down.
		if count >= 16 {
			return result, ErrRateLimited
		}
	} else {
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM edge_audit WHERE node_id=$1 AND action='provider_credential_requested' AND details->>'session_id'=$2 AND details->>'generation'=$3`, node, req.SessionID, jsonNumber(req.Generation)).Scan(&count); err != nil {
			return result, err
		}
		if count >= 3 {
			return result, ErrRateLimited
		}
	}
	details, err := json.Marshal(req)
	if err != nil {
		return result, err
	}
	if err := audit(ctx, tx, node, "node:"+node, "provider_credential_requested", string(details)); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	issued := time.Now()
	token, err := s.mintProvider(ctx, training)
	if err != nil {
		return result, err
	}
	// Re-check revocation, rotation and fencing after the external mint request.
	current, err := s.Authenticate(ctx, identity)
	if err != nil || current != node {
		return result, ErrUnauthorized
	}
	if !req.Probe {
		if err := credentialSession(ctx, s.DB, node, req, training); err != nil {
			return result, err
		}
	}
	if token == "" || time.Since(issued) > 30*time.Second {
		return result, errors.New("provider credential expired during issuance")
	}
	return edgeprotocol.ProviderCredential{JWT: token, ExpiresAt: issued.Add(time.Minute).Unix(), Training: training}, nil
}

type credentialQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func credentialSession(ctx context.Context, db credentialQuery, node string, req edgeprotocol.ProviderCredentialRequest, training bool) error {
	var valid bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM edge_sessions WHERE id=$1 AND node_id=$2 AND generation=$3 AND status='connected' AND lease_until>now() AND approved_samples>consumed_samples AND training=$4)`, req.SessionID, node, req.Generation, training).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrConflict
	}
	return nil
}
func jsonNumber(v int64) string { b, _ := json.Marshal(v); return string(b) }
