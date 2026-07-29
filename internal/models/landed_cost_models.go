package models

import (
	"time"

	"github.com/google/uuid"
)

// Landed cost component kinds.
//
// These are the charges that stand between a supplier's price and what a unit
// actually cost once it is on the shelf in Lusaka. They are listed separately
// rather than summed into one "extras" figure because they behave differently:
// duty is a function of the customs value and the tariff line, freight is a
// function of the consignment, and a clearing agent's fee is a flat charge that
// often arrives weeks after the goods.
const (
	LandedFreight   = "freight"   // sea, air or road carriage
	LandedInsurance = "insurance" // marine or transit cover
	LandedDuty      = "duty"      // customs duty and excise assessed at the border
	LandedClearing  = "clearing"  // clearing agent's fee
	LandedHandling  = "handling"  // port, terminal, storage, demurrage
	LandedOther     = "other"     //
)

var AllLandedCostKinds = []string{
	LandedFreight, LandedInsurance, LandedDuty, LandedClearing, LandedHandling, LandedOther,
}

// Apportionment bases.
//
// Which one is right depends on what drives the charge. Freight on a mixed
// consignment of transformers and cable is driven by weight, not by value, and
// apportioning it by value would load the cost onto the expensive light item.
// Value is the conventional default because it needs no data the order does not
// already carry; weight and quantity are available where the better answer is
// worth the extra keying.
//
// The basis actually used is stored on the receipt. An apportionment nobody can
// reproduce is worse than no apportionment at all, because it looks like a fact.
const (
	BasisValue    = "value"    // pro rata to line value — the default
	BasisQuantity = "quantity" // pro rata to unit count
	BasisWeight   = "weight"   // pro rata to line weight
)

var AllApportionmentBases = []string{BasisValue, BasisQuantity, BasisWeight}

// GoodsReceipt is one physical arrival against a purchase order.
//
// It is a separate entity from the order because an order can be delivered in
// parts, and each part can carry its own freight and clear customs on its own
// entry. Making the receipt the anchor for landed cost — rather than the order —
// is what allows the first container's freight to reach the units in it without
// waiting for the second.
type GoodsReceipt struct {
	BaseModel
	PurchaseOrderID uuid.UUID `json:"purchase_order_id" gorm:"type:uuid;index;not null"`

	// ReceivedAt is when the goods physically arrived, which is the date the
	// stock movement carries. It is not the invoice date and not the order date.
	ReceivedAt time.Time `json:"received_at" gorm:"index"`

	// Reference is the delivery note or waybill number.
	Reference string `json:"reference" gorm:"index"`

	// CustomsAssessmentRef is the ZRA customs entry / assessment number for an
	// imported consignment.
	//
	// This exists because import VAT is NOT Smart Invoice VAT. Smart Invoice is
	// the evidence rule for a domestic supply; VAT on an import is paid at the
	// border and evidenced by the customs assessment, never by a foreign
	// supplier's invoice — a foreign supplier has no ZRA Mark ID and never will.
	// Recording a customs reference in Expense.SmartInvoiceRef would be a lie
	// about what the document is, so imports get their own field and
	// Expense.ReclaimableVat reads both. See the comment there.
	CustomsAssessmentRef string `json:"customs_assessment_ref"`

	// ExchangeRate is kwacha per one unit of the order's currency, at this
	// receipt. It is recorded per receipt rather than taken from the order
	// because the rate moves between ordering and clearing, and it is the rate
	// at the point cost is struck that belongs in cost.
	ExchangeRate float64 `json:"exchange_rate" gorm:"not null;default:1"`

	// Basis is the apportionment basis used to spread this receipt's cost
	// components across its lines. Stored, not inferred, so the arithmetic can
	// be re-derived years later.
	Basis string `json:"basis" gorm:"not null;default:'value'"`

	// ApportionedAt records when unit costs were last computed. A late clearing
	// invoice recalculates and moves this forward; nil means components have
	// been recorded but not yet spread.
	ApportionedAt *time.Time `json:"apportioned_at"`

	Notes string `json:"notes" gorm:"type:text"`

	ReceivedByID *uuid.UUID `json:"received_by_id" gorm:"type:uuid"`
	ReceivedBy   string     `json:"received_by"`

	Lines      []GoodsReceiptLine    `json:"lines" gorm:"foreignKey:GoodsReceiptID;constraint:OnDelete:CASCADE"`
	Components []LandedCostComponent `json:"components" gorm:"foreignKey:GoodsReceiptID;constraint:OnDelete:CASCADE"`
}

