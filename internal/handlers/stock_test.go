package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"arcusinvest/internal/models"
	"arcusinvest/internal/services"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

func withProductParam(c echo.Context, id uuid.UUID) echo.Context {
	c.SetPath("/api/v1/admin/products/:id")
	c.SetParamNames("id")
	c.SetParamValues(id.String())
	return c
}

func fixtureProduct(t *testing.T, db *gorm.DB, opening int) models.Product {
	t.Helper()
	slug := "stock-fixture-" + uuid.NewString()
	p := models.Product{
		Name: "Stock Fixture", Slug: slug, Description: "fixture",
		Price: 100, IsPublished: true,
	}
	if err := db.Create(&p).Error; err != nil {
		t.Fatalf("create fixture product: %v", err)
	}
	t.Cleanup(func() {
		db.Unscoped().Delete(&models.StockMovement{}, "product_id = ?", p.ID)
		db.Unscoped().Delete(&models.Product{}, "id = ?", p.ID)
	})
	if opening != 0 {
		m := models.StockMovement{
			ProductID: p.ID, Kind: models.StockOpening, Quantity: opening,
			Reason: "fixture opening balance",
		}
		if err := db.Create(&m).Error; err != nil {
			t.Fatalf("create fixture opening balance: %v", err)
		}
	}
	return p
}

func postMovement(t *testing.T, h Handler, e *echo.Echo, actor models.User, productID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	c, rec := adminCtx(e, http.MethodPost, body, actor)
	if err := h.AdminCreateStockMovement(withProductParam(c, productID)); err != nil {
		t.Fatalf("AdminCreateStockMovement: %v", err)
	}
	return rec
}

// Prevents: stock going negative. Selling stock that is not held is the failure
// that makes an inventory figure worthless, and the check has to survive two
// concurrent sales of the same last unit — which is why RecordMovement takes a
// row lock rather than reading and then writing.
func TestStockCannotGoNegative(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)

	p := fixtureProduct(t, db, 5)

	rec := postMovement(t, h, e, actor, p.ID, `{"kind":"sale","quantity":-8}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 selling more than is held, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = postMovement(t, h, e, actor, p.ID, `{"kind":"sale","quantity":-5}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 selling exactly what is held, got %d: %s", rec.Code, rec.Body.String())
	}
	onHand, err := services.StockOnHand(db, p.ID)
	if err != nil {
		t.Fatalf("StockOnHand: %v", err)
	}
	if onHand != 0 {
		t.Errorf("on hand = %d, want 0", onHand)
	}
}

// Prevents: a movement whose sign contradicts its kind. A "sale" that adds
// stock, or a "receipt" that removes it, would sit in the ledger looking
// authoritative while the reason it gives is false.
func TestMovementSignMustMatchItsKind(t *testing.T) {
	cases := []struct {
		name    string
		m       models.StockMovement
		wantErr bool
	}{
		{"sale removing stock", models.StockMovement{Kind: models.StockSale, Quantity: -3}, false},
		{"sale adding stock", models.StockMovement{Kind: models.StockSale, Quantity: 3}, true},
		{"receipt adding stock", models.StockMovement{Kind: models.StockReceipt, Quantity: 3}, false},
		{"receipt removing stock", models.StockMovement{Kind: models.StockReceipt, Quantity: -3}, true},
		// A stock count is the one thing that legitimately goes either way.
		{"adjustment up", models.StockMovement{Kind: models.StockAdjustment, Quantity: 2, Reason: "count"}, false},
		{"adjustment down", models.StockMovement{Kind: models.StockAdjustment, Quantity: -2, Reason: "count"}, false},
		// A shortfall with no explanation is exactly what the bare integer was.
		{"write-off without a reason", models.StockMovement{Kind: models.StockWriteOff, Quantity: -2}, true},
		{"write-off with a reason", models.StockMovement{Kind: models.StockWriteOff, Quantity: -2, Reason: "water damage"}, false},
		{"zero units", models.StockMovement{Kind: models.StockReceipt, Quantity: 0}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := services.ValidateMovement(tc.m)
			if tc.wantErr && got == "" {
				t.Errorf("expected a validation failure, got none")
			}
			if !tc.wantErr && got != "" {
				t.Errorf("expected the movement to be valid, got %q", got)
			}
		})
	}
}

