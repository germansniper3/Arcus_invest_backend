package handlers

import (
	"arcusinvest/internal/authz"
	"arcusinvest/internal/config"
	"arcusinvest/internal/models"
	"arcusinvest/internal/services"
	"arcusinvest/internal/storage"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type Handler struct {
	DB    *gorm.DB
	Cfg   *config.Config
	Store storage.Storage
}

func (h Handler) Health(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]any{"status": "ok", "service": "arcusinvest-api", "time": time.Now().UTC()})
}

func (h Handler) Login(c echo.Context) error {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	token, user, err := services.Login(h.DB, h.Cfg, req.Email, req.Password)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, errResponse(err.Error()))
	}
	// Each sign-in starts its own refresh family, so revoking one device leaves
	// the others alone. Best-effort: failing to mint a refresh token should not
	// deny a correct password — the caller simply gets a session that ends when
	// the access token does.
	if raw, rerr := services.IssueRefreshToken(h.DB, user.ID, c.RealIP(), c.Request().UserAgent()); rerr == nil {
		setRefreshCookie(c, raw)
	} else {
		c.Logger().Errorf("could not issue refresh token for %s: %v", user.ID, rerr)
	}
	// Permissions must be included here as well as on /auth/me: the client stores
	// this user object straight after login, and a payload without them would make
	// the UI fall back to a coarse role guess.
	return c.JSON(http.StatusOK, map[string]any{"token": token, "user": userWithPermissions(user)})
}

func (h Handler) Me(c echo.Context) error {
	var user models.User
	if err := h.DB.Preload("StudentProfile").First(&user, "id = ?", c.Get("user_id")).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("user not found"))
	}
	return c.JSON(http.StatusOK, userWithPermissions(user))
}

// userWithPermissions is publicUser plus the caller's effective permissions, so
// the UI can hide what it cannot use. Presentation only — the backend remains
// authoritative via middleware.RequirePermission.
func userWithPermissions(user models.User) map[string]any {
	out := publicUser(user)
	out["permissions"] = authz.PermissionsFor(user.Role)
	return out
}

func (h Handler) CreateEnrollment(c echo.Context) error {
	var req models.Enrollment
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if strings.TrimSpace(req.FullName) == "" || strings.TrimSpace(req.Email) == "" {
		return c.JSON(http.StatusBadRequest, errResponse("full name and email are required"))
	}
	req.Email = strings.ToLower(req.Email)
	// This is a public endpoint: never let the caller set admin-managed fields.
	req.Status = models.StatusSubmitted
	req.Notes = ""
	req.AssignedUserID = nil
	req.StudentUserID = nil
	req.OrientationAt = nil
	if err := h.DB.Create(&req).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not submit enrollment"))
	}
	return c.JSON(http.StatusCreated, req)
}

// AdminCreateEnrollment lets an admin start an onboarding directly, without
// waiting for a public enrollment submission. Unlike the public endpoint it may
// set the tier and notes up front, and it rejects an email that already has an
// enrollment so duplicates don't accumulate.
func (h Handler) AdminCreateEnrollment(c echo.Context) error {
	var req struct {
		FullName string `json:"full_name"`
		Email    string `json:"email"`
		Phone    string `json:"phone"`
		Location string `json:"location"`
		Tier     string `json:"tier"`
		About    string `json:"about"`
		Notes    string `json:"notes"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	req.FullName = strings.TrimSpace(req.FullName)
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.FullName == "" {
		return c.JSON(http.StatusBadRequest, errResponse("full name is required"))
	}
	if req.Email == "" || !strings.Contains(req.Email, "@") {
		return c.JSON(http.StatusBadRequest, errResponse("a valid email is required"))
	}

	var existing int64
	h.DB.Model(&models.Enrollment{}).Where("LOWER(email) = ?", req.Email).Count(&existing)
	if existing > 0 {
		return c.JSON(http.StatusConflict, errResponse("an enrollment already exists for that email — invite from the existing record instead"))
	}
	// A student account for this email means onboarding is already done; the
	// claim step would fail later, so say so now.
	var existingUser int64
	h.DB.Unscoped().Model(&models.User{}).Where("LOWER(email) = ?", req.Email).Count(&existingUser)
	if existingUser > 0 {
		return c.JSON(http.StatusConflict, errResponse("a user account already exists for that email — delete it in Users first, or use a different email"))
	}

	enrollment := models.Enrollment{
		FullName: req.FullName,
		Email:    req.Email,
		Phone:    strings.TrimSpace(req.Phone),
		Location: strings.TrimSpace(req.Location),
		Tier:     strings.TrimSpace(req.Tier),
		About:    strings.TrimSpace(req.About),
		Notes:    strings.TrimSpace(req.Notes),
		Status:   models.StatusSubmitted,
	}
	if err := h.DB.Create(&enrollment).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not create enrollment"))
	}
	return c.JSON(http.StatusCreated, enrollment)
}

func (h Handler) ListEnrollments(c echo.Context) error {
	var rows []models.Enrollment
	query := h.DB.Order("created_at desc")
	if status := c.QueryParam("status"); status != "" {
		query = query.Where("status = ?", status)
	}
	if tier := c.QueryParam("tier"); tier != "" {
		query = query.Where("tier = ?", tier)
	}
	if err := query.Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load enrollments"))
	}
	return c.JSON(http.StatusOK, rows)
}

func (h Handler) UpdateEnrollment(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid enrollment id"))
	}
	var req struct {
		Status        models.EnrollmentStatus `json:"status"`
		Notes         string                  `json:"notes"`
		Tier          string                  `json:"tier"`
		OrientationAt *time.Time              `json:"orientation_at"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	updates := map[string]any{}
	if req.Status != "" {
		updates["status"] = req.Status
	}
	if req.Notes != "" {
		updates["notes"] = req.Notes
	}
	if req.Tier != "" {
		updates["tier"] = req.Tier
	}
	if req.OrientationAt != nil {
		updates["orientation_at"] = req.OrientationAt
	}
	var row models.Enrollment
	if err := h.DB.First(&row, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("enrollment not found"))
	}
	if err := h.DB.Model(&row).Updates(updates).First(&row, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update enrollment"))
	}
	return c.JSON(http.StatusOK, row)
}

// GenerateInvite creates a secure signup invitation for an accepted enrollment.
func (h Handler) GenerateInvite(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid enrollment id"))
	}
	invite, err := services.GenerateInvitation(h.DB, id)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse(err.Error()))
	}
	claimURL := fmt.Sprintf("%s/claim-invitation?token=%s", strings.TrimRight(h.Cfg.FrontendURL, "/"), invite.Token)

	// Email the link when SMTP is available. This is best-effort: the claim URL is
	// always returned so the admin can still share it manually if delivery fails.
	emailed := false
	emailError := ""
	if h.Cfg.MailConfigured() {
		var enrollment models.Enrollment
		h.DB.First(&enrollment, "id = ?", invite.EnrollmentID)
		if err := services.SendInvitationEmail(h.Cfg, invite.Email, enrollment.FullName, enrollment.Tier, claimURL, invite.ExpiresAt); err != nil {
			emailError = err.Error()
			c.Logger().Errorf("invitation email to %s failed: %v", invite.Email, err)
		} else {
			emailed = true
		}
	} else {
		emailError = "no email transport is configured — set RESEND_API_KEY, or SMTP_HOST/SMTP_PORT"
	}

	return c.JSON(http.StatusCreated, map[string]any{
		"invitation":  invite,
		"claim_url":   claimURL,
		"emailed":     emailed,
		"email_error": emailError,
	})
}

// AdminEmailStatus reports whether outbound email is usable, without exposing
// the password. Diagnostic only — it does not attempt delivery.
func (h Handler) AdminEmailStatus(c echo.Context) error {
	advice := services.MailAdvice(h.Cfg)
	return c.JSON(http.StatusOK, map[string]any{
		"configured":    h.Cfg.MailConfigured(),
		"transport":     h.Cfg.MailTransport(),
		"host":          h.Cfg.SMTPHost,
		"port":          h.Cfg.SMTPPort,
		"from":          h.Cfg.MailFrom,
		"has_username":  h.Cfg.SMTPUsername != "",
		"has_password":  h.Cfg.SMTPPassword != "",
		"has_api_key":   h.Cfg.ResendAPIKey != "",
		"frontend_url":  h.Cfg.FrontendURL,
		"issues":        advice,
		"looks_healthy": h.Cfg.MailConfigured() && len(advice) == 0,
	})
}

// AdminSendTestEmail sends a diagnostic email to the requesting admin's OWN
// address, so SMTP can be proven without emailing students.
func (h Handler) AdminSendTestEmail(c echo.Context) error {
	if !h.Cfg.MailConfigured() {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error":  "no email transport is configured — set RESEND_API_KEY and MAIL_FROM, or SMTP_HOST/SMTP_PORT/MAIL_FROM",
			"issues": services.MailAdvice(h.Cfg),
		})
	}
	var actor models.User
	if err := h.DB.First(&actor, "id = ?", c.Get("user_id")).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("could not resolve your account"))
	}
	if err := services.SendTestEmail(h.Cfg, actor.Email); err != nil {
		c.Logger().Errorf("%s test email to %s failed: %v", h.Cfg.MailTransport(), actor.Email, err)
		return c.JSON(http.StatusBadGateway, map[string]any{
			"error":  "send failed: " + err.Error(),
			"issues": services.MailAdvice(h.Cfg),
		})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"message": "test email sent to " + actor.Email,
	})
}

// PreviewInvitation checks that a token is valid before the student fills in the form.
func (h Handler) PreviewInvitation(c echo.Context) error {
	token := c.Param("token")
	var invite models.OnboardingInvitation
	if err := h.DB.First(&invite, "token = ? AND status = ?", token, "pending").Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("invitation not found or already claimed"))
	}
	if time.Now().After(invite.ExpiresAt) {
		return c.JSON(http.StatusGone, errResponse("invitation has expired"))
	}
	var enrollment models.Enrollment
	h.DB.First(&enrollment, "id = ?", invite.EnrollmentID)
	return c.JSON(http.StatusOK, map[string]any{
		"email":      invite.Email,
		"full_name":  enrollment.FullName,
		"tier":       enrollment.Tier,
		"expires_at": invite.ExpiresAt,
	})
}

// ClaimInvitation creates the student account from an invitation token.
func (h Handler) ClaimInvitation(c echo.Context) error {
	var req struct {
		Token           string `json:"token"`
		Password        string `json:"password"`
		CapstoneTitle   string `json:"capstone_title"`
		CapstoneSummary string `json:"capstone_summary"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if err := services.ValidatePassword(req.Password); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse(err.Error()))
	}
	user, err := services.ClaimInvitation(h.DB, req.Token, req.Password, req.CapstoneTitle, req.CapstoneSummary)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse(err.Error()))
	}
	return c.JSON(http.StatusCreated, publicUser(*user))
}

func (h Handler) StudentDashboard(c echo.Context) error {
	var profile models.StudentProfile
	if err := h.DB.Where("user_id = ?", c.Get("user_id")).First(&profile).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("student profile not found"))
	}
	var enrollment models.Enrollment
	if profile.EnrollmentID != nil {
		_ = h.DB.First(&enrollment, "id = ?", profile.EnrollmentID).Error
	}
	var milestones []models.CapstoneMilestone
	h.DB.Where("student_profile_id = ?", profile.ID).Order("created_at asc").Find(&milestones)
	var comments []models.CapstoneComment
	h.DB.Where("student_profile_id = ?", profile.ID).Order("created_at asc").Find(&comments)
	var reports []models.ProgressReport
	h.DB.Where("student_profile_id = ?", profile.ID).Order("created_at desc").Find(&reports)
	var extensions []models.ExtensionRequest
	h.DB.Where("student_profile_id = ?", profile.ID).Order("created_at desc").Find(&extensions)
	var submissions []models.Submission
	h.DB.Where("student_profile_id = ?", profile.ID).Order("created_at desc").Find(&submissions)
	return c.JSON(http.StatusOK, map[string]any{
		"profile":          profile,
		"enrollment":       enrollment,
		"milestones":       milestones,
		"comments":         comments,
		"progress_reports": progressReportsJSON(reports),
		"extensions":       extensionsJSON(extensions),
		"submissions":      submissionsJSON(submissions),
	})
}

func (h Handler) UpdateCapstone(c echo.Context) error {
	var req struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	var profile models.StudentProfile
	if err := h.DB.Where("user_id = ?", c.Get("user_id")).First(&profile).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("student profile not found"))
	}
	h.DB.Model(&profile).Updates(map[string]any{"capstone_title": req.Title, "capstone_summary": req.Summary})
	return c.JSON(http.StatusOK, profile)
}

// UpdateMilestone lets a student update the STATUS of one of their OWN
// milestones. Feedback is a mentor-only field and is never accepted here.
func (h Handler) UpdateMilestone(c echo.Context) error {
	milestoneID, err := uuid.Parse(c.Param("mid"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid milestone id"))
	}
	var profile models.StudentProfile
	if err := h.DB.Where("user_id = ?", c.Get("user_id")).First(&profile).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("student profile not found"))
	}
	var milestone models.CapstoneMilestone
	if err := h.DB.First(&milestone, "id = ?", milestoneID).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("milestone not found"))
	}
	// Ownership check: students may only touch their own milestones.
	if milestone.StudentProfileID != profile.ID {
		return c.JSON(http.StatusNotFound, errResponse("milestone not found"))
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	// Students may work a milestone and submit it for review, but only a
	// mentor can mark it completed — completion is a sign-off, not a
	// self-report.
	switch req.Status {
	case "", "pending", "in_progress", "pending_review":
		// allowed
	case "completed":
		return c.JSON(http.StatusForbidden, errResponse("completion requires mentor sign-off — submit the milestone for review instead"))
	default:
		return c.JSON(http.StatusBadRequest, errResponse("status must be pending, in_progress, or pending_review"))
	}
	// A mentor-completed milestone is locked for the student; ask a mentor to
	// reopen it.
	if milestone.Status == "completed" {
		return c.JSON(http.StatusConflict, errResponse("milestone is completed and locked — ask a mentor to reopen it"))
	}
	h.applyMilestoneUpdate(&milestone, req.Status, nil)
	return c.JSON(http.StatusOK, milestone)
}

// validMilestoneStatus whitelists the milestone status values mentors may set.
func validMilestoneStatus(status string) bool {
	switch status {
	case "pending", "in_progress", "pending_review", "completed":
		return true
	}
	return false
}

// applyMilestoneUpdate applies a status change (and, for mentors, feedback) to a
// milestone, then recomputes the owning profile's progress. A nil feedback
// pointer leaves the feedback field untouched.
func (h Handler) applyMilestoneUpdate(milestone *models.CapstoneMilestone, status string, feedback *string) {
	updates := map[string]any{}
	if status != "" {
		updates["status"] = status
		if status == "completed" {
			now := time.Now()
			updates["completed_at"] = &now
		} else {
			// Un-completing a milestone clears its stale completion timestamp.
			updates["completed_at"] = nil
		}
	}
	if feedback != nil {
		updates["feedback"] = *feedback
	}
	if len(updates) > 0 {
		h.DB.Model(milestone).Updates(updates)
		h.DB.First(milestone, "id = ?", milestone.ID)
	}
	// Update profile progress based on completed milestones.
	var total, completed int64
	h.DB.Model(&models.CapstoneMilestone{}).Where("student_profile_id = ?", milestone.StudentProfileID).Count(&total)
	h.DB.Model(&models.CapstoneMilestone{}).Where("student_profile_id = ? AND status = ?", milestone.StudentProfileID, "completed").Count(&completed)
	if total > 0 {
		pct := int((completed * 100) / total)
		h.DB.Model(&models.StudentProfile{}).Where("id = ?", milestone.StudentProfileID).Update("progress_pct", pct)
	}
}

func (h Handler) PostComment(c echo.Context) error {
	var req struct {
		Message string `json:"message"`
	}
	if err := c.Bind(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		return c.JSON(http.StatusBadRequest, errResponse("message is required"))
	}
	var user models.User
	if err := h.DB.Preload("StudentProfile").First(&user, "id = ?", c.Get("user_id")).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("user not found"))
	}
	if user.StudentProfile == nil {
		return c.JSON(http.StatusNotFound, errResponse("student profile not found"))
	}
	comment := models.CapstoneComment{
		StudentProfileID: user.StudentProfile.ID,
		AuthorName:       user.FullName,
		AuthorRole:       user.Role,
		Message:          req.Message,
	}
	if err := h.DB.Create(&comment).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not save comment"))
	}
	return c.JSON(http.StatusCreated, comment)
}

// Admin student detail view — milestones + comments
func (h Handler) AdminStudentDetail(c echo.Context) error {
	sid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid student id"))
	}
	var user models.User
	if err := h.DB.Preload("StudentProfile").First(&user, "id = ?", sid).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("student not found"))
	}
	if user.StudentProfile == nil {
		return c.JSON(http.StatusNotFound, errResponse("student profile not found"))
	}
	profile := user.StudentProfile
	var milestones []models.CapstoneMilestone
	h.DB.Where("student_profile_id = ?", profile.ID).Order("created_at asc").Find(&milestones)
	var comments []models.CapstoneComment
	h.DB.Where("student_profile_id = ?", profile.ID).Order("created_at asc").Find(&comments)
	var reports []models.ProgressReport
	h.DB.Where("student_profile_id = ?", profile.ID).Order("created_at desc").Find(&reports)
	var extensions []models.ExtensionRequest
	h.DB.Where("student_profile_id = ?", profile.ID).Order("created_at desc").Find(&extensions)
	var submissions []models.Submission
	h.DB.Where("student_profile_id = ?", profile.ID).Order("created_at desc").Find(&submissions)
	return c.JSON(http.StatusOK, map[string]any{
		"user": publicUser(user), "profile": profile,
		"milestones": milestones, "comments": comments,
		"progress_reports": progressReportsJSON(reports),
		"extensions":       extensionsJSON(extensions),
		"submissions":      submissionsJSON(submissions),
	})
}

// Admin can post a comment/feedback on a student's capstone
func (h Handler) AdminPostComment(c echo.Context) error {
	sid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid student id"))
	}
	var req struct {
		Message string `json:"message"`
	}
	if err := c.Bind(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		return c.JSON(http.StatusBadRequest, errResponse("message is required"))
	}
	var adminUser models.User
	if err := h.DB.First(&adminUser, "id = ?", c.Get("user_id")).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("admin user not found"))
	}
	var targetUser models.User
	if err := h.DB.Preload("StudentProfile").First(&targetUser, "id = ?", sid).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("student not found"))
	}
	if targetUser.StudentProfile == nil {
		return c.JSON(http.StatusNotFound, errResponse("student profile not found"))
	}
	comment := models.CapstoneComment{
		StudentProfileID: targetUser.StudentProfile.ID,
		AuthorName:       adminUser.FullName,
		AuthorRole:       adminUser.Role,
		Message:          req.Message,
	}
	if err := h.DB.Create(&comment).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not save comment"))
	}
	return c.JSON(http.StatusCreated, comment)
}

// AdminUpdateMilestone lets a mentor/admin update the status AND feedback of a
// milestone. It validates that the :id student owns the :mid milestone.
func (h Handler) AdminUpdateMilestone(c echo.Context) error {
	studentID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid student id"))
	}
	milestoneID, err := uuid.Parse(c.Param("mid"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid milestone id"))
	}
	var profile models.StudentProfile
	if err := h.DB.Where("user_id = ?", studentID).First(&profile).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("student not found"))
	}
	var milestone models.CapstoneMilestone
	if err := h.DB.First(&milestone, "id = ?", milestoneID).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("milestone not found"))
	}
	if milestone.StudentProfileID != profile.ID {
		return c.JSON(http.StatusNotFound, errResponse("milestone not found"))
	}
	var req struct {
		Status   string  `json:"status"`
		Feedback *string `json:"feedback"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if req.Status != "" && !validMilestoneStatus(req.Status) {
		return c.JSON(http.StatusBadRequest, errResponse("status must be pending, in_progress, pending_review, or completed"))
	}
	// A present-but-empty feedback string is a deliberate clear; an absent field
	// leaves feedback untouched. The admin UI prefills the existing value.
	var feedback *string
	if req.Feedback != nil {
		trimmed := strings.TrimSpace(*req.Feedback)
		feedback = &trimmed
	}
	h.applyMilestoneUpdate(&milestone, req.Status, feedback)
	return c.JSON(http.StatusOK, milestone)
}

// ListStudents returns all users with the student role
func (h Handler) ListStudents(c echo.Context) error {
	var users []models.User
	if err := h.DB.Preload("StudentProfile").Where("role = ?", models.RoleStudent).Order("created_at desc").Find(&users).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load students"))
	}
	result := make([]map[string]any, 0, len(users))
	for _, u := range users {
		result = append(result, publicUser(u))
	}
	return c.JSON(http.StatusOK, result)
}

