package models

import (
	"time"

	"github.com/google/uuid"
)

// Approval action codes. Each names one specific, high-consequence mutation a
// rule can gate. They are stable strings rather than an enum type because
// ApprovalRequest rows outlive any refactor of the handler that raised them —
// a code stored last year must still read correctly today.
const (
	ApprovalDealCloseWon   = "deal.close_won"
	ApprovalDealDelete     = "deal.delete"
	ApprovalContractSign   = "contract.sign"
	ApprovalContractDelete = "contract.delete"
	ApprovalPaymentRecord  = "payment.record"

	// Money out. ApprovalExpenseRecord gates committing the business to a
	// liability; ApprovalExpenseSettle gates paying one.
	//
	// These are two separate decisions because they are made at different times
	// by different people: a storekeeper books the supplier invoice, and someone
	// with authority over the bank account decides when it gets paid. Gating
	// only the first would let an approved K5,000 order be settled for K50,000.
	ApprovalExpenseRecord = "expense.record"
	ApprovalExpenseSettle = "expense.settle"
)

// AllApprovalActions is the enumeration the rules API validates against, so an
// unknown action code cannot be configured into a rule that would then never
// match anything.
var AllApprovalActions = []string{
	ApprovalDealCloseWon,
	ApprovalDealDelete,
	ApprovalContractSign,
	ApprovalContractDelete,
	ApprovalPaymentRecord,
	ApprovalExpenseRecord,
	ApprovalExpenseSettle,
}

// ApprovalRequest lifecycle.
//
// `consumed` is reachable only by the gate letting a retried action through —
// no endpoint sets it. That is what makes "was this action actually authorised?"
// answerable from the table alone.
const (
	ApprovalStatusPending   = "pending"
	ApprovalStatusApproved  = "approved"
	ApprovalStatusRejected  = "rejected"
	ApprovalStatusConsumed  = "consumed"
	ApprovalStatusCancelled = "cancelled"
)

// Decision values on an individual vote.
const (
	DecisionApproved = "approved"
	DecisionRejected = "rejected"
)

// Entity types an approval request can point at.
const (
	ApprovalEntityOpportunity = "opportunities"
	ApprovalEntityContract    = "contracts"
	ApprovalEntityPayment     = "payments"
	ApprovalEntityExpense     = "expenses"
)

// ApprovalRule is one configured threshold: at or above MinAmount, Action
// requires RequiredCount approvals from ApproverRole.
//
// Rules are matched most-specific-first — the highest MinAmount the amount
// clears wins — so one action can carry tiers (a manager above K50,000, a
// director above K500,000) without the matching logic needing to know about
// tiers at all.
type ApprovalRule struct {
	BaseModel
	Action string `json:"action" gorm:"index;not null"`
	// MinAmount is the inclusive floor this rule applies from. A rule at 0 gates
	// every occurrence of its action, which is how the actions that carry no
	// meaningful amount (the deletes) are configured.
	MinAmount     float64 `json:"min_amount"`
	RequiredCount int     `json:"required_count" gorm:"not null;default:1"`
	ApproverRole  Role    `json:"approver_role" gorm:"type:varchar(40);not null"`
	IsActive      bool    `json:"is_active" gorm:"index;default:true"`
	Note          string  `json:"note" gorm:"type:text"`
}

// ApprovalRequest is one blocked action awaiting a decision.
//
// It records INTENT, not a serialised mutation to replay on approval. Approving
// never performs the action; it unblocks the original requester, who retries and
// passes the gate. Signing forces this: the signature image, signer identity, IP
// and user-agent in ContractSignature must be captured from the real signer at
// the real moment, so an approver's click must never produce a signature.
type ApprovalRequest struct {
	BaseModel
	Action     string    `json:"action" gorm:"index;not null"`
	EntityType string    `json:"entity_type" gorm:"index;not null"`
	EntityID   uuid.UUID `json:"entity_id" gorm:"type:uuid;index;not null"`
	// Amount this request was raised for. The gate refuses to consume an approval
	// against a larger amount than this, so an approval for a K50,000 payment can
	// never admit a K500,000 one — that needs its own request.
	Amount float64 `json:"amount"`
	// Summary is built by the requesting handler and denormalised here, because an
	// approver deciding this is looking at the approvals screen, not at the deal.
	// "Approve request #7" is not a decision anyone can make responsibly.
	Summary string `json:"summary" gorm:"not null"`
	Status  string `json:"status" gorm:"index;not null;default:'pending'"`

	RequesterID   *uuid.UUID `json:"requester_id" gorm:"type:uuid;index"`
	RequesterName string     `json:"requester_name"` // denormalised, survives user deletion

	// RuleID is the rule that matched. RequiredCount and ApproverRole are
	// snapshotted from it at request time so that editing a rule cannot
	// retroactively change what an in-flight request needs — in either direction.
	// Raising a threshold must not auto-approve what is already pending, and
	// lowering one must not silently invalidate approvals already given.
	RuleID        *uuid.UUID `json:"rule_id" gorm:"type:uuid"`
	RequiredCount int        `json:"required_count" gorm:"not null;default:1"`
	ApproverRole  Role       `json:"approver_role" gorm:"type:varchar(40)"`

	// PendingKey enforces at most one open request per (action, entity,
	// requester) as a database guarantee rather than a prior SELECT two callers
	// could both pass. It holds that composite while the request is open and is
	// set to NULL the moment it is decided, cancelled or consumed — Postgres
	// treats NULLs in a unique index as distinct, so a decided request stops
	// occupying the slot and the requester can raise a fresh one.
	PendingKey *string `json:"-" gorm:"uniqueIndex"`

	// SupersedesID links a resubmission back to the request it replaces, so a
	// revise-and-resubmit loop reads as a chain in the history rather than
	// vanishing into a status flip.
	SupersedesID *uuid.UUID `json:"supersedes_id" gorm:"type:uuid;index"`

	DecidedAt  *time.Time `json:"decided_at"`
	ConsumedAt *time.Time `json:"consumed_at"`

	Decisions []ApprovalDecision `json:"decisions" gorm:"foreignKey:RequestID;constraint:OnDelete:CASCADE"`
}

// ApprovalDecision is one approver's vote on a request, and the carrier of the
// rejection reason that sends a request back to its originator.
//
// The composite unique index on (request_id, approver_id) is what makes
// RequiredCount mean "N distinct people": a second vote from the same approver
// is refused by the database, not by a prior SELECT that two concurrent requests
// could both pass. Decisions are never deleted, so the soft-delete-occupies-the-
// index problem documented on CustomRolePermission does not arise here.
//
// ApproverID is deliberately a non-pointer, unlike the nullable actor IDs
// elsewhere in this package: a vote with no identifiable voter is not evidence,
// and a NULL would also escape the unique index above. ApproverName stays
// denormalised so the record still reads correctly after the user is deleted.
type ApprovalDecision struct {
	BaseModel
	RequestID    uuid.UUID `json:"request_id" gorm:"type:uuid;uniqueIndex:idx_decision_request_approver;not null"`
	ApproverID   uuid.UUID `json:"approver_id" gorm:"type:uuid;uniqueIndex:idx_decision_request_approver;not null"`
	ApproverName string    `json:"approver_name" gorm:"not null"`
	ApproverRole Role      `json:"approver_role" gorm:"type:varchar(40)"`
	Decision     string    `json:"decision" gorm:"not null"` // approved, rejected
	Reason       string    `json:"reason" gorm:"type:text"`
}
