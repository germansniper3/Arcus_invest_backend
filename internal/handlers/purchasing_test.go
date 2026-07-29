package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"arcusinvest/internal/models"
	"arcusinvest/internal/services"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// These tests drive the real handlers against real Postgres, for the reason
// given at the top of approvals_test.go: money code that is tested below the
// handler ships with the handler unwired.

func withPOParam(c echo.Context, id uuid.UUID) echo.Context {
	c.SetPath("/api/v1/admin/purchase-orders/:id")
	c.SetParamNames("id")
	c.SetParamValues(id.String())
	return c
}

func withReceiptParam(c echo.Context, id uuid.UUID) echo.Context {
	c.SetPath("/api/v1/admin/goods-receipts/:receiptId/components")
	c.SetParamNames("receiptId")
	c.SetParamValues(id.String())
	return c
}

// createPO drives the create handler and returns the decoded order.
func createPO(t *testing.T, h Handler, e *echo.Echo, actor models.User, body string) map[string]any {
	t.Helper()
	c, rec := adminCtx(e, http.MethodPost, body, actor)
	if err := h.AdminCreatePurchaseOrder(c); err != nil {
		t.Fatalf("AdminCreatePurchaseOrder: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("create purchase order: got %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode purchase order: %v", err)
	}
	return out
}

func poLineIDs(t *testing.T, po map[string]any) []uuid.UUID {
	t.Helper()
	raw, ok := po["lines"].([]any)
	if !ok {
		t.Fatalf("purchase order has no lines: %v", po)
	}
	out := make([]uuid.UUID, 0, len(raw))
	for _, l := range raw {
		m := l.(map[string]any)
		id, err := uuid.Parse(m["id"].(string))
		if err != nil {
			t.Fatalf("bad line id: %v", err)
		}
		out = append(out, id)
	}
	return out
}

func issuePO(t *testing.T, h Handler, e *echo.Echo, actor models.User, poID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	c, rec := adminCtx(e, http.MethodPost, `{}`, actor)
	if err := h.AdminIssuePurchaseOrder(withPOParam(c, poID)); err != nil {
		t.Fatalf("AdminIssuePurchaseOrder: %v", err)
	}
	return rec
}

func receiveGoods(t *testing.T, h Handler, e *echo.Echo, actor models.User, poID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	c, rec := adminCtx(e, http.MethodPost, body, actor)
	if err := h.AdminReceiveGoods(withPOParam(c, poID)); err != nil {
		t.Fatalf("AdminReceiveGoods: %v", err)
	}
	return rec
}

// Prevents: an order reaching a supplier without the authorisation the rules
// table requires. Issue is the committing act — once it is sent, the supplier
// can hold the business to it long before any invoice appears in payables.
func TestPurchaseOrderCannotBeIssuedWithoutItsApproval(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	approver := fixtureUser(t, db, models.RoleSuperAdmin)
	fixtureRule(t, db, models.ApprovalPurchaseOrderIssue, 1000, 1, models.RoleSuperAdmin)

	po := createPO(t, h, e, actor, `{
		"supplier":"Generator Imports Ltd","currency":"ZMW","exchange_rate":1,
		"lines":[{"description":"20kVA generator","quantity":2,"unit_price":25000}]
	}`)
	poID := uuid.MustParse(po["id"].(string))

	// K50,000 clears the K1,000 threshold, so the first attempt must be gated.
	rec := issuePO(t, h, e, actor, poID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected issue to be gated, got %d: %s", rec.Code, rec.Body.String())
	}

	var fresh models.PurchaseOrder
	if err := db.First(&fresh, "id = ?", poID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if fresh.Status != models.POStatusPendingApproval {
		t.Errorf("status = %q, want %q", fresh.Status, models.POStatusPendingApproval)
	}
	if fresh.IssuedAt != nil {
		t.Error("a gated order must not carry an issue date")
	}
	if fresh.Number != "" {
		t.Error("a gated order must not consume a purchase order number")
	}

	// Approve, then retry: the requester passes the gate and the order issues.
	var raised models.ApprovalRequest
	if err := db.Where("entity_id = ? AND status = ?", poID, models.ApprovalStatusPending).
		First(&raised).Error; err != nil {
		t.Fatalf("no pending request raised: %v", err)
	}
	decide(t, h, e, approver, raised.ID, false, "checked against the quote")

	rec = issuePO(t, h, e, actor, poID)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected issue to succeed after approval, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := db.First(&fresh, "id = ?", poID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if fresh.Status != models.POStatusIssued {
		t.Errorf("status = %q, want %q", fresh.Status, models.POStatusIssued)
	}
	if fresh.IssuedAt == nil {
		t.Error("an issued order needs an issue date")
	}
	if fresh.Number == "" {
		t.Error("an issued order needs a number to quote to the supplier")
	}
}

// Prevents: stock appearing on the shelf that nobody authorised buying. The
// three events are separate, but their ORDER is not optional.
func TestGoodsCannotBeReceivedAgainstAnUnissuedOrder(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	clearRules(t, db, models.ApprovalPurchaseOrderIssue)
	product := fixtureProduct(t, db, 0)

	po := createPO(t, h, e, actor, fmt.Sprintf(`{
		"supplier":"Cable Supplies","currency":"ZMW","exchange_rate":1,
		"lines":[{"product_id":"%s","description":"4mm cable","quantity":10,"unit_price":50}]
	}`, product.ID))
	poID := uuid.MustParse(po["id"].(string))
	lineIDs := poLineIDs(t, po)

	body := fmt.Sprintf(`{"lines":[{"purchase_order_line_id":"%s","quantity":5}]}`, lineIDs[0])
	rec := receiveGoods(t, h, e, actor, poID, body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected receipt against a draft to be refused, got %d: %s", rec.Code, rec.Body.String())
	}

	// And nothing reached the ledger.
	onHand, err := services.StockOnHand(db, product.ID)
	if err != nil {
		t.Fatalf("StockOnHand: %v", err)
	}
	if onHand != 0 {
		t.Errorf("stock on hand = %d after a refused receipt, want 0", onHand)
	}
}

// Prevents: a part-delivered order reading as complete, or its outstanding
// quantity drifting from the receipts underneath it. Partial receipt is the
// normal case for an import, not an edge case.
func TestPartialReceiptLeavesTheRightQuantityOutstanding(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	clearRules(t, db, models.ApprovalPurchaseOrderIssue)
	product := fixtureProduct(t, db, 0)

	po := createPO(t, h, e, actor, fmt.Sprintf(`{
		"supplier":"Cable Supplies","currency":"ZMW","exchange_rate":1,
		"lines":[{"product_id":"%s","description":"4mm cable","quantity":10,"unit_price":50}]
	}`, product.ID))
	poID := uuid.MustParse(po["id"].(string))
	lineIDs := poLineIDs(t, po)

	if rec := issuePO(t, h, e, actor, poID); rec.Code != http.StatusOK {
		t.Fatalf("issue: got %d: %s", rec.Code, rec.Body.String())
	}

	// First shipment: 4 of 10.
	body := fmt.Sprintf(`{"lines":[{"purchase_order_line_id":"%s","quantity":4}]}`, lineIDs[0])
	rec := receiveGoods(t, h, e, actor, poID, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first receipt: got %d: %s", rec.Code, rec.Body.String())
	}
	var after map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	line := after["lines"].([]any)[0].(map[string]any)
	if got := line["received_quantity"].(float64); got != 4 {
		t.Errorf("received = %v, want 4", got)
	}
	if got := line["outstanding_quantity"].(float64); got != 6 {
		t.Errorf("outstanding = %v, want 6", got)
	}
	if after["status"] != models.POStatusPartlyReceived {
		t.Errorf("status = %v, want %q", after["status"], models.POStatusPartlyReceived)
	}

	// Over-receiving the remainder is refused: 7 against 6 outstanding.
	over := fmt.Sprintf(`{"lines":[{"purchase_order_line_id":"%s","quantity":7}]}`, lineIDs[0])
	if rec := receiveGoods(t, h, e, actor, poID, over); rec.Code != http.StatusBadRequest {
		t.Errorf("over-receipt: got %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// Second shipment closes it.
	rest := fmt.Sprintf(`{"lines":[{"purchase_order_line_id":"%s","quantity":6}]}`, lineIDs[0])
	rec = receiveGoods(t, h, e, actor, poID, rest)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second receipt: got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	line = after["lines"].([]any)[0].(map[string]any)
	if got := line["outstanding_quantity"].(float64); got != 0 {
		t.Errorf("outstanding after full delivery = %v, want 0", got)
	}
	if after["status"] != models.POStatusReceived {
		t.Errorf("status = %v, want %q", after["status"], models.POStatusReceived)
	}
}

// Prevents: the receipt path growing a stock column by the back door. Quantity
// on hand is the sum of the ledger and must stay that way — the invariant the
// whole stock model rests on.
func TestStockOnHandStillEqualsTheSumOfMovementsAfterReceipts(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	clearRules(t, db, models.ApprovalPurchaseOrderIssue)
	product := fixtureProduct(t, db, 3) // opening balance

	po := createPO(t, h, e, actor, fmt.Sprintf(`{
		"supplier":"Cable Supplies","currency":"ZMW","exchange_rate":1,
		"lines":[{"product_id":"%s","description":"4mm cable","quantity":10,"unit_price":50}]
	}`, product.ID))
	poID := uuid.MustParse(po["id"].(string))
	lineIDs := poLineIDs(t, po)
	if rec := issuePO(t, h, e, actor, poID); rec.Code != http.StatusOK {
		t.Fatalf("issue: got %d: %s", rec.Code, rec.Body.String())
	}

	for _, qty := range []int{4, 6} {
		body := fmt.Sprintf(`{"lines":[{"purchase_order_line_id":"%s","quantity":%d}]}`, lineIDs[0], qty)
		if rec := receiveGoods(t, h, e, actor, poID, body); rec.Code != http.StatusCreated {
			t.Fatalf("receipt of %d: got %d: %s", qty, rec.Code, rec.Body.String())
		}
	}

	onHand, err := services.StockOnHand(db, product.ID)
	if err != nil {
		t.Fatalf("StockOnHand: %v", err)
	}
	if onHand != 13 { // 3 opening + 4 + 6
		t.Errorf("stock on hand = %d, want 13", onHand)
	}

	// And that figure is genuinely the sum of the rows, not a cached number.
	var summed *int
	if err := db.Model(&models.StockMovement{}).
		Where("product_id = ?", product.ID).
		Select("SUM(quantity)").Scan(&summed).Error; err != nil {
		t.Fatalf("sum movements: %v", err)
	}
	if summed == nil || *summed != onHand {
		t.Errorf("SUM(quantity) = %v, StockOnHand = %d — these must never disagree", summed, onHand)
	}

	var receipts int64
	db.Model(&models.StockMovement{}).
		Where("product_id = ? AND kind = ?", product.ID, models.StockReceipt).Count(&receipts)
	if receipts != 2 {
		t.Errorf("wrote %d receipt movements, want 2 — one per delivery", receipts)
	}
}

// Prevents: the whole point of the exercise being lost — freight, duty and
// clearing arriving in the system but never reaching what a unit cost. A
// generator bought for USD 4,000 does not cost USD 4,000.
func TestLandedCostReachesTheStockLedger(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	clearRules(t, db, models.ApprovalPurchaseOrderIssue)
	product := fixtureProduct(t, db, 0)

	// USD 4,000 a unit, two units, at 25 kwacha to the dollar => K200,000 goods.
	po := createPO(t, h, e, actor, fmt.Sprintf(`{
		"supplier":"Guangzhou Power Co","currency":"USD","exchange_rate":25,
		"incoterms":"FOB",
		"lines":[{"product_id":"%s","description":"20kVA generator","quantity":2,"unit_price":4000}]
	}`, product.ID))
	poID := uuid.MustParse(po["id"].(string))
	lineIDs := poLineIDs(t, po)
	if rec := issuePO(t, h, e, actor, poID); rec.Code != http.StatusOK {
		t.Fatalf("issue: got %d: %s", rec.Code, rec.Body.String())
	}

	// K30,000 of freight and K10,000 of duty on top.
	body := fmt.Sprintf(`{
		"lines":[{"purchase_order_line_id":"%s","quantity":2}],
		"exchange_rate":25,
		"basis":"value",
		"customs_assessment_ref":"C-2026-000123",
		"components":[
			{"kind":"freight","amount":30000,"currency":"ZMW","exchange_rate":1},
			{"kind":"duty","amount":10000,"currency":"ZMW","exchange_rate":1}
		]
	}`, lineIDs[0])
	if rec := receiveGoods(t, h, e, actor, poID, body); rec.Code != http.StatusCreated {
		t.Fatalf("receipt: got %d: %s", rec.Code, rec.Body.String())
	}

	var movement models.StockMovement
	if err := db.Where("product_id = ? AND kind = ?", product.ID, models.StockReceipt).
		First(&movement).Error; err != nil {
		t.Fatalf("no receipt movement: %v", err)
	}

	// Goods K200,000 + K40,000 landed, over 2 units = K120,000 each. The
	// supplier's price alone would have been K100,000 — a 20% understatement
	// that would have flowed into every margin figure downstream.
	const want = 120000.0
	if movement.UnitCost != want {
		t.Errorf("unit cost = %v, want %v (supplier price alone would be 100000)", movement.UnitCost, want)
	}

	// The components are retained, not collapsed into the result.
	var components int64
	db.Model(&models.LandedCostComponent{}).
		Joins("JOIN goods_receipts ON goods_receipts.id = landed_cost_components.goods_receipt_id").
		Where("goods_receipts.purchase_order_id = ?", poID).Count(&components)
	if components != 2 {
		t.Errorf("kept %d cost components, want 2 — the derivation must survive", components)
	}
}

// Prevents: a late clearing invoice being ignored because the goods were
// already costed. Late charges are normal; stock carried at what the first
// invoice happened to say is wrong.
func TestALateChargeRecostsTheLedgerInPlace(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	clearRules(t, db, models.ApprovalPurchaseOrderIssue)
	product := fixtureProduct(t, db, 0)

	po := createPO(t, h, e, actor, fmt.Sprintf(`{
		"supplier":"Guangzhou Power Co","currency":"ZMW","exchange_rate":1,
		"lines":[{"product_id":"%s","description":"20kVA generator","quantity":2,"unit_price":50000}]
	}`, product.ID))
	poID := uuid.MustParse(po["id"].(string))
	lineIDs := poLineIDs(t, po)
	if rec := issuePO(t, h, e, actor, poID); rec.Code != http.StatusOK {
		t.Fatalf("issue: got %d: %s", rec.Code, rec.Body.String())
	}
	body := fmt.Sprintf(`{
		"lines":[{"purchase_order_line_id":"%s","quantity":2}],
		"components":[{"kind":"freight","amount":20000,"currency":"ZMW","exchange_rate":1}]
	}`, lineIDs[0])
	if rec := receiveGoods(t, h, e, actor, poID, body); rec.Code != http.StatusCreated {
		t.Fatalf("receipt: got %d: %s", rec.Code, rec.Body.String())
	}

	var movement models.StockMovement
	if err := db.Where("product_id = ? AND kind = ?", product.ID, models.StockReceipt).
		First(&movement).Error; err != nil {
		t.Fatalf("no receipt movement: %v", err)
	}
	if movement.UnitCost != 60000 { // (100000 + 20000) / 2
		t.Fatalf("unit cost before the late charge = %v, want 60000", movement.UnitCost)
	}

	// The clearing agent's invoice turns up three weeks later.
	var receipt models.GoodsReceipt
	if err := db.Where("purchase_order_id = ?", poID).First(&receipt).Error; err != nil {
		t.Fatalf("no receipt row: %v", err)
	}
	c, rec := adminCtx(e, http.MethodPost, `{"kind":"clearing","amount":4000,"currency":"ZMW","exchange_rate":1}`, actor)
	if err := h.AdminAddLandedCostComponent(withReceiptParam(c, receipt.ID)); err != nil {
		t.Fatalf("AdminAddLandedCostComponent: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("add component: got %d: %s", rec.Code, rec.Body.String())
	}

	if err := db.First(&movement, "id = ?", movement.ID).Error; err != nil {
		t.Fatalf("reload movement: %v", err)
	}
	if movement.UnitCost != 62000 { // (100000 + 24000) / 2
		t.Errorf("unit cost after the late charge = %v, want 62000", movement.UnitCost)
	}

	// Recosting must not have moved any goods — only what they cost changed.
	onHand, err := services.StockOnHand(db, product.ID)
	if err != nil {
		t.Fatalf("StockOnHand: %v", err)
	}
	if onHand != 2 {
		t.Errorf("stock on hand = %d after recosting, want 2 — recosting must not move goods", onHand)
	}
}

// Prevents: a threshold in kwacha being cleared by a foreign-currency order.
// A USD 20,000 order is K500,000 and must be gated as such.
func TestApprovalThresholdIsAppliedToTheKwachaValue(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	// Threshold well above the USD figure but well below its kwacha value.
	fixtureRule(t, db, models.ApprovalPurchaseOrderIssue, 100000, 1, models.RoleSuperAdmin)

	po := createPO(t, h, e, actor, `{
		"supplier":"Guangzhou Power Co","currency":"USD","exchange_rate":25,
		"lines":[{"description":"switchgear","quantity":1,"unit_price":20000}]
	}`)
	poID := uuid.MustParse(po["id"].(string))

	rec := issuePO(t, h, e, actor, poID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("a USD 20,000 (K500,000) order must be gated by a K100,000 rule, got %d: %s",
			rec.Code, rec.Body.String())
	}
	var raised models.ApprovalRequest
	if err := db.Where("entity_id = ?", poID).First(&raised).Error; err != nil {
		t.Fatalf("no request raised: %v", err)
	}
	if raised.Amount != 500000 {
		t.Errorf("request raised for %v, want 500000 — the kwacha value, not the USD one", raised.Amount)
	}
}

// Prevents: an issued order being quietly rewritten, which would leave the
// approval that authorised it as evidence for a document that no longer exists.
func TestAnIssuedOrderCannotBeEdited(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	clearRules(t, db, models.ApprovalPurchaseOrderIssue)

	po := createPO(t, h, e, actor, `{
		"supplier":"Cable Supplies","currency":"ZMW","exchange_rate":1,
		"lines":[{"description":"4mm cable","quantity":10,"unit_price":50}]
	}`)
	poID := uuid.MustParse(po["id"].(string))
	if rec := issuePO(t, h, e, actor, poID); rec.Code != http.StatusOK {
		t.Fatalf("issue: got %d: %s", rec.Code, rec.Body.String())
	}

	c, rec := adminCtx(e, http.MethodPut, `{
		"supplier":"Cable Supplies","currency":"ZMW","exchange_rate":1,
		"lines":[{"description":"4mm cable","quantity":1000,"unit_price":50}]
	}`, actor)
	if err := h.AdminUpdatePurchaseOrder(withPOParam(c, poID)); err != nil {
		t.Fatalf("AdminUpdatePurchaseOrder: %v", err)
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("editing an issued order: got %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// Prevents: margin being struck against the supplier's price instead of what
// the goods actually landed for. This is the whole reason the buy side exists:
// for a business that imports and resells, a margin computed on the invoice
// price alone is wrong on every deal.
func TestLandedCostFlowsIntoMarginForAWonDeal(t *testing.T) {
	db := testDB(t)
	h := Handler{DB: db}
	e := echo.New()
	actor := fixtureUser(t, db, models.RoleAdmin)
	clearRules(t, db, models.ApprovalPurchaseOrderIssue)
	product := fixtureProduct(t, db, 0)
	deal := fixtureDeal(t, db, 200000, models.StageWon)

	// Buy 2 units at K50,000 with K20,000 of freight => K60,000 landed each.
	po := createPO(t, h, e, actor, fmt.Sprintf(`{
		"supplier":"Guangzhou Power Co","currency":"ZMW","exchange_rate":1,
		"lines":[{"product_id":"%s","description":"20kVA generator","quantity":2,"unit_price":50000}]
	}`, product.ID))
	poID := uuid.MustParse(po["id"].(string))
	lineIDs := poLineIDs(t, po)
	if rec := issuePO(t, h, e, actor, poID); rec.Code != http.StatusOK {
		t.Fatalf("issue: got %d: %s", rec.Code, rec.Body.String())
	}
	body := fmt.Sprintf(`{
		"lines":[{"purchase_order_line_id":"%s","quantity":2}],
		"components":[{"kind":"freight","amount":20000,"currency":"ZMW","exchange_rate":1}]
	}`, lineIDs[0])
	if rec := receiveGoods(t, h, e, actor, poID, body); rec.Code != http.StatusCreated {
		t.Fatalf("receipt: got %d: %s", rec.Code, rec.Body.String())
	}

	// Issue both units against the deal.
	issue := models.StockMovement{
		ProductID: product.ID, Kind: models.StockSale, Quantity: -2,
		OpportunityID: &deal.ID,
	}
	if err := services.RecordMovement(db, &issue); err != nil {
		t.Fatalf("issue stock to the deal: %v", err)
	}

	// And book a direct expense against it, using the field that until now
	// nothing ever set.
	exp := models.Expense{
		Supplier: "Site Crew Ltd", Category: models.ExpTransport,
		NetAmount: 5000, VatAmount: 0, VatTreatment: models.VatNone,
		IncurredAt: time.Now().UTC(), OpportunityID: &deal.ID,
	}
	if err := db.Create(&exp).Error; err != nil {
		t.Fatalf("create attributed expense: %v", err)
	}

	costing, err := services.CostDeal(db, deal)
	if err != nil {
		t.Fatalf("CostDeal: %v", err)
	}

	// 2 x K60,000 landed = K120,000, not the K100,000 the supplier invoiced.
	if costing.CostOfGoods != 120000 {
		t.Errorf("cost of goods = %v, want 120000 (supplier price alone would be 100000)", costing.CostOfGoods)
	}
	if costing.DirectExpenses != 5000 {
		t.Errorf("direct expenses = %v, want 5000", costing.DirectExpenses)
	}
	if costing.TotalCost != 125000 {
		t.Errorf("total cost = %v, want 125000", costing.TotalCost)
	}
	if costing.Margin != 75000 { // 200000 - 125000
		t.Errorf("margin = %v, want 75000", costing.Margin)
	}
	if costing.GoodsAtCostUnknown != 0 {
		t.Errorf("uncosted units = %d, want 0", costing.GoodsAtCostUnknown)
	}
	// Had freight been ignored, margin would have read K95,000 — a K20,000
	// overstatement on a single deal.
	if costing.Margin == 95000 {
		t.Error("margin was struck against the supplier price, not the landed cost")
	}
}

// Prevents: irrecoverable input VAT quietly vanishing from a job's cost. VAT
// the business paid and cannot reclaim is a real cost of the deal.
func TestIrrecoverableVatCountsAgainstTheDeal(t *testing.T) {
	db := testDB(t)
	deal := fixtureDeal(t, db, 100000, models.StageWon)

	// Standard-rated with no evidence at all: the VAT is sunk.
	sunk := models.Expense{
		Supplier: "Parts Depot", Category: models.ExpPurchases,
		NetAmount: 10000, VatAmount: 1600, VatTreatment: models.VatStandard,
		IncurredAt: time.Now().UTC(), OpportunityID: &deal.ID,
	}
	if err := db.Create(&sunk).Error; err != nil {
		t.Fatalf("create expense: %v", err)
	}

	costing, err := services.CostDeal(db, deal)
	if err != nil {
		t.Fatalf("CostDeal: %v", err)
	}
	if costing.IrrecoverableVat != 1600 {
		t.Errorf("irrecoverable VAT = %v, want 1600", costing.IrrecoverableVat)
	}
	if costing.TotalCost != 11600 {
		t.Errorf("total cost = %v, want 11600 — sunk VAT is a cost of the job", costing.TotalCost)
	}
}