// --- Events ---

func (h Handler) ListPublicEvents(c echo.Context) error {
	var events []models.Event
	if err := h.DB.Where("is_published = ?", true).Order("date asc").Find(&events).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load events"))
	}
	return c.JSON(http.StatusOK, events)
}

func (h Handler) GetPublicEvent(c echo.Context) error {
	var event models.Event
	if err := h.DB.Where("slug = ? AND is_published = ?", c.Param("slug"), true).First(&event).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("event not found"))
	}
	var count int64
	h.DB.Model(&models.Reservation{}).Where("event_id = ? AND status = ?", event.ID, "confirmed").Count(&count)
	return c.JSON(http.StatusOK, map[string]any{"event": event, "reservations_count": count})
}

func (h Handler) CreateReservation(c echo.Context) error {
	eventID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid event id"))
	}
	var event models.Event
	if err := h.DB.First(&event, "id = ? AND is_published = ?", eventID, true).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("event not found"))
	}
	var req struct {
		FullName string `json:"full_name"`
		Email    string `json:"email"`
		Phone    string `json:"phone"`
		Notes    string `json:"notes"`
	}
	if err := c.Bind(&req); err != nil || req.FullName == "" || req.Email == "" {
		return c.JSON(http.StatusBadRequest, errResponse("full name and email are required"))
	}
	// Check capacity
	if event.Capacity > 0 {
		var count int64
		h.DB.Model(&models.Reservation{}).Where("event_id = ? AND status = ?", eventID, "confirmed").Count(&count)
		if int(count) >= event.Capacity {
			return c.JSON(http.StatusConflict, errResponse("event is at full capacity"))
		}
	}
	res := models.Reservation{
		EventID: eventID, FullName: req.FullName,
		Email: strings.ToLower(req.Email), Phone: req.Phone, Notes: req.Notes, Status: "pending",
	}
	if idRaw, ok := c.Get("user_id").(string); ok && idRaw != "" {
		id := services.UserIDFromString(idRaw)
		if id != uuid.Nil {
			res.UserID = &id
		}
	}
	if err := h.DB.Create(&res).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not save reservation"))
	}
	return c.JSON(http.StatusCreated, res)
}

func (h Handler) AdminListEvents(c echo.Context) error {
	var events []models.Event
	if err := h.DB.Order("date asc").Find(&events).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load events"))
	}
	return c.JSON(http.StatusOK, events)
}

func (h Handler) AdminCreateEvent(c echo.Context) error {
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Date        string `json:"date"`
		Location    string `json:"location"`
		Capacity    int    `json:"capacity"`
		IsPublished bool   `json:"is_published"`
		ImageURL    string `json:"image_url"`
		Slug        string `json:"slug"`
	}
	if err := c.Bind(&req); err != nil || strings.TrimSpace(req.Title) == "" {
		return c.JSON(http.StatusBadRequest, errResponse("title is required"))
	}
	parsedDate, err := time.Parse("2006-01-02T15:04", req.Date)
	if err != nil {
		parsedDate, err = time.Parse(time.RFC3339, req.Date)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errResponse("invalid date format — use YYYY-MM-DDTHH:MM"))
		}
	}
	slug := req.Slug
	if slug == "" {
		slug = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(req.Title), " ", "-"))
	}
	event := models.Event{
		Title: req.Title, Description: req.Description, Date: parsedDate,
		Location: req.Location, Capacity: req.Capacity, IsPublished: req.IsPublished,
		ImageURL: req.ImageURL, Slug: slug,
	}
	if err := h.DB.Create(&event).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not create event"))
	}
	return c.JSON(http.StatusCreated, event)
}

func (h Handler) AdminUpdateEvent(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid event id"))
	}
	var event models.Event
	if err := h.DB.First(&event, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("event not found"))
	}
	var req struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		Date        *string `json:"date"`
		Location    *string `json:"location"`
		Capacity    *int    `json:"capacity"`
		IsPublished *bool   `json:"is_published"`
		ImageURL    *string `json:"image_url"`
		Slug        *string `json:"slug"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid body"))
	}
	if req.Title != nil {
		event.Title = *req.Title
	}
	if req.Description != nil {
		event.Description = *req.Description
	}
	if req.Location != nil {
		event.Location = *req.Location
	}
	if req.ImageURL != nil {
		event.ImageURL = *req.ImageURL
	}
	if req.Slug != nil {
		event.Slug = *req.Slug
	}
	if req.Capacity != nil {
		event.Capacity = *req.Capacity
	}
	if req.IsPublished != nil {
		event.IsPublished = *req.IsPublished
	}
	if req.Date != nil && *req.Date != "" {
		parsed, err := time.Parse("2006-01-02T15:04", *req.Date)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, *req.Date)
		}
		if err == nil {
			event.Date = parsed
		}
	}
	h.DB.Save(&event)
	return c.JSON(http.StatusOK, event)
}

func (h Handler) AdminDeleteEvent(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid event id"))
	}
	h.DB.Delete(&models.Event{}, "id = ?", id)
	return c.JSON(http.StatusOK, map[string]string{"message": "event deleted"})
}

func (h Handler) AdminListReservations(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid event id"))
	}
	var reservations []models.Reservation
	h.DB.Where("event_id = ?", id).Order("created_at desc").Find(&reservations)
	return c.JSON(http.StatusOK, reservations)
}

// AdminApproveReservation changes a pending reservation to confirmed
func (h Handler) AdminApproveReservation(c echo.Context) error {
	rid, err := uuid.Parse(c.Param("rid"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid reservation id"))
	}
	var res models.Reservation
	if err := h.DB.First(&res, "id = ?", rid).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("reservation not found"))
	}
	res.Status = "confirmed"
	h.DB.Save(&res)
	return c.JSON(http.StatusOK, res)
}

func (h Handler) AdminBroadcast(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid event id"))
	}
	var event models.Event
	if err := h.DB.First(&event, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("event not found"))
	}
	var req struct {
		Subject string `json:"subject"`
		Message string `json:"message"`
	}
	if err := c.Bind(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		return c.JSON(http.StatusBadRequest, errResponse("message is required"))
	}

	var reservations []models.Reservation
	h.DB.Where("event_id = ? AND status = ?", id, "confirmed").Find(&reservations)
	recipients := make([]string, 0, len(reservations))
	for _, r := range reservations {
		if strings.TrimSpace(r.Email) != "" {
			recipients = append(recipients, r.Email)
		}
	}

	broadcast := models.Broadcast{
		EventID:        id,
		Subject:        req.Subject,
		Message:        req.Message,
		RecipientCount: len(recipients),
		Status:         "queued",
	}
	if err := h.DB.Create(&broadcast).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not save broadcast"))
	}

	sent := false
	var sendErr error
	if h.Cfg.MailConfigured() && len(recipients) > 0 {
		if sendErr = services.SendBroadcastEmail(h.Cfg, recipients, req.Subject, req.Message); sendErr == nil {
			now := time.Now().UTC()
			broadcast.Status = "sent"
			broadcast.SentAt = &now
			h.DB.Model(&broadcast).Updates(map[string]any{"status": "sent", "sent_at": &now})
			sent = true
		} else {
			// Keep the stored broadcast as 'queued' but make the failure diagnosable.
			c.Logger().Errorf("broadcast %s: %s send failed: %v", broadcast.ID, h.Cfg.MailTransport(), sendErr)
		}
	}

	message := "broadcast stored and queued — email not sent (no transport configured)"
	if sent {
		message = "broadcast sent"
	} else if len(recipients) == 0 {
		message = "broadcast stored — no confirmed recipients to email"
	} else if h.Cfg.MailConfigured() {
		message = "broadcast stored and queued — email delivery failed"
	}

	return c.JSON(http.StatusOK, map[string]any{
		"message":    message,
		"recipients": broadcast.RecipientCount,
		"status":     broadcast.Status,
		"subject":    req.Subject,
	})
}

// --- Quotes ---

func (h Handler) CreateQuote(c echo.Context) error {
	var req models.QuoteRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if req.Name == "" || req.Email == "" || req.Message == "" {
		return c.JSON(http.StatusBadRequest, errResponse("name, email, and message are required"))
	}
	req.Email = strings.ToLower(req.Email)
	// Public endpoint: admin-managed fields must not be settable by the caller.
	req.Status = "new"
	req.AdminNotes = ""
	if err := h.DB.Create(&req).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not submit quote request"))
	}
	return c.JSON(http.StatusCreated, req)
}

func (h Handler) ListQuotes(c echo.Context) error {
	var rows []models.QuoteRequest
	if err := h.DB.Order("created_at desc").Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load quote requests"))
	}
	return c.JSON(http.StatusOK, rows)
}

func (h Handler) AdminUpdateQuote(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid quote id"))
	}
	var req struct {
		Status     string `json:"status"`
		AdminNotes string `json:"admin_notes"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	var quote models.QuoteRequest
	if err := h.DB.First(&quote, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("quote request not found"))
	}
	updates := map[string]any{}
	if req.Status != "" {
		updates["status"] = req.Status
	}
	// Allow clearing or setting admin notes
	updates["admin_notes"] = req.AdminNotes

	if err := h.DB.Model(&quote).Updates(updates).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update quote request"))
	}
	h.DB.First(&quote, "id = ?", id)
	return c.JSON(http.StatusOK, quote)
}

// --- Chat ---

func (h Handler) Chat(c echo.Context) error {
	var req struct {
		SessionID string `json:"session_id"`
		Question  string `json:"question"`
	}
	if err := c.Bind(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		return c.JSON(http.StatusBadRequest, errResponse("question is required"))
	}
	if req.SessionID == "" {
		req.SessionID = uuid.NewString()
	}
	answer, source, err := services.Chat(h.Cfg, req.Question)
	if err != nil {
		return c.JSON(http.StatusBadGateway, errResponse(err.Error()))
	}
	msg := models.ChatMessage{SessionID: req.SessionID, Question: req.Question, Answer: answer, Source: source}
	if idRaw, ok := c.Get("user_id").(string); ok && idRaw != "" {
		id := services.UserIDFromString(idRaw)
		if id != uuid.Nil {
			msg.UserID = &id
		}
	}
	_ = h.DB.Create(&msg).Error
	return c.JSON(http.StatusOK, msg)
}

// --- Admin User Management ---

// roleAssignable reports whether a role slug may be assigned to a user. Any row
// in custom_roles qualifies; built-in roles also qualify even when the table is
// empty, so a failed role seed degrades to the shipped roles instead of making
// every role "invalid" and blocking user administration entirely.
func (h Handler) roleAssignable(role models.Role) bool {
	var n int64
	h.DB.Model(&models.CustomRole{}).Where("name = ?", string(role)).Count(&n)
	return n > 0 || authz.IsBuiltInRole(role)
}

func roleHasPrivilegedAccess(role models.Role) bool {
	for _, res := range []authz.Resource{authz.ResUsers, authz.ResRoles} {
		if authz.Can(role, res, authz.ActionCreate) || authz.Can(role, res, authz.ActionUpdate) || authz.Can(role, res, authz.ActionDelete) {
			return true
		}
	}
	return false
}

