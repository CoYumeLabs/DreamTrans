package billing

import "context"

func (s *Service) RecordSessionMetrics(ctx context.Context, connID, userID string, sessionID *string, p50, p90 float64, samples int, route string) error {
	if samples <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO session_metrics(conn_id,user_id,session_id,latency_p50_ms,latency_p90_ms,samples,route) SELECT $1,$2,(SELECT id FROM sessions WHERE id=$3 AND user_id=$2),$4,$5,$6,$7 ON CONFLICT(conn_id) DO NOTHING`, connID, userID, sessionID, p50, p90, samples, route)
	return err
}

// RecordPaymentFee preserves unknown fees as NULL until Stripe provides a
// settled balance transaction. Repeated webhook delivery fills the same row.
func (s *Service) RecordPaymentFee(ctx context.Context, objectID string, usd float64, currency string, minorUnits int64) error {
	if !finiteNonNegative(usd) || minorUnits < 0 {
		return invalidBillingInputf("invalid payment fee")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE payments SET fee_usd=$2,fee_currency=$3,fee_minor_units=$4,fee_recorded_at=NOW() WHERE stripe_object_id=$1 AND kind IN ('topup','membership')`, objectID, usd, currency, minorUnits)
	return err
}

func (s *Service) PaymentAmount(ctx context.Context, objectID string) (float64, error) {
	var amount float64
	err := s.db.QueryRowContext(ctx, `SELECT amount_usd FROM payments WHERE stripe_object_id=$1 AND kind IN ('topup','membership')`, objectID).Scan(&amount)
	return amount, err
}
