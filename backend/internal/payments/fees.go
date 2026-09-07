package payments

import (
	"context"
	"fmt"
	"strings"

	"github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/invoice"
	"github.com/stripe/stripe-go/v81/paymentintent"
)

type PaymentFee struct {
	USD        float64
	Currency   string
	MinorUnits int64
}

// PaymentProcessingFee retrieves Stripe's settled fee, never an advertised
// percentage. Non-USD settlement fees use the payment's recorded USD basis.
// https://docs.stripe.com/expand/use-cases#stripe-fee-for-payment
func (c *StripeClient) PaymentProcessingFee(ctx context.Context, objectID string, paidUSD float64) (*PaymentFee, error) {
	if !c.Enabled() {
		return nil, ErrNotConfigured
	}
	var charge *stripe.Charge
	switch {
	case strings.HasPrefix(objectID, "pi_"):
		params := &stripe.PaymentIntentParams{}
		params.Context = ctx
		params.AddExpand("latest_charge.balance_transaction")
		intent, err := paymentintent.Get(objectID, params)
		if err != nil {
			return nil, err
		}
		charge = intent.LatestCharge
	case strings.HasPrefix(objectID, "in_"):
		params := &stripe.InvoiceParams{}
		params.Context = ctx
		params.AddExpand("charge.balance_transaction")
		item, err := invoice.Get(objectID, params)
		if err != nil {
			return nil, err
		}
		charge = item.Charge
	default:
		return nil, fmt.Errorf("unsupported payment reference")
	}
	if charge == nil || charge.BalanceTransaction == nil {
		return nil, fmt.Errorf("processing fee is not yet available")
	}
	return feeFromBalanceTransaction(charge.BalanceTransaction, paidUSD)
}
func feeFromBalanceTransaction(transaction *stripe.BalanceTransaction, paidUSD float64) (*PaymentFee, error) {
	if transaction.ID == "" || transaction.Amount <= 0 || transaction.Fee < 0 || paidUSD <= 0 {
		return nil, fmt.Errorf("invalid fee transaction")
	}
	amount := float64(transaction.Fee) / float64(transaction.Amount) * paidUSD
	if transaction.Currency == stripe.CurrencyUSD {
		amount = float64(transaction.Fee) / 100
	}
	return &PaymentFee{USD: amount, Currency: string(transaction.Currency), MinorUnits: transaction.Fee}, nil
}