func (h Handler) CreateUser(c echo.Context) error {
	var req struct {
		Email    string      `json:"email"`
		FullName string      `json:"full_name"`
		Password string      `json:"password"`
		Role     models.Role `json:"role"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	// Validate before doing any expensive work.
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.FullName = strings.TrimSpace(req.FullName)
	if req.Email == "" || !strings.Contains(req.Email, "@") {
		return c.JSON(http.StatusBadRequest, errResponse("a valid email is required"))
	}
	if req.FullName == "" {
		return c.JSON(http.StatusBadRequest, errResponse("full name is required"))
	}
	if err := services.ValidatePassword(req.Password); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse(err.Error()))
	}
	// Whitelist roles. Students are created only through invitation claims.
	role := req.Role
	if role == "" {
		role = models.RoleAdmissions
	}
	if role == models.RoleStudent {
		return c.JSON(http.StatusBadRequest, errResponse("student accounts cannot be created directly — use onboarding invitation"))
	}
	if !h.roleAssignable(role) {
		return c.JSON(http.StatusBadRequest, errResponse("invalid role"))
	}

	// Assigning a role that holds write access on users or roles requires super_admin.
	callerRole, _ := c.Get("role").(models.Role)
	if roleHasPrivilegedAccess(role) && callerRole != models.RoleSuperAdmin {
		return c.JSON(http.StatusForbidden, errResponse("only a super admin can create a user with user or role management permissions"))
	}

	// Reject duplicate emails with a clean message instead of leaking the raw DB constraint error.
	var existing int64
	h.DB.Model(&models.User{}).Where("email = ?", req.Email).Count(&existing)
	if existing > 0 {
		return c.JSON(http.StatusConflict, errResponse("a user with that email already exists"))
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not create user"))
	}
	user := models.User{Email: req.Email, FullName: req.FullName, PasswordHash: string(hash), Role: role, IsActive: true}
	if err := h.DB.Create(&user).Error; err != nil {
		return c.JSON(http.StatusConflict, errResponse("could not create user"))
	}
	return c.JSON(http.StatusCreated, publicUser(user))
}

// AdminListUsers returns every account with its role and activity state, so
// admins can see exactly who exists (including leftover test accounts).
func (h Handler) AdminListUsers(c echo.Context) error {
	var users []models.User
	q := h.DB.Preload("StudentProfile").Order("created_at desc")
	if role := c.QueryParam("role"); role != "" {
		q = q.Where("role = ?", role)
	}
	if err := q.Find(&users).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load users"))
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		row := publicUser(u)
		row["created_at"] = u.CreatedAt
		row["last_login_at"] = u.LastLoginAt
		out = append(out, row)
	}
	return c.JSON(http.StatusOK, out)
}

// AdminUpdateUser toggles a user's active state or changes their role. Guards:
// only a super admin may grant or revoke super-admin or privileged roles (write on users/roles),
// and nobody may lock themselves out by deactivating or demoting their own account.
func (h Handler) AdminUpdateUser(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid user id"))
	}
	var target models.User
	if err := h.DB.First(&target, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("user not found"))
	}
	var req struct {
		IsActive *bool   `json:"is_active"`
		Role     *string `json:"role"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid body"))
	}

	callerRole, _ := c.Get("role").(models.Role)
	callerID, _ := c.Get("user_id").(string)
	isSelf := callerID == target.ID.String()

	updates := map[string]any{}
	if req.IsActive != nil {
		if isSelf && !*req.IsActive {
			return c.JSON(http.StatusBadRequest, errResponse("you cannot deactivate your own account"))
		}
		if !*req.IsActive && roleHasPrivilegedAccess(target.Role) && callerRole != models.RoleSuperAdmin {
			return c.JSON(http.StatusForbidden, errResponse("only a super admin can deactivate a privileged account"))
		}
		updates["is_active"] = *req.IsActive
	}
	if req.Role != nil {
		role := models.Role(*req.Role)
		if !h.roleAssignable(role) {
			return c.JSON(http.StatusBadRequest, errResponse("invalid role"))
		}
		if isSelf && role != target.Role {
			return c.JSON(http.StatusBadRequest, errResponse("you cannot change your own role"))
		}
		// Granting or revoking a role with write access on users or roles requires super_admin.
		if (roleHasPrivilegedAccess(role) || roleHasPrivilegedAccess(target.Role)) && callerRole != models.RoleSuperAdmin {
			return c.JSON(http.StatusForbidden, errResponse("only a super admin can manage administrative role assignments"))
		}
		updates["role"] = role
	}
	if len(updates) == 0 {
		return c.JSON(http.StatusBadRequest, errResponse("nothing to update"))
	}
	if err := h.DB.Model(&target).Updates(updates).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update user"))
	}
	h.DB.Preload("StudentProfile").First(&target, "id = ?", id)
	return c.JSON(http.StatusOK, publicUser(target))
}

// AdminDeleteUser permanently removes an account. This is a HARD delete on
// purpose: User.Email carries a unique index that a soft-deleted row would keep
// occupying, which would block ever re-onboarding that email. For a student it
// also clears the profile and detaches the enrollment so it can be re-invited.
func (h Handler) AdminDeleteUser(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid user id"))
	}
	var target models.User
	if err := h.DB.Preload("StudentProfile").First(&target, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("user not found"))
	}
	if callerID, _ := c.Get("user_id").(string); callerID == target.ID.String() {
		return c.JSON(http.StatusBadRequest, errResponse("you cannot delete your own account"))
	}
	callerRole, _ := c.Get("role").(models.Role)
	if target.Role == models.RoleSuperAdmin && callerRole != models.RoleSuperAdmin {
		return c.JSON(http.StatusForbidden, errResponse("only a super admin can delete a super admin"))
	}

	err = h.DB.Transaction(func(tx *gorm.DB) error {
		if target.StudentProfile != nil {
			profileID := target.StudentProfile.ID
			// Clear capstone artefacts belonging to this profile.
			tx.Unscoped().Where("student_profile_id = ?", profileID).Delete(&models.CapstoneMilestone{})
			tx.Unscoped().Where("student_profile_id = ?", profileID).Delete(&models.CapstoneComment{})
			tx.Unscoped().Where("student_profile_id = ?", profileID).Delete(&models.ProgressReport{})
			tx.Unscoped().Where("student_profile_id = ?", profileID).Delete(&models.ExtensionRequest{})
			tx.Unscoped().Where("student_profile_id = ?", profileID).Delete(&models.Submission{})
			if err := tx.Unscoped().Delete(&models.StudentProfile{}, "id = ?", profileID).Error; err != nil {
				return err
			}
		}
		// Detach the enrollment and reopen it, and drop any invitation, so the
		// same person can be onboarded again.
		var enrollments []models.Enrollment
		tx.Where("student_user_id = ?", target.ID).Find(&enrollments)
		for _, e := range enrollments {
			tx.Model(&models.Enrollment{}).Where("id = ?", e.ID).Updates(map[string]any{
				"student_user_id": nil, "status": models.StatusSubmitted,
			})
			tx.Unscoped().Where("enrollment_id = ?", e.ID).Delete(&models.OnboardingInvitation{})
		}
		return tx.Unscoped().Delete(&models.User{}, "id = ?", target.ID).Error
	})
	if err != nil {
		c.Logger().Errorf("delete user %s failed: %v", target.ID, err)
		return c.JSON(http.StatusInternalServerError, errResponse("could not delete user"))
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "user deleted"})
}

func (h Handler) Metrics(c echo.Context) error {
	var enrollments, quotes, students, events int64
	h.DB.Model(&models.Enrollment{}).Count(&enrollments)
	h.DB.Model(&models.QuoteRequest{}).Where("status = ?", "new").Count(&quotes)
	h.DB.Model(&models.User{}).Where("role = ?", models.RoleStudent).Count(&students)
	h.DB.Model(&models.Event{}).Where("is_published = ?", true).Count(&events)
	return c.JSON(http.StatusOK, map[string]any{
		"enrollments": enrollments, "open_quotes": quotes,
		"students": students, "active_events": events,
	})
}

// --- Products ---

func (h Handler) ListPublicProducts(c echo.Context) error {
	var rows []models.Product
	if err := h.DB.Where("is_published = ?", true).Order("name asc").Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load products"))
	}
	out, err := h.productsJSON(rows)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load products"))
	}
	return c.JSON(http.StatusOK, out)
}

func (h Handler) GetPublicProduct(c echo.Context) error {
	var row models.Product
	if err := h.DB.Where("slug = ? AND is_published = ?", c.Param("slug"), true).First(&row).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("product not found"))
	}
	onHand, err := services.StockOnHand(h.DB, row.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load the product"))
	}
	return c.JSON(http.StatusOK, productJSON(row, onHand))
}

func (h Handler) AdminListProducts(c echo.Context) error {
	var rows []models.Product
	if err := h.DB.Order("name asc").Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load products"))
	}
	out, err := h.productsJSON(rows)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load products"))
	}
	return c.JSON(http.StatusOK, out)
}

func (h Handler) AdminCreateProduct(c echo.Context) error {
	var req struct {
		Name        string  `json:"name"`
		Description string  `json:"description"`
		Price       float64 `json:"price"`
		Stock       int     `json:"stock"`
		ImageURL    string  `json:"image_url"`
		Specs       string  `json:"specs"`
		IsPublished bool    `json:"is_published"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if strings.TrimSpace(req.Name) == "" {
		return c.JSON(http.StatusBadRequest, errResponse("product name is required"))
	}
	if req.Stock < 0 {
		return c.JSON(http.StatusBadRequest, errResponse("opening stock cannot be negative"))
	}
	product := models.Product{
		Name:        req.Name,
		Description: req.Description,
		Price:       req.Price,
		ImageURL:    req.ImageURL,
		Specs:       req.Specs,
		IsPublished: req.IsPublished,
		Slug:        strings.ToLower(strings.ReplaceAll(strings.TrimSpace(req.Name), " ", "-")),
	}
	if err := h.DB.Create(&product).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not create product"))
	}
	// Stock supplied at creation is an opening balance, not a column write. It
	// is the one legitimate use of the opening kind, and it goes through the
	// ledger like every other change.
	if req.Stock > 0 {
		opening := models.StockMovement{
			ProductID: product.ID, Kind: models.StockOpening, Quantity: req.Stock,
			Reason: "Opening balance set when the product was created",
		}
		if err := h.recordMovement(c, &opening); err != nil {
			return c.JSON(http.StatusInternalServerError, errResponse("the product was created, but its opening stock could not be recorded"))
		}
	}
	return c.JSON(http.StatusCreated, productJSON(product, req.Stock))
}

func (h Handler) AdminUpdateProduct(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid product id"))
	}
	var row models.Product
	if err := h.DB.First(&row, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("product not found"))
	}
	var req struct {
		Name        *string  `json:"name"`
		Description *string  `json:"description"`
		Price       *float64 `json:"price"`
		Stock       *int     `json:"stock"`
		ImageURL    *string  `json:"image_url"`
		Specs       *string  `json:"specs"`
		IsPublished *bool    `json:"is_published"`
		Slug        *string  `json:"slug"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid body"))
	}
	if req.Name != nil {
		row.Name = *req.Name
	}
	if req.Description != nil {
		row.Description = *req.Description
	}
	if req.Specs != nil {
		row.Specs = *req.Specs
	}
	if req.ImageURL != nil {
		row.ImageURL = *req.ImageURL
	}
	if req.Slug != nil {
		row.Slug = *req.Slug
	}
	if req.Price != nil {
		row.Price = *req.Price
	}
	if req.IsPublished != nil {
		row.IsPublished = *req.IsPublished
	}

	if err := h.DB.Save(&row).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update product"))
	}

	onHand, err := services.StockOnHand(h.DB, row.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not compute stock on hand"))
	}
	// A stock figure sent with a product edit is a stock count: someone has
	// looked at the shelf and is telling us what is actually there. It becomes
	// an adjustment movement rather than overwriting the balance, so the
	// correction carries an actor and a reason like every other change. A
	// figure that matches what the ledger already says is not a correction, so
	// it writes nothing.
	if req.Stock != nil && *req.Stock != onHand {
		delta := *req.Stock - onHand
		adjustment := models.StockMovement{
			ProductID: row.ID, Kind: models.StockAdjustment, Quantity: delta,
			Reason: fmt.Sprintf("Stock count: corrected from %d to %d", onHand, *req.Stock),
		}
		if reason := services.ValidateMovement(adjustment); reason != "" {
			return c.JSON(http.StatusBadRequest, errResponse(reason))
		}
		if err := h.recordMovement(c, &adjustment); err != nil {
			if errors.Is(err, services.ErrInsufficientStock) {
				return c.JSON(http.StatusConflict, errResponse(err.Error()))
			}
			return c.JSON(http.StatusInternalServerError, errResponse("could not record the stock adjustment"))
		}
		onHand = *req.Stock
	}
	return c.JSON(http.StatusOK, productJSON(row, onHand))
}

func (h Handler) AdminDeleteProduct(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid product id"))
	}
	if err := h.DB.Delete(&models.Product{}, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not delete product"))
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "product deleted"})
}

// --- Product images (upload + public serve) ---

// maxProductImageSize caps product image uploads at 5 MB.
const maxProductImageSize = 5 * 1024 * 1024

// allowedProductImageExts whitelists the image extensions the admin may upload
// for a product photo.
var allowedProductImageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true,
}

// UploadProductImage accepts a multipart/form-data image from an admin, stores
// it under an opaque server-generated key, and returns a path (relative to the
// API base) that ServeProductImage will stream publicly. The returned path is
// what the admin then saves as the product's image_url.
func (h Handler) UploadProductImage(c echo.Context) error {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("an image file is required"))
	}
	if fileHeader.Size <= 0 {
		return c.JSON(http.StatusBadRequest, errResponse("the uploaded image is empty"))
	}
	if fileHeader.Size > maxProductImageSize {
		return c.JSON(http.StatusBadRequest, errResponse("image too large — the maximum size is 5 MB"))
	}

	ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
	if !allowedProductImageExts[ext] {
		return c.JSON(http.StatusBadRequest, errResponse("unsupported image type — allowed: png, jpg, jpeg, webp, gif"))
	}

	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	src, err := fileHeader.Open()
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("could not read the uploaded image"))
	}
	defer src.Close()

	name := uuid.NewString() + ext
	key := "product-images/" + name
	if err := h.Store.Save(key, src, fileHeader.Size, contentType); err != nil {
		c.Logger().Errorf("product image storage failed (driver=%s dir=%s key=%s): %v", h.Cfg.StorageDriver, h.Cfg.StorageDir, key, err)
		return c.JSON(http.StatusInternalServerError, errResponse("could not store the uploaded image"))
	}

	// Path is relative to the API base ("/api/v1"); the frontend turns it into an
	// absolute URL before saving it as the product's image_url.
	return c.JSON(http.StatusCreated, map[string]string{"url": "/products/images/" + name})
}

// ServeProductImage streams a previously uploaded product image inline. It is a
// public route (product photos are shown on the public catalogue). The name is
// a server-generated uuid+ext; anything with path separators or a non-image
// extension is rejected before it reaches storage.
func (h Handler) ServeProductImage(c echo.Context) error {
	name := c.Param("name")
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return c.JSON(http.StatusBadRequest, errResponse("invalid image name"))
	}
	ext := strings.ToLower(filepath.Ext(name))
	if !allowedProductImageExts[ext] {
		return c.JSON(http.StatusNotFound, errResponse("image not found"))
	}
	rc, err := h.Store.Open("product-images/" + name)
	if err != nil {
		return c.JSON(http.StatusNotFound, errResponse("image not found"))
	}
	defer rc.Close()
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	// Immutable: keys are content-addressed by uuid, so the bytes never change.
	c.Response().Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	return c.Stream(http.StatusOK, contentType, rc)
}

// --- Gallery (public "our work" showcase) ---

// galleryCategories whitelists the groupings shown on the public site.
var galleryCategories = map[string]bool{
	"Electronics": true, "Fabrication": true, "Software": true,
	"Prototyping": true, "Installations": true, "Other": true,
}

// ListPublicGallery returns published gallery items for the public site,
// ordered by the admin-chosen position.
func (h Handler) ListPublicGallery(c echo.Context) error {
	var rows []models.GalleryItem
	if err := h.DB.Where("is_published = ?", true).
		Order("position asc, created_at desc").Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load gallery"))
	}
	return c.JSON(http.StatusOK, rows)
}

// AdminListGallery returns every item, published or not.
func (h Handler) AdminListGallery(c echo.Context) error {
	var rows []models.GalleryItem
	if err := h.DB.Order("position asc, created_at desc").Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load gallery"))
	}
	return c.JSON(http.StatusOK, rows)
}

type galleryRequest struct {
	Title       string `json:"title"`
	Caption     string `json:"caption"`
	Category    string `json:"category"`
	ImageURL    string `json:"image_url"`
	Position    *int   `json:"position"`
	IsPublished *bool  `json:"is_published"`
}

