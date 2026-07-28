package models

import (
	"time"

	"github.com/google/uuid"
)

// Stock movement kinds.
//
// The kind is not decoration: it is the answer to "why did this number change",
// which a bare integer could never give. A shortfall that is a write-off is a
// loss to investigate; the same shortfall as a sale is a good week.
const (
	StockOpening    = "opening"    // balance carried in when the ledger started
	StockReceipt    = "receipt"    // goods in from a supplier
	StockSale       = "sale"       // goods out to a customer
	StockReturn     = "return"     // goods back in from a customer
	StockAdjustment = "adjustment" // a stock count correcting the books
	StockWriteOff   = "write_off"  // damaged, expired or missing
)

var AllStockKinds = []string{
	StockOpening, StockReceipt, StockSale, StockReturn, StockAdjustment, StockWriteOff,
}

// StockDirection reports the sign a kind must carry, or 0 where either is
// valid. Only a stock count can legitimately move in both directions: a sale
// that increases stock, or a receipt that decreases it, is a data-entry error
// that would otherwise sit in the ledger looking authoritative.
func StockDirection(kind string) int {
	switch kind {
	case StockOpening, StockReceipt, StockReturn:
		return 1
	case StockSale, StockWriteOff:
		return -1
	default:
		return 0
	}
}

// StockMovement is one change to the quantity of a product held.
//
// Stock used to be a bare integer on Product, mutated in place. Nobody could
// answer why it changed, when, or on whose authority — and a mis-keyed edit was
// indistinguishable from a theft. This is the ledger that replaces it: the
// quantity on hand is the sum of these rows, computed the same way every other
// derived figure in this system is.
type StockMovement struct {
	BaseModel
	ProductID uuid.UUID `json:"product_id" gorm:"type:uuid;index;not null"`
	Kind      string    `json:"kind" gorm:"index;not null"`
	// Quantity is signed — negative takes stock out. Storing the sign rather
	// than inferring it from the kind means the running balance is a plain SUM
	// with no case analysis, so a new kind cannot silently break the total.
	Quantity int `json:"quantity" gorm:"not null"`
	// Reason is required for the kinds where the number alone is not an
	// explanation: adjustments and write-offs. See the handler.
	Reason string `json:"reason" gorm:"type:text"`
	// UnitCost is what a unit cost on the way in, carried on receipts so cost
	// of sales has something to draw on later. Zero on the way out.
	UnitCost float64 `json:"unit_cost"`

	// What caused the movement, where there is something to point at.
	OpportunityID *uuid.UUID `json:"opportunity_id" gorm:"type:uuid;index"`
	ExpenseID     *uuid.UUID `json:"expense_id" gorm:"type:uuid;index"`

	OccurredAt time.Time  `json:"occurred_at" gorm:"index"`
	ActorID    *uuid.UUID `json:"actor_id" gorm:"type:uuid"`
	ActorName  string     `json:"actor_name"` // denormalised: survives user deletion
}
