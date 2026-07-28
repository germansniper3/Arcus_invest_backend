package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"arcusinvest/internal/authz"
	"arcusinvest/internal/models"
	"arcusinvest/internal/services"

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
	// Best-effort, like the audit middleware: a notification that fails to send
	// must not turn a correctly-gated action into a 500.
	services.NotifyApprovalPending(h.DB, pending, func(r models.Role, resource string) bool {
		return authz.Can(r, authz.Resource(resource), authz.ActionRead)
	})
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

// ---------------------------------------------------------------------------
// Rules
// ---------------------------------------------------------------------------

func approvalRuleJSON(r models.ApprovalRule) map[string]any {
	return map[string]any{
		"id": r.ID, "created_at": r.CreatedAt, "action": r.Action,
		"min_amount": r.MinAmount, "required_count": r.RequiredCount,
		"approver_role": r.ApproverRole, "is_active": r.IsActive, "note": r.Note,
	}
}

var validApprovalActions = func() map[string]bool {
	m := make(map[string]bool, len(models.AllApprovalActions))
	for _, a := range models.AllApprovalActions {
		m[a] = true
	}
	return m
}()

// validateRule rejects a rule that could never be satisfied.
//
// The approver-role checks matter more than they look: a rule naming a role
// that does not exist, or one that cannot decide approvals, blocks its action
// permanently with nobody able to release it. That failure would present as
// "the app is broken", days after someone saved a typo in a settings screen.
func (h Handler) validateRule(c echo.Context, action string, minAmount float64, required int, role models.Role) (bool, error) {
	if !validApprovalActions[action] {
		return false, c.JSON(http.StatusBadRequest, errResponse("unknown approval action"))
	}
	if minAmount < 0 {
		return false, c.JSON(http.StatusBadRequest, errResponse("minimum amount cannot be negative"))
	}
	if required < 1 {
		return false, c.JSON(http.StatusBadRequest, errResponse("a rule must require at least one approval"))
	}
	var exists int64
	h.DB.Model(&models.CustomRole{}).Where("name = ?", string(role)).Count(&exists)
	if exists == 0 {
		return false, c.JSON(http.StatusBadRequest,
			errResponse(fmt.Sprintf("no role named %q exists", role)))
	}
	if !authz.Can(role, authz.ResApprovals, authz.ActionUpdate) {
		return false, c.JSON(http.StatusBadRequest, errResponse(fmt.Sprintf(
			"%s cannot decide approvals, so a rule naming it would block this action permanently", role)))
	}
	return true, nil
}

func (h Handler) AdminListApprovalRules(c echo.Context) error {
	var rows []models.ApprovalRule
	if err := h.DB.Order("action ASC, min_amount ASC").Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load approval rules"))
	}
	items := []map[string]any{}
	for _, r := range rows {
		items = append(items, approvalRuleJSON(r))
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items})
}

func (h Handler) AdminCreateApprovalRule(c echo.Context) error {
	var req struct {
		Action        string  `json:"action"`
		MinAmount     float64 `json:"min_amount"`
		RequiredCount int     `json:"required_count"`
		ApproverRole  string  `json:"approver_role"`
		Note          string  `json:"note"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if req.RequiredCount == 0 {
		req.RequiredCount = 1
	}
	role := models.Role(strings.TrimSpace(req.ApproverRole))
	if ok, err := h.validateRule(c, req.Action, req.MinAmount, req.RequiredCount, role); !ok {
		return err
	}
	rule := models.ApprovalRule{
		Action: req.Action, MinAmount: req.MinAmount, RequiredCount: req.RequiredCount,
		ApproverRole: role, IsActive: true, Note: strings.TrimSpace(req.Note),
	}
	if err := h.DB.Create(&rule).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not create the rule"))
	}
	return c.JSON(http.StatusCreated, approvalRuleJSON(rule))
}

// AdminUpdateApprovalRule edits a rule. In-flight requests are unaffected: they
// carry their own snapshot of RequiredCount and ApproverRole, so raising a
// threshold cannot auto-approve what is already pending and lowering one cannot
// invalidate approvals already given.
func (h Handler) AdminUpdateApprovalRule(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid rule id"))
	}
	var rule models.ApprovalRule
	if err := h.DB.First(&rule, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("rule not found"))
	}
	var req struct {
		MinAmount     *float64 `json:"min_amount"`
		RequiredCount *int     `json:"required_count"`
		ApproverRole  *string  `json:"approver_role"`
		IsActive      *bool    `json:"is_active"`
		Note          *string  `json:"note"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}

	next := rule
	if req.MinAmount != nil {
		next.MinAmount = *req.MinAmount
	}
	if req.RequiredCount != nil {
		next.RequiredCount = *req.RequiredCount
	}
	if req.ApproverRole != nil {
		next.ApproverRole = models.Role(strings.TrimSpace(*req.ApproverRole))
	}
	if ok, verr := h.validateRule(c, next.Action, next.MinAmount, next.RequiredCount, next.ApproverRole); !ok {
		return verr
	}

	updates := map[string]any{
		"min_amount": next.MinAmount, "required_count": next.RequiredCount,
		"approver_role": next.ApproverRole,
	}
	if req.IsActive != nil {
		updates["is_active"] = *req.IsActive
	}
	if req.Note != nil {
		updates["note"] = strings.TrimSpace(*req.Note)
	}
	if err := h.DB.Model(&rule).Updates(updates).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update the rule"))
	}
	h.DB.First(&rule, "id = ?", id)
	return c.JSON(http.StatusOK, approvalRuleJSON(rule))
}