func (h Handler) AdminCreateGalleryItem(c echo.Context) error {
	var req galleryRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	req.Title = strings.TrimSpace(req.Title)
	req.ImageURL = strings.TrimSpace(req.ImageURL)
	if req.Title == "" {
		return c.JSON(http.StatusBadRequest, errResponse("a title is required"))
	}
	if req.ImageURL == "" {
		return c.JSON(http.StatusBadRequest, errResponse("upload an image first"))
	}
	category := strings.TrimSpace(req.Category)
	if category == "" {
		category = "Other"
	}
	if !galleryCategories[category] {
		return c.JSON(http.StatusBadRequest, errResponse("invalid category"))
	}

	item := models.GalleryItem{
		Title:       req.Title,
		Caption:     strings.TrimSpace(req.Caption),
		Category:    category,
		ImageURL:    req.ImageURL,
		IsPublished: true,
	}
	if req.Position != nil {
		item.Position = *req.Position
	} else {
		// Append to the end so new photos do not jump to the front.
		var maxPos struct{ Max *int }
		h.DB.Model(&models.GalleryItem{}).Select("MAX(position) as max").Scan(&maxPos)
		if maxPos.Max != nil {
			item.Position = *maxPos.Max + 1
		}
	}
	if req.IsPublished != nil {
		item.IsPublished = *req.IsPublished
	}
	if err := h.DB.Create(&item).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not create gallery item"))
	}
	return c.JSON(http.StatusCreated, item)
}

func (h Handler) AdminUpdateGalleryItem(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid gallery item id"))
	}
	var row models.GalleryItem
	if err := h.DB.First(&row, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("gallery item not found"))
	}
	var req galleryRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid body"))
	}
	updates := map[string]any{}
	if t := strings.TrimSpace(req.Title); t != "" {
		updates["title"] = t
	}
	if req.Caption != "" || req.Title != "" {
		updates["caption"] = strings.TrimSpace(req.Caption)
	}
	if cat := strings.TrimSpace(req.Category); cat != "" {
		if !galleryCategories[cat] {
			return c.JSON(http.StatusBadRequest, errResponse("invalid category"))
		}
		updates["category"] = cat
	}
	if u := strings.TrimSpace(req.ImageURL); u != "" {
		updates["image_url"] = u
	}
	if req.Position != nil {
		updates["position"] = *req.Position
	}
	if req.IsPublished != nil {
		updates["is_published"] = *req.IsPublished
	}
	if len(updates) == 0 {
		return c.JSON(http.StatusBadRequest, errResponse("nothing to update"))
	}
	if err := h.DB.Model(&row).Updates(updates).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update gallery item"))
	}
	h.DB.First(&row, "id = ?", id)
	return c.JSON(http.StatusOK, row)
}

func (h Handler) AdminDeleteGalleryItem(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid gallery item id"))
	}
	if err := h.DB.Delete(&models.GalleryItem{}, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not delete gallery item"))
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "gallery item deleted"})
}

// UploadGalleryImage stores a gallery photo and returns the API-relative path.
// Same limits and whitelist as product images.
func (h Handler) UploadGalleryImage(c echo.Context) error {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("an image file is required"))
	}
	if fileHeader.Size <= 0 {
		return c.JSON(http.StatusBadRequest, errResponse("the uploaded image is empty"))
	}
	if fileHeader.Size > maxProductImageSize {
		return c.JSON(http.StatusBadRequest, errResponse("image too large — the maximum size is 5 MB"))
	}
	ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
	if !allowedProductImageExts[ext] {
		return c.JSON(http.StatusBadRequest, errResponse("unsupported image type — allowed: png, jpg, jpeg, webp, gif"))
	}
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	src, err := fileHeader.Open()
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("could not read the uploaded image"))
	}
	defer src.Close()

	name := uuid.NewString() + ext
	key := "gallery-images/" + name
	if err := h.Store.Save(key, src, fileHeader.Size, contentType); err != nil {
		c.Logger().Errorf("gallery image storage failed (driver=%s dir=%s key=%s): %v", h.Cfg.StorageDriver, h.Cfg.StorageDir, key, err)
		return c.JSON(http.StatusInternalServerError, errResponse("could not store the uploaded image"))
	}
	return c.JSON(http.StatusCreated, map[string]string{"url": "/gallery/images/" + name})
}

// ServeGalleryImage streams a gallery photo inline. Public: these appear on the
// marketing site. Names are server-generated uuid+ext, so path traversal and
// non-image extensions are rejected outright.
func (h Handler) ServeGalleryImage(c echo.Context) error {
	name := c.Param("name")
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return c.JSON(http.StatusBadRequest, errResponse("invalid image name"))
	}
	ext := strings.ToLower(filepath.Ext(name))
	if !allowedProductImageExts[ext] {
		return c.JSON(http.StatusNotFound, errResponse("image not found"))
	}
	rc, err := h.Store.Open("gallery-images/" + name)
	if err != nil {
		return c.JSON(http.StatusNotFound, errResponse("image not found"))
	}
	defer rc.Close()
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Response().Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	return c.Stream(http.StatusOK, contentType, rc)
}

// --- Opportunities (B2B sales pipeline) ---

// validOpportunityStages / validOpportunityGrades whitelist the enum values a
// client may set. Anything else is rejected before it reaches the database.
var validOpportunityStages = map[models.OpportunityStage]bool{
	models.StageProspecting: true, models.StageQualified: true,
	models.StageProposal: true, models.StageNegotiation: true,
	models.StageWon: true, models.StageLost: true,
}

var validOpportunityGrades = map[models.OpportunityGrade]bool{
	models.GradeBronze: true, models.GradeSilver: true,
	models.GradeGold: true, models.GradePlatinum: true,
}

// validOpportunitySegments whitelists the ABM account-segment values.
var validOpportunitySegments = map[string]bool{
	"strategic": true, "growth": true, "standard": true,
}

// contactRequest is a buying-committee member as sent by the client, embedded in
// the opportunity create/update payloads.
type contactRequest struct {
	Name      string `json:"name"`
	Role      string `json:"role"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	IsPrimary bool   `json:"is_primary"`
}

// buildContacts converts client contact rows into models, dropping blank names.
func buildContacts(rows []contactRequest) []models.OpportunityContact {
	out := make([]models.OpportunityContact, 0, len(rows))
	for _, r := range rows {
		if strings.TrimSpace(r.Name) == "" {
			continue
		}
		out = append(out, models.OpportunityContact{
			Name:      strings.TrimSpace(r.Name),
			Role:      strings.TrimSpace(r.Role),
			Email:     strings.TrimSpace(r.Email),
			Phone:     strings.TrimSpace(r.Phone),
			IsPrimary: r.IsPrimary,
		})
	}
	return out
}

func contactsJSON(rows []models.OpportunityContact) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, map[string]any{
			"id": c.ID, "name": c.Name, "role": c.Role,
			"email": c.Email, "phone": c.Phone, "is_primary": c.IsPrimary,
		})
	}
	return out
}

// lineItemRequest is a priced line as sent by the client, embedded in the
// opportunity create/update payloads.
type lineItemRequest struct {
	Description string  `json:"description"`
	Quantity    float64 `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
}

// buildLineItems converts client line rows into models, dropping blank
// descriptions and defaulting a missing quantity to 1. Position preserves order.
func buildLineItems(rows []lineItemRequest) []models.OpportunityLineItem {
	out := make([]models.OpportunityLineItem, 0, len(rows))
	for _, r := range rows {
		if strings.TrimSpace(r.Description) == "" {
			continue
		}
		qty := r.Quantity
		if qty == 0 {
			qty = 1
		}
		out = append(out, models.OpportunityLineItem{
			Description: strings.TrimSpace(r.Description),
			Quantity:    qty,
			UnitPrice:   r.UnitPrice,
			Position:    len(out),
		})
	}
	return out
}

func lineItemsJSON(rows []models.OpportunityLineItem) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, li := range rows {
		out = append(out, map[string]any{
			"id": li.ID, "description": li.Description, "quantity": li.Quantity,
			"unit_price": li.UnitPrice, "line_total": li.Quantity * li.UnitPrice,
			"position": li.Position,
		})
	}
	return out
}

func lineItemsTotal(rows []models.OpportunityLineItem) float64 {
	var total float64
	for _, li := range rows {
		total += li.Quantity * li.UnitPrice
	}
	return total
}

// stageDefaultProbability is the default win-probability seeded when a deal
// enters a stage (used unless the caller sets an explicit probability).
func stageDefaultProbability(stage models.OpportunityStage) int {
	switch stage {
	case models.StageProspecting:
		return 10
	case models.StageQualified:
		return 30
	case models.StageProposal:
		return 50
	case models.StageNegotiation:
		return 70
	case models.StageWon:
		return 100
	case models.StageLost:
		return 0
	default:
		return 10
	}
}

func clampProbability(p int) int {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// opportunityJSON renders an Opportunity plus the derived weighted_value
// (deal_value × probability), so the forecast figure is authoritative server-side.
func opportunityJSON(o models.Opportunity) map[string]any {
	return map[string]any{
		"id":                o.ID,
		"created_at":        o.CreatedAt,
		"updated_at":        o.UpdatedAt,
		"name":              o.Name,
		"account_name":      o.AccountName,
		"contact_name":      o.ContactName,
		"contact_email":     o.ContactEmail,
		"sector":            o.Sector,
		"segment":           o.Segment,
		"stage":             o.Stage,
		"grade":             o.Grade,
		"deal_value":        o.DealValue,
		"probability":       o.Probability,
		"weighted_value":    o.DealValue * float64(o.Probability) / 100.0,
		"owner_id":          o.OwnerID,
		"source_quote_id":   o.SourceQuoteID,
		"expected_close_at": o.ExpectedCloseAt,
		"invoiced_at":       o.InvoicedAt,
		"apply_vat":         o.ApplyVat,
		"notes":             o.Notes,
		"contacts":          contactsJSON(o.Contacts),
		"line_items":        lineItemsJSON(o.LineItems),
		"line_items_total":  lineItemsTotal(o.LineItems),
		// What the client was billed, VAT included where it applies. Always
		// computed, never stored — a persisted total drifts as line items change.
		"invoiced_total": services.InvoicedTotal(o),
	}
}

// AdminListStaff returns active non-student users, used as the owner picker for
// opportunities. Password hashes are never included (publicUser omits them).
func (h Handler) AdminListStaff(c echo.Context) error {
	var users []models.User
	if err := h.DB.Where("role <> ? AND is_active = ?", models.RoleStudent, true).Order("full_name asc").Find(&users).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load staff"))
	}
	result := make([]map[string]any, 0, len(users))
	for _, u := range users {
		result = append(result, publicUser(u))
	}
	return c.JSON(http.StatusOK, result)
}

// scopeToCaller narrows a query to the caller's own records when their role only
// grants ScopeOwn on that resource. ownerColumn is the column holding the
// responsible user (e.g. "owner_id"). A ScopeNone caller never reaches a handler
// — RequirePermission rejects them first — but it is treated as deny-all here
// too so this is safe to call unconditionally.
func scopeToCaller(c echo.Context, q *gorm.DB, res authz.Resource, ownerColumn string) *gorm.DB {
	role, _ := c.Get("role").(models.Role)
	switch authz.ReadScope(role, res) {
	case authz.ScopeAll:
		return q
	case authz.ScopeOwn:
		return q.Where(ownerColumn+" = ?", c.Get("user_id"))
	default:
		return q.Where("1 = 0")
	}
}

func (h Handler) AdminListOpportunities(c echo.Context) error {
	var rows []models.Opportunity
	q := h.DB.Preload("Contacts").Preload("LineItems").Order("created_at desc")
	q = scopeToCaller(c, q, authz.ResOpportunities, "owner_id")
	if stage := c.QueryParam("stage"); stage != "" {
		q = q.Where("stage = ?", stage)
	}
	if err := q.Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load opportunities"))
	}
	out := make([]map[string]any, 0, len(rows))
	for _, o := range rows {
		out = append(out, opportunityJSON(o))
	}
	return c.JSON(http.StatusOK, out)
}

func (h Handler) AdminCreateOpportunity(c echo.Context) error {
	var req struct {
		Name            string            `json:"name"`
		AccountName     string            `json:"account_name"`
		ContactName     string            `json:"contact_name"`
		ContactEmail    string            `json:"contact_email"`
		Sector          string            `json:"sector"`
		Segment         string            `json:"segment"`
		Stage           string            `json:"stage"`
		Grade           string            `json:"grade"`
		DealValue       float64           `json:"deal_value"`
		Probability     *int              `json:"probability"`
		OwnerID         *uuid.UUID        `json:"owner_id"`
		SourceQuoteID   *uuid.UUID        `json:"source_quote_id"`
		ExpectedCloseAt *time.Time        `json:"expected_close_at"`
		Notes           string            `json:"notes"`
		Contacts        []contactRequest  `json:"contacts"`
		LineItems       []lineItemRequest `json:"line_items"`
		ApplyVat        bool              `json:"apply_vat"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if strings.TrimSpace(req.Name) == "" {
		return c.JSON(http.StatusBadRequest, errResponse("opportunity name is required"))
	}

	stage := models.OpportunityStage(req.Stage)
	if stage == "" {
		stage = models.StageProspecting
	}
	if !validOpportunityStages[stage] {
		return c.JSON(http.StatusBadRequest, errResponse("invalid stage"))
	}
	// A deal created directly at `won` would otherwise walk straight past the
	// close-won gate, since the gate lives on the stage transition. There is no
	// record for an approver to look at yet and no id to bind an approval to, so
	// the honest answer is to refuse the shortcut rather than invent an entity.
	if stage == models.StageWon {
		rule, err := h.matchRule(models.ApprovalDealCloseWon, req.DealValue)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, errResponse("could not check approval requirements"))
		}
		if rule != nil {
			return c.JSON(http.StatusConflict, errResponse(
				"a deal of this value cannot be created already Won — create it, then close it so the approval can be reviewed"))
		}
	}
	grade := models.OpportunityGrade(req.Grade)
	if grade == "" {
		grade = models.GradeBronze
	}
	if !validOpportunityGrades[grade] {
		return c.JSON(http.StatusBadRequest, errResponse("invalid grade"))
	}
	segment := strings.TrimSpace(req.Segment)
	if segment == "" {
		segment = "standard"
	}
	if !validOpportunitySegments[segment] {
		return c.JSON(http.StatusBadRequest, errResponse("invalid segment"))
	}

	probability := stageDefaultProbability(stage)
	if req.Probability != nil {
		probability = clampProbability(*req.Probability)
	}

	opp := models.Opportunity{
		Name:            strings.TrimSpace(req.Name),
		AccountName:     req.AccountName,
		ContactName:     req.ContactName,
		ContactEmail:    req.ContactEmail,
		Sector:          req.Sector,
		Segment:         segment,
		Stage:           stage,
		Grade:           grade,
		DealValue:       req.DealValue,
		Probability:     probability,
		SourceQuoteID:   req.SourceQuoteID,
		ExpectedCloseAt: req.ExpectedCloseAt,
		Notes:           req.Notes,
		Contacts:        buildContacts(req.Contacts),
		LineItems:       buildLineItems(req.LineItems),
		ApplyVat:        req.ApplyVat,
	}
	if req.OwnerID != nil && *req.OwnerID != uuid.Nil {
		opp.OwnerID = req.OwnerID
	}
	if err := h.DB.Create(&opp).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not create opportunity"))
	}
	return c.JSON(http.StatusCreated, opportunityJSON(opp))
}

func (h Handler) AdminUpdateOpportunity(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid opportunity id"))
	}
	var row models.Opportunity
	if err := h.DB.First(&row, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("opportunity not found"))
	}
	var req struct {
		Name            *string            `json:"name"`
		AccountName     *string            `json:"account_name"`
		ContactName     *string            `json:"contact_name"`
		ContactEmail    *string            `json:"contact_email"`
		Sector          *string            `json:"sector"`
		Stage           *string            `json:"stage"`
		Grade           *string            `json:"grade"`
		DealValue       *float64           `json:"deal_value"`
		Probability     *int               `json:"probability"`
		OwnerID         *uuid.UUID         `json:"owner_id"`
		ExpectedCloseAt *time.Time         `json:"expected_close_at"`
		Notes           *string            `json:"notes"`
		Segment         *string            `json:"segment"`
		Contacts        *[]contactRequest  `json:"contacts"`
		LineItems       *[]lineItemRequest `json:"line_items"`
		ApplyVat        *bool              `json:"apply_vat"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid body"))
	}

	updates := map[string]any{}
	if req.Name != nil {
		if strings.TrimSpace(*req.Name) == "" {
			return c.JSON(http.StatusBadRequest, errResponse("opportunity name cannot be empty"))
		}
		updates["name"] = strings.TrimSpace(*req.Name)
	}
	if req.AccountName != nil {
		updates["account_name"] = *req.AccountName
	}
	if req.ContactName != nil {
		updates["contact_name"] = *req.ContactName
	}
	if req.ContactEmail != nil {
		updates["contact_email"] = *req.ContactEmail
	}
	if req.Sector != nil {
		updates["sector"] = *req.Sector
	}
	if req.Segment != nil {
		segment := strings.TrimSpace(*req.Segment)
		if segment == "" {
			segment = "standard"
		}
		if !validOpportunitySegments[segment] {
			return c.JSON(http.StatusBadRequest, errResponse("invalid segment"))
		}
		updates["segment"] = segment
	}
	if req.DealValue != nil {
		updates["deal_value"] = *req.DealValue
	}
	if req.ExpectedCloseAt != nil {
		updates["expected_close_at"] = *req.ExpectedCloseAt
	}
	if req.Notes != nil {
		updates["notes"] = *req.Notes
	}
	// Whether the invoice carries VAT changes what the client owes, so it is
	// part of the deal rather than a per-view toggle.
	if req.ApplyVat != nil {
		updates["apply_vat"] = *req.ApplyVat
	}
	// owner_id: a nil UUID clears the owner (unassign); a real UUID assigns it.
	if req.OwnerID != nil {
		if *req.OwnerID == uuid.Nil {
			updates["owner_id"] = nil
		} else {
			updates["owner_id"] = *req.OwnerID
		}
	}
	if req.Grade != nil {
		grade := models.OpportunityGrade(*req.Grade)
		if !validOpportunityGrades[grade] {
			return c.JSON(http.StatusBadRequest, errResponse("invalid grade"))
		}
		updates["grade"] = grade
	}
	// Stage change re-seeds probability to that stage's default, unless the
	// caller also supplied an explicit probability in the same request.
	if req.Stage != nil {
		stage := models.OpportunityStage(*req.Stage)
		if !validOpportunityStages[stage] {
			return c.JSON(http.StatusBadRequest, errResponse("invalid stage"))
		}
		// Closing as Won is where revenue is recognised, so it is gated on value.
		// The amount is what the deal will be worth AFTER this request, not
		// before: raising the value and closing in one call would otherwise be
		// measured against the old figure and slip under the threshold.
		if stage == models.StageWon && row.Stage != models.StageWon {
			amount := row.DealValue
			if req.DealValue != nil {
				amount = *req.DealValue
			}
			blocked, err := h.gate(c, models.ApprovalDealCloseWon, models.ApprovalEntityOpportunity, row.ID, amount,
				fmt.Sprintf("Close %q as Won — %s", row.Name, zmw(amount)))
			if err != nil {
				return c.JSON(http.StatusInternalServerError, errResponse("could not check approval requirements"))
			}
			if blocked != nil {
				return h.blockedResponse(c, blocked)
			}
		}
		updates["stage"] = stage
		if req.Probability == nil {
			updates["probability"] = stageDefaultProbability(stage)
		}
	}
	if req.Probability != nil {
		updates["probability"] = clampProbability(*req.Probability)
	}

	if err := h.DB.Model(&row).Updates(updates).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update opportunity"))
	}
	// If the client sent a contacts array, replace the buying committee wholesale
	// (the modal always submits the full list).
	if req.Contacts != nil {
		h.DB.Where("opportunity_id = ?", id).Delete(&models.OpportunityContact{})
		fresh := buildContacts(*req.Contacts)
		for i := range fresh {
			fresh[i].OpportunityID = id
		}
		if len(fresh) > 0 {
			h.DB.Create(&fresh)
		}
	}
	// Same wholesale replacement for line items when the client sends them.
	if req.LineItems != nil {
		h.DB.Where("opportunity_id = ?", id).Delete(&models.OpportunityLineItem{})
		fresh := buildLineItems(*req.LineItems)
		for i := range fresh {
			fresh[i].OpportunityID = id
		}
		if len(fresh) > 0 {
			h.DB.Create(&fresh)
		}
	}
	h.DB.Preload("Contacts").Preload("LineItems").First(&row, "id = ?", id)
	return c.JSON(http.StatusOK, opportunityJSON(row))
}