// GoodsReceiptLine is the quantity of one ordered line that actually turned up
// in this delivery.
//
// Received quantity lives here rather than as a running total on the order line
// for the same reason stock on hand is not a column on Product: the total is
// the sum of the events, so it cannot drift from them.
type GoodsReceiptLine struct {
	BaseModel
	GoodsReceiptID    uuid.UUID `json:"goods_receipt_id" gorm:"type:uuid;index;not null"`
	PurchaseOrderLineID uuid.UUID `json:"purchase_order_line_id" gorm:"type:uuid;index;not null"`

	Quantity int `json:"quantity" gorm:"not null"`

	// Weight is the line's total weight in kilograms, needed only when the
	// receipt apportions by weight. Zero otherwise.
	Weight float64 `json:"weight"`

	// UnitCostZMW is the landed cost of one unit, in kwacha: the supplier's
	// price at this receipt's rate, plus this line's share of the cost
	// components. It is written here as well as onto the stock movement so the
	// figure remains auditable against its components after the fact.
	//
	// It is stored rather than derived because it is the number the stock ledger
	// was written with. Recomputing it on read would silently disagree with the
	// movement rows the moment a late charge arrived, and the ledger is the one
	// that has already been used to cost a sale.
	UnitCostZMW float64 `json:"unit_cost_zmw"`
	// ApportionedZMW is this line's total share of the cost components, kept
	// separately so "what did freight add to this item" is answerable without
	// re-running the apportionment.
	ApportionedZMW float64 `json:"apportioned_zmw"`

	// StockMovementID links to the ledger row this line wrote, where the line
	// carries a catalogue product. Nil for a non-stock line.
	StockMovementID *uuid.UUID `json:"stock_movement_id" gorm:"type:uuid;index"`
}

// LandedCostComponent is one charge incurred bringing a consignment in.
//
// Components are retained individually and never collapsed into the unit cost
// they produce. Two reasons, both practical: a late clearing invoice is normal
// and must be able to recalculate without re-keying what came before, and an
// auditor asking why a generator is carried at K91,000 needs to see the freight
// and the duty, not a single number with no derivation.
type LandedCostComponent struct {
	BaseModel
	GoodsReceiptID uuid.UUID `json:"goods_receipt_id" gorm:"type:uuid;index;not null"`

	Kind        string `json:"kind" gorm:"index;not null"`
	Description string `json:"description"`

	// Currency and Amount are the charge as billed. ExchangeRate and AmountZMW
	// restate it in kwacha. All four are stored: a freight invoice in USD and
	// the kwacha it converted to are different facts, and only keeping both
	// makes the conversion checkable.
	Currency     string  `json:"currency" gorm:"not null;default:'ZMW'"`
	Amount       float64 `json:"amount"`
	ExchangeRate float64 `json:"exchange_rate" gorm:"not null;default:1"`

	// Reference is the carrier's or agent's invoice number.
	Reference string `json:"reference"`

	IncurredAt time.Time `json:"incurred_at" gorm:"index"`

	// ExpenseID links the component to the payable it was booked as, where one
	// exists. The component explains cost; the expense is the liability. They
	// are separate because freight is frequently in cost before its invoice
	// arrives, and the goods must still be costed in the meantime.
	ExpenseID *uuid.UUID `json:"expense_id" gorm:"type:uuid;index"`

	RecordedByID *uuid.UUID `json:"recorded_by_id" gorm:"type:uuid"`
	RecordedBy   string     `json:"recorded_by"`
}

// AmountZMW is the charge in kwacha at the rate recorded on it.
func (c LandedCostComponent) AmountZMW() float64 {
	return c.Amount * c.ExchangeRate
}

// TotalComponentsZMW is everything this receipt added on top of the supplier's
// price, in kwacha.
func (r GoodsReceipt) TotalComponentsZMW() float64 {
	var total float64
	for _, c := range r.Components {
		total += c.AmountZMW()
	}
	return total
}
