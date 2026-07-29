package handlers

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"arcusinvest/internal/models"
	"arcusinvest/internal/services"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

var validVatTreatments = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range models.AllVatTreatments {
		m[t] = true
	}
	return m
}()

var validExpenseCategories = func() map[string]bool {
	m := map[string]bool{}
	for _, cat := range models.AllExpenseCategories {
		m[cat] = true
	}
	return m
}()

// expenseJSON renders an expense with the figures that are computed rather than
// stored, so no caller has to re-derive them and get it wrong.
func expenseJSON(e models.Expense) map[string]any {
	return map[string]any{
		"id": e.ID, "created_at": e.CreatedAt, "updated_at": e.UpdatedAt,
		"supplier": e.Supplier, "supplier_tpin": e.SupplierTPIN,
		"category": e.Category, "reference": e.Reference,
		"smart_invoice_ref":      e.SmartInvoiceRef,
		"customs_assessment_ref": e.CustomsAssessmentRef,
		"purchase_order_id":      e.PurchaseOrderID,
		"net_amount":             e.NetAmount,
		"vat_amount":             e.VatAmount,
		"vat_treatment":          e.VatTreatment,
		"gross":                  e.Gross(),
		"reclaimable_vat":        e.ReclaimableVat(),
		// Says out loud why VAT on this expense is or is not recoverable, so the
		// screen never has to reimplement the evidence rules — Smart Invoice for a
		// domestic supply, customs assessment for an import.
		"vat_recoverable": e.ReclaimableVat() > 0,
		"settled":         e.Settled(),
		"outstanding":     e.Outstanding(),
		"incurred_at":     e.IncurredAt,
		"due_date":        e.DueDate,
		"opportunity_id":  e.OpportunityID,
		"notes":           e.Notes,
		"recorded_by":     e.RecordedBy,
		"settlements":     e.Settlements,
	}
}

// AdminListExpenses returns the expense ledger, newest first.
func (h Handler) AdminListExpenses(c echo.Context) error {
	q := h.DB.Preload("Settlements").Order("incurred_at DESC")
	if cat := strings.TrimSpace(c.QueryParam("category")); cat != "" {
		q = q.Where("category = ?", cat)
	}
	if oppID := strings.TrimSpace(c.QueryParam("opportunity_id")); oppID != "" {
		parsed, err := uuid.Parse(oppID)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errResponse("invalid opportunity id"))
		}
		q = q.Where("opportunity_id = ?", parsed)
	}
	var rows []models.Expense
	if err := q.Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load expenses"))
	}
	out := make([]map[string]any, 0, len(rows))
	for _, e := range rows {
		out = append(out, expenseJSON(e))
	}
	return c.JSON(http.StatusOK, map[string]any{"items": out})
}

type expenseRequest struct {
	Supplier        string `json:"supplier"`
	SupplierTPIN    string `json:"supplier_tpin"`
	Category        string `json:"category"`
	Reference       string `json:"reference"`
	SmartInvoiceRef string `json:"smart_invoice_ref"`
	// CustomsAssessmentRef is the import's evidence for its input VAT. See the
	// field comment on models.Expense for why it is not the Smart Invoice field.
	CustomsAssessmentRef string     `json:"customs_assessment_ref"`
	PurchaseOrderID      *uuid.UUID `json:"purchase_order_id"`
	NetAmount            float64    `json:"net_amount"`
	VatAmount            float64    `json:"vat_amount"`
	VatTreatment         string     `json:"vat_treatment"`
	IncurredAt           *time.Time `json:"incurred_at"`
	DueDate              *time.Time `json:"due_date"`
	OpportunityID        *uuid.UUID `json:"opportunity_id"`
	Notes                string     `json:"notes"`
}

// validateExpense applies the rules that must hold whether the row is being
// created or edited. It returns a human-readable reason, or "" when valid.
func validateExpense(req expenseRequest) string {
	if strings.TrimSpace(req.Supplier) == "" {
		return "supplier is required"
	}
	if req.NetAmount <= 0 {
		return "net amount must be greater than zero"
	}
	if req.VatAmount < 0 {
		return "VAT amount cannot be negative"
	}
	if !validExpenseCategories[req.Category] {
		return "invalid expense category"
	}
	if !validVatTreatments[req.VatTreatment] {
		return "invalid VAT treatment"
	}
	// A supply that is exempt, zero-rated or from an unregistered supplier
	// cannot carry VAT. Accepting a figure here would put irrecoverable tax into
	// the VAT account and quietly overstate what is reclaimable.
	if req.VatTreatment != models.VatStandard && req.VatAmount != 0 {
		return "only standard-rated expenses can carry VAT"
	}
	return ""
}

