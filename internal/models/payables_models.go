package models

import (
	"time"

	"github.com/google/uuid"
)

// VatRate is Zambia's standard VAT rate. It lives here as well as in the
// frontend's document renderer because the server must be able to state what a
// figure means without trusting a client to have applied the same rate.
const VatRate = 0.16

// VAT treatment of a supplier invoice.
//
// The distinction that matters is not the arithmetic — it is whether the VAT on
// this expense is money the business gets back or money it has spent. Only
// VatStandard can ever be reclaimed, and even then only against a Smart Invoice
// (see Expense.SmartInvoiceRef).
const (
	VatStandard  = "standard"   // 16%, potentially reclaimable input VAT
	VatZeroRated = "zero_rated" // taxable at 0% — exports, prescribed supplies
	VatExempt    = "exempt"     // outside the VAT net entirely
	VatNone      = "none"       // supplier is not VAT-registered, so no VAT charged
)

var AllVatTreatments = []string{VatStandard, VatZeroRated, VatExempt, VatNone}

// Expense categories.
//
// This is deliberately a flat list rather than a numbered chart of accounts.
// Zambia's three-tier reporting framework puts an entity below K20 million of
// turnover on the Zambian Financial Reporting Standard for Micro and Small
// Entities, a simplified IFRS for SMEs, which reports expenditure by nature —
// not through the ledger codes a full chart of accounts implies. A category per
// line is what an accountant preparing those statements actually needs, and it
// is what a storekeeper can be expected to pick correctly.
const (
	ExpPurchases    = "purchases"    // goods for resale — the cost of sales line
	ExpSalaries     = "salaries"     // wages, PAYE, NAPSA
	ExpRent         = "rent"         // premises
	ExpUtilities    = "utilities"    // ZESCO, water, internet, airtime
	ExpTransport    = "transport"    // fuel, freight, vehicle running
	ExpRepairs      = "repairs"      // maintenance of premises, tools, vehicles
	ExpProfessional = "professional" // accountancy, legal, consultancy
	ExpBankCharges  = "bank_charges" // bank and mobile money fees
	ExpMarketing    = "marketing"    // advertising, signage
	ExpInsurance    = "insurance"    //
	ExpStatutory    = "statutory"    // licences, levies, council fees, ZRA penalties
	ExpEquipment    = "equipment"    // tools and plant bought outright
	ExpOther        = "other"        //
)

var AllExpenseCategories = []string{
	ExpPurchases, ExpSalaries, ExpRent, ExpUtilities, ExpTransport,
	ExpRepairs, ExpProfessional, ExpBankCharges, ExpMarketing,
	ExpInsurance, ExpStatutory, ExpEquipment, ExpOther,
}

