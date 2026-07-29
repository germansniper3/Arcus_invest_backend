package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"arcusinvest/internal/models"
	"arcusinvest/internal/services"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

var validLandedCostKinds = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range models.AllLandedCostKinds {
		m[k] = true
	}
	return m
}()

var validApportionmentBases = func() map[string]bool {
	m := map[string]bool{}
	for _, b := range models.AllApportionmentBases {
		m[b] = true
	}
	return m
}()

// receivedByLine totals what has already been received against each line of an
// order. It is a query rather than a stored column for the same reason stock on
// hand is: the total is the sum of the receipts, so it cannot drift from them.
func (h Handler) receivedByLine(db *gorm.DB, poID uuid.UUID) (map[uuid.UUID]int, error) {
	var rows []struct {
		PurchaseOrderLineID uuid.UUID
		Total               int
	}
	err := db.Model(&models.GoodsReceiptLine{}).
		Select("goods_receipt_lines.purchase_order_line_id, COALESCE(SUM(goods_receipt_lines.quantity), 0) AS total").
		Joins("JOIN goods_receipts ON goods_receipts.id = goods_receipt_lines.goods_receipt_id").
		Where("goods_receipts.purchase_order_id = ? AND goods_receipts.deleted_at IS NULL AND goods_receipt_lines.deleted_at IS NULL", poID).
		Group("goods_receipt_lines.purchase_order_line_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]int, len(rows))
	for _, r := range rows {
		out[r.PurchaseOrderLineID] = r.Total
	}
	return out, nil
}

// purchaseOrderJSON renders an order with the figures that are derived rather
// than stored — outstanding quantity per line above all, which is the number
// that says whether anything is still at sea.
func purchaseOrderJSON(p models.PurchaseOrder, received map[uuid.UUID]int) map[string]any {
	lines := make([]map[string]any, 0, len(p.Lines))
	var fullyReceived, anyReceived bool = true, false
	for _, l := range p.Lines {
		got := received[l.ID]
		outstanding := l.Quantity - got
		if outstanding < 0 {
			outstanding = 0
		}
		if got > 0 {
			anyReceived = true
		}
		if got < l.Quantity {
			fullyReceived = false
		}
		lines = append(lines, map[string]any{
			"id": l.ID, "product_id": l.ProductID,
			"description":          l.Description,
			"quantity":             l.Quantity,
			"unit_price":           l.UnitPrice,
			"line_total":           l.LineTotal(),
			"position":             l.Position,
			"received_quantity":    got,
			"outstanding_quantity": outstanding,
		})
	}
	if len(p.Lines) == 0 {
		fullyReceived = false
	}
	return map[string]any{
		"id": p.ID, "created_at": p.CreatedAt, "updated_at": p.UpdatedAt,
		"number": p.Number, "supplier": p.Supplier, "supplier_tpin": p.SupplierTPIN,
		"currency": p.Currency, "exchange_rate": p.ExchangeRate,
		"status":            p.Status,
		"order_date":        p.OrderDate,
		"expected_delivery": p.ExpectedDelivery,
		"incoterms":         p.Incoterms,
		"shipping_term":     p.ShippingTerm,
		"opportunity_id":    p.OpportunityID,
		"notes":             p.Notes,
		"issued_at":         p.IssuedAt,
		"raised_by":         p.RaisedBy,
		"subtotal":          p.Subtotal(),
		"subtotal_zmw":      p.SubtotalZMW(),
		// Says out loud where the order stands on delivery, so no screen has to
		// re-derive it from the line quantities and get it subtly wrong.
		"fully_received":  fullyReceived,
		"partly_received": anyReceived && !fullyReceived,
		"lines":           lines,
	}
}

func (h Handler) loadPurchaseOrder(id uuid.UUID) (models.PurchaseOrder, map[uuid.UUID]int, error) {
	var po models.PurchaseOrder
	if err := h.DB.Preload("Lines", func(db *gorm.DB) *gorm.DB {
		return db.Order("position ASC")
	}).First(&po, "id = ?", id).Error; err != nil {
		return po, nil, err
	}
	received, err := h.receivedByLine(h.DB, po.ID)
	if err != nil {
		return po, nil, err
	}
	return po, received, nil
}

// AdminListPurchaseOrders returns the order book, newest first.
func (h Handler) AdminListPurchaseOrders(c echo.Context) error {
	q := h.DB.Preload("Lines", func(db *gorm.DB) *gorm.DB {
		return db.Order("position ASC")
	}).Order("created_at DESC")
	if status := strings.TrimSpace(c.QueryParam("status")); status != "" {
		q = q.Where("status = ?", status)
	}
	if supplier := strings.TrimSpace(c.QueryParam("supplier")); supplier != "" {
		q = q.Where("supplier ILIKE ?", "%"+supplier+"%")
	}
	if oppID := strings.TrimSpace(c.QueryParam("opportunity_id")); oppID != "" {
		parsed, err := uuid.Parse(oppID)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errResponse("invalid opportunity id"))
		}
		q = q.Where("opportunity_id = ?", parsed)
	}
	var rows []models.PurchaseOrder
	if err := q.Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load purchase orders"))
	}
	out := make([]map[string]any, 0, len(rows))
	for _, po := range rows {
		received, err := h.receivedByLine(h.DB, po.ID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errResponse("could not load purchase orders"))
		}
		out = append(out, purchaseOrderJSON(po, received))
	}
	return c.JSON(http.StatusOK, map[string]any{"items": out})
}