func (h Handler) AdminDeleteOpportunity(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid opportunity id"))
	}
	// Loaded before the delete so the gate has a value to measure against the
	// threshold and a name to put in front of an approver — "delete deal
	// 8f3c…" is not a decision anyone can make responsibly. A missing row now
	// answers 404 rather than reporting a successful delete of nothing.
	var row models.Opportunity
	if err := h.DB.First(&row, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("opportunity not found"))
	}
	blocked, err := h.gate(c, models.ApprovalDealDelete, models.ApprovalEntityOpportunity, row.ID, row.DealValue,
		fmt.Sprintf("Delete deal %q — %s", row.Name, zmw(row.DealValue)))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not check approval requirements"))
	}
	if blocked != nil {
		return h.blockedResponse(c, blocked)
	}
	if err := h.DB.Delete(&models.Opportunity{}, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not delete opportunity"))
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "opportunity deleted"})
}

// AdminConvertQuote turns a lead (quote request) into a pipeline opportunity,
// carrying over the contact details, linking back via source_quote_id, and
// marking the quote converted. Converting the same quote twice creates two
// opportunities — the UI guards against that.
func (h Handler) AdminConvertQuote(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid quote id"))
	}
	var quote models.QuoteRequest
	if err := h.DB.First(&quote, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("quote request not found"))
	}

	name := strings.TrimSpace(quote.Service)
	if name == "" {
		name = "New opportunity"
	}
	account := strings.TrimSpace(quote.Company)
	if account == "" {
		account = quote.Name
	}
	opp := models.Opportunity{
		Name:          name,
		AccountName:   account,
		ContactName:   quote.Name,
		ContactEmail:  quote.Email,
		Stage:         models.StageProspecting,
		Grade:         models.GradeBronze,
		Probability:   stageDefaultProbability(models.StageProspecting),
		SourceQuoteID: &quote.ID,
		Notes:         quote.Message,
	}
	if err := h.DB.Create(&opp).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not create opportunity"))
	}
	// Mark the lead converted (best-effort; the opportunity already exists).
	h.DB.Model(&quote).Update("status", "converted")
	return c.JSON(http.StatusCreated, opportunityJSON(opp))
}

// --- Engagement log (deal activity timeline) ---

// validActivityTypes whitelists engagement-log entry types.
var validActivityTypes = map[string]bool{
	"call": true, "meeting": true, "email": true,
	"note": true, "task": true, "other": true,
}

func activityJSON(a models.OpportunityActivity) map[string]any {
	return map[string]any{
		"id":             a.ID,
		"created_at":     a.CreatedAt,
		"opportunity_id": a.OpportunityID,
		"actor_id":       a.ActorID,
		"actor_name":     a.ActorName,
		"actor_role":     a.ActorRole,
		"type":           a.Type,
		"body":           a.Body,
		"occurred_at":    a.OccurredAt,
	}
}

// AdminListActivities returns a deal's engagement log, most-recent engagement first.
func (h Handler) AdminListActivities(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid opportunity id"))
	}
	var rows []models.OpportunityActivity
	if err := h.DB.Where("opportunity_id = ?", id).Order("occurred_at desc, created_at desc").Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load activities"))
	}
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, activityJSON(a))
	}
	return c.JSON(http.StatusOK, out)
}

