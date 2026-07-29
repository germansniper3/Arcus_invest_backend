package models

import (
	"time"

	"github.com/google/uuid"
)

// Sourcing request lifecycle.
const (
	SourcingOpen      = "open"      // gathering quotes
	SourcingDecided   = "decided"   // a supplier has been selected
	SourcingCancelled = "cancelled" // the requirement went away
)

var AllSourcingStatuses = []string{SourcingOpen, SourcingDecided, SourcingCancelled}

// SourcingRequest is one requirement being priced, and the record of who was
// asked and who was chosen.
//
// Its first purpose is evidence, not cost control. For supply into government
// and parastatal buyers a documented comparison is an audit artefact — the
// buyer's own procurement rules oblige them to show their suppliers were
// selected on some basis, and "we always use this supplier" is not one. Price
// discipline is the second benefit and the smaller of the two.
type SourcingRequest struct {
	BaseModel

	Title       string `json:"title" gorm:"not null"`
	Description string `json:"description" gorm:"type:text"`

	// OpportunityID ties the sourcing exercise to the deal it prices, so the
	// eventual margin can be traced back to what was actually shopped around.
	OpportunityID *uuid.UUID `json:"opportunity_id" gorm:"type:uuid;index"`

	Status     string     `json:"status" gorm:"index;not null;default:'open'"`
	RequiredBy *time.Time `json:"required_by"`

	// SelectedQuoteID is the quote that won.
	SelectedQuoteID *uuid.UUID `json:"selected_quote_id" gorm:"type:uuid;index"`
	// SelectionReason is why. It is required on every selection, not only on a
	// single-source one, because "cheapest" is a decision someone should have to
	// write down when it is not — lead time and payment terms routinely beat
	// headline price, and a comparison with no stated basis is not evidence.
	SelectionReason string `json:"selection_reason" gorm:"type:text"`
	// SingleSourceReason is the justification for deciding on fewer quotes than
	// policy asks for. Stored separately from SelectionReason so an auditor can
	// find every single-sourced decision without reading prose.
	SingleSourceReason string     `json:"single_source_reason" gorm:"type:text"`
	DecidedAt          *time.Time `json:"decided_at"`

	RaisedByID *uuid.UUID `json:"raised_by_id" gorm:"type:uuid"`
	RaisedBy   string     `json:"raised_by"`

	Quotes []SupplierQuote `json:"quotes" gorm:"foreignKey:SourcingRequestID;constraint:OnDelete:CASCADE"`
}

// SupplierQuote is one supplier's answer to a sourcing request.
//
// It carries its own currency and rate for the same reason a purchase order
// does: comparing a USD quote against a ZMW one at an unrecorded rate is how a
// comparison becomes an argument nobody can settle later.
type SupplierQuote struct {
	BaseModel
	SourcingRequestID uuid.UUID `json:"sourcing_request_id" gorm:"type:uuid;index;not null"`

	Supplier     string `json:"supplier" gorm:"index;not null"`
	SupplierTPIN string `json:"supplier_tpin"`
	Reference    string `json:"reference"`

	Currency     string  `json:"currency" gorm:"not null;default:'ZMW'"`
	ExchangeRate float64 `json:"exchange_rate" gorm:"not null;default:1"`
	Amount       float64 `json:"amount"`

	// LeadTimeDays and ValidUntil are the two fields that most often decide a
	// comparison against the headline price. A cheaper quote that lands six
	// weeks late is not the cheaper quote.
	LeadTimeDays int        `json:"lead_time_days"`
	ValidUntil   *time.Time `json:"valid_until"`
	PaymentTerms string     `json:"payment_terms"`

	Notes string `json:"notes" gorm:"type:text"`

	RecordedByID *uuid.UUID `json:"recorded_by_id" gorm:"type:uuid"`
	RecordedBy   string     `json:"recorded_by"`
}

// AmountZMW restates the quote in kwacha at its own recorded rate, which is what
// makes quotes in different currencies comparable at all.
func (q SupplierQuote) AmountZMW() float64 { return q.Amount * q.ExchangeRate }

// SourcingPolicy is how many quotes a decision needs, and above what value.
//
// It is configuration rather than a constant on purpose. A three-quote minimum
// is a real control in a large organisation and an obstruction in a small one:
// a great many parts in Zambia have a single distributor in-country, often the
// sole agent for the brand, and demanding three quotes for one of those does not
// produce competition — it produces two fabricated quotes, which is worse than
// none because it looks like evidence.
//
// The threshold is the mechanism that makes both true at once. Below
// MinAmountZMW nothing is required; above it, a decision needs MinQuotes — and
// may still proceed on fewer, provided somebody records why. That justification
// is the audit artefact. Refusing the decision outright would simply move the
// purchase outside the system.
type SourcingPolicy struct {
	BaseModel
	MinQuotes    int     `json:"min_quotes" gorm:"not null;default:3"`
	MinAmountZMW float64 `json:"min_amount_zmw" gorm:"not null;default:0"`
	// AllowSingleSource permits a decision on fewer quotes with a recorded
	// reason. Turning it off makes the minimum absolute — available for a buyer
	// whose own procurement rules require it, and not the default.
	AllowSingleSource bool   `json:"allow_single_source" gorm:"not null;default:true"`
	IsActive          bool   `json:"is_active" gorm:"index;default:true"`
	Note              string `json:"note" gorm:"type:text"`
}

// DefaultSourcingPolicy is what applies when nothing has been configured.
//
// MinAmountZMW is 0 and MinQuotes is 1, i.e. nothing is obstructed out of the
// box. A control the client has not asked for should not start switched on and
// block the first order somebody tries to place.
func DefaultSourcingPolicy() SourcingPolicy {
	return SourcingPolicy{MinQuotes: 1, MinAmountZMW: 0, AllowSingleSource: true, IsActive: true}
}