func (h Handler) AdminGetPurchaseOrder(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid purchase order id"))
	}
	po, received, err := h.loadPurchaseOrder(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, errResponse("purchase order not found"))
		}
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the purchase order"))
	}
	return c.JSON(http.StatusOK, purchaseOrderJSON(po, received))
}

type purchaseOrderLineRequest struct {
	ProductID   *uuid.UUID `json:"product_id"`
	Description string     `json:"description"`
	Quantity    int        `json:"quantity"`
	UnitPrice   float64    `json:"unit_price"`
}

type purchaseOrderRequest struct {
	Supplier         string                     `json:"supplier"`
	SupplierTPIN     string                     `json:"supplier_tpin"`
	Currency         string                     `json:"currency"`
	ExchangeRate     float64                    `json:"exchange_rate"`
	OrderDate        *time.Time                 `json:"order_date"`
	ExpectedDelivery *time.Time                 `json:"expected_delivery"`
	Incoterms        string                     `json:"incoterms"`
	ShippingTerm     string                     `json:"shipping_term"`
	OpportunityID    *uuid.UUID                 `json:"opportunity_id"`
	Notes            string                     `json:"notes"`
	Lines            []purchaseOrderLineRequest `json:"lines"`
}

func validatePurchaseOrder(req purchaseOrderRequest) string {
	if strings.TrimSpace(req.Supplier) == "" {
		return "supplier is required"
	}
	if len(req.Lines) == 0 {
		return "a purchase order needs at least one line"
	}
	if strings.TrimSpace(req.Currency) == "" {
		return "currency is required"
	}
	// A zero or negative rate would silently value the whole order at nothing.
	if req.ExchangeRate <= 0 {
		return "exchange rate must be greater than zero"
	}
	for i, l := range req.Lines {
		if strings.TrimSpace(l.Description) == "" {
			return fmt.Sprintf("line %d needs a description", i+1)
		}
		if l.Quantity <= 0 {
			return fmt.Sprintf("line %d needs a quantity greater than zero", i+1)
		}
		if l.UnitPrice < 0 {
			return fmt.Sprintf("line %d cannot have a negative unit price", i+1)
		}
	}
	return ""
}

func linesFromRequest(reqLines []purchaseOrderLineRequest) []models.PurchaseOrderLine {
	out := make([]models.PurchaseOrderLine, 0, len(reqLines))
	for i, l := range reqLines {
		out = append(out, models.PurchaseOrderLine{
			ProductID:   l.ProductID,
			Description: strings.TrimSpace(l.Description),
			Quantity:    l.Quantity,
			UnitPrice:   l.UnitPrice,
			Position:    i,
		})
	}
	return out
}

