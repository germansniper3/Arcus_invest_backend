package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"arcusinvest/internal/models"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// These tests drive the real handlers against real Postgres, for the reason
// given at the top of approvals_test.go: a unit test that calls the model
// helper directly cannot catch a create path that never wires the field up.

func withExpenseParam(c echo.Context, id uuid.UUID) echo.Context {
	c.SetPath("/api/v1/admin/expenses/:id")
	c.SetParamNames("id")
	c.SetParamValues(id.String())
	return c
}

// createExpense posts a supplier invoice through the handler and returns the
// recorder, so tests exercise the same path a client does.
func createExpense(t *testing.T, h Handler, e *echo.Echo, actor models.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	c, rec := adminCtx(e, http.MethodPost, body, actor)
	c.SetPath("/api/v1/admin/expenses")
	if err := h.AdminCreateExpense(c); err != nil {
		t.Fatalf("AdminCreateExpense: %v", err)
	}
	return rec
}

func decodeExpense(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode expense: %v (body %s)", err, rec.Body.String())
	}
	return out
}

func fixtureExpense(t *testing.T, db *gorm.DB, e models.Expense) models.Expense {
	t.Helper()
	if e.Supplier == "" {
		e.Supplier = "Payables Fixture Supplier"
	}
	if e.Category == "" {
		e.Category = models.ExpPurchases
	}
	if e.VatTreatment == "" {
		e.VatTreatment = models.VatNone
	}
	if e.IncurredAt.IsZero() {
		e.IncurredAt = time.Now().UTC()
	}
	if err := db.Create(&e).Error; err != nil {
		t.Fatalf("create fixture expense: %v", err)
	}
	t.Cleanup(func() {
		db.Unscoped().Delete(&models.ExpenseSettlement{}, "expense_id = ?", e.ID)
		db.Unscoped().Delete(&models.Expense{}, "id = ?", e.ID)
	})
	return e
}

// Prevents: input VAT being reported as reclaimable on a purchase that ZRA will
// not accept a claim for. From 1 January 2026 a claim is only admissible where
// the purchase is backed by a Smart Invoice, so VAT on an invoice with no Mark
// ID is a cost. Reporting it as recoverable overstates the VAT account and
// understates the expense.
func TestVatIsOnlyReclaimableAgainstASmartInvoice(t *testing.T) {
	standardWithRef := models.Expense{
		VatTreatment: models.VatStandard, NetAmount: 1000, VatAmount: 160,
		SmartInvoiceRef: "ZRA-MARK-0001",
	}
	if got := standardWithRef.ReclaimableVat(); got != 160 {
		t.Errorf("standard-rated with a Smart Invoice ref: reclaimable = %v, want 160", got)
	}

	standardNoRef := models.Expense{
		VatTreatment: models.VatStandard, NetAmount: 1000, VatAmount: 160,
	}
	if got := standardNoRef.ReclaimableVat(); got != 0 {
		t.Errorf("standard-rated with no Smart Invoice ref: reclaimable = %v, want 0", got)
	}
	// The VAT was still paid — it just cannot be recovered, so it must not
	// vanish from the gross payable.
	if got := standardNoRef.Gross(); got != 1160 {
		t.Errorf("gross = %v, want 1160", got)
	}

	// A Mark ID cannot make an exempt supply reclaimable.
	exempt := models.Expense{
		VatTreatment: models.VatExempt, NetAmount: 1000, VatAmount: 0,
		SmartInvoiceRef: "ZRA-MARK-0002",
	}
	if got := exempt.ReclaimableVat(); got != 0 {
		t.Errorf("exempt: reclaimable = %v, want 0", got)
	}
}

