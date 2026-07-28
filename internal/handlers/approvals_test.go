package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"arcusinvest/internal/database"
	"arcusinvest/internal/models"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// These tests drive the real HTTP handlers against real Postgres rather than
// calling the gate directly. That is deliberate: apply_vat once shipped broken
// because its tests called InvoicedTotal directly and never traversed the
// handler, so the create path went unwired without anything failing.

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed handler test")
	}
	db, err := database.Connect(dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// fixtureUser creates a throwaway staff account. Real accounts live in this
// database alongside seed data, so tests never reuse one.
func fixtureUser(t *testing.T, db *gorm.DB, role models.Role) models.User {
	t.Helper()
	u := models.User{
		Email:    "approvals-test-" + uuid.NewString() + "@example.invalid",
		FullName: "Approvals Fixture",
		Role:     role,
		IsActive: true,
	}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create fixture user: %v", err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(&models.User{}, "id = ?", u.ID) })
	return u
}

func fixtureRule(t *testing.T, db *gorm.DB, action string, minAmount float64, required int, approver models.Role) models.ApprovalRule {
	t.Helper()
	r := models.ApprovalRule{
		Action: action, MinAmount: minAmount, RequiredCount: required,
		ApproverRole: approver, IsActive: true,
	}
	if err := db.Create(&r).Error; err != nil {
		t.Fatalf("create fixture rule: %v", err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(&models.ApprovalRule{}, "id = ?", r.ID) })
	return r
}

func fixtureDeal(t *testing.T, db *gorm.DB, value float64, stage models.OpportunityStage) models.Opportunity {
	t.Helper()
	o := models.Opportunity{
		Name: "Approvals Fixture Deal", DealValue: value, Stage: stage,
		Grade: models.GradeBronze, Segment: "standard",
	}
	if err := db.Create(&o).Error; err != nil {
		t.Fatalf("create fixture deal: %v", err)
	}
	t.Cleanup(func() {
		db.Unscoped().Delete(&models.ApprovalRequest{}, "entity_id = ?", o.ID)
		db.Unscoped().Delete(&models.Opportunity{}, "id = ?", o.ID)
	})
	return o
}

// adminCtx stands in for the Auth middleware, which sets user_id as a string.
func adminCtx(e *echo.Echo, method, body string, actor models.User) (echo.Context, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set("user_id", actor.ID.String())
	c.Set("role", actor.Role)
	return c, rec
}

func withDealParam(c echo.Context, id uuid.UUID) echo.Context {
	c.SetPath("/api/v1/admin/opportunities/:id")
	c.SetParamNames("id")
	c.SetParamValues(id.String())
	return c
}

// TestMatchRulePicksHighestApplicableFloor prevents the tiering bug where a
// K600,000 deal matches the K50,000 manager rule instead of the K500,000
// director rule, quietly routing a large transaction to a junior approver.
func TestMatchRulePicksHighestApplicableFloor(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	action := "test.tiering." + uuid.NewString()

	fixtureRule(t, db, action, 50_000, 1, models.RoleAdmin)
	director := fixtureRule(t, db, action, 500_000, 2, models.RoleSuperAdmin)

	got, err := h.matchRule(action, 600_000)
	if err != nil {
		t.Fatalf("matchRule: %v", err)
	}
	if got == nil || got.ID != director.ID {
		t.Fatalf("matchRule(600k) picked %+v, want the 500k director rule", got)
	}

	// Below every floor is not gated at all.
	below, err := h.matchRule(action, 10_000)
	if err != nil {
		t.Fatalf("matchRule: %v", err)
	}
	if below != nil {
		t.Errorf("matchRule(10k) = %+v, want nil (no rule applies)", below)
	}
}

// TestCloseWonBelowThresholdProceeds pins that the gate does not block what it
// was never configured to block. A control that stops ordinary work gets turned
// off, and a control that is off protects nothing.
func TestCloseWonBelowThresholdProceeds(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	fixtureRule(t, db, models.ApprovalDealCloseWon, 500_000, 1, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 10_000, models.StageNegotiation)

	c, rec := adminCtx(e, http.MethodPut, `{"stage":"won"}`, actor)
	if err := h.AdminUpdateOpportunity(withDealParam(c, deal.ID)); err != nil {
		t.Fatalf("AdminUpdateOpportunity: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var after models.Opportunity
	db.First(&after, "id = ?", deal.ID)
	if after.Stage != models.StageWon {
		t.Errorf("stage = %q, want won", after.Stage)
	}
}

// TestCloseWonAboveThresholdIsBlockedAndDoesNotMutate is the core requirement:
// the action must be genuinely blocked server-side, not merely hidden in the UI.
// Asserting the 409 alone would pass even if the handler wrote the stage and
// then reported a conflict, so the stored row is re-read.
func TestCloseWonAboveThresholdIsBlockedAndDoesNotMutate(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	fixtureRule(t, db, models.ApprovalDealCloseWon, 100_000, 1, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 750_000, models.StageNegotiation)

	c, rec := adminCtx(e, http.MethodPut, `{"stage":"won"}`, actor)
	if err := h.AdminUpdateOpportunity(withDealParam(c, deal.ID)); err != nil {
		t.Fatalf("AdminUpdateOpportunity: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}

	var after models.Opportunity
	db.First(&after, "id = ?", deal.ID)
	if after.Stage == models.StageWon {
		t.Fatal("deal was closed as Won despite the gate returning 409 — the block is cosmetic")
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["approval_request_id"] == nil {
		t.Error("409 body carries no approval_request_id, so the client cannot show what is pending")
	}

	var raised models.ApprovalRequest
	if err := db.Where("entity_id = ? AND action = ?", deal.ID, models.ApprovalDealCloseWon).
		First(&raised).Error; err != nil {
		t.Fatalf("no approval request was raised: %v", err)
	}
	if raised.Status != models.ApprovalStatusPending {
		t.Errorf("raised status = %q, want pending", raised.Status)
	}
	if raised.Amount != 750_000 {
		t.Errorf("raised amount = %v, want the deal value 750000", raised.Amount)
	}
	if raised.ApproverRole != models.RoleSuperAdmin {
		t.Errorf("approver role = %q, want the rule's super_admin snapshot", raised.ApproverRole)
	}
}

// TestCloseWonUsesPostUpdateValue prevents the bypass where raising the deal
// value and closing it in the same request is measured against the old, smaller
// value and slips under the threshold.
func TestCloseWonUsesPostUpdateValue(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	fixtureRule(t, db, models.ApprovalDealCloseWon, 100_000, 1, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 1_000, models.StageNegotiation)

	c, rec := adminCtx(e, http.MethodPut, `{"stage":"won","deal_value":900000}`, actor)
	if err := h.AdminUpdateOpportunity(withDealParam(c, deal.ID)); err != nil {
		t.Fatalf("AdminUpdateOpportunity: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — the gate measured the old value; body = %s", rec.Code, rec.Body.String())
	}

	var after models.Opportunity
	db.First(&after, "id = ?", deal.ID)
	if after.Stage == models.StageWon {
		t.Error("deal closed as Won by raising its value in the same request")
	}
}

// TestSecondBlockedAttemptReusesTheSameRequest stops a requester from filling an
// approver's queue with duplicates by retrying, and pins the unique pending_key.
func TestSecondBlockedAttemptReusesTheSameRequest(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	fixtureRule(t, db, models.ApprovalDealCloseWon, 100_000, 1, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 750_000, models.StageNegotiation)

	for i := 0; i < 3; i++ {
		c, rec := adminCtx(e, http.MethodPut, `{"stage":"won"}`, actor)
		if err := h.AdminUpdateOpportunity(withDealParam(c, deal.ID)); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if rec.Code != http.StatusConflict {
			t.Fatalf("attempt %d status = %d, want 409", i, rec.Code)
		}
	}

	var count int64
	db.Model(&models.ApprovalRequest{}).
		Where("entity_id = ? AND action = ?", deal.ID, models.ApprovalDealCloseWon).Count(&count)
	if count != 1 {
		t.Errorf("%d approval requests raised across 3 retries, want 1", count)
	}
}

// TestCreateAtWonIsRefusedWhenRuleExists closes the bypass where a gated close
// is achieved by creating the deal already Won — the gate lives on the stage
// transition, which a create never performs.
func TestCreateAtWonIsRefusedWhenRuleExists(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	fixtureRule(t, db, models.ApprovalDealCloseWon, 100_000, 1, models.RoleSuperAdmin)

	body := `{"name":"Straight to won","deal_value":900000,"stage":"won"}`
	c, rec := adminCtx(e, http.MethodPost, body, actor)
	c.SetPath("/api/v1/admin/opportunities")
	if err := h.AdminCreateOpportunity(c); err != nil {
		t.Fatalf("AdminCreateOpportunity: %v", err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(&models.Opportunity{}, "name = ?", "Straight to won") })

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	var count int64
	db.Model(&models.Opportunity{}).Where("name = ?", "Straight to won").Count(&count)
	if count != 0 {
		t.Error("a deal was created already Won despite an active close-won rule")
	}
}

// TestCreateAtWonAllowedWithoutRule pins that the refusal above is tied to an
// active rule, not a blanket ban — importing historical won deals must still
// work on a system with no thresholds configured.
func TestCreateAtWonAllowedWithoutRule(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	name := "Historic won " + uuid.NewString()
	body := `{"name":"` + name + `","deal_value":900000,"stage":"won"}`
	c, rec := adminCtx(e, http.MethodPost, body, actor)
	c.SetPath("/api/v1/admin/opportunities")
	if err := h.AdminCreateOpportunity(c); err != nil {
		t.Fatalf("AdminCreateOpportunity: %v", err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(&models.Opportunity{}, "name = ?", name) })

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
}