// AdminCreatePurchaseOrder raises a draft order. Creating one commits the
// business to nothing, so it is deliberately ungated — the approval sits on
// issue, which is the act the supplier can hold us to.
func (h Handler) AdminCreatePurchaseOrder(c echo.Context) error {
	var req purchaseOrderRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if req.Currency == "" {
		req.Currency = "ZMW"
	}
	if req.ExchangeRate == 0 {
		req.ExchangeRate = 1
	}
	if reason := validatePurchaseOrder(req); reason != "" {
		return c.JSON(http.StatusBadRequest, errResponse(reason))
	}

	orderDate := time.Now().UTC()
	if req.OrderDate != nil {
		orderDate = *req.OrderDate
	}
	po := models.PurchaseOrder{
		Supplier:         strings.TrimSpace(req.Supplier),
		SupplierTPIN:     strings.TrimSpace(req.SupplierTPIN),
		Currency:         strings.ToUpper(strings.TrimSpace(req.Currency)),
		ExchangeRate:     req.ExchangeRate,
		Status:           models.POStatusDraft,
		OrderDate:        orderDate,
		ExpectedDelivery: req.ExpectedDelivery,
		Incoterms:        strings.TrimSpace(req.Incoterms),
		ShippingTerm:     strings.TrimSpace(req.ShippingTerm),
		OpportunityID:    req.OpportunityID,
		Notes:            strings.TrimSpace(req.Notes),
		Lines:            linesFromRequest(req.Lines),
	}
	var actor models.User
	if err := h.DB.First(&actor, "id = ?", c.Get("user_id")).Error; err == nil {
		po.RaisedByID = &actor.ID
		po.RaisedBy = actor.FullName
	}
	if err := h.DB.Create(&po).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not create the purchase order"))
	}
	loaded, received, err := h.loadPurchaseOrder(po.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the purchase order"))
	}
	return c.JSON(http.StatusCreated, purchaseOrderJSON(loaded, received))
}

// AdminUpdatePurchaseOrder edits a draft.
//
// Only a draft may be edited. Once an order has been issued it is a document
// the supplier holds a copy of, and silently rewriting its lines would make the
// approval that authorised it evidence for something that no longer exists.
func (h Handler) AdminUpdatePurchaseOrder(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid purchase order id"))
	}
	var po models.PurchaseOrder
	if err := h.DB.First(&po, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, errResponse("purchase order not found"))
		}
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the purchase order"))
	}
	if po.Status != models.POStatusDraft && po.Status != models.POStatusRejected {
		return c.JSON(http.StatusConflict, errResponse("only a draft purchase order can be edited"))
	}

	var req purchaseOrderRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if req.Currency == "" {
		req.Currency = po.Currency
	}
	if req.ExchangeRate == 0 {
		req.ExchangeRate = po.ExchangeRate
	}
	if reason := validatePurchaseOrder(req); reason != "" {
		return c.JSON(http.StatusBadRequest, errResponse(reason))
	}

	err = h.DB.Transaction(func(tx *gorm.DB) error {
		updates := map[string]any{
			"supplier":          strings.TrimSpace(req.Supplier),
			"supplier_tpin":     strings.TrimSpace(req.SupplierTPIN),
			"currency":          strings.ToUpper(strings.TrimSpace(req.Currency)),
			"exchange_rate":     req.ExchangeRate,
			"expected_delivery": req.ExpectedDelivery,
			"incoterms":         strings.TrimSpace(req.Incoterms),
			"shipping_term":     strings.TrimSpace(req.ShippingTerm),
			"opportunity_id":    req.OpportunityID,
			"notes":             strings.TrimSpace(req.Notes),
			// An edited order is a fresh proposal: a rejected one returns to draft
			// rather than staying rejected, so resubmission is an explicit act.
			"status": models.POStatusDraft,
		}
		if req.OrderDate != nil {
			updates["order_date"] = *req.OrderDate
		}
		if err := tx.Model(&po).Updates(updates).Error; err != nil {
			return err
		}
		// Lines are replaced wholesale, as they are on an opportunity. A draft has
		// no receipts against it, so nothing can be orphaned by this.
		if err := tx.Unscoped().Delete(&models.PurchaseOrderLine{}, "purchase_order_id = ?", po.ID).Error; err != nil {
			return err
		}
		lines := linesFromRequest(req.Lines)
		for i := range lines {
			lines[i].PurchaseOrderID = po.ID
		}
		return tx.Create(&lines).Error
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update the purchase order"))
	}

	loaded, received, err := h.loadPurchaseOrder(po.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the purchase order"))
	}
	return c.JSON(http.StatusOK, purchaseOrderJSON(loaded, received))
}

// nextPurchaseOrderNumber allocates the supplier-facing reference.
//
// Allocated on issue rather than on create, so drafts that are abandoned do not
// punch holes in a sequence an auditor expects to run unbroken.
func nextPurchaseOrderNumber(tx *gorm.DB, at time.Time) (string, error) {
	year := at.Year()
	prefix := fmt.Sprintf("PO-%d-", year)
	var count int64
	if err := tx.Model(&models.PurchaseOrder{}).
		Where("number LIKE ?", prefix+"%").Count(&count).Error; err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%04d", prefix, count+1), nil
}

