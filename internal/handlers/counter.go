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

var validCounterMethods = func() map[string]bool {
	m := map[string]bool{}
	for _, x := range models.AllCounterMethods {
		m[x] = true
	}
	return m
}()

func counterSaleJSON(s models.CounterSale) map[string]any {
	return map[string]any{
		"id": s.ID, "created_at": s.CreatedAt,
		"till_session_id": s.TillSessionID,
		"customer_name":   s.CustomerName, "customer_tpin": s.CustomerTPIN,
		"apply_vat": s.ApplyVat, "payment_method": s.PaymentMethod,
		"amount_tendered": s.AmountTendered, "reference": s.Reference,
		"smart_invoice_ref": s.SmartInvoiceRef,
		"note":              s.Note, "sold_by": s.SoldBy,
		"lines":    s.Lines,
		"subtotal": s.Subtotal(), "vat": s.Vat(), "total": s.Total(),
		"change": s.Change(),
	}
}

// --- Till sessions ---------------------------------------------------------

// openSessionFor returns the caller's open till session, if they have one.
func (h Handler) openSessionFor(userID string) (*models.TillSession, error) {
	var s models.TillSession
	err := h.DB.Where("status = ? AND opened_by_id = ?", models.TillOpen, userID).
		Order("opened_at DESC").First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// AdminOpenTill starts a shift with a counted opening float.
func (h Handler) AdminOpenTill(c echo.Context) error {
	userID, _ := c.Get("user_id").(string)
	if userID == "" {
		return c.JSON(http.StatusUnauthorized, errResponse("no authenticated caller"))
	}
	// One open session per operator. Two would split a shift's takings across
	// two counts, and neither would reconcile.
	existing, err := h.openSessionFor(userID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not check for an open till"))
	}
	if existing != nil {
		return c.JSON(http.StatusConflict, errResponse("you already have a till open — close it before opening another"))
	}

	var req struct {
		OpeningFloat float64 `json:"opening_float"`
		Note         string  `json:"note"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if req.OpeningFloat < 0 {
		return c.JSON(http.StatusBadRequest, errResponse("the opening float cannot be negative"))
	}

	session := models.TillSession{
		Status: models.TillOpen, OpenedAt: time.Now().UTC(),
		OpeningFloat: req.OpeningFloat, Note: strings.TrimSpace(req.Note),
	}
	var actor models.User
	if err := h.DB.First(&actor, "id = ?", userID).Error; err == nil {
		session.OpenedByID = &actor.ID
		session.OpenedBy = actor.FullName
	}
	if err := h.DB.Create(&session).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not open the till"))
	}
	return c.JSON(http.StatusCreated, session)
}

// tillTotals sums a session's takings by method. Cash is separated because it
// is the only one that has to be reconciled against a physical count.
func (h Handler) tillTotals(sessionID uuid.UUID) (map[string]float64, error) {
	var sales []models.CounterSale
	if err := h.DB.Preload("Lines").Where("till_session_id = ?", sessionID).Find(&sales).Error; err != nil {
		return nil, err
	}
	totals := map[string]float64{}
	for _, x := range models.AllCounterMethods {
		totals[x] = 0
	}
	for _, s := range sales {
		totals[s.PaymentMethod] += s.Total()
	}
	return totals, nil
}

// AdminTillSummary reports where a session stands: takings by method, the cash
// the drawer should hold, and — once closed — the variance against the count.
func (h Handler) AdminTillSummary(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid till session id"))
	}
	var session models.TillSession
	if err := h.DB.First(&session, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("till session not found"))
	}
	totals, err := h.tillTotals(session.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not total the till"))
	}
	return c.JSON(http.StatusOK, tillSummaryJSON(session, totals))
}

// tillSummaryJSON renders a session with its expected drawer and variance. Both
// are computed here rather than stored: a persisted variance drifts the moment
// a sale is corrected, and a stale variance is worse than none because someone
// will go looking for money that was never missing.
func tillSummaryJSON(s models.TillSession, totals map[string]float64) map[string]any {
	expectedCash := s.OpeningFloat + totals[models.CounterCash]
	out := map[string]any{
		"session": s, "takings": totals,
		"expected_cash": expectedCash,
		"total_takings": totals[models.CounterCash] + totals[models.CounterMobileMoney] +
			totals[models.CounterCard] + totals[models.CounterBankTransfer],
	}
	if s.CountedCash != nil {
		// Positive means the drawer holds more than it should, which is as much
		// a control failure as a shortfall — it usually means a sale went
		// unrecorded.
		out["variance"] = *s.CountedCash - expectedCash
	}
	return out
}

// AdminCloseTill ends a shift against a physical count of the drawer.
func (h Handler) AdminCloseTill(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid till session id"))
	}
	var session models.TillSession
	if err := h.DB.First(&session, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("till session not found"))
	}
	if session.Status == models.TillClosed {
		return c.JSON(http.StatusConflict, errResponse("this till has already been closed"))
	}
	var req struct {
		// A pointer so an omitted count is refused rather than read as zero —
		// an empty drawer and an uncounted one are very different claims.
		CountedCash *float64 `json:"counted_cash"`
		Note        string   `json:"note"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if req.CountedCash == nil {
		return c.JSON(http.StatusBadRequest, errResponse("count the drawer before closing the till"))
	}
	if *req.CountedCash < 0 {
		return c.JSON(http.StatusBadRequest, errResponse("the counted cash cannot be negative"))
	}

	updates := map[string]any{
		"status": models.TillClosed, "closed_at": time.Now().UTC(),
		"counted_cash": *req.CountedCash,
	}
	if note := strings.TrimSpace(req.Note); note != "" {
		updates["note"] = note
	}
	var actor models.User
	if err := h.DB.First(&actor, "id = ?", c.Get("user_id")).Error; err == nil {
		updates["closed_by_id"] = actor.ID
		updates["closed_by"] = actor.FullName
	}
	if err := h.DB.Model(&session).Updates(updates).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not close the till"))
	}
	h.DB.First(&session, "id = ?", id)
	totals, err := h.tillTotals(session.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not total the till"))
	}
	return c.JSON(http.StatusOK, tillSummaryJSON(session, totals))
}

// AdminListTillSessions returns recent shifts, newest first.
func (h Handler) AdminListTillSessions(c echo.Context) error {
	var rows []models.TillSession
	if err := h.DB.Order("opened_at DESC").Limit(50).Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load till sessions"))
	}
	return c.JSON(http.StatusOK, map[string]any{"items": rows})
}

