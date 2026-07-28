package services

import (
	"errors"
	"fmt"

	"arcusinvest/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrInsufficientStock is returned when a movement would take the quantity on
// hand below zero.
var ErrInsufficientStock = errors.New("not enough stock on hand")

// StockLevels returns the quantity on hand for every product, as one aggregate
// query rather than a query per product.
//
// Products with no movements at all are absent from the map; callers should
// treat a missing key as zero. That is deliberate — an explicit zero row would
// have to be written by something, and nothing should be writing balances.
func StockLevels(db *gorm.DB) (map[uuid.UUID]int, error) {
	var rows []struct {
		ProductID uuid.UUID
		Total     int
	}
	if err := db.Model(&models.StockMovement{}).
		Select("product_id, COALESCE(SUM(quantity), 0) AS total").
		Group("product_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]int, len(rows))
	for _, r := range rows {
		out[r.ProductID] = r.Total
	}
	return out, nil
}

// StockOnHand returns the quantity held of one product.
func StockOnHand(db *gorm.DB, productID uuid.UUID) (int, error) {
	var total *int
	if err := db.Model(&models.StockMovement{}).
		Where("product_id = ?", productID).
		Select("SUM(quantity)").Scan(&total).Error; err != nil {
		return 0, err
	}
	if total == nil {
		return 0, nil
	}
	return *total, nil
}

// ValidateMovement applies the rules that hold regardless of who is writing.
// It returns a human-readable reason, or "" when the movement is valid.
func ValidateMovement(m models.StockMovement) string {
	if m.Quantity == 0 {
		return "a movement of zero units records nothing"
	}
	want := models.StockDirection(m.Kind)
	if want == 0 && m.Kind != models.StockAdjustment {
		return "unknown stock movement kind"
	}
	if want > 0 && m.Quantity < 0 {
		return fmt.Sprintf("a %s adds stock, so its quantity cannot be negative", m.Kind)
	}
	if want < 0 && m.Quantity > 0 {
		return fmt.Sprintf("a %s removes stock, so its quantity must be negative", m.Kind)
	}
	// A number with no explanation is what the old bare integer already was.
	// Receipts and sales explain themselves; a correction or a loss does not.
	switch m.Kind {
	case models.StockAdjustment, models.StockWriteOff:
		if m.Reason == "" {
			return fmt.Sprintf("a %s needs a reason", m.Kind)
		}
	}
	return ""
}

// RecordMovement appends a movement, refusing any that would drive the balance
// negative.
//
// The check and the insert share one transaction with a row lock on the
// product, because two concurrent sales of the last unit must not both read a
// balance of one and both succeed. Overselling is the specific failure this
// exists to prevent, and it is exactly the case a read-then-write loses.
func RecordMovement(db *gorm.DB, m *models.StockMovement) error {
	return db.Transaction(func(tx *gorm.DB) error {
		// Serialises concurrent movements for this product against each other
		// without blocking movements of anything else.
		var product models.Product
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&product, "id = ?", m.ProductID).Error; err != nil {
			return err
		}

		onHand, err := StockOnHand(tx, m.ProductID)
		if err != nil {
			return err
		}
		if onHand+m.Quantity < 0 {
			return fmt.Errorf("%w: %d held, movement of %d", ErrInsufficientStock, onHand, m.Quantity)
		}
		return tx.Create(m).Error
	})
}
