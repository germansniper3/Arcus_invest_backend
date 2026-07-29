package models

import (
	"time"

	"github.com/google/uuid"
)

// Purchase order status.
//
// The flow is deliberately small. Every state here answers a question someone
// actually asks — "has anyone authorised this?", "has it been sent?", "is any
// of it still at sea?" — and no state exists merely to be tidy.
//
// Only POApproved may be issued, and only POIssued may receive goods. That is
// enforced on the server in the handler, not in the UI, because a status flow
// a client can skip is not a control.
const (
	POStatusDraft           = "draft"
	POStatusPendingApproval = "pending_approval"
	POStatusApproved        = "approved"
	POStatusRejected        = "rejected"
	POStatusIssued          = "issued"
	POStatusPartlyReceived  = "partly_received"
	POStatusReceived        = "received"
	POStatusClosed          = "closed"
	POStatusCancelled       = "cancelled"
)

var AllPurchaseOrderStatuses = []string{
	POStatusDraft, POStatusPendingApproval, POStatusApproved, POStatusRejected,
	POStatusIssued, POStatusPartlyReceived, POStatusReceived, POStatusClosed,
	POStatusCancelled,
}

// POStatusIsOpen reports whether an order is still expecting goods. Used for
// the outstanding-commitment figure, which is money promised but not yet owed
// and therefore invisible in the payables ledger.
func POStatusIsOpen(status string) bool {
	switch status {
	case POStatusIssued, POStatusPartlyReceived:
		return true
	}
	return false
}

// PurchaseOrder is the document that sits between "we decided to buy" and
// "goods arrived".
//
// It is not an expense and it is not stock. Committing to buy, taking delivery
// and being invoiced are three separate events that happen at different times
// and in different quantities — a consignment can arrive in two shipments and
// be invoiced once, or be paid for months before it ships. Collapsing them
// would produce the familiar failure where goods cannot be received because the
// supplier's invoice has not arrived. See GoodsReceipt and Expense for the
// other two.
type PurchaseOrder struct {
	BaseModel

	// Number is the human reference quoted to the supplier. Allocated on issue
	// rather than on create, so abandoned drafts do not consume numbers from a
	// sequence that an auditor expects to be unbroken.
	Number string `json:"number" gorm:"index"`

	Supplier string `json:"supplier" gorm:"index;not null"`
	// SupplierTPIN is the supplier's ZRA taxpayer number, carried for the same
	// reason Expense carries it: its absence signals a document that will not
	// support a VAT claim. Empty for a foreign supplier, who has no TPIN.
	SupplierTPIN string `json:"supplier_tpin"`

	// Currency is the ISO code the supplier priced in. Line unit prices are in
	// this currency, never converted on the way in.
	//
	// Arcus imports, so this is routinely USD, ZAR, CNY or EUR. Storing the
	// foreign figure alongside the rate and the resulting kwacha — rather than
	// only the converted number — is what makes the kwacha figure explainable
	// later. A converted-only amount cannot be checked against the supplier's
	// invoice and cannot be restated when the rate used turns out to be wrong.
	Currency string `json:"currency" gorm:"not null;default:'ZMW'"`
	// ExchangeRate is kwacha per one unit of Currency, as used to state this
	// order in ZMW. Exactly 1 for a ZMW order.
	//
	// This is the ordering rate and it is not the rate that ends up in cost:
	// customs assess duty at their own published rate and the bank fills at a
	// third on the day of payment. The receipt carries its own rate for that
	// reason — see GoodsReceipt.ExchangeRate.
	ExchangeRate float64 `json:"exchange_rate" gorm:"not null;default:1"`

	Status string `json:"status" gorm:"index;not null;default:'draft'"`

	OrderDate        time.Time  `json:"order_date" gorm:"index"`
	ExpectedDelivery *time.Time `json:"expected_delivery" gorm:"index"`

	// Incoterms states where the supplier's responsibility ends and Arcus's
	// begins — EXW, FOB, CIF, DDP. On an import it is the single most useful
	// field for predicting which landed-cost components are still to come: on
	// DDP the price already includes freight and duty, on EXW almost nothing is
	// included and the whole apportionment is still ahead.
	Incoterms    string `json:"incoterms"`
	ShippingTerm string `json:"shipping_term"`

	// OpportunityID attributes the whole order to a deal, which is what lets
	// goods bought specifically for a job reach that job's margin. Nullable
	// because stock is also bought speculatively, for the shelf.
	OpportunityID *uuid.UUID `json:"opportunity_id" gorm:"type:uuid;index"`

	Notes string `json:"notes" gorm:"type:text"`

	IssuedAt *time.Time `json:"issued_at" gorm:"index"`

	RaisedByID *uuid.UUID `json:"raised_by_id" gorm:"type:uuid"`
	RaisedBy   string     `json:"raised_by"` // denormalised: survives user deletion

	Lines []PurchaseOrderLine `json:"lines" gorm:"foreignKey:PurchaseOrderID;constraint:OnDelete:CASCADE"`
}

// PurchaseOrderLine is one thing ordered.
type PurchaseOrderLine struct {
	BaseModel
	PurchaseOrderID uuid.UUID `json:"purchase_order_id" gorm:"type:uuid;index;not null"`

	// ProductID is nullable on purpose. Not everything bought is a catalogue
	// product — a spare, a service, a one-off fabrication. A line without a
	// product still costs money and still belongs on the order; it simply never
	// reaches the stock ledger, because there is no product to hold.
	ProductID *uuid.UUID `json:"product_id" gorm:"type:uuid;index"`

	Description string `json:"description" gorm:"not null"`
	Quantity    int    `json:"quantity" gorm:"not null"`
	// UnitPrice is in the order's Currency, as quoted by the supplier.
	UnitPrice float64 `json:"unit_price"`

	Position int `json:"position"` // display order
}

// LineTotal is the supplier's price for this line, in the order's currency.
func (l PurchaseOrderLine) LineTotal() float64 {
	return float64(l.Quantity) * l.UnitPrice
}

// Subtotal is the order's value in its own currency.
func (p PurchaseOrder) Subtotal() float64 {
	var total float64
	for _, l := range p.Lines {
		total += l.LineTotal()
	}
	return total
}

// SubtotalZMW restates the order in kwacha at the rate recorded on it. It is
// derived, never stored — the same discipline every other money figure in this
// system follows.
func (p PurchaseOrder) SubtotalZMW() float64 {
	return p.Subtotal() * p.ExchangeRate
}
