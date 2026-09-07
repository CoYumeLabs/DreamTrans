package risk

import (
	"context"
	"database/sql"
	"fmt"
	"math"
)

// ReserveRewardTx must follow account/profile locks. The spend entry and grant
// commit together; a failed grant rolls back its budget reservation. Human
// approval never bypasses this cap. Legacy/admin-created users are excluded.
func ReserveRewardTx(ctx context.Context, tx *sql.Tx, userID, kind string, amount float64) (bool, error) {
	switch kind {
	case "trial", "promotion", "promotion_topup", "promotion_session":
	default:
		return false, ErrInput
	}
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
		return false, ErrInput
	}
	var profile string
	err := tx.QueryRowContext(ctx, `SELECT id FROM signup_risk_profiles WHERE user_id=$1 FOR UPDATE`, userID).Scan(&profile)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(731820260908)`); err != nil {
		return false, err
	}
	key := userID + ":" + kind
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM signup_risk_reward_spend WHERE receipt_key=$1)`, key).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return true, nil
	}
	var fits bool
	// PostgreSQL decimal arithmetic matches the six-decimal billing ledger.
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT SUM(amount_usd) FROM signup_risk_reward_spend WHERE created_at>NOW()-INTERVAL '24 hours'),0)+$1::numeric <= daily_reward_budget_cents::numeric/100 FROM signup_risk_settings WHERE singleton`, fmt.Sprintf("%.6f", amount)).Scan(&fits); err != nil {
		return false, err
	}
	if !fits {
		_, err = tx.ExecContext(ctx, `UPDATE signup_risk_profiles SET budget_blocked=TRUE,budget_holds=CASE WHEN budget_holds ? $2 THEN budget_holds ELSE budget_holds || jsonb_build_array($2::text) END WHERE id=$1`, profile, kind)
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO signup_risk_reward_spend(receipt_key,amount_usd) VALUES($1,$2::numeric)`, key, fmt.Sprintf("%.6f", amount)); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE signup_risk_profiles SET budget_holds=budget_holds-$2::text,budget_blocked=(budget_holds-$2::text <> '[]'::jsonb) WHERE id=$1`, profile, kind)
	return true, err
}

type Budget struct {
	LimitCents int64  `json:"limit_cents"`
	SpentUSD   string `json:"spent_usd"`
	Blocked    int    `json:"blocked"`
}

func (s *Service) Budget(ctx context.Context) (Budget, error) {
	var b Budget
	err := s.db.QueryRowContext(ctx, `SELECT daily_reward_budget_cents,COALESCE((SELECT SUM(amount_usd)::text FROM signup_risk_reward_spend WHERE created_at>NOW()-INTERVAL '24 hours'),'0'),(SELECT COUNT(*) FROM signup_risk_profiles WHERE budget_blocked AND user_id IS NOT NULL) FROM signup_risk_settings WHERE singleton`).Scan(&b.LimitCents, &b.SpentUSD, &b.Blocked)
	return b, err
}
