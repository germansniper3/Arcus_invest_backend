package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"arcusinvest/internal/models"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// The approval gate
//
// One shared implementation, called by every gated handler. Four inline copies
// would drift, and a drifted gate is an unguarded action.
// ---------------------------------------------------------------------------

// matchRule finds the rule governing an action at a given amount: among active
// rules for that action whose floor the amount clears, the highest floor wins.
// That is what lets one action carry tiers — a manager above ZMW 50,000, a
// director above ZMW 500,000 — without this function knowing tiers exist.
//
// No match means the action is not gated. An unconfigured rule set therefore
// blocks nothing, which is the right default: deploying this feature must not
// silently freeze a working system.
func (h Handler) matchRule(action string, amount float64) (*models.ApprovalRule, error) {
	var rule models.ApprovalRule
	err := h.DB.Where("action = ? AND is_active = ? AND min_amount <= ?", action, true, amount).
		Order("min_amount DESC").First(&rule).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rule, nil
}

// pendingKeyFor builds the composite the unique index enforces: at most one open
// request per (action, entity, requester).
func pendingKeyFor(action string, entityID uuid.UUID, requesterID string) string {
	return fmt.Sprintf("%s:%s:%s", action, entityID, requesterID)
}

// gate decides whether a high-consequence action may proceed.
//
// A nil request means go ahead — either no rule covers this action at this
// amount, or an approval was found and consumed. A non-nil request means the
// caller MUST abort without mutating anything and render h.blockedResponse.
//
// Call this BEFORE writing. A gate checked afterwards is an audit log, not a
// control.
//
// Approvals are bound to the requester, not just to the entity. The approver is
// agreeing that *this person* may take *this action* on *this record*; letting
// a third party spend someone else's approval would break the maker-checker
// pairing that the whole feature exists to create.
func (h Handler) gate(c echo.Context, action, entityType string, entityID uuid.UUID, amount float64, summary string) (*models.ApprovalRequest, error) {
	rule, err := h.matchRule(action, amount)
	if err != nil {
		return nil, err
	}
	if rule == nil {
		return nil, nil
	}

	requesterID, _ := c.Get("user_id").(string)
	if requesterID == "" {
		// Without an identifiable caller there is no maker to pair an approval
		// with. Fail closed rather than letting a gated action through ungated.
		return nil, errors.New("approval gate: no authenticated caller")
	}

	// The most recent request that is still live for this exact (action, entity,
	// requester). Consumed and cancelled requests are spent and must not be
	// found — that is what makes an approval single-use.
	var existing models.ApprovalRequest
	err = h.DB.Where("action = ? AND entity_id = ? AND requester_id = ? AND status IN ?",
		action, entityID, requesterID,
		[]string{
			models.ApprovalStatusPending,
			models.ApprovalStatusApproved,
			models.ApprovalStatusRejected,
		}).
		Order("created_at DESC").First(&existing).Error
	switch {
	case err == nil:
		if existing.Status != models.ApprovalStatusApproved {
			// Pending: still waiting. Rejected: the answer was no, and the
			// requester must resubmit explicitly rather than retry their way
			// past a decision.
			return &existing, nil
		}
		spent, cerr := h.consume(existing, amount)
		if cerr != nil {
			return nil, cerr
		}
		if spent {
			return nil, nil
		}
		// Approved, but not for an action this large. Fall through and raise a
		// fresh request at the real amount.
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return nil, err
	}

	return h.raise(rule, action, entityType, entityID, amount, summary, requesterID)
}

// consume marks an approval spent, and is the only path to the `consumed`
// status — no endpoint sets it. That is what makes "was this action actually
// authorised?" answerable from the table alone.
//
// It is a conditional UPDATE rather than a read-then-write: two concurrent
// retries must not both find the same approval usable and both proceed.
// RowsAffected is the arbiter, so exactly one caller wins.
//
// The amount check lives inside the same statement for the same reason. An
// approval granted for ZMW 50,000 must never admit a ZMW 500,000 action —
// otherwise a requester could have a small transaction waved through and then
// execute a large one against it.
func (h Handler) consume(req models.ApprovalRequest, amount float64) (bool, error) {
	res := h.DB.Model(&models.ApprovalRequest{}).
		Where("id = ? AND status = ? AND amount >= ?", req.ID, models.ApprovalStatusApproved, amount).
		Updates(map[string]any{
			"status":      models.ApprovalStatusConsumed,
			"consumed_at": time.Now(),
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// raise records a new pending request. RequiredCount and ApproverRole are copied
// off the rule here and never re-read, so editing a rule cannot retroactively
// change what an in-flight request needs.
func (h Handler) raise(rule *models.ApprovalRule, action, entityType string, entityID uuid.UUID, amount float64, summary, requesterID string) (*models.ApprovalRequest, error) {
	parsed, err := uuid.Parse(requesterID)
	if err != nil {
		return nil, fmt.Errorf("approval gate: unparseable caller id: %w", err)
	}
	var actor models.User
	h.DB.First(&actor, "id = ?", parsed)

	// A rule saved with a nonsensical count must not produce a request that can
	// never be satisfied, or one that needs nobody.
	required := rule.RequiredCount
	if required < 1 {
		required = 1
	}

	key := pendingKeyFor(action, entityID, requesterID)
	pending := models.ApprovalRequest{
		Action:        action,
		EntityType:    entityType,
		EntityID:      entityID,
		Amount:        amount,
		Summary:       summary,
		Status:        models.ApprovalStatusPending,
		RequesterID:   &parsed,
		RequesterName: actor.FullName,
		RuleID:        &rule.ID,
		RequiredCount: required,
		ApproverRole:  rule.ApproverRole,
		PendingKey:    &key,
	}
	if err := h.DB.Create(&pending).Error; err != nil {
		return nil, err
	}
	return &pending, nil
}

// blockedResponse renders a gate refusal.
//
// 409 rather than 403: 403 means "you may not", which is the wrong claim — this
// caller may, once someone else agrees. 409 means "not yet", and keeps the two
// cases distinguishable to the client, which has to react differently to them.
func (h Handler) blockedResponse(c echo.Context, req *models.ApprovalRequest) error {
	msg := fmt.Sprintf("This action needs approval from %s. Request is awaiting a decision.", req.ApproverRole)
	if req.Status == models.ApprovalStatusRejected {
		var last models.ApprovalDecision
		reason := ""
		if err := h.DB.Where("request_id = ? AND decision = ?", req.ID, models.DecisionRejected).
			Order("created_at DESC").First(&last).Error; err == nil {
			reason = last.Reason
		}
		if reason != "" {
			msg = fmt.Sprintf("This action was rejected: %s. Revise and resubmit before trying again.", reason)
		} else {
			msg = "This action was rejected. Revise and resubmit before trying again."
		}
	}
	return c.JSON(http.StatusConflict, map[string]any{
		"error":               msg,
		"approval_request_id": req.ID,
		"approval_status":     req.Status,
	})
}

// zmw renders an amount for the human-readable summary an approver reads.
// It reuses the same two-decimal formatting as the CSV exports.
func zmw(amount float64) string { return "ZMW " + money(amount) }