// AdminCreateActivity appends an attributed entry to a deal's engagement log.
// The entry is attributed to the acting admin; OccurredAt lets the caller
// back-date an engagement (e.g. a call logged after the fact).
func (h Handler) AdminCreateActivity(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid opportunity id"))
	}
	var opp models.Opportunity
	if err := h.DB.First(&opp, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("opportunity not found"))
	}
	var req struct {
		Type       string     `json:"type"`
		Body       string     `json:"body"`
		OccurredAt *time.Time `json:"occurred_at"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if strings.TrimSpace(req.Body) == "" {
		return c.JSON(http.StatusBadRequest, errResponse("an activity note is required"))
	}
	actType := strings.TrimSpace(req.Type)
	if actType == "" {
		actType = "note"
	}
	if !validActivityTypes[actType] {
		return c.JSON(http.StatusBadRequest, errResponse("invalid activity type"))
	}
	occurred := time.Now()
	if req.OccurredAt != nil {
		occurred = *req.OccurredAt
	}
	activity := models.OpportunityActivity{
		OpportunityID: id,
		Type:          actType,
		Body:          strings.TrimSpace(req.Body),
		OccurredAt:    occurred,
	}
	// Attribute to the acting admin (best-effort; the entry is still valid without).
	var actor models.User
	if err := h.DB.First(&actor, "id = ?", c.Get("user_id")).Error; err == nil {
		activity.ActorID = &actor.ID
		activity.ActorName = actor.FullName
		activity.ActorRole = actor.Role
	}
	if err := h.DB.Create(&activity).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not save activity"))
	}
	return c.JSON(http.StatusCreated, activityJSON(activity))
}

// --- Payments (basis for receipts and invoice balances) ---

var validPaymentMethods = map[string]bool{
	"cash": true, "bank_transfer": true, "mobile_money": true,
	"cheque": true, "card": true, "other": true,
}

func paymentJSON(p models.Payment) map[string]any {
	return map[string]any{
		"id":             p.ID,
		"created_at":     p.CreatedAt,
		"opportunity_id": p.OpportunityID,
		"amount":         p.Amount,
		"method":         p.Method,
		"reference":      p.Reference,
		"paid_at":        p.PaidAt,
		"note":           p.Note,
		"recorded_by_id": p.RecordedByID,
		"recorded_by":    p.RecordedBy,
	}
}

// AdminListPayments returns the payments recorded against a deal, newest first.
func (h Handler) AdminListPayments(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid opportunity id"))
	}
	var rows []models.Payment
	if err := h.DB.Where("opportunity_id = ?", id).Order("paid_at desc, created_at desc").Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load payments"))
	}
	out := make([]map[string]any, 0, len(rows))
	for _, p := range rows {
		out = append(out, paymentJSON(p))
	}
	return c.JSON(http.StatusOK, out)
}

// AdminCreatePayment records a payment received against a deal (the basis for a
// receipt). It records only — no funds are processed or transferred.
func (h Handler) AdminCreatePayment(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid opportunity id"))
	}
	var opp models.Opportunity
	if err := h.DB.First(&opp, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("opportunity not found"))
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
		return c.JSON(http.StatusBadRequest, errResponse("payment amount must be greater than zero"))
	}
	method := strings.TrimSpace(req.Method)
	if method == "" {
		method = "bank_transfer"
	}
	if !validPaymentMethods[method] {
		return c.JSON(http.StatusBadRequest, errResponse("invalid payment method"))
	}
	// A payment has no id until it exists, so the approval is bound to the deal
	// it lands against. That is also the record an approver needs to look at to
	// judge whether the receipt is plausible.
	blocked, gerr := h.gate(c, models.ApprovalPaymentRecord, models.ApprovalEntityOpportunity, opp.ID, req.Amount,
		fmt.Sprintf("Record a %s payment of %s against %q", method, zmw(req.Amount), opp.Name))
	if gerr != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not check approval requirements"))
	}
	if blocked != nil {
		return h.blockedResponse(c, blocked)
	}
	paidAt := time.Now()
	if req.PaidAt != nil {
		paidAt = *req.PaidAt
	}
	payment := models.Payment{
		OpportunityID: id,
		Amount:        req.Amount,
		Method:        method,
		Reference:     strings.TrimSpace(req.Reference),
		PaidAt:        paidAt,
		Note:          strings.TrimSpace(req.Note),
	}
	var actor models.User
	if err := h.DB.First(&actor, "id = ?", c.Get("user_id")).Error; err == nil {
		payment.RecordedByID = &actor.ID
		payment.RecordedBy = actor.FullName
	}
	if err := h.DB.Create(&payment).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not record payment"))
	}
	return c.JSON(http.StatusCreated, paymentJSON(payment))
}

// AdminDeletePayment removes a recorded payment (e.g. an entry error).
func (h Handler) AdminDeletePayment(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid payment id"))
	}
	if err := h.DB.Delete(&models.Payment{}, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not delete payment"))
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "payment deleted"})
}

// AdminPipelineForecast summarises the pipeline: open value, weighted forecast,
// per-stage counts/value, and win rate — the numbers behind the forecast board.
func (h Handler) AdminPipelineForecast(c echo.Context) error {
	var rows []models.Opportunity
	if err := h.DB.Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load pipeline"))
	}

	type stageSummary struct {
		Stage    models.OpportunityStage `json:"stage"`
		Count    int                     `json:"count"`
		Value    float64                 `json:"value"`
		Weighted float64                 `json:"weighted_value"`
	}
	order := []models.OpportunityStage{
		models.StageProspecting, models.StageQualified, models.StageProposal,
		models.StageNegotiation, models.StageWon, models.StageLost,
	}
	byStage := map[models.OpportunityStage]*stageSummary{}
	for _, s := range order {
		byStage[s] = &stageSummary{Stage: s}
	}

	var openValue, weightedForecast, wonValue float64
	var wonCount, lostCount int
	for _, o := range rows {
		s := byStage[o.Stage]
		if s == nil {
			continue
		}
		s.Count++
		s.Value += o.DealValue
		weighted := o.DealValue * float64(o.Probability) / 100.0
		s.Weighted += weighted
		switch o.Stage {
		case models.StageWon:
			wonValue += o.DealValue
			wonCount++
		case models.StageLost:
			lostCount++
		default:
			openValue += o.DealValue
			weightedForecast += weighted
		}
	}

	stages := make([]stageSummary, 0, len(order))
	for _, s := range order {
		stages = append(stages, *byStage[s])
	}
	winRate := 0.0
	if wonCount+lostCount > 0 {
		winRate = float64(wonCount) / float64(wonCount+lostCount) * 100.0
	}

	return c.JSON(http.StatusOK, map[string]any{
		"open_value":        openValue,
		"weighted_forecast": weightedForecast,
		"won_value":         wonValue,
		"won_count":         wonCount,
		"lost_count":        lostCount,
		"win_rate":          winRate,
		"total_count":       len(rows),
		"stages":            stages,
	})
}

// gradeRank orders maturity grades so the "best" grade per account can be found.
func gradeRank(g models.OpportunityGrade) int {
	switch g {
	case models.GradePlatinum:
		return 4
	case models.GradeGold:
		return 3
	case models.GradeSilver:
		return 2
	case models.GradeBronze:
		return 1
	default:
		return 0
	}
}

// AdminAccountsIndex derives, from the opportunity pipeline, a ranked view of
// accounts and of sectors (Vertical Sales Indexing). Values exclude lost deals;
// "open" is the live pipeline, "won" is closed-won. Both lists are ranked by
// total value (open + won) descending.
func (h Handler) AdminAccountsIndex(c echo.Context) error {
	var rows []models.Opportunity
	if err := h.DB.Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load accounts"))
	}

	type accountAgg struct {
		Account   string  `json:"account"`
		Sector    string  `json:"sector"`
		Segment   string  `json:"segment"`
		DealCount int     `json:"deal_count"`
		OpenCount int     `json:"open_count"`
		OpenValue float64 `json:"open_value"`
		Weighted  float64 `json:"weighted_value"`
		WonValue  float64 `json:"won_value"`
		Total     float64 `json:"total_value"`
		TopGrade  string  `json:"top_grade"`
		topRank   int
		segRank   int
	}
	segmentRank := map[string]int{"strategic": 3, "growth": 2, "standard": 1}
	type sectorAgg struct {
		Sector    string  `json:"sector"`
		Accounts  int     `json:"account_count"`
		DealCount int     `json:"deal_count"`
		OpenValue float64 `json:"open_value"`
		Weighted  float64 `json:"weighted_value"`
		WonValue  float64 `json:"won_value"`
		Total     float64 `json:"total_value"`
		accounts  map[string]bool
	}

	accounts := map[string]*accountAgg{}
	sectors := map[string]*sectorAgg{}

	for _, o := range rows {
		if o.Stage == models.StageLost {
			continue // lost deals don't count toward account/sector value
		}
		acctKey := strings.TrimSpace(o.AccountName)
		if acctKey == "" {
			acctKey = "Unattributed"
		}
		sectorKey := strings.TrimSpace(o.Sector)
		if sectorKey == "" {
			sectorKey = "Unspecified"
		}
		weighted := o.DealValue * float64(o.Probability) / 100.0

		a := accounts[acctKey]
		if a == nil {
			a = &accountAgg{Account: acctKey, Sector: sectorKey}
			accounts[acctKey] = a
		}
		if a.Sector == "Unspecified" && sectorKey != "Unspecified" {
			a.Sector = sectorKey
		}
		a.DealCount++
		if r := gradeRank(o.Grade); r > a.topRank {
			a.topRank = r
			a.TopGrade = string(o.Grade)
		}
		if seg := strings.TrimSpace(o.Segment); seg != "" {
			if r := segmentRank[seg]; r > a.segRank {
				a.segRank = r
				a.Segment = seg
			}
		}

		s := sectors[sectorKey]
		if s == nil {
			s = &sectorAgg{Sector: sectorKey, accounts: map[string]bool{}}
			sectors[sectorKey] = s
		}
		s.DealCount++
		s.accounts[acctKey] = true

		if o.Stage == models.StageWon {
			a.WonValue += o.DealValue
			s.WonValue += o.DealValue
		} else {
			a.OpenCount++
			a.OpenValue += o.DealValue
			a.Weighted += weighted
			s.OpenValue += o.DealValue
			s.Weighted += weighted
		}
	}

	accountList := make([]*accountAgg, 0, len(accounts))
	for _, a := range accounts {
		a.Total = a.OpenValue + a.WonValue
		accountList = append(accountList, a)
	}
	sort.Slice(accountList, func(i, j int) bool { return accountList[i].Total > accountList[j].Total })

	sectorList := make([]*sectorAgg, 0, len(sectors))
	for _, s := range sectors {
		s.Accounts = len(s.accounts)
		s.Total = s.OpenValue + s.WonValue
		sectorList = append(sectorList, s)
	}
	sort.Slice(sectorList, func(i, j int) bool { return sectorList[i].Total > sectorList[j].Total })

	return c.JSON(http.StatusOK, map[string]any{
		"accounts": accountList,
		"sectors":  sectorList,
	})
}

// --- Cross-sell / upsell recommendations ---

// segmentScore and gradeScore weight an account's strategic standing; a deeper
// relationship makes any recommendation more likely to land.
func segmentScore(segment string) int {
	switch segment {
	case "strategic":
		return 20
	case "growth":
		return 10
	default:
		return 0
	}
}

func gradeScore(g models.OpportunityGrade) int {
	switch g {
	case models.GradePlatinum:
		return 15
	case models.GradeGold:
		return 10
	case models.GradeSilver:
		return 5
	default:
		return 0
	}
}

// AdminAccountRecommendations ranks catalogue products as cross-sell/upsell
// candidates for one account. The ranking is a deterministic heuristic over the
// account's own pipeline (so it works with no AI key configured); the AI service
// only supplies the per-product rationale text when available.
func (h Handler) AdminAccountRecommendations(c echo.Context) error {
	account := strings.TrimSpace(c.Param("name"))
	if account == "" {
		return c.JSON(http.StatusBadRequest, errResponse("account name is required"))
	}

	var deals []models.Opportunity
	if err := h.DB.Where("LOWER(account_name) = ?", strings.ToLower(account)).Find(&deals).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load account deals"))
	}
	if len(deals) == 0 {
		return c.JSON(http.StatusNotFound, errResponse("no deals found for this account"))
	}

	var products []models.Product
	if err := h.DB.Where("is_published = ?", true).Order("name asc").Find(&products).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load products"))
	}
	stockLevels, err := services.StockLevels(h.DB)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load stock levels"))
	}

	// Summarise the account: strategic standing, spend, and what it already has.
	var sector, segment string
	var topGrade models.OpportunityGrade
	var topRank int
	var wonValue, maxWonValue, openValue float64
	var wonCount int
	segRank := 0
	segmentRanks := map[string]int{"strategic": 3, "growth": 2, "standard": 1}
	owned := map[string]bool{} // lowercased text of deals already won
	for _, d := range deals {
		if sector == "" {
			sector = strings.TrimSpace(d.Sector)
		}
		if r := segmentRanks[strings.TrimSpace(d.Segment)]; r > segRank {
			segRank = r
			segment = strings.TrimSpace(d.Segment)
		}
		if r := gradeRank(d.Grade); r > topRank {
			topRank = r
			topGrade = d.Grade
		}
		switch d.Stage {
		case models.StageWon:
			wonCount++
			wonValue += d.DealValue
			if d.DealValue > maxWonValue {
				maxWonValue = d.DealValue
			}
			owned[strings.ToLower(d.Name)] = true
		case models.StageLost:
			// lost deals tell us nothing about appetite
		default:
			openValue += d.DealValue
			owned[strings.ToLower(d.Name)] = true // already in play; don't re-pitch
		}
	}
	avgWon := 0.0
	if wonCount > 0 {
		avgWon = wonValue / float64(wonCount)
	}

	type recommendation struct {
		Slug        string   `json:"slug"`
		Name        string   `json:"name"`
		Price       float64  `json:"price"`
		ImageURL    string   `json:"image_url"`
		Kind        string   `json:"kind"` // cross_sell or upsell
		Probability int      `json:"probability"`
		Rationale   string   `json:"rationale"`
		Reasons     []string `json:"reasons"`
	}

	base := 35
	sectorKey := strings.ToLower(sector)
	recs := make([]recommendation, 0, len(products))
	for _, p := range products {
		// Skip anything this account already owns or is actively negotiating.
		if owned[strings.ToLower(p.Name)] {
			continue
		}
		score := base
		reasons := make([]string, 0, 4)

		if s := segmentScore(segment); s > 0 {
			score += s
			reasons = append(reasons, segment+" account")
		}
		if s := gradeScore(topGrade); s > 0 {
			score += s
			reasons = append(reasons, string(topGrade)+" grade relationship")
		}
		// Sector affinity: the catalogue text speaks to the account's vertical.
		if sectorKey != "" {
			haystack := strings.ToLower(p.Name + " " + p.Description + " " + p.Specs)
			if strings.Contains(haystack, sectorKey) {
				score += 15
				reasons = append(reasons, "matches "+sector+" sector")
			}
		}
		if wonCount > 0 {
			depth := wonCount * 5
			if depth > 15 {
				depth = 15
			}
			score += depth
			reasons = append(reasons, fmt.Sprintf("%d closed deal(s) already", wonCount))
		}
		// Affordability relative to prior spend.
		kind := "cross_sell"
		if maxWonValue > 0 && p.Price > maxWonValue {
			kind = "upsell"
		}
		if avgWon > 0 {
			switch {
			case p.Price <= avgWon:
				score += 10
				reasons = append(reasons, "within typical deal size")
			case p.Price > avgWon*3:
				score -= 10
				reasons = append(reasons, "well above typical deal size")
			}
		}
		if stockLevels[p.ID] <= 0 {
			score -= 10
			reasons = append(reasons, "out of stock")
		}

		if score < 5 {
			score = 5
		}
		if score > 95 {
			score = 95
		}
		recs = append(recs, recommendation{
			Slug: p.Slug, Name: p.Name, Price: p.Price, ImageURL: p.ImageURL,
			Kind: kind, Probability: score, Reasons: reasons,
		})
	}

	sort.Slice(recs, func(i, j int) bool { return recs[i].Probability > recs[j].Probability })
	if len(recs) > 6 {
		recs = recs[:6]
	}

	// Enrich the shortlist with AI rationales; falls back to the heuristic
	// reasons when no key is configured or the call fails.
	slugs := make([]string, 0, len(recs))
	for _, r := range recs {
		slugs = append(slugs, r.Slug)
	}
	accountContext := fmt.Sprintf(
		"Account: %s\nSector: %s\nSegment: %s\nTop grade: %s\nClosed-won deals: %d (total ZMW %.0f, average ZMW %.0f)\nOpen pipeline: ZMW %.0f\nExisting/in-flight engagements: %s",
		account, orDash(sector), orDash(segment), orDash(string(topGrade)),
		wonCount, wonValue, avgWon, openValue, strings.Join(dealNames(deals), "; "),
	)
	rationales, source := services.RecommendationRationales(h.Cfg, accountContext, slugs)
	for i := range recs {
		if r, ok := rationales[recs[i].Slug]; ok {
			recs[i].Rationale = r
		} else if len(recs[i].Reasons) > 0 {
			recs[i].Rationale = "Suggested because: " + strings.Join(recs[i].Reasons, ", ") + "."
		}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"account":         account,
		"sector":          sector,
		"segment":         segment,
		"top_grade":       topGrade,
		"won_count":       wonCount,
		"won_value":       wonValue,
		"open_value":      openValue,
		"source":          source,
		"recommendations": recs,
	})
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unspecified"
	}
	return s
}

// dealNames lists the account's deal names for the AI context.
func dealNames(deals []models.Opportunity) []string {
	out := make([]string, 0, len(deals))
	for _, d := range deals {
		if d.Stage == models.StageLost {
			continue
		}
		out = append(out, fmt.Sprintf("%s (%s)", d.Name, d.Stage))
	}
	return out
}

// --- Contracts ---

const maxContractSize = 15 * 1024 * 1024

// allowedContractExts whitelists document types that may be attached to a contract.
var allowedContractExts = map[string]bool{
	".pdf": true, ".doc": true, ".docx": true,
}

var validContractStatuses = map[string]bool{
	"draft": true, "sent": true, "signed": true, "active": true, "expired": true,
}

func contractJSON(ct models.Contract) map[string]any {
	return map[string]any{
		"id":             ct.ID,
		"created_at":     ct.CreatedAt,
		"opportunity_id": ct.OpportunityID,
		"account_name":   ct.AccountName,
		"title":          ct.Title,
		"status":         ct.Status,
		"value":          ct.Value,
		"start_date":     ct.StartDate,
		"renewal_date":   ct.RenewalDate,
		"notes":          ct.Notes,
		"file_name":       ct.FileName,
		"content_type":    ct.ContentType,
		"size":            ct.Size,
		"has_file":        ct.StoredKey != "",
		"file_hash":       ct.FileHash,
		"current_version": ct.CurrentVersion,
	}
}

func (h Handler) AdminListContracts(c echo.Context) error {
	var rows []models.Contract
	// Nulls-last ordering so contracts with a renewal date surface first.
	if err := h.DB.Order("renewal_date IS NULL, renewal_date asc, created_at desc").Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load contracts"))
	}
	out := make([]map[string]any, 0, len(rows))
	for _, ct := range rows {
		out = append(out, contractJSON(ct))
	}
	return c.JSON(http.StatusOK, out)
}

func (h Handler) AdminCreateContract(c echo.Context) error {
	var req struct {
		OpportunityID *uuid.UUID `json:"opportunity_id"`
		AccountName   string     `json:"account_name"`
		Title         string     `json:"title"`
		Status        string     `json:"status"`
		Value         float64    `json:"value"`
		StartDate     *time.Time `json:"start_date"`
		RenewalDate   *time.Time `json:"renewal_date"`
		Notes         string     `json:"notes"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if strings.TrimSpace(req.Title) == "" {
		return c.JSON(http.StatusBadRequest, errResponse("contract title is required"))
	}
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = "draft"
	}
	if !validContractStatuses[status] {
		return c.JSON(http.StatusBadRequest, errResponse("invalid status"))
	}
	ct := models.Contract{
		AccountName: req.AccountName,
		Title:       strings.TrimSpace(req.Title),
		Status:      status,
		Value:       req.Value,
		StartDate:   req.StartDate,
		RenewalDate: req.RenewalDate,
		Notes:       req.Notes,
	}
	if req.OpportunityID != nil && *req.OpportunityID != uuid.Nil {
		ct.OpportunityID = req.OpportunityID
	}
	if err := h.DB.Create(&ct).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not create contract"))
	}
	return c.JSON(http.StatusCreated, contractJSON(ct))
}

func (h Handler) AdminUpdateContract(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid contract id"))
	}
	var row models.Contract
	if err := h.DB.First(&row, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("contract not found"))
	}
	var req struct {
		OpportunityID *uuid.UUID `json:"opportunity_id"`
		AccountName   *string    `json:"account_name"`
		Title         *string    `json:"title"`
		Status        *string    `json:"status"`
		Value         *float64   `json:"value"`
		StartDate     *time.Time `json:"start_date"`
		RenewalDate   *time.Time `json:"renewal_date"`
		Notes         *string    `json:"notes"`
		ClearRenewal  bool       `json:"clear_renewal"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid body"))
	}
	updates := map[string]any{}
	if req.Title != nil {
		if strings.TrimSpace(*req.Title) == "" {
			return c.JSON(http.StatusBadRequest, errResponse("contract title cannot be empty"))
		}
		updates["title"] = strings.TrimSpace(*req.Title)
	}
	if req.AccountName != nil {
		updates["account_name"] = *req.AccountName
	}
	if req.OpportunityID != nil {
		if *req.OpportunityID == uuid.Nil {
			updates["opportunity_id"] = nil
		} else {
			updates["opportunity_id"] = *req.OpportunityID
		}
	}
	if req.Status != nil {
		if !validContractStatuses[*req.Status] {
			return c.JSON(http.StatusBadRequest, errResponse("invalid status"))
		}
		// `signed` is reachable only by actually signing. Allowing it here would
		// let a dropdown assert a signature that no ContractSignature backs, so
		// the status would stop meaning anything. Re-sending the value a signed
		// contract already has is fine — that is an edit of other fields.
		if *req.Status == "signed" && row.Status != "signed" {
			return c.JSON(http.StatusBadRequest, errResponse("a contract becomes signed by being signed, not by changing its status"))
		}
		updates["status"] = *req.Status
	}
	if req.Value != nil {
		updates["value"] = *req.Value
	}
	if req.StartDate != nil {
		updates["start_date"] = *req.StartDate
	}
	if req.ClearRenewal {
		updates["renewal_date"] = nil
	} else if req.RenewalDate != nil {
		updates["renewal_date"] = *req.RenewalDate
	}
	if req.Notes != nil {
		updates["notes"] = *req.Notes
	}
	if err := h.DB.Model(&row).Updates(updates).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update contract"))
	}
	h.DB.First(&row, "id = ?", id)
	return c.JSON(http.StatusOK, contractJSON(row))
}

func (h Handler) AdminDeleteContract(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid contract id"))
	}
	// See AdminDeleteOpportunity: the row is loaded so the gate has something to
	// measure and an approver has something to read.
	var ct models.Contract
	if err := h.DB.First(&ct, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("contract not found"))
	}
	blocked, err := h.gate(c, models.ApprovalContractDelete, models.ApprovalEntityContract, ct.ID, ct.Value,
		fmt.Sprintf("Delete contract %q — %s", ct.Title, zmw(ct.Value)))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not check approval requirements"))
	}
	if blocked != nil {
		return h.blockedResponse(c, blocked)
	}
	if err := h.DB.Delete(&models.Contract{}, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not delete contract"))
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "contract deleted"})
}

// UploadContractFile attaches (or replaces) the stored document for a contract.
func (h Handler) UploadContractFile(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid contract id"))
	}
	var ct models.Contract
	if err := h.DB.First(&ct, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("contract not found"))
	}
	fileHeader, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("a file is required"))
	}
	if fileHeader.Size <= 0 {
		return c.JSON(http.StatusBadRequest, errResponse("the uploaded file is empty"))
	}
	if fileHeader.Size > maxContractSize {
		return c.JSON(http.StatusBadRequest, errResponse("file too large — the maximum upload size is 15 MB"))
	}
	ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
	if !allowedContractExts[ext] {
		return c.JSON(http.StatusBadRequest, errResponse("unsupported file type — allowed: pdf, doc, docx"))
	}
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = fileHeader.Header.Get("Content-Type")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	src, err := fileHeader.Open()
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("could not read the uploaded file"))
	}
	defer src.Close()

	// A fresh key per upload: replacing a document must never overwrite the
	// bytes of the one it supersedes, because the old version stays downloadable
	// through its DocumentVersion row.
	key := "contracts/" + uuid.NewString() + ext
	fileHash, err := h.saveHashed(key, src, fileHeader.Size, contentType)
	if err != nil {
		c.Logger().Errorf("contract storage failed (driver=%s dir=%s key=%s): %v", h.Cfg.StorageDriver, h.Cfg.StorageDir, key, err)
		return c.JSON(http.StatusInternalServerError, errResponse("could not store the uploaded file"))
	}

	version := models.DocumentVersion{
		ParentType:  models.DocParentContract,
		ParentID:    ct.ID,
		Version:     h.nextDocumentVersion(models.DocParentContract, ct.ID),
		FileName:    filepath.Base(fileHeader.Filename),
		StoredKey:   key,
		ContentType: contentType,
		Size:        fileHeader.Size,
		FileHash:    fileHash,
		Note:        strings.TrimSpace(c.FormValue("note")),
	}
	var actor models.User
	if err := h.DB.First(&actor, "id = ?", c.Get("user_id")).Error; err == nil {
		version.UploadedByID = &actor.ID
		version.UploadedBy = actor.FullName
	}
	// The version row is the durable record, so it is written before the
	// contract's pointer moves. Failing here leaves the contract on its previous
	// file rather than pointing at an unversioned upload.
	if err := h.DB.Create(&version).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not record the document version"))
	}

	if err := h.DB.Model(&ct).Updates(map[string]any{
		"file_name": version.FileName, "stored_key": key,
		"content_type": contentType, "size": fileHeader.Size,
		"file_hash": fileHash, "current_version": version.Version,
	}).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not save the contract file"))
	}
	h.DB.First(&ct, "id = ?", id)
	return c.JSON(http.StatusOK, contractJSON(ct))
}

// AdminDownloadContract streams a contract's stored document as an attachment.
func (h Handler) AdminDownloadContract(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid contract id"))
	}
	var ct models.Contract
	if err := h.DB.First(&ct, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("contract not found"))
	}
	if ct.StoredKey == "" {
		return c.JSON(http.StatusNotFound, errResponse("this contract has no file attached"))
	}
	rc, err := h.Store.Open(ct.StoredKey)
	if err != nil {
		return c.JSON(http.StatusNotFound, errResponse("contract file not found"))
	}
	defer rc.Close()

	// nil version: this is the contract's current file rather than a specific
	// historical revision.
	h.recordDocumentAccess(c, models.DocParentContract, ct.ID, nil, "download")

	contentType := ct.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	filename := ct.FileName
	if filename == "" {
		filename = "contract"
	}
	c.Response().Header().Set(echo.HeaderContentDisposition, fmt.Sprintf("attachment; filename=%q", filename))
	return c.Stream(http.StatusOK, contentType, rc)
}

// --- Capstone progress reports & extension requests ---

// progressReportJSON renders a ProgressReport exactly per the API contract:
// the period_* dates are YYYY-MM-DD strings while timestamps stay RFC3339.
func progressReportJSON(pr models.ProgressReport) map[string]any {
	return map[string]any{
		"id":                  pr.ID,
		"created_at":          pr.CreatedAt,
		"student_profile_id":  pr.StudentProfileID,
		"period_start":        pr.PeriodStart.Format("2006-01-02"),
		"period_end":          pr.PeriodEnd.Format("2006-01-02"),
		"accomplishments":     pr.Accomplishments,
		"challenges":          pr.Challenges,
		"status":              pr.Status,
		"supervisor_feedback": pr.SupervisorFeedback,
		"reviewed_at":         pr.ReviewedAt,
	}
}

// extensionJSON renders an ExtensionRequest exactly per the API contract:
// requested_deadline is a YYYY-MM-DD string while timestamps stay RFC3339.
func extensionJSON(e models.ExtensionRequest) map[string]any {
	return map[string]any{
		"id":                 e.ID,
		"created_at":         e.CreatedAt,
		"student_profile_id": e.StudentProfileID,
		"extension_type":     e.ExtensionType,
		"requested_deadline": e.RequestedDeadline.Format("2006-01-02"),
		"reason":             e.Reason,
		"status":             e.Status,
		"decision_note":      e.DecisionNote,
		"decided_at":         e.DecidedAt,
	}
}

func progressReportsJSON(rows []models.ProgressReport) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, progressReportJSON(r))
	}
	return out
}

func extensionsJSON(rows []models.ExtensionRequest) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, e := range rows {
		out = append(out, extensionJSON(e))
	}
	return out
}

// CreateProgressReport lets a student submit a progress report against their
// OWN capstone. Server owns status/feedback/reviewed_at.
func (h Handler) CreateProgressReport(c echo.Context) error {
	var profile models.StudentProfile
	if err := h.DB.Where("user_id = ?", c.Get("user_id")).First(&profile).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("student profile not found"))
	}
	var req struct {
		PeriodStart     string `json:"period_start"`
		PeriodEnd       string `json:"period_end"`
		Accomplishments string `json:"accomplishments"`
		Challenges      string `json:"challenges"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if strings.TrimSpace(req.Accomplishments) == "" {
		return c.JSON(http.StatusBadRequest, errResponse("accomplishments are required"))
	}
	start, err := time.Parse("2006-01-02", req.PeriodStart)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("period_start must be a valid YYYY-MM-DD date"))
	}
	end, err := time.Parse("2006-01-02", req.PeriodEnd)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("period_end must be a valid YYYY-MM-DD date"))
	}
	report := models.ProgressReport{
		StudentProfileID: profile.ID,
		PeriodStart:      start,
		PeriodEnd:        end,
		Accomplishments:  req.Accomplishments,
		Challenges:       req.Challenges,
		Status:           "submitted",
	}
	if err := h.DB.Create(&report).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not submit progress report"))
	}
	return c.JSON(http.StatusCreated, progressReportJSON(report))
}