// AdminIssuePurchaseOrder sends an order to the supplier, through the approval
// engine.
//
// This is the committing act and the only one gated. The gate is the existing
// threshold engine — the same rules table that governs expenses and payments —
// rather than a second mechanism, so "over K X needs N approvals from role R"
// is configured in exactly one place.
func (h Handler) AdminIssuePurchaseOrder(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid purchase order id"))
	}
	po, _, err := h.loadPurchaseOrder(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, errResponse("purchase order not found"))
		}
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the purchase order"))
	}
	// Issuable states. pending_approval is here because the gate itself puts the
	// order there while a request is open: leaving it out means an approved
	// order can never be issued, since the retry that spends the approval is
	// refused by its own waiting state. Rejected is absent deliberately — a
	// rejected order must be edited, which returns it to draft, rather than
	// retried until the answer changes.
	switch po.Status {
	case models.POStatusDraft, models.POStatusPendingApproval, models.POStatusApproved:
	default:
		return c.JSON(http.StatusConflict, errResponse("only a draft purchase order can be issued"))
	}
	if len(po.Lines) == 0 {
		return c.JSON(http.StatusBadRequest, errResponse("a purchase order needs at least one line before it can be issued"))
	}

	// Gated on the kwacha value, because a threshold in the rules table is in
	// kwacha and a USD order compared against it directly would clear a K500,000
	// gate at USD 20,000.
	amount := po.SubtotalZMW()
	blocked, gerr := h.gate(c, models.ApprovalPurchaseOrderIssue, models.ApprovalEntityPurchaseOrder, po.ID, amount,
		fmt.Sprintf("Issue purchase order of %s to %q", zmw(amount), po.Supplier))
	if gerr != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not check approval requirements"))
	}
	if blocked != nil {
		// Reflect the wait on the order itself, so the queue is visible from the
		// order and not only from the approvals screen.
		if po.Status == models.POStatusDraft && blocked.Status == models.ApprovalStatusPending {
			h.DB.Model(&po).Update("status", models.POStatusPendingApproval)
		}
		if blocked.Status == models.ApprovalStatusRejected {
			h.DB.Model(&po).Update("status", models.POStatusRejected)
		}
		return h.blockedResponse(c, blocked)
	}

	now := time.Now().UTC()
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		number := po.Number
		if number == "" {
			n, nerr := nextPurchaseOrderNumber(tx, now)
			if nerr != nil {
				return nerr
			}
			number = n
		}
		return tx.Model(&models.PurchaseOrder{}).Where("id = ?", po.ID).Updates(map[string]any{
			"status":    models.POStatusIssued,
			"issued_at": now,
			"number":    number,
		}).Error
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not issue the purchase order"))
	}

	loaded, received, err := h.loadPurchaseOrder(po.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the purchase order"))
	}
	return c.JSON(http.StatusOK, purchaseOrderJSON(loaded, received))
}

// AdminCancelPurchaseOrder closes an order that will not be fulfilled.
func (h Handler) AdminCancelPurchaseOrder(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid purchase order id"))
	}
	var po models.PurchaseOrder
	if err := h.DB.First(&po, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, errResponse("purchase order not found"))
		}
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the purchase order"))
	}
	// Goods already received cannot be un-received by cancelling the paperwork.
	received, err := h.receivedByLine(h.DB, po.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the purchase order"))
	}
	for _, got := range received {
		if got > 0 {
			return c.JSON(http.StatusConflict, errResponse("this order has goods receipted against it and cannot be cancelled"))
		}
	}
	if err := h.DB.Model(&po).Update("status", models.POStatusCancelled).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not cancel the purchase order"))
	}
	loaded, rec, err := h.loadPurchaseOrder(po.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the purchase order"))
	}
	return c.JSON(http.StatusOK, purchaseOrderJSON(loaded, rec))
}

// --- Goods receipt ---------------------------------------------------------

type goodsReceiptLineRequest struct {
	PurchaseOrderLineID uuid.UUID `json:"purchase_order_line_id"`
	Quantity            int       `json:"quantity"`
	Weight              float64   `json:"weight"`
}