// AdminCreateExpense records a supplier invoice. It records only — no funds are
// moved. Settling it is a separate, separately gated action.
func (h Handler) AdminCreateExpense(c echo.Context) error {
	var req expenseRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if reason := validateExpense(req); reason != "" {
		return c.JSON(http.StatusBadRequest, errResponse(reason))
	}

	gross := req.NetAmount + req.VatAmount
	// The expense has no id until it exists, so the approval is bound to a nil
	// entity and judged on the summary and amount alone. This mirrors how a
	// payment is gated against the deal it lands on: there is nothing else
	// stable to point an approver at before the row is written.
	blocked, gerr := h.gate(c, models.ApprovalExpenseRecord, models.ApprovalEntityExpense, uuid.Nil, gross,
		fmt.Sprintf("Record a %s expense of %s from %q", req.Category, zmw(gross), strings.TrimSpace(req.Supplier)))
	if gerr != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not check approval requirements"))
	}
	if blocked != nil {
		return h.blockedResponse(c, blocked)
	}

	incurred := time.Now().UTC()
	if req.IncurredAt != nil {
		incurred = *req.IncurredAt
	}
	row := models.Expense{
		Supplier:             strings.TrimSpace(req.Supplier),
		SupplierTPIN:         strings.TrimSpace(req.SupplierTPIN),
		Category:             req.Category,
		Reference:            strings.TrimSpace(req.Reference),
		SmartInvoiceRef:      strings.TrimSpace(req.SmartInvoiceRef),
		CustomsAssessmentRef: strings.TrimSpace(req.CustomsAssessmentRef),
		PurchaseOrderID:      req.PurchaseOrderID,
		NetAmount:            req.NetAmount,
		VatAmount:            req.VatAmount,
		VatTreatment:         req.VatTreatment,
		IncurredAt:           incurred,
		DueDate:              req.DueDate,
		OpportunityID:        req.OpportunityID,
		Notes:                strings.TrimSpace(req.Notes),
	}
	var actor models.User
	if err := h.DB.First(&actor, "id = ?", c.Get("user_id")).Error; err == nil {
		row.RecordedByID = &actor.ID
		row.RecordedBy = actor.FullName
	}
	if err := h.DB.Create(&row).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not record the expense"))
	}
	return c.JSON(http.StatusCreated, expenseJSON(row))
}

// AdminUpdateExpense edits an unsettled supplier invoice.
func (h Handler) AdminUpdateExpense(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid expense id"))
	}
	var row models.Expense
	if err := h.DB.Preload("Settlements").First(&row, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("expense not found"))
	}
	var req expenseRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if reason := validateExpense(req); reason != "" {
		return c.JSON(http.StatusBadRequest, errResponse(reason))
	}
	// Editing the amount of an invoice that has already been part-paid would
	// silently rewrite history: the settlements stay, the total moves, and the
	// outstanding figure changes without anything recording why. Raise a credit
	// note as a separate expense instead.
	if row.Settled() > 0 && req.NetAmount+req.VatAmount != row.Gross() {
		return c.JSON(http.StatusConflict, errResponse("this expense has been part-paid, so its total cannot be changed"))
	}

	updates := map[string]any{
		"supplier":          strings.TrimSpace(req.Supplier),
		"supplier_tpin":     strings.TrimSpace(req.SupplierTPIN),
		"category":          req.Category,
		"reference":         strings.TrimSpace(req.Reference),
		"smart_invoice_ref":      strings.TrimSpace(req.SmartInvoiceRef),
		"customs_assessment_ref": strings.TrimSpace(req.CustomsAssessmentRef),
		"purchase_order_id":      req.PurchaseOrderID,
		"net_amount":             req.NetAmount,
		"vat_amount":             req.VatAmount,
		"vat_treatment":          req.VatTreatment,
		"due_date":               req.DueDate,
		"opportunity_id":         req.OpportunityID,
		"notes":                  strings.TrimSpace(req.Notes),
	}
	if req.IncurredAt != nil {
		updates["incurred_at"] = *req.IncurredAt
	}
	if err := h.DB.Model(&row).Updates(updates).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update the expense"))
	}
	h.DB.Preload("Settlements").First(&row, "id = ?", id)
	return c.JSON(http.StatusOK, expenseJSON(row))
}

