package services

import (
	"math"
	"testing"
	"time"

	"arcusinvest/internal/models"
)

func approx(t *testing.T, got, want float64, label string) {
	t.Helper()
	if math.Abs(got-want) > 0.005 {
		t.Errorf("%s = %.4f, want %.4f", label, got, want)
	}
}

// The invoiced total must equal what the document generator prints, or the
// receivables report contradicts the invoice the client is holding.
func TestInvoicedTotalMatchesTheGeneratedInvoice(t *testing.T) {
	itemised := models.Opportunity{
		DealValue: 9999, // must be ignored once line items exist
		LineItems: []models.OpportunityLineItem{
			{Quantity: 2, UnitPrice: 1500},
			{Quantity: 1, UnitPrice: 500},
		},
	}
	approx(t, InvoicedTotal(itemised), 3500, "itemised total")

	// The generator falls back to a single line at the deal value when nothing
	// is itemised, so this must too.
	approx(t, InvoicedTotal(models.Opportunity{DealValue: 4200}), 4200, "unitemised total")

	withVat := models.Opportunity{DealValue: 1000, ApplyVat: true}
	approx(t, InvoicedTotal(withVat), 1160, "total with 16% VAT")
}

// A stored balance drifts the moment a payment or a line item changes, so it is
// always derived. These pin the arithmetic.
func TestOutstandingSubtractsPayments(t *testing.T) {
	deal := models.Opportunity{DealValue: 5000}
	payments := []models.Payment{{Amount: 1500}, {Amount: 500}}
	approx(t, Outstanding(deal, payments), 3000, "outstanding")
	approx(t, Outstanding(deal, nil), 5000, "outstanding with no payments")
}

// An overpaid deal is a credit, not a negative debt. Letting it go negative
// would net it off against other accounts in the aged report and understate the
// real total owed — hiding both the credit and the debt.
func TestOutstandingNeverGoesNegative(t *testing.T) {
	deal := models.Opportunity{DealValue: 1000}
	over := []models.Payment{{Amount: 2500}}
	if got := Outstanding(deal, over); got != 0 {
		t.Errorf("Outstanding with an overpayment = %.2f, want 0", got)
	}
}

func TestAgeBucketBoundaries(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	at := func(daysAgo int) *time.Time {
		d := now.AddDate(0, 0, -daysAgo)
		return &d
	}

	cases := []struct {
		days int
		want string
	}{
		{0, "current"}, {29, "current"},
		{30, "30"}, {59, "30"},
		{60, "60"}, {89, "60"},
		{90, "90+"}, {365, "90+"},
	}
	for _, tc := range cases {
		if got := AgeBucket(at(tc.days), now); got != tc.want {
			t.Errorf("AgeBucket(%d days ago) = %q, want %q", tc.days, got, tc.want)
		}
	}

	// Never invoiced means never overdue — it is not a receivable at all.
	if got := AgeBucket(nil, now); got != "current" {
		t.Errorf("AgeBucket(nil) = %q, want current", got)
	}
	// A future-dated invoice must not read as overdue.
	future := now.AddDate(0, 0, 14)
	if got := AgeBucket(&future, now); got != "current" {
		t.Errorf("AgeBucket(future) = %q, want current", got)
	}
}