// CreateExtensionRequest lets a student request a deadline extension on their
// OWN capstone. Server owns status/decision_note/decided_at.
func (h Handler) CreateExtensionRequest(c echo.Context) error {
	var profile models.StudentProfile
	if err := h.DB.Where("user_id = ?", c.Get("user_id")).First(&profile).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("student profile not found"))
	}
	var req struct {
		ExtensionType     string `json:"extension_type"`
		RequestedDeadline string `json:"requested_deadline"`
		Reason            string `json:"reason"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if strings.TrimSpace(req.ExtensionType) == "" {
		return c.JSON(http.StatusBadRequest, errResponse("extension_type is required"))
	}
	if strings.TrimSpace(req.Reason) == "" {
		return c.JSON(http.StatusBadRequest, errResponse("reason is required"))
	}
	deadline, err := time.Parse("2006-01-02", req.RequestedDeadline)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("requested_deadline must be a valid YYYY-MM-DD date"))
	}
	ext := models.ExtensionRequest{
		StudentProfileID:  profile.ID,
		ExtensionType:     req.ExtensionType,
		RequestedDeadline: deadline,
		Reason:            req.Reason,
		Status:            "pending",
	}
	if err := h.DB.Create(&ext).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not submit extension request"))
	}
	return c.JSON(http.StatusCreated, extensionJSON(ext))
}

// AdminRespondProgressReport lets a mentor/admin set supervisor feedback and
// mark a report reviewed. Feedback uses pointer semantics so an empty string
// deliberately clears it (mirrors AdminUpdateMilestone).
func (h Handler) AdminRespondProgressReport(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid progress report id"))
	}
	var report models.ProgressReport
	if err := h.DB.First(&report, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("progress report not found"))
	}
	var req struct {
		SupervisorFeedback *string `json:"supervisor_feedback"`
		Status             string  `json:"status"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	updates := map[string]any{}
	if req.Status != "" {
		switch req.Status {
		case "submitted", "reviewed":
			updates["status"] = req.Status
			if req.Status == "reviewed" {
				now := time.Now()
				updates["reviewed_at"] = &now
			} else {
				updates["reviewed_at"] = nil
			}
		default:
			return c.JSON(http.StatusBadRequest, errResponse("status must be submitted or reviewed"))
		}
	}
	if req.SupervisorFeedback != nil {
		updates["supervisor_feedback"] = strings.TrimSpace(*req.SupervisorFeedback)
	}
	if len(updates) > 0 {
		if err := h.DB.Model(&report).Updates(updates).Error; err != nil {
			return c.JSON(http.StatusInternalServerError, errResponse("could not update progress report"))
		}
		h.DB.First(&report, "id = ?", id)
	}
	return c.JSON(http.StatusOK, progressReportJSON(report))
}

// AdminRespondExtension lets a mentor/admin approve or deny an extension request
// with a decision note. decision_note uses pointer semantics so an empty string
// deliberately clears it.
func (h Handler) AdminRespondExtension(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid extension request id"))
	}
	var ext models.ExtensionRequest
	if err := h.DB.First(&ext, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("extension request not found"))
	}
	var req struct {
		Status       string  `json:"status"`
		DecisionNote *string `json:"decision_note"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	updates := map[string]any{}
	if req.Status != "" {
		switch req.Status {
		case "pending", "approved", "denied":
			updates["status"] = req.Status
			if req.Status == "approved" || req.Status == "denied" {
				now := time.Now()
				updates["decided_at"] = &now
			} else {
				updates["decided_at"] = nil
			}
		default:
			return c.JSON(http.StatusBadRequest, errResponse("status must be pending, approved, or denied"))
		}
	}
	if req.DecisionNote != nil {
		updates["decision_note"] = strings.TrimSpace(*req.DecisionNote)
	}
	if len(updates) > 0 {
		if err := h.DB.Model(&ext).Updates(updates).Error; err != nil {
			return c.JSON(http.StatusInternalServerError, errResponse("could not update extension request"))
		}
		h.DB.First(&ext, "id = ?", id)
	}
	return c.JSON(http.StatusOK, extensionJSON(ext))
}

// --- Submissions (file uploads) ---

// maxSubmissionSize caps uploaded files at 15 MB.
const maxSubmissionSize = 15 * 1024 * 1024

// allowedSubmissionExts whitelists file extensions (lower-case, with the dot)
// that students may upload. Anything else is rejected with a clean message.
var allowedSubmissionExts = map[string]bool{
	".pdf": true, ".doc": true, ".docx": true, ".ppt": true, ".pptx": true,
	".xls": true, ".xlsx": true, ".png": true, ".jpg": true, ".jpeg": true,
	".zip": true, ".txt": true, ".md": true, ".csv": true,
}

// submissionJSON renders a Submission exactly per the API contract. The opaque
// storage key is never exposed; size is the byte count; reviewed_at is RFC3339
// or null.
func submissionJSON(s models.Submission) map[string]any {
	return map[string]any{
		"id":                 s.ID,
		"created_at":         s.CreatedAt,
		"student_profile_id": s.StudentProfileID,
		"title":              s.Title,
		"kind":               s.Kind,
		"file_name":          s.FileName,
		"content_type":       s.ContentType,
		"size":               s.Size,
		"status":             s.Status,
		"review_note":        s.ReviewNote,
		"reviewed_at":        s.ReviewedAt,
	}
}

func submissionsJSON(rows []models.Submission) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, s := range rows {
		out = append(out, submissionJSON(s))
	}
	return out
}

// submissionStageOrder is the gated submission pipeline: each step unlocks only
// once the previous step has a mentor-accepted submission.
var submissionStageOrder = []string{"proposal", "report", "final"}

// checkSubmissionGate enforces the Proposal → Report → Final ordering for a
// student's uploads. It returns (0, "") when the upload is allowed, otherwise an
// HTTP status code and a message to return to the client.
func (h Handler) checkSubmissionGate(profileID uuid.UUID, kind string) (int, string) {
	idx := -1
	for i, s := range submissionStageOrder {
		if s == kind {
			idx = i
			break
		}
	}
	if idx == -1 {
		return http.StatusBadRequest, "invalid submission type — must be proposal, report or final"
	}
	accepted := func(k string) bool {
		var n int64
		h.DB.Model(&models.Submission{}).
			Where("student_profile_id = ? AND kind = ? AND status = ?", profileID, k, "accepted").
			Count(&n)
		return n > 0
	}
	if accepted(kind) {
		return http.StatusConflict, "this step is already approved"
	}
	if idx > 0 && !accepted(submissionStageOrder[idx-1]) {
		return http.StatusForbidden, "complete and get approval for the previous step first"
	}
	return 0, ""
}

// CreateSubmission accepts a multipart/form-data upload from a student against
// their OWN profile. The file is stored under a server-generated opaque key;
// the client filename is kept for display only, never used as a key.
func (h Handler) CreateSubmission(c echo.Context) error {
	var profile models.StudentProfile
	if err := h.DB.Where("user_id = ?", c.Get("user_id")).First(&profile).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("student profile not found"))
	}
	title := strings.TrimSpace(c.FormValue("title"))
	if title == "" {
		return c.JSON(http.StatusBadRequest, errResponse("title is required"))
	}
	kind := strings.ToLower(strings.TrimSpace(c.FormValue("kind")))

	// Gated pipeline: submissions proceed Proposal → Report → Final. A step
	// unlocks only once the previous step has a mentor-accepted submission, and
	// an already-approved step cannot be re-submitted. Enforced here so the gate
	// cannot be bypassed by calling the API directly.
	if code, msg := h.checkSubmissionGate(profile.ID, kind); code != 0 {
		return c.JSON(code, errResponse(msg))
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("a file is required"))
	}
	if fileHeader.Size <= 0 {
		return c.JSON(http.StatusBadRequest, errResponse("the uploaded file is empty"))
	}
	if fileHeader.Size > maxSubmissionSize {
		return c.JSON(http.StatusBadRequest, errResponse("file too large — the maximum upload size is 15 MB"))
	}

	ext := strings.ToLower(filepath.Ext(fileHeader.Filename))
	if !allowedSubmissionExts[ext] {
		return c.JSON(http.StatusBadRequest, errResponse("unsupported file type — allowed: pdf, doc, docx, ppt, pptx, xls, xlsx, png, jpg, jpeg, zip, txt, md, csv"))
	}

	// Content type: trust the derived extension over the client-supplied header.
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = fileHeader.Header.Get("Content-Type")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	src, err := fileHeader.Open()
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("could not read the uploaded file"))
	}
	defer src.Close()

	key := "submissions/" + uuid.NewString() + ext
	fileHash, err := h.saveHashed(key, src, fileHeader.Size, contentType)
	if err != nil {
		c.Logger().Errorf("submission storage failed (driver=%s dir=%s key=%s): %v", h.Cfg.StorageDriver, h.Cfg.StorageDir, key, err)
		return c.JSON(http.StatusInternalServerError, errResponse("could not store the uploaded file"))
	}

	submission := models.Submission{
		StudentProfileID: profile.ID,
		Title:            title,
		Kind:             kind,
		FileName:         filepath.Base(fileHeader.Filename),
		StoredKey:        key,
		ContentType:      contentType,
		Size:             fileHeader.Size,
		FileHash:         fileHash,
		Status:           "submitted",
	}
	if err := h.DB.Create(&submission).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not save the submission"))
	}
	return c.JSON(http.StatusCreated, submissionJSON(submission))
}

// streamSubmission opens the stored object and streams it to the client as an
// attachment using the original display filename.
func (h Handler) streamSubmission(c echo.Context, s models.Submission) error {
	rc, err := h.Store.Open(s.StoredKey)
	if err != nil {
		return c.JSON(http.StatusNotFound, errResponse("submission file not found"))
	}
	defer rc.Close()

	// Both the student and the admin download route funnel through here, so one
	// call covers every read of a submission.
	h.recordDocumentAccess(c, models.DocParentSubmission, s.ID, nil, "download")

	contentType := s.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	filename := s.FileName
	if filename == "" {
		filename = "submission"
	}
	c.Response().Header().Set(echo.HeaderContentDisposition, fmt.Sprintf("attachment; filename=%q", filename))
	return c.Stream(http.StatusOK, contentType, rc)
}

// StudentDownloadSubmission streams a student's OWN submission file.
func (h Handler) StudentDownloadSubmission(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid submission id"))
	}
	var profile models.StudentProfile
	if err := h.DB.Where("user_id = ?", c.Get("user_id")).First(&profile).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("student profile not found"))
	}
	var submission models.Submission
	if err := h.DB.First(&submission, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("submission not found"))
	}
	// Ownership check: students may only download their own submissions.
	if submission.StudentProfileID != profile.ID {
		return c.JSON(http.StatusNotFound, errResponse("submission not found"))
	}
	return h.streamSubmission(c, submission)
}

// AdminDownloadSubmission streams any submission's file (no ownership check).
func (h Handler) AdminDownloadSubmission(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid submission id"))
	}
	var submission models.Submission
	if err := h.DB.First(&submission, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("submission not found"))
	}
	return h.streamSubmission(c, submission)
}

// AdminReviewSubmission lets a mentor/admin accept a submission or request a
// revision, with an optional review note. review_note uses pointer semantics so
// an empty string deliberately clears it (mirrors AdminRespondProgressReport).
func (h Handler) AdminReviewSubmission(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid submission id"))
	}
	var submission models.Submission
	if err := h.DB.First(&submission, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("submission not found"))
	}
	var req struct {
		Status     string  `json:"status"`
		ReviewNote *string `json:"review_note"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	updates := map[string]any{}
	if req.Status != "" {
		switch req.Status {
		case "submitted", "accepted", "revise":
			updates["status"] = req.Status
			if req.Status == "accepted" || req.Status == "revise" {
				now := time.Now()
				updates["reviewed_at"] = &now
			} else {
				updates["reviewed_at"] = nil
			}
		default:
			return c.JSON(http.StatusBadRequest, errResponse("status must be submitted, accepted, or revise"))
		}
	}
	if req.ReviewNote != nil {
		updates["review_note"] = strings.TrimSpace(*req.ReviewNote)
	}
	if len(updates) > 0 {
		if err := h.DB.Model(&submission).Updates(updates).Error; err != nil {
			return c.JSON(http.StatusInternalServerError, errResponse("could not update submission"))
		}
		h.DB.First(&submission, "id = ?", id)
	}
	return c.JSON(http.StatusOK, submissionJSON(submission))
}