// ---------------------------------------------------------------------------
// Decisions
// ---------------------------------------------------------------------------

func approvalDecisionJSON(d models.ApprovalDecision) map[string]any {
	return map[string]any{
		"id":            d.ID,
		"created_at":    d.CreatedAt,
		"approver_id":   d.ApproverID,
		"approver_name": d.ApproverName,
		"approver_role": d.ApproverRole,
		"decision":      d.Decision,
		"reason":        d.Reason,
	}
}

func approvalJSON(r models.ApprovalRequest) map[string]any {
	// Never nil: a nil slice marshals to `null` and a client doing
	// decisions.length would crash on a request nobody has voted on yet.
	decisions := []map[string]any{}
	approved := 0
	for _, d := range r.Decisions {
		decisions = append(decisions, approvalDecisionJSON(d))
		if d.Decision == models.DecisionApproved {
			approved++
		}
	}
	return map[string]any{
		"id":             r.ID,
		"created_at":     r.CreatedAt,
		"action":         r.Action,
		"entity_type":    r.EntityType,
		"entity_id":      r.EntityID,
		"amount":         r.Amount,
		"summary":        r.Summary,
		"status":         r.Status,
		"requester_id":   r.RequesterID,
		"requester_name": r.RequesterName,
		"required_count": r.RequiredCount,
		"approver_role":  r.ApproverRole,
		"approved_count": approved,
		"supersedes_id":  r.SupersedesID,
		"decided_at":     r.DecidedAt,
		"consumed_at":    r.ConsumedAt,
		"decisions":      decisions,
	}
}

// caller resolves the authenticated user. Approval records denormalise the name
// alongside the id so the trail still reads correctly after a user is deleted.
func (h Handler) caller(c echo.Context) (models.User, error) {
	var u models.User
	err := h.DB.First(&u, "id = ?", c.Get("user_id")).Error
	return u, err
}

// AdminListApprovals returns the approval queue. Filters are additive:
// ?status=pending, ?mine=true (raised by the caller), ?awaiting=true (waiting on
// a decision the caller is eligible to make).
func (h Handler) AdminListApprovals(c echo.Context) error {
	me, err := h.caller(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, errResponse("could not identify the caller"))
	}

	q := h.DB.Model(&models.ApprovalRequest{})
	if s := c.QueryParam("status"); s != "" {
		q = q.Where("status = ?", s)
	}
	if c.QueryParam("mine") == "true" {
		q = q.Where("requester_id = ?", me.ID)
	}
	if c.QueryParam("awaiting") == "true" {
		// Eligible means pending, addressed to the caller's role, and not their
		// own — the same three conditions the approve endpoint enforces, so the
		// list never shows a button that the server would refuse.
		q = q.Where("status = ? AND approver_role = ? AND requester_id <> ?",
			models.ApprovalStatusPending, me.Role, me.ID)
	}

	var rows []models.ApprovalRequest
	if err := q.Preload("Decisions").Order("created_at DESC").Limit(200).Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load approvals"))
	}
	items := []map[string]any{}
	for _, r := range rows {
		items = append(items, approvalJSON(r))
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items})
}

