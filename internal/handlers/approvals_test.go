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

// testDB hands back a transaction that is rolled back when the test ends, so a
// run leaves the database exactly as it found it. The suite shares one database
// with real dev data; committing test rows into it is how a "why is this deal
// closed?" mystery starts.
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
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin: %v", tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx
}

// clearRules removes every rule for one action inside the test transaction.
//
// Rolling back isolates what a test WRITES, not what it READS: a rule an
// operator has genuinely configured in the dev database is committed data and
// still matches. Without this, whether these tests pass depends on how the
// local system happens to be configured — which is how a suite starts failing
// for reasons that have nothing to do with the code.
func clearRules(t *testing.T, db *gorm.DB, action string) {
	t.Helper()
	if err := db.Unscoped().Where("action = ?", action).Delete(&models.ApprovalRule{}).Error; err != nil {
		t.Fatalf("clear rules for %s: %v", action, err)
	}
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

// fixtureRule installs the ONLY rule for its action, so a test measures its own
// threshold rather than whichever pre-existing rule happens to sort first.
func fixtureRule(t *testing.T, db *gorm.DB, action string, minAmount float64, required int, approver models.Role) models.ApprovalRule {
	t.Helper()
	clearRules(t, db, action)
	r := models.ApprovalRule{
		Action: action, MinAmount: minAmount, RequiredCount: required,
		ApproverRole: approver, IsActive: true,
	}
	if err := db.Create(&r).Error; err != nil {
		t.Fatalf("create fixture rule: %v", err)
	}
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

func withApprovalParam(c echo.Context, id uuid.UUID) echo.Context {
	c.SetPath("/api/v1/admin/approvals/:id")
	c.SetParamNames("id")
	c.SetParamValues(id.String())
	return c
}

// closeWon drives the real update handler and returns the recorder, so tests
// exercise the same path a client does.
func closeWon(t *testing.T, h Handler, e *echo.Echo, actor models.User, dealID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	c, rec := adminCtx(e, http.MethodPut, `{"stage":"won"}`, actor)
	if err := h.AdminUpdateOpportunity(withDealParam(c, dealID)); err != nil {
		t.Fatalf("AdminUpdateOpportunity: %v", err)
	}
	return rec
}

// blockedRequest closes a deal, asserts it was gated, and returns the raised
// request so decision tests start from a real one.
func blockedRequest(t *testing.T, h Handler, db *gorm.DB, e *echo.Echo, actor models.User, dealID uuid.UUID) models.ApprovalRequest {
	t.Helper()
	rec := closeWon(t, h, e, actor, dealID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected the action to be gated, got %d: %s", rec.Code, rec.Body.String())
	}
	var raised models.ApprovalRequest
	if err := db.Where("entity_id = ? AND status = ?", dealID, models.ApprovalStatusPending).
		First(&raised).Error; err != nil {
		t.Fatalf("no pending request raised: %v", err)
	}
	return raised
}

func decide(t *testing.T, h Handler, e *echo.Echo, approver models.User, reqID uuid.UUID, reject bool, reason string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"reason":"` + reason + `"}`
	c, rec := adminCtx(e, http.MethodPatch, body, approver)
	var err error
	if reject {
		err = h.AdminRejectRequest(withApprovalParam(c, reqID))
	} else {
		err = h.AdminApproveRequest(withApprovalParam(c, reqID))
	}
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	return rec
}

// TestRequesterCannotApproveOwnRequest is the single most important guard in the
// feature. Without it, anyone holding the approver role signs off their own
// transaction and maker-checker is decorative.
func TestRequesterCannotApproveOwnRequest(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	// The requester deliberately HOLDS the approver role — the check must be on
	// identity, not on permission.
	actor := fixtureUser(t, db, models.RoleSuperAdmin)
	fixtureRule(t, db, models.ApprovalDealCloseWon, 100_000, 1, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 750_000, models.StageNegotiation)

	raised := blockedRequest(t, h, db, e, actor, deal.ID)
	rec := decide(t, h, e, actor, raised.ID, false, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self-approval status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}

	var after models.ApprovalRequest
	db.First(&after, "id = ?", raised.ID)
	if after.Status != models.ApprovalStatusPending {
		t.Errorf("status = %q after self-approval attempt, want it left pending", after.Status)
	}
	var votes int64
	db.Model(&models.ApprovalDecision{}).Where("request_id = ?", raised.ID).Count(&votes)
	if votes != 0 {
		t.Errorf("%d decisions recorded for a refused self-approval, want 0", votes)
	}
}

// TestWrongRoleCannotDecide pins that the rule's approver role is enforced and
// that there is no standing super_admin override — a standing override is a
// standing bypass.
func TestWrongRoleCannotDecide(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	wrongRole := fixtureUser(t, db, models.RoleSuperAdmin)
	fixtureRule(t, db, models.ApprovalDealCloseWon, 100_000, 1, models.RoleAdmissions)
	deal := fixtureDeal(t, db, 750_000, models.StageNegotiation)

	raised := blockedRequest(t, h, db, e, actor, deal.ID)
	rec := decide(t, h, e, wrongRole, raised.ID, false, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — super_admin decided a request addressed to admissions", rec.Code)
	}
}

// TestApprovedRequestUnblocksThenIsSpent covers the happy path and single-use in
// one run: the retry after approval succeeds, and a later attempt at the same
// action does not ride the same approval a second time.
func TestApprovedRequestUnblocksThenIsSpent(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	approver := fixtureUser(t, db, models.RoleSuperAdmin)
	fixtureRule(t, db, models.ApprovalDealCloseWon, 100_000, 1, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 750_000, models.StageNegotiation)

	raised := blockedRequest(t, h, db, e, actor, deal.ID)

	if rec := decide(t, h, e, approver, raised.ID, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("approve status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var approved models.ApprovalRequest
	db.First(&approved, "id = ?", raised.ID)
	if approved.Status != models.ApprovalStatusApproved {
		t.Fatalf("status = %q, want approved", approved.Status)
	}
	if approved.PendingKey != nil {
		t.Error("pending_key still held after the decision — the requester could never raise another request")
	}

	// The requester retries; the gate consumes the approval.
	if rec := closeWon(t, h, e, actor, deal.ID); rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var after models.Opportunity
	db.First(&after, "id = ?", deal.ID)
	if after.Stage != models.StageWon {
		t.Fatalf("stage = %q after an approved retry, want won", after.Stage)
	}
	var spent models.ApprovalRequest
	db.First(&spent, "id = ?", raised.ID)
	if spent.Status != models.ApprovalStatusConsumed {
		t.Errorf("status = %q, want consumed", spent.Status)
	}

	// Reopen the deal and try again. The approval is spent, so this must be
	// gated afresh rather than sliding through on the previous sign-off.
	db.Model(&models.Opportunity{}).Where("id = ?", deal.ID).Update("stage", models.StageNegotiation)
	if rec := closeWon(t, h, e, actor, deal.ID); rec.Code != http.StatusConflict {
		t.Fatalf("second close status = %d, want 409 — a consumed approval was reused", rec.Code)
	}
}

// TestApprovalIsEntityBound stops an approval to close one deal from closing a
// different one.
func TestApprovalIsEntityBound(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	approver := fixtureUser(t, db, models.RoleSuperAdmin)
	fixtureRule(t, db, models.ApprovalDealCloseWon, 100_000, 1, models.RoleSuperAdmin)
	dealA := fixtureDeal(t, db, 750_000, models.StageNegotiation)
	dealB := fixtureDeal(t, db, 750_000, models.StageNegotiation)

	raised := blockedRequest(t, h, db, e, actor, dealA.ID)
	decide(t, h, e, approver, raised.ID, false, "")

	if rec := closeWon(t, h, e, actor, dealB.ID); rec.Code != http.StatusConflict {
		t.Fatalf("deal B status = %d, want 409 — an approval for deal A closed deal B", rec.Code)
	}
	var b models.Opportunity
	db.First(&b, "id = ?", dealB.ID)
	if b.Stage == models.StageWon {
		t.Error("deal B was closed using deal A's approval")
	}
}

// TestApprovalDoesNotCoverALargerAmount stops the escalation where a small
// transaction is waved through and then executed at a much larger value.
func TestApprovalDoesNotCoverALargerAmount(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	approver := fixtureUser(t, db, models.RoleSuperAdmin)
	fixtureRule(t, db, models.ApprovalDealCloseWon, 10_000, 1, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 50_000, models.StageNegotiation)

	raised := blockedRequest(t, h, db, e, actor, deal.ID)
	decide(t, h, e, approver, raised.ID, false, "")
	if raised.Amount != 50_000 {
		t.Fatalf("request raised for %v, want 50000", raised.Amount)
	}

	// Same deal, same approval — but ten times the value.
	c, rec := adminCtx(e, http.MethodPut, `{"stage":"won","deal_value":500000}`, actor)
	if err := h.AdminUpdateOpportunity(withDealParam(c, deal.ID)); err != nil {
		t.Fatalf("AdminUpdateOpportunity: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 — a 50k approval admitted a 500k close", rec.Code)
	}

	var stillApproved models.ApprovalRequest
	db.First(&stillApproved, "id = ?", raised.ID)
	if stillApproved.Status == models.ApprovalStatusConsumed {
		t.Error("the 50k approval was consumed by a 500k action")
	}
	var after models.Opportunity
	db.First(&after, "id = ?", deal.ID)
	if after.Stage == models.StageWon {
		t.Error("deal closed at 500k on a 50k approval")
	}
}

// TestSameApproverCannotSatisfyATwoApproverRule pins that RequiredCount means N
// distinct people, enforced by the composite unique index rather than by a
// prior SELECT two concurrent votes could both pass.
func TestSameApproverCannotSatisfyATwoApproverRule(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	approver := fixtureUser(t, db, models.RoleSuperAdmin)
	fixtureRule(t, db, models.ApprovalDealCloseWon, 100_000, 2, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 750_000, models.StageNegotiation)

	raised := blockedRequest(t, h, db, e, actor, deal.ID)
	if raised.RequiredCount != 2 {
		t.Fatalf("required_count = %d, want the rule's 2", raised.RequiredCount)
	}

	if rec := decide(t, h, e, approver, raised.ID, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("first approval status = %d, want 200", rec.Code)
	}
	var mid models.ApprovalRequest
	db.First(&mid, "id = ?", raised.ID)
	if mid.Status != models.ApprovalStatusPending {
		t.Fatalf("status = %q after 1 of 2 approvals, want still pending", mid.Status)
	}

	// The same person votes again. One human must not count as two.
	if rec := decide(t, h, e, approver, raised.ID, false, ""); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate vote status = %d, want 409", rec.Code)
	}
	var after models.ApprovalRequest
	db.First(&after, "id = ?", raised.ID)
	if after.Status == models.ApprovalStatusApproved {
		t.Fatal("one approver satisfied a two-approver rule by voting twice")
	}

	// A second, distinct approver completes it.
	second := fixtureUser(t, db, models.RoleSuperAdmin)
	if rec := decide(t, h, e, second, raised.ID, false, ""); rec.Code != http.StatusOK {
		t.Fatalf("second approval status = %d, want 200", rec.Code)
	}
	db.First(&after, "id = ?", raised.ID)
	if after.Status != models.ApprovalStatusApproved {
		t.Errorf("status = %q after two distinct approvals, want approved", after.Status)
	}
}

// TestRejectRequiresAReason keeps the revise-and-resubmit loop usable: a
// rejection the requester cannot act on is a dead end.
func TestRejectRequiresAReason(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	approver := fixtureUser(t, db, models.RoleSuperAdmin)
	fixtureRule(t, db, models.ApprovalDealCloseWon, 100_000, 1, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 750_000, models.StageNegotiation)

	raised := blockedRequest(t, h, db, e, actor, deal.ID)
	if rec := decide(t, h, e, approver, raised.ID, true, "   "); rec.Code != http.StatusBadRequest {
		t.Fatalf("blank-reason reject status = %d, want 400", rec.Code)
	}
	var after models.ApprovalRequest
	db.First(&after, "id = ?", raised.ID)
	if after.Status != models.ApprovalStatusPending {
		t.Errorf("status = %q after a refused rejection, want still pending", after.Status)
	}
}

// TestRejectionBlocksRetryUntilResubmitted is the Revise & Resubmit loop: a
// rejected requester must not be able to simply retry their way past the
// decision, and the resubmission must be traceable to what it replaced.
func TestRejectionBlocksRetryUntilResubmitted(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	approver := fixtureUser(t, db, models.RoleSuperAdmin)
	fixtureRule(t, db, models.ApprovalDealCloseWon, 100_000, 1, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 750_000, models.StageNegotiation)

	raised := blockedRequest(t, h, db, e, actor, deal.ID)
	if rec := decide(t, h, e, approver, raised.ID, true, "margin is too thin"); rec.Code != http.StatusOK {
		t.Fatalf("reject status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	// Retrying must not silently raise a fresh request and reset the clock.
	rec := closeWon(t, h, e, actor, deal.ID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("post-rejection retry status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "margin is too thin") {
		t.Errorf("the rejection reason is not surfaced to the requester: %s", rec.Body.String())
	}
	var count int64
	db.Model(&models.ApprovalRequest{}).Where("entity_id = ?", deal.ID).Count(&count)
	if count != 1 {
		t.Errorf("%d requests exist after a rejected retry, want 1 — retrying bypassed the rejection", count)
	}

	// Resubmitting explicitly opens a new request linked to the old one.
	c, rrec := adminCtx(e, http.MethodPost, `{"summary":"Revised: discount removed"}`, actor)
	if err := h.AdminResubmitRequest(withApprovalParam(c, raised.ID)); err != nil {
		t.Fatalf("AdminResubmitRequest: %v", err)
	}
	if rrec.Code != http.StatusCreated {
		t.Fatalf("resubmit status = %d, want 201; body = %s", rrec.Code, rrec.Body.String())
	}
	var next models.ApprovalRequest
	if err := db.Where("entity_id = ? AND status = ?", deal.ID, models.ApprovalStatusPending).
		First(&next).Error; err != nil {
		t.Fatalf("no pending request after resubmit: %v", err)
	}
	if next.SupersedesID == nil || *next.SupersedesID != raised.ID {
		t.Errorf("supersedes_id = %v, want the rejected request %v", next.SupersedesID, raised.ID)
	}
	if next.Summary != "Revised: discount removed" {
		t.Errorf("summary = %q, want the revised text", next.Summary)
	}
}

// TestRuleValidationRefusesUnsatisfiableRules covers the footgun that would
// otherwise present as "the app is broken" days later: a rule naming a role
// that does not exist, or one that cannot decide approvals, blocks its action
// permanently with nobody able to release it.
func TestRuleValidationRefusesUnsatisfiableRules(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleSuperAdmin)

	cases := []struct {
		name string
		body string
	}{
		{"unknown action", `{"action":"deal.explode","min_amount":1,"required_count":1,"approver_role":"super_admin"}`},
		{"nonexistent role", `{"action":"deal.close_won","min_amount":1,"required_count":1,"approver_role":"wizard"}`},
		{"role that cannot decide", `{"action":"deal.close_won","min_amount":1,"required_count":1,"approver_role":"admissions"}`},
		{"zero approvals required", `{"action":"deal.close_won","min_amount":1,"required_count":-1,"approver_role":"super_admin"}`},
		{"negative threshold", `{"action":"deal.close_won","min_amount":-5,"required_count":1,"approver_role":"super_admin"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := adminCtx(e, http.MethodPost, tc.body, actor)
			c.SetPath("/api/v1/admin/approval-rules")
			if err := h.AdminCreateApprovalRule(c); err != nil {
				t.Fatalf("AdminCreateApprovalRule: %v", err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestCreateValidRuleSucceeds is the counterpart: the validation above must not
// be so strict that a legitimate rule cannot be saved.
func TestCreateValidRuleSucceeds(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleSuperAdmin)

	c, rec := adminCtx(e, http.MethodPost,
		`{"action":"contract.sign","min_amount":250000,"required_count":2,"approver_role":"super_admin","note":"board sign-off"}`, actor)
	c.SetPath("/api/v1/admin/approval-rules")
	if err := h.AdminCreateApprovalRule(c); err != nil {
		t.Fatalf("AdminCreateApprovalRule: %v", err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(&models.ApprovalRule{}, "note = ?", "board sign-off") })
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	var saved models.ApprovalRule
	if err := db.Where("note = ?", "board sign-off").First(&saved).Error; err != nil {
		t.Fatalf("rule not persisted: %v", err)
	}
	if !saved.IsActive {
		t.Error("a newly created rule should be active")
	}
}

func notificationsFor(db *gorm.DB, userID uuid.UUID, kind string) []models.Notification {
	var out []models.Notification
	db.Where("user_id = ? AND kind = ?", userID, kind).Find(&out)
	return out
}

// TestApproversAreNotifiedButTheRequesterIsNot pins the recipient rule. A
// notification inviting someone to approve their own request would contradict
// the guard that refuses it, and would train people to ignore the bell.
func TestApproversAreNotifiedButTheRequesterIsNot(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	// The requester holds the approver role, so role alone would include them.
	actor := fixtureUser(t, db, models.RoleSuperAdmin)
	approver := fixtureUser(t, db, models.RoleSuperAdmin)
	bystander := fixtureUser(t, db, models.RoleAdmissions)
	t.Cleanup(func() {
		db.Unscoped().Delete(&models.Notification{}, "user_id IN ?",
			[]uuid.UUID{actor.ID, approver.ID, bystander.ID})
	})

	fixtureRule(t, db, models.ApprovalDealCloseWon, 100_000, 1, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 750_000, models.StageNegotiation)
	blockedRequest(t, h, db, e, actor, deal.ID)

	if got := notificationsFor(db, approver.ID, models.NotifyApprovalPending); len(got) != 1 {
		t.Errorf("approver got %d pending notifications, want 1", len(got))
	}
	if got := notificationsFor(db, actor.ID, models.NotifyApprovalPending); len(got) != 0 {
		t.Errorf("the requester was invited to approve their own request (%d notifications)", len(got))
	}
	// admissions cannot read approvals, so it must not hear about them.
	if got := notificationsFor(db, bystander.ID, models.NotifyApprovalPending); len(got) != 0 {
		t.Errorf("a role without approvals access was notified (%d notifications)", len(got))
	}
}

// TestRejectionNotifiesTheRequesterWithTheReason is what makes the
// revise-and-resubmit loop reachable rather than a dead end.
func TestRejectionNotifiesTheRequesterWithTheReason(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	approver := fixtureUser(t, db, models.RoleSuperAdmin)
	t.Cleanup(func() {
		db.Unscoped().Delete(&models.Notification{}, "user_id IN ?", []uuid.UUID{actor.ID, approver.ID})
	})

	fixtureRule(t, db, models.ApprovalDealCloseWon, 100_000, 1, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 750_000, models.StageNegotiation)

	raised := blockedRequest(t, h, db, e, actor, deal.ID)
	decide(t, h, e, approver, raised.ID, true, "discount not authorised")

	got := notificationsFor(db, actor.ID, models.NotifyApprovalDecided)
	if len(got) != 1 {
		t.Fatalf("requester got %d decision notifications, want 1", len(got))
	}
	if !strings.Contains(got[0].Body, "discount not authorised") {
		t.Errorf("decision notification omits the reason: %q", got[0].Body)
	}
	if got[0].EntityType != "approvals" || got[0].EntityID == nil || *got[0].EntityID != raised.ID {
		t.Error("decision notification does not point back at the request, so the bell cannot deep-link")
	}
}

// TestPartialApprovalDoesNotNotifyTheRequester stops a two-approver rule from
// telling the requester they are unblocked after one sign-off, which would send
// them back to retry straight into another 409.
func TestPartialApprovalDoesNotNotifyTheRequester(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	first := fixtureUser(t, db, models.RoleSuperAdmin)
	second := fixtureUser(t, db, models.RoleSuperAdmin)
	t.Cleanup(func() {
		db.Unscoped().Delete(&models.Notification{}, "user_id IN ?",
			[]uuid.UUID{actor.ID, first.ID, second.ID})
	})

	fixtureRule(t, db, models.ApprovalDealCloseWon, 100_000, 2, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 750_000, models.StageNegotiation)

	raised := blockedRequest(t, h, db, e, actor, deal.ID)
	decide(t, h, e, first, raised.ID, false, "")
	if got := notificationsFor(db, actor.ID, models.NotifyApprovalDecided); len(got) != 0 {
		t.Fatalf("requester notified after 1 of 2 approvals (%d notifications)", len(got))
	}

	decide(t, h, e, second, raised.ID, false, "")
	if got := notificationsFor(db, actor.ID, models.NotifyApprovalDecided); len(got) != 1 {
		t.Errorf("requester got %d notifications after the deciding approval, want 1", len(got))
	}
}

func fixtureContract(t *testing.T, db *gorm.DB, value float64, status string) models.Contract {
	t.Helper()
	ct := models.Contract{
		Title: "Approvals Fixture Contract", Value: value, Status: status,
		// A stored key is needed to reach the signing guards. It is never opened:
		// the gate refuses before any file work, which the sign test relies on.
		StoredKey: "contracts/fixture-never-read.pdf", ContentType: "application/pdf",
		FileName: "fixture.pdf",
	}
	if err := db.Create(&ct).Error; err != nil {
		t.Fatalf("create fixture contract: %v", err)
	}
	t.Cleanup(func() {
		db.Unscoped().Delete(&models.ApprovalRequest{}, "entity_id = ?", ct.ID)
		db.Unscoped().Delete(&models.Contract{}, "id = ?", ct.ID)
	})
	return ct
}

// TestRecordingALargePaymentIsBlocked pins that no Payment row is written when
// the gate refuses. Receivables are computed from payments, so a payment that
// slipped past the gate would silently move the debtor book.
func TestRecordingALargePaymentIsBlocked(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	fixtureRule(t, db, models.ApprovalPaymentRecord, 100_000, 1, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 900_000, models.StageNegotiation)

	c, rec := adminCtx(e, http.MethodPost, `{"amount":250000,"method":"bank_transfer"}`, actor)
	c.SetPath("/api/v1/admin/opportunities/:id/payments")
	c.SetParamNames("id")
	c.SetParamValues(deal.ID.String())
	if err := h.AdminCreatePayment(c); err != nil {
		t.Fatalf("AdminCreatePayment: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}

	var payments int64
	db.Model(&models.Payment{}).Where("opportunity_id = ?", deal.ID).Count(&payments)
	if payments != 0 {
		t.Errorf("%d payments recorded despite the gate returning 409", payments)
	}
}

// TestDeletingADealIsBlocked pins that the row survives a refused delete.
func TestDeletingADealIsBlocked(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	// MinAmount 0 — deletes are gated regardless of value.
	fixtureRule(t, db, models.ApprovalDealDelete, 0, 1, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 5_000, models.StageQualified)

	c, rec := adminCtx(e, http.MethodDelete, ``, actor)
	if err := h.AdminDeleteOpportunity(withDealParam(c, deal.ID)); err != nil {
		t.Fatalf("AdminDeleteOpportunity: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}

	var still models.Opportunity
	if err := db.First(&still, "id = ?", deal.ID).Error; err != nil {
		t.Fatal("the deal was soft-deleted despite the gate returning 409")
	}
}

// TestDeletingAContractIsBlocked matters more than the deal case: a contract
// carries document versions and signature evidence that a soft delete orphans.
func TestDeletingAContractIsBlocked(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	fixtureRule(t, db, models.ApprovalContractDelete, 0, 1, models.RoleSuperAdmin)
	ct := fixtureContract(t, db, 400_000, "active")

	c, rec := adminCtx(e, http.MethodDelete, ``, actor)
	c.SetPath("/api/v1/admin/contracts/:id")
	c.SetParamNames("id")
	c.SetParamValues(ct.ID.String())
	if err := h.AdminDeleteContract(c); err != nil {
		t.Fatalf("AdminDeleteContract: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}

	var still models.Contract
	if err := db.First(&still, "id = ?", ct.ID).Error; err != nil {
		t.Fatal("the contract was soft-deleted despite the gate returning 409")
	}
}

// TestSigningIsBlockedBeforeAnyFileWork pins that the gate runs ahead of
// decoding the signature and opening the stored PDF. Handler.Store is
// deliberately nil here: if the gate let execution reach the storage layer this
// test would panic rather than quietly pass.
func TestSigningIsBlockedBeforeAnyFileWork(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db} // no Store on purpose
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	fixtureRule(t, db, models.ApprovalContractSign, 100_000, 1, models.RoleSuperAdmin)
	ct := fixtureContract(t, db, 400_000, "sent")

	c, rec := adminCtx(e, http.MethodPost, `{"image":"data:image/png;base64,AAAA","page":1}`, actor)
	c.SetPath("/api/v1/admin/contracts/:id/sign")
	c.SetParamNames("id")
	c.SetParamValues(ct.ID.String())
	if err := h.AdminSignContract(c); err != nil {
		t.Fatalf("AdminSignContract: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}

	var sigs int64
	db.Model(&models.ContractSignature{}).Where("contract_id = ?", ct.ID).Count(&sigs)
	if sigs != 0 {
		t.Errorf("%d signature evidence records written for a blocked signing", sigs)
	}
	var after models.Contract
	db.First(&after, "id = ?", ct.ID)
	if after.Status != "sent" {
		t.Errorf("contract status = %q, want it left at sent", after.Status)
	}
}

// TestOnlyRequesterCanResubmit keeps the loop pointed back at the originator.
func TestOnlyRequesterCanResubmit(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()

	actor := fixtureUser(t, db, models.RoleAdmin)
	other := fixtureUser(t, db, models.RoleAdmin)
	approver := fixtureUser(t, db, models.RoleSuperAdmin)
	fixtureRule(t, db, models.ApprovalDealCloseWon, 100_000, 1, models.RoleSuperAdmin)
	deal := fixtureDeal(t, db, 750_000, models.StageNegotiation)

	raised := blockedRequest(t, h, db, e, actor, deal.ID)
	decide(t, h, e, approver, raised.ID, true, "no")

	c, rec := adminCtx(e, http.MethodPost, `{}`, other)
	if err := h.AdminResubmitRequest(withApprovalParam(c, raised.ID)); err != nil {
		t.Fatalf("AdminResubmitRequest: %v", err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
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
	clearRules(t, db, models.ApprovalDealCloseWon)
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
