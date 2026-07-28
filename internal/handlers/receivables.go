package handlers

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"arcusinvest/internal/models"
	"arcusinvest/internal/services"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// receivableRow is one invoiced deal with money still outstanding.
type receivableRow struct {
	OpportunityID uuid.UUID  `json:"opportunity_id"`
	Name          string     `json:"name"`
	AccountName   string     `json:"account_name"`
	InvoicedAt    *time.Time `json:"invoiced_at"`
	Invoiced      float64    `json:"invoiced"`
	Paid          float64    `json:"paid"`
	Outstanding   float64    `json:"outstanding"`
	Bucket        string     `json:"bucket"`
	DaysOverdue   int        `json:"days_overdue"`
	ApplyVat      bool       `json:"apply_vat"`
}

// buildReceivables computes the outstanding position for every invoiced deal.
//
// Payments are fetched in one query and grouped in memory rather than queried
// per deal — the per-deal version issues one round trip per row, which is the
// usual way a report like this becomes unusably slow once real data arrives.
func (h Handler) buildReceivables(now time.Time) ([]receivableRow, error) {
	var deals []models.Opportunity
	if err := h.DB.Preload("LineItems").
		Where("invoiced_at IS NOT NULL AND stage <> ?", models.StageLost).
		Find(&deals).Error; err != nil {
		return nil, err
	}
	if len(deals) == 0 {
		return []receivableRow{}, nil
	}

	ids := make([]uuid.UUID, 0, len(deals))
	for _, d := range deals {
		ids = append(ids, d.ID)
	}
	var payments []models.Payment
	if err := h.DB.Where("opportunity_id IN ?", ids).Find(&payments).Error; err != nil {
		return nil, err
	}
	byDeal := map[uuid.UUID][]models.Payment{}
	for _, p := range payments {
		byDeal[p.OpportunityID] = append(byDeal[p.OpportunityID], p)
	}

	rows := []receivableRow{}
	for _, d := range deals {
		outstanding := services.Outstanding(d, byDeal[d.ID])
		// A settled deal is not a receivable; listing it as a zero row buries the
		// ones that still need chasing.
		if outstanding <= 0 {
			continue
		}
		days := 0
		if d.InvoicedAt != nil {
			if elapsed := int(now.Sub(*d.InvoicedAt).Hours() / 24); elapsed > 0 {
				days = elapsed
			}
		}
		rows = append(rows, receivableRow{
			OpportunityID: d.ID, Name: d.Name, AccountName: d.AccountName,
			InvoicedAt:  d.InvoicedAt,
			Invoiced:    services.InvoicedTotal(d),
			Paid:        services.PaidTotal(byDeal[d.ID]),
			Outstanding: outstanding,
			Bucket:      services.AgeBucket(d.InvoicedAt, now),
			DaysOverdue: days,
			ApplyVat:    d.ApplyVat,
		})
	}
	// Oldest debt first: that is the order anyone chasing payment works in.
	sort.Slice(rows, func(i, j int) bool { return rows[i].DaysOverdue > rows[j].DaysOverdue })
	return rows, nil
}

// AdminReceivables returns the aged receivables report: per-deal rows plus the
// current/30/60/90+ totals.
func (h Handler) AdminReceivables(c echo.Context) error {
	rows, err := h.buildReceivables(time.Now().UTC())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not build the receivables report"))
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

// AdminAccountPayments returns every payment recorded against an account,
// across all of its deals — the per-account history the per-deal view cannot
// show.
func (h Handler) AdminAccountPayments(c echo.Context) error {
	account := c.Param("name")
	if account == "" {
		return c.JSON(http.StatusBadRequest, errResponse("account name is required"))
	}

	var deals []models.Opportunity
	if err := h.DB.Where("account_name = ?", account).Find(&deals).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the account"))
	}
	if len(deals) == 0 {
		return c.JSON(http.StatusOK, map[string]any{"payments": []any{}, "total": 0.0})
	}
	names := map[uuid.UUID]string{}
	ids := make([]uuid.UUID, 0, len(deals))
	for _, d := range deals {
		ids = append(ids, d.ID)
		names[d.ID] = d.Name
	}

	var payments []models.Payment
	if err := h.DB.Where("opportunity_id IN ?", ids).Order("paid_at DESC").Find(&payments).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load payments"))
	}
	out := []map[string]any{}
	var total float64
	for _, p := range payments {
		total += p.Amount
		out = append(out, map[string]any{
			"id": p.ID, "opportunity_id": p.OpportunityID, "opportunity_name": names[p.OpportunityID],
			"amount": p.Amount, "method": p.Method, "reference": p.Reference,
			"paid_at": p.PaidAt, "note": p.Note, "recorded_by": p.RecordedBy,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"payments": out, "total": total})
}

