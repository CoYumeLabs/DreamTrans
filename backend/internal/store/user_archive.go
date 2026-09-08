package store

import (
	"context"
	"database/sql"
	"fmt"
)

// archiveUserTx removes product content and credentials while preserving the
// stable anonymous identity used by payments, refunds and agent settlements.
func archiveUserTx(ctx context.Context, tx *sql.Tx, userID string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE billing_accounts SET status='suspended',auto_topup_threshold_usd=NULL,auto_topup_amount_usd=NULL WHERE id=(SELECT billing_account_id FROM users WHERE id=$1)`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_profiles SET status='suspended',updated_at=NOW() WHERE user_id=$1`, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE batch_submissions SET title='Deleted account',transcript=NULL WHERE user_id=$1`, userID); err != nil {
		return err
	}
	// Blob paths have already been queued by the caller before any rows vanish.
	// These tables contain user content or credentials, never financial entries.
	for _, statement := range []string{`DELETE FROM refresh_tokens WHERE user_id=$1`, `DELETE FROM email_verification_tokens WHERE user_id=$1`, `DELETE FROM translation_request_results WHERE user_id=$1`, `DELETE FROM ai_generation_requests WHERE user_id=$1`, `DELETE FROM ai_artifacts WHERE user_id=$1`, `DELETE FROM session_ai_chunks WHERE user_id=$1`, `DELETE FROM knowledge_sources WHERE user_id=$1`, `DELETE FROM ai_projects WHERE user_id=$1`, `DELETE FROM sessions WHERE user_id=$1`, `DELETE FROM user_model_preferences WHERE user_id=$1`, `DELETE FROM transcription_completions WHERE user_id=$1`} {
		if _, err := tx.ExecContext(ctx, statement, userID); err != nil {
			return fmt.Errorf("erase account content: %w", err)
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE users SET deleted_at=NOW(),is_active=false,email='deleted+'||id::text||'@invalid.example',password_hash='!deleted',name='Deleted account',role='user',email_verified=false,last_login_at=NULL,training_opt_in=false,speechmatics_route=NULL,referral_code=NULL,admin_role_id=NULL,updated_at=NOW() WHERE id=$1`, userID)
	return err
}