// --- Counter sales ---------------------------------------------------------

type counterLineRequest struct {
	ProductID   *uuid.UUID `json:"product_id"`
	Description string     `json:"description"`
	Quantity    int        `json:"quantity"`
	UnitPrice   float64    `json:"unit_price"`
}

// AdminCreateCounterSale records a walk-in sale and takes the goods out of
// stock in the same transaction.
//
// There is deliberately no approval gate here. Counter sales are the highest
// volume action in the system and happen with a customer waiting; a
// maker-checker step would either be bypassed or would stop the shop. The
// controls that fit this shape are the till count at the end of the shift and
// the stock ledger, both of which are after the fact and neither of which
// blocks the queue.
func (h Handler) AdminCreateCounterSale(c echo.Context) error {
	userID, _ := c.Get("user_id").(string)
	if userID == "" {
		return c.JSON(http.StatusUnauthorized, errResponse("no authenticated caller"))
	}
	var req struct {
		TillSessionID   *uuid.UUID           `json:"till_session_id"`
		CustomerName    string               `json:"customer_name"`
		CustomerTPIN    string               `json:"customer_tpin"`
		ApplyVat        bool                 `json:"apply_vat"`
		PaymentMethod   string               `json:"payment_method"`
		AmountTendered  float64              `json:"amount_tendered"`
		Reference       string               `json:"reference"`
		SmartInvoiceRef string               `json:"smart_invoice_ref"`
		Note            string               `json:"note"`
		Lines           []counterLineRequest `json:"lines"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if len(req.Lines) == 0 {
		return c.JSON(http.StatusBadRequest, errResponse("a sale needs at least one item"))
	}
	method := strings.TrimSpace(req.PaymentMethod)
	if method == "" {
		method = models.CounterCash
	}
	if !validCounterMethods[method] {
		// See the note in counter_models.go: "credit" is not a counter method.
		// A customer who wants terms is a deal, not a till transaction.
		return c.JSON(http.StatusBadRequest, errResponse("invalid payment method for a counter sale"))
	}

	// Every sale belongs to a shift, or the drawer cannot be reconciled.
	session, err := h.resolveSession(userID, req.TillSessionID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not resolve the till session"))
	}
	if session == nil {
		return c.JSON(http.StatusConflict, errResponse("open a till before selling"))
	}

	sale := models.CounterSale{
		TillSessionID:   session.ID,
		CustomerName:    strings.TrimSpace(req.CustomerName),
		CustomerTPIN:    strings.TrimSpace(req.CustomerTPIN),
		ApplyVat:        req.ApplyVat,
		PaymentMethod:   method,
		AmountTendered:  req.AmountTendered,
		Reference:       strings.TrimSpace(req.Reference),
		SmartInvoiceRef: strings.TrimSpace(req.SmartInvoiceRef),
		Note:            strings.TrimSpace(req.Note),
	}
	for _, l := range req.Lines {
		if l.Quantity <= 0 {
			return c.JSON(http.StatusBadRequest, errResponse("every line needs a quantity of at least one"))
		}
		if l.UnitPrice < 0 {
			return c.JSON(http.StatusBadRequest, errResponse("a unit price cannot be negative"))
		}
		description := strings.TrimSpace(l.Description)
		if description == "" && l.ProductID == nil {
			return c.JSON(http.StatusBadRequest, errResponse("a line with no product needs a description"))
		}
		sale.Lines = append(sale.Lines, models.CounterSaleLine{
			ProductID: l.ProductID, Description: description,
			Quantity: l.Quantity, UnitPrice: l.UnitPrice,
		})
	}
	if method == models.CounterCash && req.AmountTendered+0.005 < sale.Total() {
		return c.JSON(http.StatusBadRequest, errResponse("the amount tendered is less than the total"))
	}

	var actor models.User
	if err := h.DB.First(&actor, "id = ?", userID).Error; err == nil {
		sale.SoldByID = &actor.ID
		sale.SoldBy = actor.FullName
	}

	// The sale and the stock it consumes are one transaction. A sale that
	// records but does not move stock leaves the shelf figure permanently
	// wrong, and a stock movement with no sale behind it is unexplained
	// shrinkage — either half alone is worse than failing outright.
	err = h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&sale).Error; err != nil {
			return err
		}
		for _, line := range sale.Lines {
			if line.ProductID == nil {
				continue // a one-off item that was never in the catalogue
			}
			movement := models.StockMovement{
				ProductID: *line.ProductID, Kind: models.StockSale,
				Quantity:      -line.Quantity,
				Reason:        fmt.Sprintf("Counter sale to %s", customerLabel(sale)),
				CounterSaleID: &sale.ID,
				OccurredAt:    time.Now().UTC(),
				ActorID:       sale.SoldByID, ActorName: sale.SoldBy,
			}
			if err := services.RecordMovement(tx, &movement); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, services.ErrInsufficientStock) {
			return c.JSON(http.StatusConflict, errResponse(err.Error()))
		}
		return c.JSON(http.StatusInternalServerError, errResponse("could not record the sale"))
	}
	return c.JSON(http.StatusCreated, counterSaleJSON(sale))
}

// customerLabel names the buyer for the stock ledger. Most walk-ins are
// anonymous, and "Counter sale to " with nothing after it reads as a bug.
func customerLabel(s models.CounterSale) string {
	if s.CustomerName != "" {
		return s.CustomerName
	}
	return "a walk-in customer"
}

// resolveSession picks the session a sale belongs to: the one named, or the
// caller's own open one.
func (h Handler) resolveSession(userID string, named *uuid.UUID) (*models.TillSession, error) {
	if named == nil {
		return h.openSessionFor(userID)
	}
	var s models.TillSession
	if err := h.DB.First(&s, "id = ? AND status = ?", *named, models.TillOpen).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// AdminListCounterSales returns recent counter sales, optionally for one shift.
func (h Handler) AdminListCounterSales(c echo.Context) error {
	q := h.DB.Preload("Lines").Order("created_at DESC").Limit(200)
	if raw := strings.TrimSpace(c.QueryParam("till_session_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errResponse("invalid till session id"))
		}
		q = q.Where("till_session_id = ?", id)
	}
	var rows []models.CounterSale
	if err := q.Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load counter sales"))
	}
	out := make([]map[string]any, 0, len(rows))
	for _, s := range rows {
		out = append(out, counterSaleJSON(s))
	}
	return c.JSON(http.StatusOK, map[string]any{"items": out})
}