// Expense is money owed to, or already paid to, a supplier — the counterpart to
// Payment on the receivable side.
//
// Amounts are stored exactly as they appear on the supplier's document, net and
// VAT separately, rather than deriving VAT from the net at 16%. Suppliers round
// differently and a stored figure that silently disagrees with the paper in the
// file is worse than useless during a ZRA audit. What is never stored is the
// balance outstanding: that is Gross() minus the settlements, computed on read,
// consistent with how every other derived figure in this system works.
type Expense struct {
	BaseModel

	Supplier string `json:"supplier" gorm:"index;not null"`
	// SupplierTPIN is the supplier's ten-digit ZRA taxpayer number. It is
	// required on a valid tax invoice, so its absence is itself a signal that
	// the document backing this expense will not support a VAT claim.
	SupplierTPIN string `json:"supplier_tpin"`

	Category string `json:"category" gorm:"index;not null"`
	// Reference is the supplier's own invoice number.
	Reference string `json:"reference" gorm:"index"`

	// SmartInvoiceRef is the Mark ID that ZRA's Smart Invoice system stamps on
	// every invoice it issues.
	//
	// This field is the whole reason input VAT is modelled rather than assumed.
	// Smart Invoice has been mandatory for VAT-registered businesses since
	// October 2024, and from 1 January 2026 ZRA accepts an input VAT claim only
	// where the purchase is backed by a Smart Invoice. A standard-rated expense
	// with this field empty therefore carries VAT the business has paid and
	// cannot recover — a real cost. Recording it as reclaimable would overstate
	// the VAT account and understate the expense.
	SmartInvoiceRef string `json:"smart_invoice_ref"`

	// CustomsAssessmentRef is the ZRA customs entry / assessment number backing
	// an imported purchase.
	//
	// It exists because the Smart Invoice rule above is a rule about DOMESTIC
	// supply. VAT on an import is paid at the border and evidenced by the
	// customs assessment; a foreign supplier has no ZRA Mark ID and cannot ever
	// issue a Smart Invoice. Before this field, an import with standard-rated
	// VAT fell through ReclaimableVat's Smart Invoice test and was reported as
	// irrecoverable — overstating cost and understating the recoverable VAT
	// account by the whole of the import VAT.
	//
	// It is a separate field rather than a customs number written into
	// SmartInvoiceRef because the two are different documents proving different
	// things, and a report that cannot tell them apart cannot be corrected later.
	CustomsAssessmentRef string `json:"customs_assessment_ref"`

	// PurchaseOrderID links the supplier invoice back to the order it bills.
	// Nullable: not every expense comes from a purchase order, and an invoice
	// may arrive before or after the goods it covers.
	PurchaseOrderID *uuid.UUID `json:"purchase_order_id" gorm:"type:uuid;index"`

	NetAmount    float64 `json:"net_amount"`
	VatAmount    float64 `json:"vat_amount"`
	VatTreatment string  `json:"vat_treatment" gorm:"index;not null;default:'none'"`

	// IncurredAt is the supplier's invoice date; DueDate is when payment falls
	// due. Ageing runs off DueDate where there is one and IncurredAt otherwise,
	// because an invoice with no stated terms is due on presentation.
	IncurredAt time.Time  `json:"incurred_at" gorm:"index"`
	DueDate    *time.Time `json:"due_date" gorm:"index"`

	// OpportunityID attributes the cost to a deal, which is what makes job
	// costing possible: margin on a won deal is its value less the expenses
	// booked against it.
	OpportunityID *uuid.UUID `json:"opportunity_id" gorm:"type:uuid;index"`

	Notes string `json:"notes" gorm:"type:text"`

	RecordedByID *uuid.UUID `json:"recorded_by_id" gorm:"type:uuid"`
	RecordedBy   string     `json:"recorded_by"`

	Settlements []ExpenseSettlement `json:"settlements" gorm:"foreignKey:ExpenseID;constraint:OnDelete:CASCADE"`
}

// Gross is the total payable to the supplier.
func (e Expense) Gross() float64 { return e.NetAmount + e.VatAmount }

// ReclaimableVat is the portion of VatAmount the business may claim back from
// ZRA.
//
// Two kinds of evidence support a claim, and they are not interchangeable:
// a Smart Invoice for a domestic supply (see SmartInvoiceRef), or a customs
// assessment for an import (see CustomsAssessmentRef). Either will do; neither
// present means the VAT was paid and cannot be recovered, so it is a cost.
//
// Requiring a Smart Invoice on an import would be wrong, not merely strict —
// no foreign supplier can issue one, so every import would report its border
// VAT as sunk.
func (e Expense) ReclaimableVat() float64 {
	if e.VatTreatment != VatStandard {
		return 0
	}
	if e.SmartInvoiceRef == "" && e.CustomsAssessmentRef == "" {
		return 0
	}
	return e.VatAmount
}

// Settled totals what has actually been paid against this expense.
func (e Expense) Settled() float64 {
	var total float64
	for _, s := range e.Settlements {
		total += s.Amount
	}
	return total
}

// Outstanding is what is still owed. It is never stored.
func (e Expense) Outstanding() float64 {
	out := e.Gross() - e.Settled()
	if out < 0 {
		return 0
	}
	return out
}

// ExpenseSettlement is one payment made against an expense — the mirror of
// Payment on the receivable side, including its method vocabulary, so "how was
// this settled" reads the same in both directions.
type ExpenseSettlement struct {
	BaseModel
	ExpenseID uuid.UUID `json:"expense_id" gorm:"type:uuid;index;not null"`
	Amount    float64   `json:"amount"`
	// cash, bank_transfer, mobile_money, cheque, card, other
	Method    string    `json:"method"`
	Reference string    `json:"reference"`
	PaidAt    time.Time `json:"paid_at"`
	Note      string    `json:"note" gorm:"type:text"`

	RecordedByID *uuid.UUID `json:"recorded_by_id" gorm:"type:uuid"`
	RecordedBy   string     `json:"recorded_by"`
}