type landedCostComponentRequest struct {
	Kind         string     `json:"kind"`
	Description  string     `json:"description"`
	Currency     string     `json:"currency"`
	Amount       float64    `json:"amount"`
	ExchangeRate float64    `json:"exchange_rate"`
	Reference    string     `json:"reference"`
	IncurredAt   *time.Time `json:"incurred_at"`
}

type goodsReceiptRequest struct {
	ReceivedAt           *time.Time                   `json:"received_at"`
	Reference            string                       `json:"reference"`
	CustomsAssessmentRef string                       `json:"customs_assessment_ref"`
	ExchangeRate         float64                      `json:"exchange_rate"`
	Basis                string                       `json:"basis"`
	Notes                string                       `json:"notes"`
	Lines                []goodsReceiptLineRequest    `json:"lines"`
	Components           []landedCostComponentRequest `json:"components"`
}

// AdminReceiveGoods records one physical arrival against an issued order.
//
// This is the second of the three events and it is deliberately separate from
// both the order and the supplier invoice. It writes the stock ledger; it does
// not create a payable. Goods routinely arrive before their invoice, and a
// storekeeper who cannot receive them until accounts have the paperwork will
// simply stop using the system.
//
// Partial receipt is the normal case, not an edge case: a line may be received
// across several deliveries, and the outstanding quantity is what is left.
func (h Handler) AdminReceiveGoods(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid purchase order id"))
	}
	po, alreadyReceived, err := h.loadPurchaseOrder(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, errResponse("purchase order not found"))
		}
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the purchase order"))
	}
	// Only an issued order may receive goods. Receiving against a draft would
	// put stock on the shelf that nobody authorised buying.
	if po.Status != models.POStatusIssued && po.Status != models.POStatusPartlyReceived {
		return c.JSON(http.StatusConflict, errResponse("goods can only be received against an issued purchase order"))
	}

	var req goodsReceiptRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if len(req.Lines) == 0 {
		return c.JSON(http.StatusBadRequest, errResponse("a goods receipt needs at least one line"))
	}
	if req.ExchangeRate == 0 {
		req.ExchangeRate = po.ExchangeRate
	}
	if req.ExchangeRate <= 0 {
		return c.JSON(http.StatusBadRequest, errResponse("exchange rate must be greater than zero"))
	}
	if req.Basis == "" {
		req.Basis = models.BasisValue
	}
	if !validApportionmentBases[req.Basis] {
		return c.JSON(http.StatusBadRequest, errResponse("invalid apportionment basis"))
	}
	for _, comp := range req.Components {
		if !validLandedCostKinds[comp.Kind] {
			return c.JSON(http.StatusBadRequest, errResponse("invalid landed cost kind"))
		}
		if comp.Amount < 0 {
			return c.JSON(http.StatusBadRequest, errResponse("a cost component cannot be negative"))
		}
	}

	// Index the order's lines, and check every receipt line belongs to this
	// order and does not exceed what is still outstanding on it.
	byID := map[uuid.UUID]models.PurchaseOrderLine{}
	for _, l := range po.Lines {
		byID[l.ID] = l
	}
	for i, rl := range req.Lines {
		ol, ok := byID[rl.PurchaseOrderLineID]
		if !ok {
			return c.JSON(http.StatusBadRequest, errResponse(fmt.Sprintf("line %d is not on this purchase order", i+1)))
		}
		if rl.Quantity <= 0 {
			return c.JSON(http.StatusBadRequest, errResponse(fmt.Sprintf("line %d needs a quantity greater than zero", i+1)))
		}
		outstanding := ol.Quantity - alreadyReceived[ol.ID]
		if rl.Quantity > outstanding {
			return c.JSON(http.StatusBadRequest, errResponse(fmt.Sprintf(
				"line %d: receiving %d but only %d outstanding", i+1, rl.Quantity, outstanding)))
		}
	}

	receivedAt := time.Now().UTC()
	if req.ReceivedAt != nil {
		receivedAt = *req.ReceivedAt
	}

	receipt := models.GoodsReceipt{
		PurchaseOrderID:      po.ID,
		ReceivedAt:           receivedAt,
		Reference:            strings.TrimSpace(req.Reference),
		CustomsAssessmentRef: strings.TrimSpace(req.CustomsAssessmentRef),
		ExchangeRate:         req.ExchangeRate,
		Basis:                req.Basis,
		Notes:                strings.TrimSpace(req.Notes),
	}
	var actor models.User
	if err := h.DB.First(&actor, "id = ?", c.Get("user_id")).Error; err == nil {
		receipt.ReceivedByID = &actor.ID
		receipt.ReceivedBy = actor.FullName
	}

	err = h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&receipt).Error; err != nil {
			return err
		}
		for _, comp := range req.Components {
			rate := comp.ExchangeRate
			if rate == 0 {
				rate = 1
			}
			currency := strings.ToUpper(strings.TrimSpace(comp.Currency))
			if currency == "" {
				currency = "ZMW"
			}
			incurred := receivedAt
			if comp.IncurredAt != nil {
				incurred = *comp.IncurredAt
			}
			row := models.LandedCostComponent{
				GoodsReceiptID: receipt.ID,
				Kind:           comp.Kind,
				Description:    strings.TrimSpace(comp.Description),
				Currency:       currency,
				Amount:         comp.Amount,
				ExchangeRate:   rate,
				Reference:      strings.TrimSpace(comp.Reference),
				IncurredAt:     incurred,
				RecordedByID:   receipt.ReceivedByID,
				RecordedBy:     receipt.ReceivedBy,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}

		lines := make([]models.GoodsReceiptLine, 0, len(req.Lines))
		for _, rl := range req.Lines {
			lines = append(lines, models.GoodsReceiptLine{
				GoodsReceiptID:      receipt.ID,
				PurchaseOrderLineID: rl.PurchaseOrderLineID,
				Quantity:            rl.Quantity,
				Weight:              rl.Weight,
			})
		}
		if err := tx.Create(&lines).Error; err != nil {
			return err
		}

		// Cost the receipt, then write the stock ledger with the landed figure.
		if err := h.apportionAndPost(tx, receipt, lines, byID, actor); err != nil {
			return err
		}

		return h.refreshPurchaseOrderStatus(tx, po.ID)
	})
	if err != nil {
		if errors.Is(err, services.ErrInsufficientStock) {
			return c.JSON(http.StatusConflict, errResponse("that receipt would take stock negative"))
		}
		return c.JSON(http.StatusInternalServerError, errResponse("could not record the goods receipt"))
	}

	loaded, rec, err := h.loadPurchaseOrder(po.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the purchase order"))
	}
	return c.JSON(http.StatusCreated, purchaseOrderJSON(loaded, rec))
}