// Prevents: VAT being booked against a supply that cannot legally carry it —
// an exempt, zero-rated or unregistered-supplier purchase — which would put
// irrecoverable tax into the VAT account.
func TestNonStandardExpensesCannotCarryVat(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	clearRules(t, db, models.ApprovalExpenseRecord)

	rec := createExpense(t, h, e, actor,
		`{"supplier":"Kitwe Hardware","category":"purchases","vat_treatment":"exempt","net_amount":500,"vat_amount":80}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for VAT on an exempt supply, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Prevents: an expense being recorded without approval when a threshold covers
// it. Committing the business to a liability is the maker half of maker-checker.
func TestRecordingAnExpenseIsGatedByThreshold(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	fixtureRule(t, db, models.ApprovalExpenseRecord, 1000, 1, models.RoleSuperAdmin)
	t.Cleanup(func() {
		db.Unscoped().Delete(&models.ApprovalRequest{}, "action = ?", models.ApprovalExpenseRecord)
	})

	rec := createExpense(t, h, e, actor,
		`{"supplier":"Kitwe Hardware","category":"purchases","vat_treatment":"none","net_amount":5000}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected the expense to be gated, got %d: %s", rec.Code, rec.Body.String())
	}
	var raised models.ApprovalRequest
	if err := db.Where("action = ? AND status = ?", models.ApprovalExpenseRecord, models.ApprovalStatusPending).
		First(&raised).Error; err != nil {
		t.Fatalf("no pending request raised: %v", err)
	}
	if raised.Amount != 5000 {
		t.Errorf("request raised for %v, want the gross 5000", raised.Amount)
	}

	// Below the threshold the same call must go straight through, or deploying
	// a threshold would freeze routine purchasing.
	rec = createExpense(t, h, e, actor,
		`{"supplier":"Kitwe Hardware","category":"purchases","vat_treatment":"none","net_amount":200}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected an under-threshold expense to be created, got %d: %s", rec.Code, rec.Body.String())
	}
	var created models.Expense
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err == nil && created.ID != uuid.Nil {
		t.Cleanup(func() { db.Unscoped().Delete(&models.Expense{}, "id = ?", created.ID) })
	}
}

// Prevents: paying a supplier more than is owed. The aged report has no way to
// represent a supplier who owes the business money, so an overpayment would
// disappear rather than showing up as a credit.
func TestSettlementCannotExceedOutstanding(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	clearRules(t, db, models.ApprovalExpenseSettle)

	exp := fixtureExpense(t, db, models.Expense{NetAmount: 1000})

	c, rec := adminCtx(e, http.MethodPost, `{"amount":1500,"method":"cash"}`, actor)
	if err := h.AdminSettleExpense(withExpenseParam(c, exp.ID)); err != nil {
		t.Fatalf("AdminSettleExpense: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an over-settlement, got %d: %s", rec.Code, rec.Body.String())
	}

	// A part payment is fine, and the balance must come back computed.
	c, rec = adminCtx(e, http.MethodPost, `{"amount":400,"method":"mobile_money"}`, actor)
	if err := h.AdminSettleExpense(withExpenseParam(c, exp.ID)); err != nil {
		t.Fatalf("AdminSettleExpense: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a part settlement, got %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeExpense(t, rec)
	if body["outstanding"] != 600.0 {
		t.Errorf("outstanding = %v, want 600", body["outstanding"])
	}
}

// Prevents: deleting an expense that has been paid, which would erase the only
// record that money left the business alongside the liability it settled.
func TestASettledExpenseCannotBeDeleted(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)

	exp := fixtureExpense(t, db, models.Expense{NetAmount: 800})
	if err := db.Create(&models.ExpenseSettlement{
		ExpenseID: exp.ID, Amount: 800, Method: "cash", PaidAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatalf("create settlement: %v", err)
	}

	c, rec := adminCtx(e, http.MethodDelete, "", actor)
	if err := h.AdminDeleteExpense(withExpenseParam(c, exp.ID)); err != nil {
		t.Fatalf("AdminDeleteExpense: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 deleting a settled expense, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Prevents: ageing a payable from its invoice date when the supplier granted
// terms. A 30-day invoice raised 20 days ago is not overdue, and showing it in
// an ageing bucket sends someone to pay a bill that is not yet due.
func TestPayablesAgeFromTheDueDateWhenThereIsOne(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	now := time.Now().UTC()

	// Raised 40 days ago on 30-day terms: due 10 days ago, so still "current"
	// by the 30/60/90 bands but genuinely overdue by 10 days.
	due := now.AddDate(0, 0, -10)
	onTerms := fixtureExpense(t, db, models.Expense{
		Supplier: "Terms Supplier " + uuid.NewString(), NetAmount: 1000,
		IncurredAt: now.AddDate(0, 0, -40), DueDate: &due,
	})

	rows, err := h.buildPayables(now)
	if err != nil {
		t.Fatalf("buildPayables: %v", err)
	}
	var found *payableRow
	for i := range rows {
		if rows[i].ExpenseID == onTerms.ID {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		t.Fatal("the unsettled expense is missing from the payables report")
	}
	if found.DaysOverdue != 10 {
		t.Errorf("days overdue = %d, want 10 (aged from the due date, not the 40-day-old invoice date)", found.DaysOverdue)
	}
	if found.Bucket != "current" {
		t.Errorf("bucket = %q, want %q", found.Bucket, "current")
	}
}
