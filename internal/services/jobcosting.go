package services

import (
	"arcusinvest/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// DealCosting is what a deal actually made, once the goods it consumed and the
// costs booked against it are taken off its value.
//
// It is computed on read and never stored, like every other derived figure in
// this system. A stored margin would be a number that silently disagreed with
// its inputs the moment a late clearing invoice recosted the stock behind it.
type DealCosting struct {
	DealValue float64 `json:"deal_value"`
	// CostOfGoods values the stock issued against this deal at what it landed
	// for, not at the supplier's price.
	CostOfGoods float64 `json:"cost_of_goods"`
	// DirectExpenses is what was booked straight to the deal — subcontract
	// labour, site transport, an installation crew's per diem.
	DirectExpenses float64 `json:"direct_expenses"`
	// IrrecoverableVat is input VAT paid on those expenses that ZRA will not
	// accept a claim for. It is a real cost of the job and belongs in margin;
	// leaving it out flatters every deal by up to 16% of its expense base.
	IrrecoverableVat float64 `json:"irrecoverable_vat"`
	TotalCost        float64 `json:"total_cost"`
	Margin           float64 `json:"margin"`
	// MarginPercent is margin over deal value. Zero when the deal has no value
	// yet, rather than an infinity that would render as NaN on screen.
	MarginPercent float64 `json:"margin_percent"`
	// GoodsAtCostUnknown counts issued units the ledger could not cost, because
	// nothing priced ever came in for that product. Surfacing the count is the
	// honest alternative to costing them at zero and reporting a margin that
	// looks better than the deal was.
	GoodsAtCostUnknown int `json:"goods_at_cost_unknown"`
}

// weightedAverageCost values a product at the average of what came in.
//
// Inbound movements carry a unit cost; outbound ones are deliberately zero —
// see the comment on StockMovement.UnitCost — so cost of sales has to be struck
// against the receipts rather than read off the issue. Weighted average is the
// convention here because it needs no lot tracking, which this ledger does not
// carry, and because it is the method an accountant preparing Zambian
// micro-entity statements will expect to see.
func weightedAverageCost(db *gorm.DB, productIDs []uuid.UUID) (map[uuid.UUID]float64, error) {
	out := map[uuid.UUID]float64{}
	if len(productIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		ProductID uuid.UUID
		Qty       int
		Value     float64
	}
	err := db.Model(&models.StockMovement{}).
		Select("product_id, COALESCE(SUM(quantity), 0) AS qty, COALESCE(SUM(quantity * unit_cost), 0) AS value").
		Where("product_id IN ? AND quantity > 0 AND kind IN ?",
			productIDs, []string{models.StockOpening, models.StockReceipt, models.StockReturn}).
		Group("product_id").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Qty > 0 && r.Value > 0 {
			out[r.ProductID] = r.Value / float64(r.Qty)
		}
	}
	return out, nil
}

// CostDeal builds the margin picture for one opportunity.
func CostDeal(db *gorm.DB, opp models.Opportunity) (DealCosting, error) {
	out := DealCosting{DealValue: opp.DealValue}

	// Goods issued against this deal. Negative quantities are the ones that
	// left, which is what a deal consumes.
	var issued []models.StockMovement
	if err := db.Where("opportunity_id = ? AND quantity < 0", opp.ID).Find(&issued).Error; err != nil {
		return out, err
	}
	productIDs := make([]uuid.UUID, 0, len(issued))
	seen := map[uuid.UUID]bool{}
	for _, m := range issued {
		if !seen[m.ProductID] {
			seen[m.ProductID] = true
			productIDs = append(productIDs, m.ProductID)
		}
	}
	costs, err := weightedAverageCost(db, productIDs)
	if err != nil {
		return out, err
	}
	for _, m := range issued {
		units := -m.Quantity // back to a positive count
		unitCost, ok := costs[m.ProductID]
		if !ok {
			out.GoodsAtCostUnknown += units
			continue
		}
		out.CostOfGoods += float64(units) * unitCost
	}

	// Expenses booked straight to the deal. This is the field that existed and
	// that nothing set until now.
	var expenses []models.Expense
	if err := db.Where("opportunity_id = ?", opp.ID).Find(&expenses).Error; err != nil {
		return out, err
	}
	for _, e := range expenses {
		out.DirectExpenses += e.NetAmount
		out.IrrecoverableVat += e.VatAmount - e.ReclaimableVat()
	}

	out.TotalCost = out.CostOfGoods + out.DirectExpenses + out.IrrecoverableVat
	out.Margin = out.DealValue - out.TotalCost
	if out.DealValue != 0 {
		out.MarginPercent = out.Margin / out.DealValue * 100
	}
	return out, nil
}