func (h Handler) AdminGetApproval(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid approval id"))
	}
	var row models.ApprovalRequest
	if err := h.DB.Preload("Decisions").First(&row, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("approval request not found"))
	}
	return c.JSON(http.StatusOK, approvalJSON(row))
}

// eligibleToDecide enforces the three conditions that make a vote legitimate.
// The first return value is whether the caller may proceed; when it is false the
// response has already been written and the caller must return the error as-is.
//
// It reports eligibility as a bool rather than as a non-nil error, because
// echo's c.JSON returns nil on success — a guard written as
// `if resp := eligible(...); resp != nil` therefore writes 403 and then falls
// straight through to record the vote anyway. That exact bug shipped here for
// the length of one test run.
//
// The self-approval check is the single most important line in this feature.
// Without it a requester holding the approver role signs off their own
// transaction and the whole control is theatre.
func (h Handler) eligibleToDecide(c echo.Context, req models.ApprovalRequest, me models.User) (bool, error) {
	if req.Status != models.ApprovalStatusPending {
		return false, c.JSON(http.StatusConflict, errResponse("this request has already been decided"))
	}
	if req.RequesterID != nil && *req.RequesterID == me.ID {
		return false, c.JSON(http.StatusForbidden, errResponse("you cannot approve your own request"))
	}
	// The rule named who may decide, and that snapshot travels on the request.
	// Deliberately strict: there is no super_admin override, because a standing
	// override is a standing bypass. A business that wants super_admin to decide
	// configures the rule that way.
	if me.Role != req.ApproverRole {
		return false, c.JSON(http.StatusForbidden,
			errResponse(fmt.Sprintf("this request must be decided by %s", req.ApproverRole)))
	}
	return true, nil
}

// recordDecision writes one vote. A duplicate vote from the same approver is
// refused by the composite unique index rather than by a prior SELECT, so two
// concurrent votes cannot both pass a check and both count toward the total.
//
// The insert is wrapped in its own transaction because that rejection is an
// EXPECTED outcome, not a fault. On Postgres a constraint violation aborts the
// surrounding transaction and every later statement on that connection fails
// with 25P02 — so without this, handling the duplicate gracefully still leaves
// the connection unusable. GORM turns this into a SAVEPOINT when a transaction
// is already open and a trivial one when it is not.
func (h Handler) recordDecision(req models.ApprovalRequest, me models.User, decision, reason string) error {
	return h.DB.Transaction(func(tx *gorm.DB) error {
		return tx.Create(&models.ApprovalDecision{
			RequestID:    req.ID,
			ApproverID:   me.ID,
			ApproverName: me.FullName,
			ApproverRole: me.Role,
			Decision:     decision,
			Reason:       reason,
		}).Error
	})
}

// finalise moves a request out of pending. The WHERE clause makes it a no-op for
// a request someone else already decided, so an approve and a reject arriving
// together resolve to whichever landed first instead of the last writer winning.
//
// pending_key is cleared here: it holds the (action, entity, requester)
// composite only while a request is open, and NULLing it frees the unique slot
// so the requester can raise a fresh request later.
func (h Handler) finalise(reqID uuid.UUID, status string) error {
	return h.DB.Model(&models.ApprovalRequest{}).
		Where("id = ? AND status = ?", reqID, models.ApprovalStatusPending).
		Updates(map[string]any{
			"status":      status,
			"decided_at":  time.Now(),
			"pending_key": nil,
		}).Error
}