// --- Audit logging ---

func auditLogJSON(a models.AuditLog) map[string]any {
	return map[string]any{
		"id":         a.ID,
		"created_at": a.CreatedAt,
		"actor_id":   a.ActorID,
		"actor_name": a.ActorName,
		"actor_role": a.ActorRole,
		"action":     a.Action,
		"entity":     a.Entity,
		"entity_id":  a.EntityID,
		"method":     a.Method,
		"path":       a.Path,
		"status":     a.Status,
	}
}

// auditAction maps an HTTP method (and a few special sub-paths) to a verb.
func auditAction(method, routePath string) string {
	switch method {
	case http.MethodPost:
		switch {
		case strings.HasSuffix(routePath, "/convert"):
			return "convert"
		case strings.HasSuffix(routePath, "/file"), strings.HasSuffix(routePath, "/image"):
			return "upload"
		case strings.HasSuffix(routePath, "/broadcast"):
			return "broadcast"
		case strings.HasSuffix(routePath, "/invite"):
			return "invite"
		case strings.HasSuffix(routePath, "/activities"):
			return "log"
		case strings.HasSuffix(routePath, "/resubmit"):
			return "resubmit"
		default:
			return "create"
		}
	case http.MethodPut, http.MethodPatch:
		switch {
		case strings.HasSuffix(routePath, "/approve"):
			return "approve"
		// Without this a rejection would be recorded as a generic "update",
		// making the two halves of a maker-checker decision indistinguishable in
		// the audit trail — which is the one place they most need to be legible.
		case strings.HasSuffix(routePath, "/reject"):
			return "reject"
		}
		return "update"
	case http.MethodDelete:
		return "delete"
	}
	return "other"
}

// auditEntity extracts the first path segment after the admin prefix from the
// matched route pattern (e.g. "/api/v1/admin/opportunities/:id" → "opportunities").
func auditEntity(routePath string) string {
	p := strings.TrimPrefix(routePath, "/api/v1/admin/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	return p
}

// AuditMutations is admin-group middleware that records every successful
// mutating request (POST/PUT/PATCH/DELETE) as an immutable AuditLog row. It runs
// after the handler so the HTTP status is known, snapshots the acting user, and
// never fails the request if the audit write itself errors.
func (h Handler) AuditMutations(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		err := next(c)

		method := c.Request().Method
		switch method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			return err
		}
		// Only record successful mutations; validation/auth failures are noise.
		if err != nil || c.Response().Status >= 400 {
			return err
		}

		routePath := c.Path()
		entry := models.AuditLog{
			Action:   auditAction(method, routePath),
			Entity:   auditEntity(routePath),
			EntityID: c.Param("id"),
			Method:   method,
			Path:     c.Request().URL.Path,
			Status:   c.Response().Status,
		}
		if idRaw, ok := c.Get("user_id").(string); ok && idRaw != "" {
			var actor models.User
			if e := h.DB.First(&actor, "id = ?", idRaw).Error; e == nil {
				entry.ActorID = &actor.ID
				entry.ActorName = actor.FullName
				entry.ActorRole = actor.Role
			}
		}
		if e := h.DB.Create(&entry).Error; e != nil {
			c.Logger().Errorf("audit log write failed (entity=%s action=%s): %v", entry.Entity, entry.Action, e)
		}
		return err
	}
}

// AdminListAuditLogs returns the most recent audit entries, newest first,
// optionally filtered by entity and/or action.
func (h Handler) AdminListAuditLogs(c echo.Context) error {
	q := h.DB.Order("created_at desc")
	if entity := c.QueryParam("entity"); entity != "" {
		q = q.Where("entity = ?", entity)
	}
	if action := c.QueryParam("action"); action != "" {
		q = q.Where("action = ?", action)
	}
	var rows []models.AuditLog
	if err := q.Limit(250).Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load audit logs"))
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, auditLogJSON(r))
	}
	return c.JSON(http.StatusOK, out)
}

// --- Role Management Handlers ---

func (h Handler) ListRoles(c echo.Context) error {
	var roles []models.CustomRole
	if err := h.DB.Preload("Permissions").Order("is_built_in desc, name asc").Find(&roles).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load roles"))
	}

	type roleCount struct {
		Role  string
		Count int64
	}
	var counts []roleCount
	h.DB.Model(&models.User{}).Where("deleted_at IS NULL").Select("role, count(*) as count").Group("role").Scan(&counts)
	userCounts := make(map[string]int64)
	for _, c := range counts {
		userCounts[c.Role] = c.Count
	}

	out := make([]map[string]any, 0, len(roles))
	for _, r := range roles {
		perms := make([]map[string]any, 0, len(r.Permissions))
		for _, p := range r.Permissions {
			perms = append(perms, map[string]any{
				"id":         p.ID,
				"resource":   p.Resource,
				"can_read":   p.CanRead,
				"can_create": p.CanCreate,
				"can_update": p.CanUpdate,
				"can_delete": p.CanDelete,
				"scope":      p.Scope,
			})
		}
		out = append(out, map[string]any{
			"id":          r.ID,
			"name":        r.Name,
			"label":       r.Label,
			"is_built_in": r.IsBuiltIn,
			"description": r.Description,
			"permissions": perms,
			"user_count":  userCounts[r.Name],
		})
	}
	return c.JSON(http.StatusOK, out)
}

type rolePermissionReq struct {
	Resource  string `json:"resource"`
	CanRead   bool   `json:"can_read"`
	CanCreate bool   `json:"can_create"`
	CanUpdate bool   `json:"can_update"`
	CanDelete bool   `json:"can_delete"`
	Scope     string `json:"scope"`
}

type createRoleReq struct {
	Name        string              `json:"name"`
	Label       string              `json:"label"`
	Description string              `json:"description"`
	Permissions []rolePermissionReq `json:"permissions"`
}

var slugRegex = regexp.MustCompile(`^[a-z0-9_]+$`)

func (h Handler) CreateRole(c echo.Context) error {
	var req createRoleReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	req.Name = strings.ToLower(strings.TrimSpace(req.Name))
	req.Label = strings.TrimSpace(req.Label)
	req.Description = strings.TrimSpace(req.Description)

	if req.Name == "" || !slugRegex.MatchString(req.Name) {
		return c.JSON(http.StatusBadRequest, errResponse("role name must contain only lowercase letters, numbers, and underscores"))
	}
	if req.Label == "" {
		return c.JSON(http.StatusBadRequest, errResponse("role label is required"))
	}

	var existing int64
	h.DB.Model(&models.CustomRole{}).Where("name = ?", req.Name).Count(&existing)
	if existing > 0 {
		return c.JSON(http.StatusConflict, errResponse("a role with that name already exists"))
	}

	callerRole, _ := c.Get("role").(models.Role)

	// Validate resources, scopes, and anti-amplification rules
	validResources := make(map[string]bool)
	for _, res := range authz.AllResources {
		validResources[string(res)] = true
	}

	permissionsToCreate := make([]models.CustomRolePermission, 0, len(req.Permissions))
	seenResources := make(map[string]bool)

	for _, p := range req.Permissions {
		if !validResources[p.Resource] {
			return c.JSON(http.StatusBadRequest, errResponse(fmt.Sprintf("invalid resource %q", p.Resource)))
		}
		if seenResources[p.Resource] {
			return c.JSON(http.StatusBadRequest, errResponse(fmt.Sprintf("duplicate permission entry for resource %q", p.Resource)))
		}
		seenResources[p.Resource] = true

		if p.Scope != string(authz.ScopeNone) && p.Scope != string(authz.ScopeOwn) && p.Scope != string(authz.ScopeAll) {
			return c.JSON(http.StatusBadRequest, errResponse(fmt.Sprintf("invalid scope %q for resource %q", p.Scope, p.Resource)))
		}

		hasAnyAction := p.CanRead || p.CanCreate || p.CanUpdate || p.CanDelete
		if hasAnyAction && p.Scope == string(authz.ScopeNone) {
			return c.JSON(http.StatusBadRequest, errResponse(fmt.Sprintf("scope cannot be 'none' when actions are enabled on %s", p.Resource)))
		}
		if !hasAnyAction {
			p.Scope = string(authz.ScopeNone)
		}

		res := authz.Resource(p.Resource)
		if p.CanRead && !authz.Can(callerRole, res, authz.ActionRead) {
			return c.JSON(http.StatusForbidden, errResponse(fmt.Sprintf("cannot grant permission you do not hold: read on %s", p.Resource)))
		}
		if p.CanCreate && !authz.Can(callerRole, res, authz.ActionCreate) {
			return c.JSON(http.StatusForbidden, errResponse(fmt.Sprintf("cannot grant permission you do not hold: create on %s", p.Resource)))
		}
		if p.CanUpdate && !authz.Can(callerRole, res, authz.ActionUpdate) {
			return c.JSON(http.StatusForbidden, errResponse(fmt.Sprintf("cannot grant permission you do not hold: update on %s", p.Resource)))
		}
		if p.CanDelete && !authz.Can(callerRole, res, authz.ActionDelete) {
			return c.JSON(http.StatusForbidden, errResponse(fmt.Sprintf("cannot grant permission you do not hold: delete on %s", p.Resource)))
		}

		permissionsToCreate = append(permissionsToCreate, models.CustomRolePermission{
			Resource:  p.Resource,
			CanRead:   p.CanRead,
			CanCreate: p.CanCreate,
			CanUpdate: p.CanUpdate,
			CanDelete: p.CanDelete,
			Scope:     p.Scope,
		})
	}

	role := models.CustomRole{
		Name:        req.Name,
		Label:       req.Label,
		Description: req.Description,
		IsBuiltIn:   false,
	}

	err := h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&role).Error; err != nil {
			return err
		}
		for i := range permissionsToCreate {
			permissionsToCreate[i].RoleID = role.ID
			if err := tx.Create(&permissionsToCreate[i]).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not create role"))
	}

	authz.RefreshCache(h.DB)
	role.Permissions = permissionsToCreate
	return c.JSON(http.StatusCreated, role)
}

type updateRoleReq struct {
	Label       *string             `json:"label"`
	Description *string             `json:"description"`
	Permissions []rolePermissionReq `json:"permissions"`
}

func (h Handler) UpdateRole(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid role id"))
	}
	var role models.CustomRole
	if err := h.DB.Preload("Permissions").First(&role, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("role not found"))
	}

	var req updateRoleReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}

	// Built-in role GRANTS are immutable, not just super_admin's. seed.Roles()
	// rewrites them from authz.BuiltInGrants on every boot, so accepting an edit
	// here would silently revert on the next deploy — and would break the
	// guarantee that seeded roles match the tested specification. Label and
	// description remain editable; to vary permissions, create a custom role.
	if role.IsBuiltIn && req.Permissions != nil {
		return c.JSON(http.StatusForbidden, errResponse(
			"built-in role permissions cannot be modified — create a custom role instead"))
	}

	callerRole, _ := c.Get("role").(models.Role)
	updates := map[string]any{}

	if req.Label != nil {
		trimmed := strings.TrimSpace(*req.Label)
		if trimmed == "" {
			return c.JSON(http.StatusBadRequest, errResponse("role label cannot be empty"))
		}
		updates["label"] = trimmed
	}
	if req.Description != nil {
		updates["description"] = strings.TrimSpace(*req.Description)
	}

	var newPermissions []models.CustomRolePermission
	if req.Permissions != nil {
		validResources := make(map[string]bool)
		for _, res := range authz.AllResources {
			validResources[string(res)] = true
		}

		seenResources := make(map[string]bool)
		for _, p := range req.Permissions {
			if !validResources[p.Resource] {
				return c.JSON(http.StatusBadRequest, errResponse(fmt.Sprintf("invalid resource %q", p.Resource)))
			}
			if seenResources[p.Resource] {
				return c.JSON(http.StatusBadRequest, errResponse(fmt.Sprintf("duplicate permission entry for resource %q", p.Resource)))
			}
			seenResources[p.Resource] = true

			if p.Scope != string(authz.ScopeNone) && p.Scope != string(authz.ScopeOwn) && p.Scope != string(authz.ScopeAll) {
				return c.JSON(http.StatusBadRequest, errResponse(fmt.Sprintf("invalid scope %q for resource %q", p.Scope, p.Resource)))
			}

			hasAnyAction := p.CanRead || p.CanCreate || p.CanUpdate || p.CanDelete
			if hasAnyAction && p.Scope == string(authz.ScopeNone) {
				return c.JSON(http.StatusBadRequest, errResponse(fmt.Sprintf("scope cannot be 'none' when actions are enabled on %s", p.Resource)))
			}
			if !hasAnyAction {
				p.Scope = string(authz.ScopeNone)
			}

			res := authz.Resource(p.Resource)
			if p.CanRead && !authz.Can(callerRole, res, authz.ActionRead) {
				return c.JSON(http.StatusForbidden, errResponse(fmt.Sprintf("cannot grant permission you do not hold: read on %s", p.Resource)))
			}
			if p.CanCreate && !authz.Can(callerRole, res, authz.ActionCreate) {
				return c.JSON(http.StatusForbidden, errResponse(fmt.Sprintf("cannot grant permission you do not hold: create on %s", p.Resource)))
			}
			if p.CanUpdate && !authz.Can(callerRole, res, authz.ActionUpdate) {
				return c.JSON(http.StatusForbidden, errResponse(fmt.Sprintf("cannot grant permission you do not hold: update on %s", p.Resource)))
			}
			if p.CanDelete && !authz.Can(callerRole, res, authz.ActionDelete) {
				return c.JSON(http.StatusForbidden, errResponse(fmt.Sprintf("cannot grant permission you do not hold: delete on %s", p.Resource)))
			}

			newPermissions = append(newPermissions, models.CustomRolePermission{
				RoleID:    role.ID,
				Resource:  p.Resource,
				CanRead:   p.CanRead,
				CanCreate: p.CanCreate,
				CanUpdate: p.CanUpdate,
				CanDelete: p.CanDelete,
				Scope:     p.Scope,
			})
		}
	}

	err = h.DB.Transaction(func(tx *gorm.DB) error {
		if len(updates) > 0 {
			if err := tx.Model(&role).Updates(updates).Error; err != nil {
				return err
			}
		}
		if req.Permissions != nil {
			// Unscoped delete to clear old permissions completely and avoid soft-delete unique index collisions
			if err := tx.Unscoped().Where("role_id = ?", role.ID).Delete(&models.CustomRolePermission{}).Error; err != nil {
				return err
			}
			for i := range newPermissions {
				if err := tx.Create(&newPermissions[i]).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update role"))
	}

	authz.RefreshCache(h.DB)
	h.DB.Preload("Permissions").First(&role, "id = ?", id)
	return c.JSON(http.StatusOK, role)
}

func (h Handler) DeleteRole(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid role id"))
	}
	var role models.CustomRole
	if err := h.DB.First(&role, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("role not found"))
	}

	if role.IsBuiltIn {
		return c.JSON(http.StatusBadRequest, errResponse("built-in roles cannot be deleted"))
	}

	var userCount int64
	h.DB.Model(&models.User{}).Where("role = ? AND deleted_at IS NULL", role.Name).Count(&userCount)
	if userCount > 0 {
		return c.JSON(http.StatusConflict, errResponse(fmt.Sprintf("cannot delete role — it is currently assigned to %d active user(s)", userCount)))
	}

	err = h.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Where("role_id = ?", role.ID).Delete(&models.CustomRolePermission{}).Error; err != nil {
			return err
		}
		return tx.Unscoped().Delete(&role).Error
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not delete role"))
	}

	authz.RefreshCache(h.DB)
	return c.JSON(http.StatusOK, map[string]string{"message": "role deleted"})
}

func errResponse(message string) map[string]string { return map[string]string{"error": message} }

func publicUser(user models.User) map[string]any {
	return map[string]any{
		"id": user.ID, "email": user.Email, "full_name": user.FullName,
		"role": user.Role, "is_active": user.IsActive, "student_profile": user.StudentProfile,
	}
}
