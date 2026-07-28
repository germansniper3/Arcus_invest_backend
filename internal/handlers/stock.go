package handlers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"arcusinvest/internal/models"
	"arcusinvest/internal/services"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// productJSON renders a product with its quantity on hand.
//
// Stock is no longer a column: it is the sum of the movement ledger, resolved
// by the caller and passed in. The JSON keeps the same `stock` key it always
// had, so nothing downstream has to know the storage changed.
func productJSON(p models.Product, onHand int) map[string]any {
	return map[string]any{
		"id": p.ID, "created_at": p.CreatedAt, "updated_at": p.UpdatedAt,
		"name": p.Name, "slug": p.Slug, "description": p.Description,
		"price": p.Price, "image_url": p.ImageURL, "specs": p.Specs,
		"is_published": p.IsPublished,
		"stock":        onHand,
	}
}

// productsJSON renders a list, taking the levels for all of them in one query
// rather than one per row.
func (h Handler) productsJSON(rows []models.Product) ([]map[string]any, error) {
	levels, err := services.StockLevels(h.DB)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, p := range rows {
		out = append(out, productJSON(p, levels[p.ID]))
	}
	return out, nil
}

// recordMovement writes a movement, stamping the acting user onto it. Every
// change to stock goes through here, which is what makes "who moved this, and
// why" answerable at all.
func (h Handler) recordMovement(c echo.Context, m *models.StockMovement) error {
	if m.OccurredAt.IsZero() {
		m.OccurredAt = time.Now().UTC()
	}
	var actor models.User
	if err := h.DB.First(&actor, "id = ?", c.Get("user_id")).Error; err == nil {
		m.ActorID = &actor.ID
		m.ActorName = actor.FullName
	}
	return services.RecordMovement(h.DB, m)
}

// AdminListStockMovements returns the ledger for one product, newest first —
// the history that a bare quantity could never provide.
func (h Handler) AdminListStockMovements(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid product id"))
	}
	var product models.Product
	if err := h.DB.First(&product, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("product not found"))
	}
	var rows []models.StockMovement
	if err := h.DB.Where("product_id = ?", id).
		Order("occurred_at DESC, created_at DESC").Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load stock movements"))
	}
	onHand, err := services.StockOnHand(h.DB, id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not compute stock on hand"))
	}
	return c.JSON(http.StatusOK, map[string]any{"items": rows, "on_hand": onHand})
}

// AdminCreateStockMovement appends one movement to a product's ledger.
func (h Handler) AdminCreateStockMovement(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid product id"))
	}
	var product models.Product
	if err := h.DB.First(&product, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("product not found"))
	}
	var req struct {
		Kind          string     `json:"kind"`
		Quantity      int        `json:"quantity"`
		Reason        string     `json:"reason"`
		UnitCost      float64    `json:"unit_cost"`
		OpportunityID *uuid.UUID `json:"opportunity_id"`
		ExpenseID     *uuid.UUID `json:"expense_id"`
		OccurredAt    *time.Time `json:"occurred_at"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	// An opening balance is written once, by the backfill, when the ledger
	// starts. Letting it be posted by hand afterwards would give a product two
	// beginnings and no way to tell which one was real.
	if req.Kind == models.StockOpening {
		return c.JSON(http.StatusBadRequest, errResponse("opening balances are set when the ledger is created, not posted by hand"))
	}
	movement := models.StockMovement{
		ProductID: id, Kind: req.Kind, Quantity: req.Quantity,
		Reason:        strings.TrimSpace(req.Reason),
		UnitCost:      req.UnitCost,
		OpportunityID: req.OpportunityID,
		ExpenseID:     req.ExpenseID,
	}
	if req.OccurredAt != nil {
		movement.OccurredAt = *req.OccurredAt
	}
	if reason := services.ValidateMovement(movement); reason != "" {
		return c.JSON(http.StatusBadRequest, errResponse(reason))
	}
	if err := h.recordMovement(c, &movement); err != nil {
		if errors.Is(err, services.ErrInsufficientStock) {
			return c.JSON(http.StatusConflict, errResponse(err.Error()))
		}
		return c.JSON(http.StatusInternalServerError, errResponse("could not record the stock movement"))
	}
	onHand, err := services.StockOnHand(h.DB, id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not compute stock on hand"))
	}
	return c.JSON(http.StatusCreated, map[string]any{"movement": movement, "on_hand": onHand})
}

// BackfillStockOpeningBalances writes one opening movement per product from the
// quantity that used to live in products.stock.
//
// The column is left in place — GORM's AutoMigrate never drops columns, and the
// old value is the only evidence of what was held when the ledger started, so
// destroying it during the migration that depends on it would be reckless.
//
// Idempotent: a product that already has any movement is skipped, so this is
// safe to run on every boot and safe to re-run after a partial failure.
func BackfillStockOpeningBalances(db *gorm.DB) error {
	if !db.Migrator().HasColumn(&models.Product{}, "stock") {
		return nil // already a fresh install; there is nothing to carry in
	}
	var legacy []struct {
		ID    uuid.UUID
		Stock int
	}
	if err := db.Table("products").
		Select("products.id, products.stock").
		Where("products.deleted_at IS NULL").
		Where("NOT EXISTS (SELECT 1 FROM stock_movements m WHERE m.product_id = products.id)").
		Scan(&legacy).Error; err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, p := range legacy {
		if p.Stock == 0 {
			// Nothing held and nothing to explain. A zero opening row would be
			// noise in every ledger that never had stock in the first place.
			continue
		}
		m := models.StockMovement{
			ProductID: p.ID, Kind: models.StockOpening, Quantity: p.Stock,
			Reason:     "Opening balance carried in when the stock ledger was introduced",
			OccurredAt: now, ActorName: "System",
		}
		if err := db.Create(&m).Error; err != nil {
			return err
		}
	}
	return nil
}