// AdminApproveRequest records one approval and, once RequiredCount distinct
// approvers have signed off, marks the request approved.
//
// Approving does not perform the gated action. The requester retries it and the
// gate consumes this approval — see the note on gate for why.
func (h Handler) AdminApproveRequest(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid approval id"))
	}
	me, err := h.caller(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, errResponse("could not identify the caller"))
	}
	var row models.ApprovalRequest
	if err := h.DB.First(&row, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("approval request not found"))
	}
	if ok, err := h.eligibleToDecide(c, row, me); !ok {
		return err
	}

	var note struct {
		Reason string `json:"reason"`
	}
	_ = c.Bind(&note)

	if err := h.recordDecision(row, me, models.DecisionApproved, note.Reason); err != nil {
		// The unique index is the only realistic failure here.
		return c.JSON(http.StatusConflict, errResponse("you have already decided this request"))
	}

	var approved int64
	h.DB.Model(&models.ApprovalDecision{}).
		Where("request_id = ? AND decision = ?", row.ID, models.DecisionApproved).Count(&approved)
	if int(approved) >= row.RequiredCount {
		if err := h.finalise(row.ID, models.ApprovalStatusApproved); err != nil {
			return c.JSON(http.StatusInternalServerError, errResponse("could not finalise the approval"))
		}
		// Only once the request is actually decided. Under a two-approver rule
		// the first sign-off changes nothing the requester can act on, and
		// telling them otherwise would send them back to retry into a 409.
		services.NotifyApprovalDecided(h.DB, row, models.DecisionApproved, note.Reason, me.FullName)
	}

	h.DB.Preload("Decisions").First(&row, "id = ?", row.ID)
	return c.JSON(http.StatusOK, approvalJSON(row))
}

// AdminRejectRequest refuses a request and sends it back to its originator.
//
// One rejection is decisive even when RequiredCount is higher: requiring every
// approver to separately say no would leave a refused request sitting in the
// queue looking undecided. The reason is mandatory — a rejection the requester
// cannot act on is a dead end, and the revise-and-resubmit loop depends on it.
func (h Handler) AdminRejectRequest(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid approval id"))
	}
	me, err := h.caller(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, errResponse("could not identify the caller"))
	}
	var row models.ApprovalRequest
	if err := h.DB.First(&row, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("approval request not found"))
	}
	if ok, err := h.eligibleToDecide(c, row, me); !ok {
		return err
	}

	var req struct {
		Reason string `json:"reason"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		return c.JSON(http.StatusBadRequest, errResponse("a reason is required to reject a request"))
	}

	if err := h.recordDecision(row, me, models.DecisionRejected, reason); err != nil {
		return c.JSON(http.StatusConflict, errResponse("you have already decided this request"))
	}
	if err := h.finalise(row.ID, models.ApprovalStatusRejected); err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not record the rejection"))
	}
	services.NotifyApprovalDecided(h.DB, row, models.DecisionRejected, reason, me.FullName)

	h.DB.Preload("Decisions").First(&row, "id = ?", row.ID)
	return c.JSON(http.StatusOK, approvalJSON(row))
}

// AdminResubmitRequest raises a fresh request in place of a rejected one, linked
// back to it by SupersedesID so the loop reads as a chain in the history rather
// than vanishing into a status flip.
//
// Only the original requester may resubmit, and the rule is re-matched rather
// than copied: a resubmission is a new decision, and it should be measured
// against the thresholds in force now.
func (h Handler) AdminResubmitRequest(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid approval id"))
	}
	me, err := h.caller(c)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, errResponse("could not identify the caller"))
	}
	var prev models.ApprovalRequest
	if err := h.DB.First(&prev, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("approval request not found"))
	}
	if prev.RequesterID == nil || *prev.RequesterID != me.ID {
		return c.JSON(http.StatusForbidden, errResponse("only the original requester can resubmit"))
	}
	if prev.Status != models.ApprovalStatusRejected {
		return c.JSON(http.StatusConflict, errResponse("only a rejected request can be resubmitted"))
	}

	var body struct {
		Summary string `json:"summary"`
	}
	_ = c.Bind(&body)
	summary := strings.TrimSpace(body.Summary)
	if summary == "" {
		summary = prev.Summary
	}

	rule, err := h.matchRule(prev.Action, prev.Amount)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not check approval requirements"))
	}
	if rule == nil {
		// The threshold moved and this no longer needs approval at all. Say so
		// rather than raising a request nobody is required to decide.
		return c.JSON(http.StatusConflict,
			errResponse("this action no longer requires approval — retry it directly"))
	}

	next, err := h.raise(rule, prev.Action, prev.EntityType, prev.EntityID, prev.Amount, summary, me.ID.String())
	if err != nil {
		return c.JSON(http.StatusConflict, errResponse("a request for this action is already open"))
	}
	if err := h.DB.Model(next).Update("supersedes_id", prev.ID).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not link the resubmission"))
	}
	next.SupersedesID = &prev.ID
	return c.JSON(http.StatusCreated, approvalJSON(*next))
}