// apportionAndPost spreads the receipt's cost components across its lines and
// writes a stock movement per line that carries a catalogue product.
//
// The unit cost written to the ledger is the landed cost: the supplier's price
// at this receipt's rate, plus the line's share of freight, duty and clearing.
// That is the number cost of sales draws on, so getting it right here is what
// makes every downstream margin figure true.
func (h Handler) apportionAndPost(
	tx *gorm.DB,
	receipt models.GoodsReceipt,
	lines []models.GoodsReceiptLine,
	orderLines map[uuid.UUID]models.PurchaseOrderLine,
	actor models.User,
) error {
	var components []models.LandedCostComponent
	if err := tx.Find(&components, "goods_receipt_id = ?", receipt.ID).Error; err != nil {
		return err
	}
	var totalComponents float64
	for _, comp := range components {
		totalComponents += comp.AmountZMW()
	}

	weights := make([]services.LineBasisWeight, 0, len(lines))
	for _, l := range lines {
		ol := orderLines[l.PurchaseOrderLineID]
		valueZMW := float64(l.Quantity) * ol.UnitPrice * receipt.ExchangeRate
		weights = append(weights, services.LineBasisWeight{
			ValueNgwee:  int64(valueZMW*100 + 0.5),
			Quantity:    int64(l.Quantity),
			WeightGrams: int64(l.Weight*1000 + 0.5),
		})
	}

	shares, err := services.ApportionReceipt(receipt.Basis, totalComponents, weights)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for i := range lines {
		ol := orderLines[lines[i].PurchaseOrderLineID]
		goodsZMW := float64(lines[i].Quantity) * ol.UnitPrice * receipt.ExchangeRate
		lineTotal := goodsZMW + shares[i]
		unitCost := lineTotal / float64(lines[i].Quantity)

		lines[i].ApportionedZMW = shares[i]
		lines[i].UnitCostZMW = unitCost

		// A line with no catalogue product never reaches the stock ledger — a
		// service or a one-off has no quantity on hand to move — but it is still
		// costed above, because it still consumed part of the freight.
		if ol.ProductID != nil {
			movement := models.StockMovement{
				ProductID:     *ol.ProductID,
				Kind:          models.StockReceipt,
				Quantity:      lines[i].Quantity,
				UnitCost:      unitCost,
				OccurredAt:    receipt.ReceivedAt,
				OpportunityID: nil,
			}
			if actor.ID != uuid.Nil {
				movement.ActorID = &actor.ID
				movement.ActorName = actor.FullName
			}
			if err := services.RecordMovement(tx, &movement); err != nil {
				return err
			}
			lines[i].StockMovementID = &movement.ID
		}

		if err := tx.Model(&models.GoodsReceiptLine{}).Where("id = ?", lines[i].ID).Updates(map[string]any{
			"apportioned_zmw":   lines[i].ApportionedZMW,
			"unit_cost_zmw":     lines[i].UnitCostZMW,
			"stock_movement_id": lines[i].StockMovementID,
		}).Error; err != nil {
			return err
		}
	}

	return tx.Model(&models.GoodsReceipt{}).Where("id = ?", receipt.ID).
		Update("apportioned_at", now).Error
}

