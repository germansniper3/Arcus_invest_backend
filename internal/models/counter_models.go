package models

import (
	"time"

	"github.com/google/uuid"
)

// Counter payment methods.
//
// Mobile money is first-class rather than an "other", because it is not a
// fringe method in Zambia: mobile money volumes reached K486 billion in 2024,
// and a counter that cannot record it separately from cash cannot be reconciled
// against the float at the end of a shift.
const (
	CounterCash         = "cash"
	CounterMobileMoney  = "mobile_money"
	CounterCard         = "card"
	CounterBankTransfer = "bank_transfer"
)

var AllCounterMethods = []string{CounterCash, CounterMobileMoney, CounterCard, CounterBankTransfer}

// Credit is deliberately absent from the list above.
//
// Selling on account is a relationship, not a transaction: it needs terms, a
// credit limit, ageing and someone chasing it. All of that already exists on
// the deal side, where an invoiced opportunity becomes a receivable. Adding a
// fifth counter method called "credit" would create a second, weaker debtor
// book that nobody ages and nobody chases. A counter operator asked for credit
// is meant to raise a deal instead.

const (
	TillOpen   = "open"
	TillClosed = "closed"
)

// TillSession is one shift at the counter: opened with a float, closed with a
// count.
//
// It exists because cash is the one payment method that can leave without a
// record. Card, transfer and mobile money all leave a trace at the provider;
// notes in a drawer do not, so the only control is counting them against what
// the system says should be there.
type TillSession struct {
	BaseModel
	Status string `json:"status" gorm:"index;not null;default:'open'"`

	OpenedAt     time.Time  `json:"opened_at"`
	OpenedByID   *uuid.UUID `json:"opened_by_id" gorm:"type:uuid"`
	OpenedBy     string     `json:"opened_by"`
	OpeningFloat float64    `json:"opening_float"`

	ClosedAt   *time.Time `json:"closed_at"`
	ClosedByID *uuid.UUID `json:"closed_by_id" gorm:"type:uuid"`
	ClosedBy   string     `json:"closed_by"`
	// CountedCash is what was physically in the drawer at close. Nullable
	// because an open session has not been counted yet, and zero is a real
	// count that must not be confused with "not counted".
	CountedCash *float64 `json:"counted_cash"`

	Note string `json:"note" gorm:"type:text"`
}

// CounterSale is one over-the-counter transaction: goods handed over, money
// taken, immediately.
//
// This is intentionally not the deal pipeline. A deal is a negotiation with
// stages, a quotation, approvals and a contract; a counter sale is a person at
// a till who wants two metres of cable. Modelling the second as the first would
// bury genuine pipeline in noise, which is why this has its own table, its own
// permission and no stages at all.
type CounterSale struct {
	BaseModel
	TillSessionID uuid.UUID `json:"till_session_id" gorm:"type:uuid;index;not null"`

	// CustomerName and CustomerTPIN are optional: most walk-ins are anonymous.
	// A buyer who is VAT-registered and wants to reclaim the VAT needs their
	// TPIN on the invoice, so the field is here to be filled when asked for.
	CustomerName string `json:"customer_name"`
	CustomerTPIN string `json:"customer_tpin"`

	ApplyVat      bool   `json:"apply_vat"`
	PaymentMethod string `json:"payment_method" gorm:"index;not null"`
	// AmountTendered is what the customer handed over, so change can be shown
	// and the drawer reconciled. Only meaningful for cash.
	AmountTendered float64 `json:"amount_tendered"`
	// Reference is the mobile money transaction id, card slip or transfer
	// reference — the trace that makes a non-cash line checkable.
	Reference string `json:"reference"`
	// SmartInvoiceRef is the ZRA Mark ID once the sale has been put through
	// Smart Invoice. It is recorded rather than generated: this system is not
	// an approved Smart Invoice provider, and pretending otherwise would
	// produce invoices that are not valid tax documents.
	SmartInvoiceRef string `json:"smart_invoice_ref"`

	Note     string     `json:"note" gorm:"type:text"`
	SoldByID *uuid.UUID `json:"sold_by_id" gorm:"type:uuid"`
	SoldBy   string     `json:"sold_by"`

	Lines []CounterSaleLine `json:"lines" gorm:"foreignKey:CounterSaleID;constraint:OnDelete:CASCADE"`
}

// CounterSaleLine is one item on a counter sale.
//
// UnitPrice is copied off the product at the moment of sale rather than joined
// at read time. A price list changes; what the customer actually paid does not,
// and a receipt that reprices itself later is not a receipt.
type CounterSaleLine struct {
	BaseModel
	CounterSaleID uuid.UUID `json:"counter_sale_id" gorm:"type:uuid;index;not null"`
	// ProductID is nullable so a one-off item can be sold without polluting the
	// catalogue. Only lines with a product move stock.
	ProductID   *uuid.UUID `json:"product_id" gorm:"type:uuid;index"`
	Description string     `json:"description" gorm:"not null"`
	Quantity    int        `json:"quantity" gorm:"not null"`
	UnitPrice   float64    `json:"unit_price"`
}

// Subtotal is the sale before VAT.
func (s CounterSale) Subtotal() float64 {
	var total float64
	for _, l := range s.Lines {
		total += float64(l.Quantity) * l.UnitPrice
	}
	return total
}

// Vat is the tax charged on the sale, at the Zambian standard rate.
func (s CounterSale) Vat() float64 {
	if !s.ApplyVat {
		return 0
	}
	return s.Subtotal() * VatRate
}

// Total is what the customer pays.
func (s CounterSale) Total() float64 { return s.Subtotal() + s.Vat() }

// Change is what is handed back. Non-cash methods are taken for the exact
// amount, so there is nothing to return.
func (s CounterSale) Change() float64 {
	if s.PaymentMethod != CounterCash {
		return 0
	}
	change := s.AmountTendered - s.Total()
	if change < 0 {
		return 0
	}
	return change
}
