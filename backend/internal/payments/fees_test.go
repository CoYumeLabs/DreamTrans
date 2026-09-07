package payments

import (
	"github.com/stripe/stripe-go/v81"
	"testing"
)

func TestActualPaymentFeeUsesSettlementCurrency(t *testing.T) {
	usd, err := feeFromBalanceTransaction(&stripe.BalanceTransaction{ID: "txn_usd", Amount: 2000, Fee: 88, Currency: stripe.CurrencyUSD}, 20)
	if err != nil || usd.USD != .88 {
		t.Fatalf("USD fee=%+v %v", usd, err)
	}
	cad, err := feeFromBalanceTransaction(&stripe.BalanceTransaction{ID: "txn_cad", Amount: 2700, Fee: 135, Currency: stripe.CurrencyCAD}, 20)
	if err != nil || cad.USD != 1 || cad.MinorUnits != 135 {
		t.Fatalf("CAD fee=%+v %v", cad, err)
	}
	if _, err = feeFromBalanceTransaction(&stripe.BalanceTransaction{ID: "txn_pending", Currency: stripe.CurrencyUSD}, 20); err == nil {
		t.Fatal("unknown fee treated as zero")
	}
}
