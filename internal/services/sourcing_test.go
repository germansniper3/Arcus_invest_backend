package services

import (
	"errors"
	"testing"

	"arcusinvest/internal/models"

	"github.com/google/uuid"
)

func quote(id uuid.UUID, supplier string, amount float64, rate float64) models.SupplierQuote {
	q := models.SupplierQuote{Supplier: supplier, Amount: amount, ExchangeRate: rate}
	q.ID = id
	return q
}

// Prevents: a control nobody asked for blocking the first purchase somebody
// tries to make. Out of the box the policy must obstruct nothing.
func TestDefaultPolicyObstructsNothing(t *testing.T) {
	p := models.DefaultSourcingPolicy()
	id := uuid.New()
	quotes := []models.SupplierQuote{quote(id, "Sole Agent Ltd", 90000, 1)}

	if err := ValidateSelection(p, quotes, id, "only distributor in-country", ""); err != nil {
		t.Errorf("default policy refused a single-quote decision: %v", err)
	}
}

// Prevents: a three-quote minimum being applied to a K400 part. The threshold
// is what lets the control be real without being an obstruction.
func TestBelowTheThresholdNoComparisonIsRequired(t *testing.T) {
	p := models.SourcingPolicy{MinQuotes: 3, MinAmountZMW: 50000, AllowSingleSource: true}
	id := uuid.New()
	quotes := []models.SupplierQuote{quote(id, "Parts Depot", 400, 1)}

	if err := ValidateSelection(p, quotes, id, "in stock locally", ""); err != nil {
		t.Errorf("a K400 decision was gated by a K50,000 policy: %v", err)
	}
}

// Prevents: a large single-sourced decision passing with no explanation. The
// justification IS the audit artefact — refusing outright would only push the
// purchase outside the system, so the rule asks for a reason, not abstinence.
func TestAboveTheThresholdSingleSourcingNeedsAReason(t *testing.T) {
	p := models.SourcingPolicy{MinQuotes: 3, MinAmountZMW: 50000, AllowSingleSource: true}
	id := uuid.New()
	quotes := []models.SupplierQuote{quote(id, "Sole Agent Ltd", 90000, 1)}

	err := ValidateSelection(p, quotes, id, "only supplier", "")
	if !errors.Is(err, ErrSingleSourceReasonRequired) {
		t.Errorf("got %v, want ErrSingleSourceReasonRequired", err)
	}

	// With the reason recorded it proceeds.
	if err := ValidateSelection(p, quotes, id, "only supplier",
		"Sole agent for this brand in Zambia; confirmed with two other dealers."); err != nil {
		t.Errorf("a justified single-source decision was refused: %v", err)
	}
}

// Prevents: the minimum being unenforceable for a buyer whose own procurement
// rules make it absolute.
func TestSingleSourcingCanBeForbiddenOutright(t *testing.T) {
	p := models.SourcingPolicy{MinQuotes: 3, MinAmountZMW: 50000, AllowSingleSource: false}
	id := uuid.New()
	quotes := []models.SupplierQuote{quote(id, "Sole Agent Ltd", 90000, 1)}

	err := ValidateSelection(p, quotes, id, "only supplier", "sole agent")
	if !errors.Is(err, ErrTooFewQuotes) {
		t.Errorf("got %v, want ErrTooFewQuotes — a reason must not override an absolute minimum", err)
	}
}

// Prevents: a decision recorded with no stated basis. Cheapest is a choice
// someone should have to write down when it is not the one they made.
func TestEverySelectionNeedsAReason(t *testing.T) {
	p := models.DefaultSourcingPolicy()
	id := uuid.New()
	quotes := []models.SupplierQuote{quote(id, "A", 100, 1), quote(uuid.New(), "B", 90, 1)}

	if err := ValidateSelection(p, quotes, id, "", ""); !errors.Is(err, ErrSelectionReasonRequired) {
		t.Errorf("got %v, want ErrSelectionReasonRequired", err)
	}
}

// Prevents: settling a request on a quote gathered for a different one.
func TestSelectedQuoteMustBeOnTheRequest(t *testing.T) {
	p := models.DefaultSourcingPolicy()
	quotes := []models.SupplierQuote{quote(uuid.New(), "A", 100, 1)}

	err := ValidateSelection(p, quotes, uuid.New(), "cheapest", "")
	if !errors.Is(err, ErrQuoteNotOnRequest) {
		t.Errorf("got %v, want ErrQuoteNotOnRequest", err)
	}
}

// Prevents: the threshold being tested against the wrong figure. It is the
// winning quote that the business commits to, so that is the one measured —
// not the highest, and not the average.
func TestThresholdIsTestedAgainstTheWinningQuote(t *testing.T) {
	p := models.SourcingPolicy{MinQuotes: 3, MinAmountZMW: 50000, AllowSingleSource: true}
	cheap := uuid.New()
	quotes := []models.SupplierQuote{
		quote(cheap, "Cheap Ltd", 40000, 1),
		quote(uuid.New(), "Dear Ltd", 900000, 1),
	}

	// Two quotes, below the three the policy wants — but the winner is under the
	// threshold, so the policy does not apply.
	if err := ValidateSelection(p, quotes, cheap, "cheapest and in stock", ""); err != nil {
		t.Errorf("a K40,000 winner was gated by a K50,000 policy: %v", err)
	}
}

// Prevents: quotes in different currencies being compared at face value. USD
// 4,000 is not cheaper than ZMW 150,000.
func TestQuotesAreComparedInKwacha(t *testing.T) {
	usd := quote(uuid.New(), "Guangzhou Power", 4000, 25) // K100,000
	zmw := quote(uuid.New(), "Lusaka Traders", 150000, 1) // K150,000

	if usd.AmountZMW() != 100000 {
		t.Errorf("USD quote in kwacha = %v, want 100000", usd.AmountZMW())
	}
	best := CheapestQuote([]models.SupplierQuote{zmw, usd})
	if best == nil || best.Supplier != "Guangzhou Power" {
		t.Errorf("cheapest = %v, want the USD quote once converted", best)
	}
}
