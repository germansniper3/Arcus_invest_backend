package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"arcusinvest/internal/models"
	"arcusinvest/internal/services"

	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

func openTill(t *testing.T, h Handler, e *echo.Echo, db *gorm.DB, actor models.User, openingFloat float64) models.TillSession {
	t.Helper()
	c, rec := adminCtx(e, http.MethodPost, fmt.Sprintf(`{"opening_float":%v}`, openingFloat), actor)
	if err := h.AdminOpenTill(c); err != nil {
		t.Fatalf("AdminOpenTill: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 opening a till, got %d: %s", rec.Code, rec.Body.String())
	}
	var s models.TillSession
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	t.Cleanup(func() {
		db.Unscoped().Delete(&models.CounterSale{}, "till_session_id = ?", s.ID)
		db.Unscoped().Delete(&models.TillSession{}, "id = ?", s.ID)
	})
	return s
}

func sell(t *testing.T, h Handler, e *echo.Echo, actor models.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	c, rec := adminCtx(e, http.MethodPost, body, actor)
	if err := h.AdminCreateCounterSale(c); err != nil {
		t.Fatalf("AdminCreateCounterSale: %v", err)
	}
	return rec
}

// Prevents: a sale recording without taking the goods off the shelf. The two
// halves are one transaction; a sale that does not move stock leaves the shelf
// figure permanently wrong, which is the failure the whole ledger exists to
// stop.
func TestACounterSaleTakesStockOut(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	openTill(t, h, e, db, actor, 500)
	p := fixtureProduct(t, db, 20)

	body := fmt.Sprintf(`{"payment_method":"cash","amount_tendered":300,
		"lines":[{"product_id":"%s","description":"Cable","quantity":3,"unit_price":50}]}`, p.ID)
	rec := sell(t, h, e, actor, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	onHand, err := services.StockOnHand(db, p.ID)
	if err != nil {
		t.Fatalf("StockOnHand: %v", err)
	}
	if onHand != 17 {
		t.Errorf("on hand = %d, want 17", onHand)
	}

	var body2 map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body2["total"] != 150.0 {
		t.Errorf("total = %v, want 150", body2["total"])
	}
	if body2["change"] != 150.0 {
		t.Errorf("change = %v, want 150", body2["change"])
	}

	// The movement must point back at the sale, or "why did stock drop" is
	// unanswerable again.
	var m models.StockMovement
	if err := db.Where("product_id = ? AND kind = ?", p.ID, models.StockSale).First(&m).Error; err != nil {
		t.Fatalf("no sale movement: %v", err)
	}
	if m.CounterSaleID == nil {
		t.Error("the stock movement does not reference the sale that caused it")
	}
}

// Prevents: selling stock that is not held. The sale and the movement share a
// transaction, so a refused movement must roll the sale back rather than
// leaving a sale with no goods behind it.
func TestOversellingIsRefusedAndRollsBackTheSale(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	openTill(t, h, e, db, actor, 0)
	p := fixtureProduct(t, db, 2)

	body := fmt.Sprintf(`{"payment_method":"cash","amount_tendered":1000,
		"lines":[{"product_id":"%s","description":"Cable","quantity":5,"unit_price":50}]}`, p.ID)
	rec := sell(t, h, e, actor, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 overselling, got %d: %s", rec.Code, rec.Body.String())
	}

	var sales int64
	db.Model(&models.CounterSale{}).Where("sold_by_id = ?", actor.ID).Count(&sales)
	if sales != 0 {
		t.Errorf("a refused sale left %d rows behind; the transaction did not roll back", sales)
	}
	if onHand, _ := services.StockOnHand(db, p.ID); onHand != 2 {
		t.Errorf("on hand = %d, want 2 — stock moved on a refused sale", onHand)
	}
}

// Prevents: selling with no shift open, which makes the drawer impossible to
// reconcile because the takings belong to nothing.
func TestSellingRequiresAnOpenTill(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	p := fixtureProduct(t, db, 5)

	body := fmt.Sprintf(`{"payment_method":"cash","amount_tendered":50,
		"lines":[{"product_id":"%s","description":"Cable","quantity":1,"unit_price":50}]}`, p.ID)
	rec := sell(t, h, e, actor, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 selling with no till open, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Prevents: credit arriving at the counter by the back door. Selling on account
// needs terms, a limit and someone chasing it — all of which live on the deal
// side. A "credit" counter method would create a second debtor book that
// nothing ages.
func TestCreditIsNotACounterPaymentMethod(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	openTill(t, h, e, db, actor, 0)

	rec := sell(t, h, e, actor,
		`{"payment_method":"credit","lines":[{"description":"Cable","quantity":1,"unit_price":50}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a credit counter sale, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Prevents: short payment being accepted on a cash sale, which would put the
// drawer out by the difference with nothing recording why.
func TestCashTenderedMustCoverTheTotal(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	openTill(t, h, e, db, actor, 0)

	rec := sell(t, h, e, actor,
		`{"payment_method":"cash","amount_tendered":40,"lines":[{"description":"Cable","quantity":1,"unit_price":50}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for short tender, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Prevents: the cash-up hiding a discrepancy. The variance is the whole point
// of counting the drawer, and it has to include the float and exclude takings
// that never touched the drawer.
func TestTillCloseReportsTheCashVariance(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	session := openTill(t, h, e, db, actor, 200)

	// 150 in cash, 300 on mobile money. Only the cash belongs in the drawer.
	if rec := sell(t, h, e, actor,
		`{"payment_method":"cash","amount_tendered":150,"lines":[{"description":"Cable","quantity":3,"unit_price":50}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("cash sale: %d %s", rec.Code, rec.Body.String())
	}
	if rec := sell(t, h, e, actor,
		`{"payment_method":"mobile_money","reference":"MM123","lines":[{"description":"Conduit","quantity":2,"unit_price":150}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("mobile money sale: %d %s", rec.Code, rec.Body.String())
	}

	// Drawer should hold 200 float + 150 cash = 350. It holds 330.
	c, rec := adminCtx(e, http.MethodPatch, `{"counted_cash":330}`, actor)
	c.SetPath("/api/v1/admin/till-sessions/:id/close")
	c.SetParamNames("id")
	c.SetParamValues(session.ID.String())
	if err := h.AdminCloseTill(c); err != nil {
		t.Fatalf("AdminCloseTill: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 closing the till, got %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["expected_cash"] != 350.0 {
		t.Errorf("expected_cash = %v, want 350 (float plus cash takings only)", out["expected_cash"])
	}
	if out["variance"] != -20.0 {
		t.Errorf("variance = %v, want -20", out["variance"])
	}
	if out["total_takings"] != 450.0 {
		t.Errorf("total_takings = %v, want 450 across both methods", out["total_takings"])
	}
}

// Prevents: closing a till without counting it. An omitted count read as zero
// would report the entire drawer as missing.
func TestClosingATillRequiresACount(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	session := openTill(t, h, e, db, actor, 100)

	c, rec := adminCtx(e, http.MethodPatch, `{"note":"end of shift"}`, actor)
	c.SetPath("/api/v1/admin/till-sessions/:id/close")
	c.SetParamNames("id")
	c.SetParamValues(session.ID.String())
	if err := h.AdminCloseTill(c); err != nil {
		t.Fatalf("AdminCloseTill: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 closing without a count, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Prevents: two open tills for one operator, which would split a shift's
// takings across two counts so that neither reconciles.
func TestAnOperatorCannotOpenTwoTills(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	openTill(t, h, e, db, actor, 100)

	c, rec := adminCtx(e, http.MethodPost, `{"opening_float":50}`, actor)
	if err := h.AdminOpenTill(c); err != nil {
		t.Fatalf("AdminOpenTill: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 opening a second till, got %d: %s", rec.Code, rec.Body.String())
	}
}