// AdminMarkInvoiced records that a deal has been billed, which is what starts
// the receivable ageing. Sending invoiced_at: null un-bills it, for a mistake.
func (h Handler) AdminMarkInvoiced(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid opportunity id"))
	}
	var row models.Opportunity
	if err := h.DB.First(&row, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("opportunity not found"))
	}
	var req struct {
		InvoicedAt *time.Time `json:"invoiced_at"`
		Clear      bool       `json:"clear"`
		ApplyVat   *bool      `json:"apply_vat"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}

	updates := map[string]any{}
	if req.Clear {
		updates["invoiced_at"] = nil
	} else {
		when := time.Now().UTC()
		if req.InvoicedAt != nil {
			when = *req.InvoicedAt
		}
		updates["invoiced_at"] = when
	}
	if req.ApplyVat != nil {
		updates["apply_vat"] = *req.ApplyVat
	}
	if err := h.DB.Model(&row).Updates(updates).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update the opportunity"))
	}
	h.DB.Preload("Contacts").Preload("LineItems").First(&row, "id = ?", id)
	return c.JSON(http.StatusOK, opportunityJSON(row))
}

// --- Exports ---------------------------------------------------------------

// writeCSV streams rows as a downloadable CSV attachment.
func writeCSV(c echo.Context, filename string, header []string, rows [][]string) error {
	c.Response().Header().Set(echo.HeaderContentDisposition, fmt.Sprintf("attachment; filename=%q", filename))
	c.Response().Header().Set(echo.HeaderContentType, "text/csv; charset=utf-8")
	c.Response().WriteHeader(http.StatusOK)

	w := csv.NewWriter(c.Response())
	if err := w.Write(header); err != nil {
		return err
	}
	for _, r := range rows {
		if err := w.Write(r); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func money(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func dateOrBlank(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02")
}

// AdminExportReceivablesCSV downloads the aged receivables report.
func (h Handler) AdminExportReceivablesCSV(c echo.Context) error {
	rows, err := h.buildReceivables(time.Now().UTC())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not build the receivables report"))
	}
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			r.AccountName, r.Name, dateOrBlank(r.InvoicedAt), money(r.Invoiced),
			money(r.Paid), money(r.Outstanding), r.Bucket, strconv.Itoa(r.DaysOverdue),
		})
	}
	return writeCSV(c, fmt.Sprintf("receivables-%s.csv", time.Now().UTC().Format("2006-01-02")),
		[]string{"Account", "Deal", "Invoiced On", "Invoiced", "Paid", "Outstanding", "Age Bucket", "Days"}, out)
}

// AdminExportPipelineCSV downloads the deal pipeline.
func (h Handler) AdminExportPipelineCSV(c echo.Context) error {
	var deals []models.Opportunity
	if err := h.DB.Preload("LineItems").Order("created_at desc").Find(&deals).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the pipeline"))
	}
	out := make([][]string, 0, len(deals))
	for _, d := range deals {
		out = append(out, []string{
			d.AccountName, d.Name, string(d.Stage), string(d.Grade), d.Segment,
			money(d.DealValue), strconv.Itoa(d.Probability),
			money(d.DealValue * float64(d.Probability) / 100.0),
			dateOrBlank(d.ExpectedCloseAt), dateOrBlank(d.InvoicedAt),
		})
	}
	return writeCSV(c, fmt.Sprintf("pipeline-%s.csv", time.Now().UTC().Format("2006-01-02")),
		[]string{"Account", "Deal", "Stage", "Grade", "Segment", "Value", "Probability %", "Weighted", "Expected Close", "Invoiced On"}, out)
}

// AdminExportPaymentsCSV downloads every recorded payment.
func (h Handler) AdminExportPaymentsCSV(c echo.Context) error {
	var payments []models.Payment
	if err := h.DB.Order("paid_at desc").Find(&payments).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load payments"))
	}
	// One lookup for the deal names rather than one per payment.
	var deals []models.Opportunity
	h.DB.Find(&deals)
	dealName := map[uuid.UUID]string{}
	account := map[uuid.UUID]string{}
	for _, d := range deals {
		dealName[d.ID] = d.Name
		account[d.ID] = d.AccountName
	}

	out := make([][]string, 0, len(payments))
	for _, p := range payments {
		out = append(out, []string{
			p.PaidAt.Format("2006-01-02"), account[p.OpportunityID], dealName[p.OpportunityID],
			money(p.Amount), p.Method, p.Reference, p.RecordedBy,
		})
	}
	return writeCSV(c, fmt.Sprintf("payments-%s.csv", time.Now().UTC().Format("2006-01-02")),
		[]string{"Paid On", "Account", "Deal", "Amount", "Method", "Reference", "Recorded By"}, out)
}