// Prevents: an opening balance being posted by hand after the ledger has
// started, which would give a product two beginnings and no way to tell which
// one described reality.
func TestOpeningBalancesCannotBePostedByHand(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	p := fixtureProduct(t, db, 3)

	rec := postMovement(t, h, e, actor, p.ID, `{"kind":"opening","quantity":100}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 posting an opening balance by hand, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Prevents: the pre-ledger quantities being lost at the migration that
// introduces the ledger. Every product would read as zero on hand, which is a
// worse lie than the bare integer this replaces.
//
// The legacy column is recreated here because a freshly migrated database no
// longer has one — which is exactly why this path would otherwise never be
// exercised until it ran against production data.
func TestOpeningBalancesAreCarriedInFromTheLegacyColumn(t *testing.T) {
	db := testDB(t)
	if err := db.Exec("ALTER TABLE products ADD COLUMN IF NOT EXISTS stock integer DEFAULT 0").Error; err != nil {
		t.Fatalf("recreate legacy column: %v", err)
	}

	held := fixtureProduct(t, db, 0)  // legacy stock of 12, no movements
	empty := fixtureProduct(t, db, 0) // legacy stock of 0
	moved := fixtureProduct(t, db, 4) // already has a movement

	if err := db.Exec("UPDATE products SET stock = ? WHERE id = ?", 12, held.ID).Error; err != nil {
		t.Fatalf("seed legacy stock: %v", err)
	}
	if err := db.Exec("UPDATE products SET stock = ? WHERE id = ?", 99, moved.ID).Error; err != nil {
		t.Fatalf("seed legacy stock: %v", err)
	}

	if err := BackfillStockOpeningBalances(db); err != nil {
		t.Fatalf("BackfillStockOpeningBalances: %v", err)
	}

	if got, _ := services.StockOnHand(db, held.ID); got != 12 {
		t.Errorf("carried-in stock = %d, want 12", got)
	}
	// A product that never held anything gets no row; a zero opening balance is
	// noise in every ledger that had nothing to carry in.
	var emptyCount int64
	db.Model(&models.StockMovement{}).Where("product_id = ?", empty.ID).Count(&emptyCount)
	if emptyCount != 0 {
		t.Errorf("a product with no legacy stock got %d movements, want 0", emptyCount)
	}
	// A product that already has movements is skipped, so re-running cannot
	// double its balance.
	if got, _ := services.StockOnHand(db, moved.ID); got != 4 {
		t.Errorf("a product with existing movements was backfilled: on hand = %d, want 4", got)
	}

	// Idempotent: running again must change nothing.
	if err := BackfillStockOpeningBalances(db); err != nil {
		t.Fatalf("second BackfillStockOpeningBalances: %v", err)
	}
	if got, _ := services.StockOnHand(db, held.ID); got != 12 {
		t.Errorf("after a second backfill, on hand = %d, want 12", got)
	}
}

// Prevents: a stock figure sent with a product edit silently overwriting the
// balance. It must land as an adjustment carrying an actor and a reason,
// because a correction nobody can attribute is how the bare integer failed.
func TestEditingAProductsStockWritesAnAdjustment(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	p := fixtureProduct(t, db, 10)

	c, rec := adminCtx(e, http.MethodPut, `{"stock":7}`, actor)
	if err := h.AdminUpdateProduct(withProductParam(c, p.ID)); err != nil {
		t.Fatalf("AdminUpdateProduct: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["stock"] != 7.0 {
		t.Errorf("stock = %v, want 7", body["stock"])
	}

	var moves []models.StockMovement
	if err := db.Where("product_id = ? AND kind = ?", p.ID, models.StockAdjustment).
		Find(&moves).Error; err != nil {
		t.Fatalf("load movements: %v", err)
	}
	if len(moves) != 1 {
		t.Fatalf("expected exactly one adjustment, got %d", len(moves))
	}
	if moves[0].Quantity != -3 {
		t.Errorf("adjustment quantity = %d, want -3", moves[0].Quantity)
	}
	if moves[0].Reason == "" {
		t.Error("the adjustment carries no reason")
	}
	if moves[0].ActorName != actor.FullName {
		t.Errorf("adjustment actor = %q, want %q", moves[0].ActorName, actor.FullName)
	}

	// Re-sending the same figure is not a correction and must write nothing —
	// otherwise every save of the product form would add a no-op row.
	c, _ = adminCtx(e, http.MethodPut, `{"stock":7}`, actor)
	if err := h.AdminUpdateProduct(withProductParam(c, p.ID)); err != nil {
		t.Fatalf("AdminUpdateProduct: %v", err)
	}
	var count int64
	db.Model(&models.StockMovement{}).
		Where("product_id = ? AND kind = ?", p.ID, models.StockAdjustment).Count(&count)
	if count != 1 {
		t.Errorf("re-sending the same stock figure wrote %d adjustments, want 1", count)
	}
}