// refreshPurchaseOrderStatus moves the order between issued, partly received
// and received based on what the receipts actually add up to. It is derived
// from the ledger rather than set by the caller, so the status cannot disagree
// with the quantities underneath it.
func (h Handler) refreshPurchaseOrderStatus(tx *gorm.DB, poID uuid.UUID) error {
	var po models.PurchaseOrder
	if err := tx.Preload("Lines").First(&po, "id = ?", poID).Error; err != nil {
		return err
	}
	received, err := h.receivedByLine(tx, poID)
	if err != nil {
		return err
	}
	all, any := true, false
	for _, l := range po.Lines {
		got := received[l.ID]
		if got > 0 {
			any = true
		}
		if got < l.Quantity {
			all = false
		}
	}
	status := po.Status
	switch {
	case all && len(po.Lines) > 0:
		status = models.POStatusReceived
	case any:
		status = models.POStatusPartlyReceived
	}
	if status == po.Status {
		return nil
	}
	return tx.Model(&models.PurchaseOrder{}).Where("id = ?", poID).Update("status", status).Error
}

// AdminListGoodsReceipts returns the delivery history for an order, with the
// cost components that were apportioned into it.
func (h Handler) AdminListGoodsReceipts(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid purchase order id"))
	}
	var rows []models.GoodsReceipt
	if err := h.DB.Preload("Lines").Preload("Components").
		Where("purchase_order_id = ?", id).
		Order("received_at DESC").Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load goods receipts"))
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"id": r.ID, "purchase_order_id": r.PurchaseOrderID,
			"received_at": r.ReceivedAt, "reference": r.Reference,
			"customs_assessment_ref": r.CustomsAssessmentRef,
			"exchange_rate":          r.ExchangeRate,
			"basis":                  r.Basis,
			"apportioned_at":         r.ApportionedAt,
			"notes":                  r.Notes,
			"received_by":            r.ReceivedBy,
			"components_total_zmw":   r.TotalComponentsZMW(),
			"lines":                  r.Lines,
			"components":             r.Components,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"items": out})
}

// AdminAddLandedCostComponent books a charge against a receipt after the fact
// and recalculates the affected unit costs.
//
// Late charges are normal, not exceptional — a clearing agent's invoice
// routinely lands weeks after the goods. Recalculating rather than ignoring is
// the difference between stock carried at what it cost and stock carried at
// what the first invoice happened to say.
func (h Handler) AdminAddLandedCostComponent(c echo.Context) error {
	receiptID, err := uuid.Parse(c.Param("receiptId"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid goods receipt id"))
	}
	var receipt models.GoodsReceipt
	if err := h.DB.Preload("Lines").First(&receipt, "id = ?", receiptID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, errResponse("goods receipt not found"))
		}
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the goods receipt"))
	}

	var req landedCostComponentRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if !validLandedCostKinds[req.Kind] {
		return c.JSON(http.StatusBadRequest, errResponse("invalid landed cost kind"))
	}
	if req.Amount < 0 {
		return c.JSON(http.StatusBadRequest, errResponse("a cost component cannot be negative"))
	}
	rate := req.ExchangeRate
	if rate == 0 {
		rate = 1
	}
	currency := strings.ToUpper(strings.TrimSpace(req.Currency))
	if currency == "" {
		currency = "ZMW"
	}
	incurred := time.Now().UTC()
	if req.IncurredAt != nil {
		incurred = *req.IncurredAt
	}

	var actor models.User
	h.DB.First(&actor, "id = ?", c.Get("user_id"))

	err = h.DB.Transaction(func(tx *gorm.DB) error {
		comp := models.LandedCostComponent{
			GoodsReceiptID: receipt.ID,
			Kind:           req.Kind,
			Description:    strings.TrimSpace(req.Description),
			Currency:       currency,
			Amount:         req.Amount,
			ExchangeRate:   rate,
			Reference:      strings.TrimSpace(req.Reference),
			IncurredAt:     incurred,
		}
		if actor.ID != uuid.Nil {
			comp.RecordedByID = &actor.ID
			comp.RecordedBy = actor.FullName
		}
		if err := tx.Create(&comp).Error; err != nil {
			return err
		}
		return h.recostReceipt(tx, receipt.ID, actor)
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not add the cost component"))
	}
	return c.JSON(http.StatusCreated, map[string]any{"ok": true})
}

