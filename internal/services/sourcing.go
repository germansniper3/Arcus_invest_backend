package services

import (
	"errors"
	"fmt"

	"arcusinvest/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	// ErrTooFewQuotes is returned when a decision needs more quotes than were
	// gathered and no single-source justification was given.
	ErrTooFewQuotes = errors.New("not enough quotes to decide")
	// ErrSingleSourceReasonRequired is returned when a decision proceeds on
	// fewer quotes than policy asks for without saying why.
	ErrSingleSourceReasonRequired = errors.New("a single-source reason is required")
	// ErrSelectionReasonRequired is returned when a winner is chosen with no
	// stated basis. A comparison with no reason recorded is not evidence.
	ErrSelectionReasonRequired = errors.New("a selection reason is required")
	// ErrQuoteNotOnRequest guards against selecting a quote from another
	// sourcing exercise.
	ErrQuoteNotOnRequest = errors.New("that quote is not on this sourcing request")
)

// ActiveSourcingPolicy returns the configured policy, or the permissive default
// when none has been set. It never returns an error for "not configured":
// an unconfigured control must not block work.
func ActiveSourcingPolicy(db *gorm.DB) (models.SourcingPolicy, error) {
	var policy models.SourcingPolicy
	err := db.Where("is_active = ?", true).Order("created_at DESC").First(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.DefaultSourcingPolicy(), nil
	}
	if err != nil {
		return models.DefaultSourcingPolicy(), err
	}
	return policy, nil
}

// ValidateSelection decides whether a sourcing request may be settled on the
// quote given, under the active policy.
//
// The rule, in one sentence: below the threshold nothing is required; above it a
// decision wants MinQuotes, and may proceed on fewer only when single-sourcing
// is permitted and a reason is recorded.
//
// The value tested is the WINNING quote in kwacha, not the highest or the
// average. It is the amount the business will actually commit, and it is the
// only one of the three that a supplier could later hold it to.
func ValidateSelection(
	policy models.SourcingPolicy,
	quotes []models.SupplierQuote,
	selectedID uuid.UUID,
	selectionReason string,
	singleSourceReason string,
) error {
	if selectionReason == "" {
		return ErrSelectionReasonRequired
	}

	var winner *models.SupplierQuote
	for i := range quotes {
		if quotes[i].ID == selectedID {
			winner = &quotes[i]
			break
		}
	}
	if winner == nil {
		return ErrQuoteNotOnRequest
	}

	// Below the threshold the policy has nothing to say.
	if winner.AmountZMW() < policy.MinAmountZMW {
		return nil
	}
	if len(quotes) >= policy.MinQuotes {
		return nil
	}

	if !policy.AllowSingleSource {
		return fmt.Errorf("%w: %d gathered, %d required above %.0f ZMW",
			ErrTooFewQuotes, len(quotes), policy.MinQuotes, policy.MinAmountZMW)
	}
	if singleSourceReason == "" {
		return fmt.Errorf("%w: %d gathered, %d required above %.0f ZMW",
			ErrSingleSourceReasonRequired, len(quotes), policy.MinQuotes, policy.MinAmountZMW)
	}
	return nil
}

// CheapestQuote returns the lowest quote in kwacha, which is the comparison's
// obvious answer and frequently the wrong one — lead time and payment terms
// decide plenty of real sourcing decisions. It exists so a screen can mark the
// cheapest without every caller re-deriving it, not to make the choice.
func CheapestQuote(quotes []models.SupplierQuote) *models.SupplierQuote {
	var best *models.SupplierQuote
	for i := range quotes {
		if best == nil || quotes[i].AmountZMW() < best.AmountZMW() {
			best = &quotes[i]
		}
	}
	return best
}
