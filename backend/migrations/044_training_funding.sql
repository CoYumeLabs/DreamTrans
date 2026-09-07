-- Gift versus paid funding. Money the customer did not pay for (trial, promo,
-- adjustments) is spent first at the standard price through the no-training
-- provider account; top-up bonuses count as paid.
ALTER TABLE grants ADD COLUMN funding VARCHAR(8) NOT NULL DEFAULT 'gift' CHECK (funding IN ('gift', 'paid'));
UPDATE grants SET funding = 'paid' WHERE kind = 'topup_bonus';

ALTER TABLE usage_logs
    ADD COLUMN gift_usd DECIMAL(18,8) NOT NULL DEFAULT 0 CHECK (gift_usd >= 0),
    ADD COLUMN funding_route VARCHAR(8) CHECK (funding_route IN ('gift', 'paid')),
    ADD COLUMN training_route VARCHAR(10) CHECK (training_route IN ('training', 'standard'));
-- Historic grant debits: the gift part is whatever did not come from a paid grant.
UPDATE usage_logs l SET gift_usd = LEAST(l.grant_usd, COALESCE((
    SELECT -SUM(bt.amount) FROM balance_transactions bt JOIN grants g ON g.id = bt.grant_id
    WHERE bt.reference_type = 'usage' AND bt.reference_id = l.id AND bt.bucket = 'grant'
      AND bt.transaction_type = 'debit' AND g.funding = 'gift'), 0))
WHERE l.grant_usd > 0;
CREATE INDEX usage_logs_idempotency_prefix ON usage_logs (user_id, idempotency_key text_pattern_ops) WHERE idempotency_key IS NOT NULL;

-- Institution tenants never reach the training account; administrators can
-- also pin a tenant or a single user to one provider account.
ALTER TABLE tenants
    ADD COLUMN kind VARCHAR(20) NOT NULL DEFAULT 'personal' CHECK (kind IN ('personal', 'institution')),
    ADD COLUMN speechmatics_route VARCHAR(10) CHECK (speechmatics_route IN ('training', 'standard'));
ALTER TABLE users ADD COLUMN speechmatics_route VARCHAR(10) CHECK (speechmatics_route IN ('training', 'standard'));

-- Snapshots for the programme statistics: the answer when a promotion reward
-- was claimed, the answer at each payment, and every change in between.
ALTER TABLE promotion_registrations ADD COLUMN training_opt_in_at_claim BOOLEAN;
ALTER TABLE payments ADD COLUMN training_opt_in BOOLEAN;
CREATE TABLE training_opt_in_changes (
    id BIGSERIAL PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    opt_in BOOLEAN NOT NULL,
    changed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX training_opt_in_changes_user ON training_opt_in_changes(user_id, changed_at);

-- One refund per gift-routed session: the discount the paid part would have
-- earned had the session been priced for the customer's own choices.
CREATE TABLE route_discount_refunds (
    key TEXT PRIMARY KEY,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_id UUID NOT NULL REFERENCES billing_accounts(id) ON DELETE CASCADE,
    paid_usd DECIMAL(18,8) NOT NULL,
    discount_percent DECIMAL(6,3) NOT NULL,
    amount_usd DECIMAL(18,8) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX route_discount_refunds_user ON route_discount_refunds(user_id, created_at DESC);