// AdminDeleteExpense removes a supplier invoice that was never paid.
func (h Handler) AdminDeleteExpense(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid expense id"))
	}
	var row models.Expense
	if err := h.DB.Preload("Settlements").First(&row, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("expense not found"))
	}
	// A settled expense is evidence that money left the business. Deleting it
	// would erase the only record of the payment alongside the liability.
	if row.Settled() > 0 {
		return c.JSON(http.StatusConflict, errResponse("this expense has payments against it and cannot be deleted"))
	}
	if err := h.DB.Delete(&row).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not delete the expense"))
	}
	return c.NoContent(http.StatusNoContent)
}

// AdminSettleExpense records a payment made against a supplier invoice. Like
// every other money route here it records only; no funds are transferred.
func (h Handler) AdminSettleExpense(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid expense id"))
	}
	var row models.Expense
	if err := h.DB.Preload("Settlements").First(&row, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("expense not found"))
	}
	var req struct {
		Amount    float64    `json:"amount"`
		Method    string     `json:"method"`
		Reference string     `json:"reference"`
		PaidAt    *time.Time `json:"paid_at"`
		Note      string     `json:"note"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if req.Amount <= 0 {
		return c.JSON(http.StatusBadRequest, errResponse("settlement amount must be greater than zero"))
	}
	// Paying more than is owed is almost always a typo, and the aged report has
	// no way to represent a supplier who owes us money.
	if req.Amount > row.Outstanding()+0.005 {
		return c.JSON(http.StatusBadRequest, errResponse("settlement is larger than the amount outstanding"))
	}
	method := strings.TrimSpace(req.Method)
	if method == "" {
		method = "bank_transfer"
	}
	if !validPaymentMethods[method] {
		return c.JSON(http.StatusBadRequest, errResponse("invalid payment method"))
	}

	blocked, gerr := h.gate(c, models.ApprovalExpenseSettle, models.ApprovalEntityExpense, row.ID, req.Amount,
		fmt.Sprintf("Pay %s to %q by %s", zmw(req.Amount), row.Supplier, method))
	if gerr != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not check approval requirements"))
	}
	if blocked != nil {
		return h.blockedResponse(c, blocked)
	}

	paidAt := time.Now().UTC()
	if req.PaidAt != nil {
		paidAt = *req.PaidAt
	}
	settlement := models.ExpenseSettlement{
		ExpenseID: row.ID,
		Amount:    req.Amount,
		Method:    method,
		Reference: strings.TrimSpace(req.Reference),
		PaidAt:    paidAt,
		Note:      strings.TrimSpace(req.Note),
	}
	var actor models.User
	if err := h.DB.First(&actor, "id = ?", c.Get("user_id")).Error; err == nil {
		settlement.RecordedByID = &actor.ID
		settlement.RecordedBy = actor.FullName
	}
	if err := h.DB.Create(&settlement).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not record the settlement"))
	}
	h.DB.Preload("Settlements").First(&row, "id = ?", row.ID)
	return c.JSON(http.StatusCreated, expenseJSON(row))
}

// --- Aged payables ---------------------------------------------------------

// payableRow is one supplier invoice with money still owed on it.
type payableRow struct {
	ExpenseID    uuid.UUID  `json:"expense_id"`
	Supplier     string     `json:"supplier"`
	Category     string     `json:"category"`
	Reference    string     `json:"reference"`
	IncurredAt   time.Time  `json:"incurred_at"`
	DueDate      *time.Time `json:"due_date"`
	Gross        float64    `json:"gross"`
	Settled      float64    `json:"settled"`
	Outstanding  float64    `json:"outstanding"`
	Bucket       string     `json:"bucket"`
	DaysOverdue  int        `json:"days_overdue"`
	VatTreatment string     `json:"vat_treatment"`
}

// agingDate is the date a payable ages from: the due date where the supplier
// stated terms, and the invoice date otherwise, because an invoice with no
// terms is payable on presentation.
func agingDate(e models.Expense) time.Time {
	if e.DueDate != nil {
		return *e.DueDate
	}
	return e.IncurredAt
}

// buildPayables computes the outstanding position for every unsettled expense.
// Settlements are preloaded in one query rather than fetched per row — the
// per-row version is how a report like this becomes unusable once real data
// arrives.
func (h Handler) buildPayables(now time.Time) ([]payableRow, error) {
	var expenses []models.Expense
	if err := h.DB.Preload("Settlements").Find(&expenses).Error; err != nil {
		return nil, err
	}
	rows := []payableRow{}
	for _, e := range expenses {
		outstanding := e.Outstanding()
		// A settled invoice is not a payable. Listing it as a zero row buries
		// the ones that still need paying.
		if outstanding <= 0 {
			continue
		}
		from := agingDate(e)
		days := 0
		if elapsed := int(now.Sub(from).Hours() / 24); elapsed > 0 {
			days = elapsed
		}
		rows = append(rows, payableRow{
			ExpenseID: e.ID, Supplier: e.Supplier, Category: e.Category,
			Reference: e.Reference, IncurredAt: e.IncurredAt, DueDate: e.DueDate,
			Gross: e.Gross(), Settled: e.Settled(), Outstanding: outstanding,
			Bucket:      services.AgeBucket(&from, now),
			DaysOverdue: days, VatTreatment: e.VatTreatment,
		})
	}
	// Oldest debt first: that is the order anyone deciding who to pay works in.
	sort.Slice(rows, func(i, j int) bool { return rows[i].DaysOverdue > rows[j].DaysOverdue })
	return rows, nil
}

// AdminPayables returns the aged payables report, mirroring the receivables one.
func (h Handler) AdminPayables(c echo.Context) error {
	rows, err := h.buildPayables(time.Now().UTC())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not build the payables report"))
	}
	buckets := map[string]float64{"current": 0, "30": 0, "60": 0, "90+": 0}
	var total float64
	for _, r := range rows {
		buckets[r.Bucket] += r.Outstanding
		total += r.Outstanding
	}
	return c.JSON(http.StatusOK, map[string]any{
		"rows": rows, "buckets": buckets, "total_outstanding": total,
	})
}

// AdminPosition is the combined view: owed to us, owed by us, and the net.
//
// This is the figure the business could not previously produce at all. Tracking
// only money in makes it possible to say what clients owe, and impossible to say
// whether the business is actually ahead.
func (h Handler) AdminPosition(c echo.Context) error {
	now := time.Now().UTC()

	receivables, err := h.buildReceivables(now)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not build the position"))
	}
	payables, err := h.buildPayables(now)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not build the position"))
	}

	var owedToUs, owedByUs float64
	for _, r := range receivables {
		owedToUs += r.Outstanding
	}
	for _, p := range payables {
		owedByUs += p.Outstanding
	}

	// Spend by category, which is the closest thing to a P&L this data supports
	// and the shape the Zambian micro-entity reporting standard expects:
	// expenditure by nature rather than by ledger code.
	var all []models.Expense
	if err := h.DB.Find(&all).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not build the position"))
	}
	byCategory := map[string]float64{}
	var recoverableVat, sunkVat float64
	for _, e := range all {
		byCategory[e.Category] += e.NetAmount
		recoverable := e.ReclaimableVat()
		recoverableVat += recoverable
		sunkVat += e.VatAmount - recoverable
	}

	return c.JSON(http.StatusOK, map[string]any{
		"owed_to_us":        owedToUs,
		"owed_by_us":        owedByUs,
		"net":               owedToUs - owedByUs,
		"spend_by_category": byCategory,
		// Input VAT that ZRA will accept a claim for, versus VAT paid that it
		// will not because no Smart Invoice backs the purchase. The second
		// number is a pure cost and is invisible in any system that assumes all
		// input VAT is recoverable.
		"recoverable_input_vat":   recoverableVat,
		"irrecoverable_input_vat": sunkVat,
	})
}

// AdminExportPayablesCSV downloads the aged payables report.
func (h Handler) AdminExportPayablesCSV(c echo.Context) error {
	rows, err := h.buildPayables(time.Now().UTC())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not build the payables report"))
	}
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			r.Supplier, r.Category, r.Reference, r.IncurredAt.Format("2006-01-02"),
			dateOrBlank(r.DueDate), money(r.Gross), money(r.Settled),
			money(r.Outstanding), r.Bucket, strconv.Itoa(r.DaysOverdue),
		})
	}
	return writeCSV(c, fmt.Sprintf("payables-%s.csv", time.Now().UTC().Format("2006-01-02")),
		[]string{"Supplier", "Category", "Reference", "Invoice Date", "Due Date", "Gross", "Settled", "Outstanding", "Age Bucket", "Days"}, out)
}