// recostReceipt re-runs the apportionment for a receipt whose components have
// changed, and corrects the unit cost already written to the stock ledger.
//
// The movement is updated in place rather than reversed and reposted. The
// quantity has not changed and no goods moved — only what they turned out to
// cost — so a compensating pair of movements would misrepresent the shelf as
// having emptied and refilled.
func (h Handler) recostReceipt(tx *gorm.DB, receiptID uuid.UUID, actor models.User) error {
	var receipt models.GoodsReceipt
	if err := tx.Preload("Lines").First(&receipt, "id = ?", receiptID).Error; err != nil {
		return err
	}
	orderLines := map[uuid.UUID]models.PurchaseOrderLine{}
	var po models.PurchaseOrder
	if err := tx.Preload("Lines").First(&po, "id = ?", receipt.PurchaseOrderID).Error; err != nil {
		return err
	}
	for _, l := range po.Lines {
		orderLines[l.ID] = l
	}

	var components []models.LandedCostComponent
	if err := tx.Find(&components, "goods_receipt_id = ?", receipt.ID).Error; err != nil {
		return err
	}
	var totalComponents float64
	for _, comp := range components {
		totalComponents += comp.AmountZMW()
	}

	weights := make([]services.LineBasisWeight, 0, len(receipt.Lines))
	for _, l := range receipt.Lines {
		ol := orderLines[l.PurchaseOrderLineID]
		valueZMW := float64(l.Quantity) * ol.UnitPrice * receipt.ExchangeRate
		weights = append(weights, services.LineBasisWeight{
			ValueNgwee:  int64(valueZMW*100 + 0.5),
			Quantity:    int64(l.Quantity),
			WeightGrams: int64(l.Weight*1000 + 0.5),
		})
	}
	shares, err := services.ApportionReceipt(receipt.Basis, totalComponents, weights)
	if err != nil {
		return err
	}

	for i, l := range receipt.Lines {
		ol := orderLines[l.PurchaseOrderLineID]
		goodsZMW := float64(l.Quantity) * ol.UnitPrice * receipt.ExchangeRate
		unitCost := (goodsZMW + shares[i]) / float64(l.Quantity)
		if err := tx.Model(&models.GoodsReceiptLine{}).Where("id = ?", l.ID).Updates(map[string]any{
			"apportioned_zmw": shares[i],
			"unit_cost_zmw":   unitCost,
		}).Error; err != nil {
			return err
		}
		if l.StockMovementID != nil {
			if err := tx.Model(&models.StockMovement{}).Where("id = ?", *l.StockMovementID).
				Update("unit_cost", unitCost).Error; err != nil {
				return err
			}
		}
	}

	return tx.Model(&models.GoodsReceipt{}).Where("id = ?", receipt.ID).
		Update("apportioned_at", time.Now().UTC()).Error
}

// --- Job costing -----------------------------------------------------------

// AdminDealCosting reports margin on one deal: its value, less the goods it
// consumed at landed cost, less what was booked directly against it.
//
// It lives next to the deal rather than on a reporting screen of its own. A
// margin somebody has to go and look for does not change behaviour; one sitting
// beside the deal value does.
func (h Handler) AdminDealCosting(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid opportunity id"))
	}
	var opp models.Opportunity
	if err := h.DB.First(&opp, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return c.JSON(http.StatusNotFound, errResponse("deal not found"))
		}
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the deal"))
	}
	costing, err := services.CostDeal(h.DB, opp)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not cost the deal"))
	}
	return c.JSON(http.StatusOK, costing)
}
