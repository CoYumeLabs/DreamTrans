package billing

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrAutoTopupRejected means the provider definitively rejected an attempt.
// Ambiguous network errors must reuse the durable attempt's idempotency key.
var ErrAutoTopupRejected = errors.New("automatic top-up rejected")

func (s *Service) maybeAutoTopup(ctx context.Context, userID string, force bool) error {
	if s.autoTopup == nil {
		return nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended('autotopup:'||$1,0))`, userID).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return nil
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, e := conn.ExecContext(c, `SELECT pg_advisory_unlock(hashtextextended('autotopup:'||$1,0))`, userID); e != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	acct, err := s.accountForUser(ctx, userID)
	if err != nil {
		return err
	}
	req := acct.autoTopupRequest()
	if req == nil || acct.Status != "active" {
		return nil
	}
	// The threshold is the wallet balance, as labeled in the account panel.
	if !force && (!acct.AutoTopupThreshold.Valid || acct.WalletUSD >= acct.AutoTopupThreshold.Float64) {
		return nil
	}
	var id, status, customer string
	var amount float64
	var created time.Time
	var paymentRequest []byte
	err = conn.QueryRowContext(ctx, `SELECT id,status,amount_usd,customer_id,created_at,payment_request FROM auto_topup_attempts WHERE account_id=$1 ORDER BY created_at DESC LIMIT 1`, acct.ID).Scan(&id, &status, &amount, &customer, &created, &paymentRequest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && status == "pending" && time.Since(created) > 23*time.Hour {
		_, saveErr := conn.ExecContext(ctx, `UPDATE auto_topup_attempts SET error='Payment outcome requires operator reconciliation; top up manually' WHERE id=$1`, id)
		if saveErr != nil {
			return saveErr
		}
		return fmt.Errorf("automatic payment outcome requires reconciliation")
	}
	if err == nil && status != "pending" && time.Since(created) < 5*time.Minute {
		return nil
	}
	if err != nil || status != "pending" {
		paymentRequest = nil
		amount = req.AmountUSD
		customer = req.StripeCustomerID
		if err := conn.QueryRowContext(ctx, `INSERT INTO auto_topup_attempts(account_id,amount_usd,customer_id) VALUES($1,$2,$3) RETURNING id`, acct.ID, amount, customer).Scan(&id); err != nil {
			return err
		}
	}
	req.AmountUSD = amount
	req.StripeCustomerID = customer
	req.IdempotencyKey = "autotopup:" + id
	req.PaymentRequest = paymentRequest
	req.SavePaymentRequest = func(c context.Context, request json.RawMessage) error {
		_, saveErr := conn.ExecContext(c, `UPDATE auto_topup_attempts SET payment_request=$2 WHERE id=$1 AND payment_request IS NULL`, id, []byte(request))
		return saveErr
	}
	callErr := s.autoTopup(ctx, *req)
	next := "succeeded"
	message := ""
	if callErr != nil {
		next = "pending"
		message = "Payment could not be completed. Retry or top up manually."
		if errors.Is(callErr, ErrAutoTopupRejected) {
			next = "failed"
		}
	}
	c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, saveErr := conn.ExecContext(c, `UPDATE auto_topup_attempts SET status=$2,error=$3,completed_at=CASE WHEN $2='pending' THEN NULL ELSE NOW() END WHERE id=$1`, id, next, message)
	if callErr != nil {
		return fmt.Errorf("automatic top-up: %w", callErr)
	}
	return saveErr
}
