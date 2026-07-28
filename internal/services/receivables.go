package services

import (
	"time"

	"arcusinvest/internal/models"
)

// VATRate is the Zambian standard rate. The frontend has its own copy in
// DocumentView.tsx for rendering documents; if the rate changes, both move.
const VATRate = 0.16

// Ageing bucket boundaries, in days past the invoice date.
const (
	AgeBucket30 = 30
	AgeBucket60 = 60
	AgeBucket90 = 90
)

// InvoicedTotal is what a deal was billed: its line items, or the deal value
// when nothing is itemised, plus VAT when the invoice carried it.
//
// This mirrors what the document generator puts on the invoice — a receivable
// that does not match the document the client received is worse than no figure
// at all.
func InvoicedTotal(o models.Opportunity) float64 {
	subtotal := 0.0
	for _, li := range o.LineItems {
		subtotal += li.Quantity * li.UnitPrice
	}
	// The generated documents fall back to a single line at the deal value when
	// a deal has no line items, so the total has to agree.
	if subtotal == 0 {
		subtotal = o.DealValue
	}
	if o.ApplyVat {
		subtotal += subtotal * VATRate
	}
	return subtotal
}

// PaidTotal is the sum of payments recorded against a deal.
func PaidTotal(payments []models.Payment) float64 {
	var total float64
	for _, p := range payments {
		total += p.Amount
	}
	return total
}

// Outstanding is what is still owed. It is always computed, never stored: a
// persisted balance drifts the moment a payment or line item changes, and a
// stale number in a financial report is a liability.
//
// Overpayments clamp to zero rather than reporting a negative receivable — a
// credit is a different thing from a debt, and netting them off across an aged
// report hides both.
func Outstanding(o models.Opportunity, payments []models.Payment) float64 {
	balance := InvoicedTotal(o) - PaidTotal(payments)
	if balance < 0 {
		return 0
	}
	return balance
}

// AgeBucket names how overdue an invoice is, in the conventional
// current/30/60/90+ bands.
//
// Ageing runs from the invoice date, so a deal that has not been invoiced has
// no age. Anything not yet 30 days old — including a future-dated invoice — is
// "current".
func AgeBucket(invoicedAt *time.Time, now time.Time) string {
	if invoicedAt == nil {
		return "current"
	}
	days := int(now.Sub(*invoicedAt).Hours() / 24)
	switch {
	case days >= AgeBucket90:
		return "90+"
	case days >= AgeBucket60:
		return "60"
	case days >= AgeBucket30:
		return "30"
	default:
		return "current"
	}
}
