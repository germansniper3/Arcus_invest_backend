package handlers

import (
	"arcusinvest/internal/config"
	"arcusinvest/internal/models"
	"arcusinvest/internal/services"
	"arcusinvest/internal/storage"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
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
	return c.JSON(http.StatusOK, map[string]any{"token": token, "user": publicUser(user)})
}

func (h Handler) Me(c echo.Context) error {
	var user models.User
	if err := h.DB.Preload("StudentProfile").First(&user, "id = ?", c.Get("user_id")).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("user not found"))
	}
	return c.JSON(http.StatusOK, publicUser(user))
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
	if req.Status != ""        { updates["status"] = req.Status }
	if req.Notes != ""         { updates["notes"] = req.Notes }
	if req.Tier != ""          { updates["tier"] = req.Tier }
	if req.OrientationAt != nil { updates["orientation_at"] = req.OrientationAt }
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
	return c.JSON(http.StatusCreated, map[string]any{
		"invitation": invite,
		"claim_url":  claimURL,
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
		"email":     invite.Email,
		"full_name": enrollment.FullName,
		"tier":      enrollment.Tier,
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
	if len(req.Password) < 10 {
		return c.JSON(http.StatusBadRequest, errResponse("password must be at least 10 characters"))
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
		if id != uuid.Nil { res.UserID = &id }
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
	if req.Title != nil { event.Title = *req.Title }
	if req.Description != nil { event.Description = *req.Description }
	if req.Location != nil { event.Location = *req.Location }
	if req.ImageURL != nil { event.ImageURL = *req.ImageURL }
	if req.Slug != nil { event.Slug = *req.Slug }
	if req.Capacity != nil { event.Capacity = *req.Capacity }
	if req.IsPublished != nil { event.IsPublished = *req.IsPublished }
	if req.Date != nil && *req.Date != "" {
		parsed, err := time.Parse("2006-01-02T15:04", *req.Date)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, *req.Date)
		}
		if err == nil { event.Date = parsed }
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
	if h.Cfg.SMTPConfigured() && len(recipients) > 0 {
		if sendErr = services.SendBroadcastEmail(h.Cfg, recipients, req.Subject, req.Message); sendErr == nil {
			now := time.Now().UTC()
			broadcast.Status = "sent"
			broadcast.SentAt = &now
			h.DB.Model(&broadcast).Updates(map[string]any{"status": "sent", "sent_at": &now})
			sent = true
		} else {
			// Keep the stored broadcast as 'queued' but make the failure diagnosable.
			c.Logger().Errorf("broadcast %s: SMTP send failed: %v", broadcast.ID, sendErr)
		}
	}

	message := "broadcast stored and queued — email not sent (SMTP not configured)"
	if sent {
		message = "broadcast sent"
	} else if len(recipients) == 0 {
		message = "broadcast stored — no confirmed recipients to email"
	} else if h.Cfg.SMTPConfigured() {
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
		if id != uuid.Nil { msg.UserID = &id }
	}
	_ = h.DB.Create(&msg).Error
	return c.JSON(http.StatusOK, msg)
}

// --- Admin User Management ---

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
	if len(req.Password) < 10 {
		return c.JSON(http.StatusBadRequest, errResponse("password must be at least 10 characters"))
	}
	// Whitelist roles. Students are created only through invitation claims.
	role := req.Role
	if role == "" {
		role = models.RoleAdmissions
	}
	switch role {
	case models.RoleAdmin, models.RoleAdmissions, models.RoleSuperAdmin:
		// allowed
	default:
		return c.JSON(http.StatusBadRequest, errResponse("invalid role"))
	}
	// Only a super admin may mint another super admin.
	if role == models.RoleSuperAdmin {
		callerRole, _ := c.Get("role").(models.Role)
		if callerRole != models.RoleSuperAdmin {
			return c.JSON(http.StatusForbidden, errResponse("only a super admin can create a super admin"))
		}
	}
	// Reject duplicate emails with a clean message instead of leaking the raw
	// DB constraint error.
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
	return c.JSON(http.StatusOK, rows)
}

func (h Handler) GetPublicProduct(c echo.Context) error {
	var row models.Product
	if err := h.DB.Where("slug = ? AND is_published = ?", c.Param("slug"), true).First(&row).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("product not found"))
	}
	return c.JSON(http.StatusOK, row)
}

func (h Handler) AdminListProducts(c echo.Context) error {
	var rows []models.Product
	if err := h.DB.Order("name asc").Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load products"))
	}
	return c.JSON(http.StatusOK, rows)
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
	product := models.Product{
		Name:        req.Name,
		Description: req.Description,
		Price:       req.Price,
		Stock:       req.Stock,
		ImageURL:    req.ImageURL,
		Specs:       req.Specs,
		IsPublished: req.IsPublished,
		Slug:        strings.ToLower(strings.ReplaceAll(strings.TrimSpace(req.Name), " ", "-")),
	}
	if err := h.DB.Create(&product).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not create product"))
	}
	return c.JSON(http.StatusCreated, product)
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
	if req.Name != nil { row.Name = *req.Name }
	if req.Description != nil { row.Description = *req.Description }
	if req.Specs != nil { row.Specs = *req.Specs }
	if req.ImageURL != nil { row.ImageURL = *req.ImageURL }
	if req.Slug != nil { row.Slug = *req.Slug }
	if req.Price != nil { row.Price = *req.Price }
	if req.Stock != nil { row.Stock = *req.Stock }
	if req.IsPublished != nil { row.IsPublished = *req.IsPublished }

	if err := h.DB.Save(&row).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update product"))
	}
	return c.JSON(http.StatusOK, row)
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
		"stage":             o.Stage,
		"grade":             o.Grade,
		"deal_value":        o.DealValue,
		"probability":       o.Probability,
		"weighted_value":    o.DealValue * float64(o.Probability) / 100.0,
		"owner_id":          o.OwnerID,
		"source_quote_id":   o.SourceQuoteID,
		"expected_close_at": o.ExpectedCloseAt,
		"notes":             o.Notes,
	}
}

func (h Handler) AdminListOpportunities(c echo.Context) error {
	var rows []models.Opportunity
	q := h.DB.Order("created_at desc")
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
		Name            string     `json:"name"`
		AccountName     string     `json:"account_name"`
		ContactName     string     `json:"contact_name"`
		ContactEmail    string     `json:"contact_email"`
		Sector          string     `json:"sector"`
		Stage           string     `json:"stage"`
		Grade           string     `json:"grade"`
		DealValue       float64    `json:"deal_value"`
		Probability     *int       `json:"probability"`
		SourceQuoteID   *uuid.UUID `json:"source_quote_id"`
		ExpectedCloseAt *time.Time `json:"expected_close_at"`
		Notes           string     `json:"notes"`
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
	grade := models.OpportunityGrade(req.Grade)
	if grade == "" {
		grade = models.GradeBronze
	}
	if !validOpportunityGrades[grade] {
		return c.JSON(http.StatusBadRequest, errResponse("invalid grade"))
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
		Stage:           stage,
		Grade:           grade,
		DealValue:       req.DealValue,
		Probability:     probability,
		SourceQuoteID:   req.SourceQuoteID,
		ExpectedCloseAt: req.ExpectedCloseAt,
		Notes:           req.Notes,
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
		Name            *string    `json:"name"`
		AccountName     *string    `json:"account_name"`
		ContactName     *string    `json:"contact_name"`
		ContactEmail    *string    `json:"contact_email"`
		Sector          *string    `json:"sector"`
		Stage           *string    `json:"stage"`
		Grade           *string    `json:"grade"`
		DealValue       *float64   `json:"deal_value"`
		Probability     *int       `json:"probability"`
		ExpectedCloseAt *time.Time `json:"expected_close_at"`
		Notes           *string    `json:"notes"`
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
	if req.DealValue != nil {
		updates["deal_value"] = *req.DealValue
	}
	if req.ExpectedCloseAt != nil {
		updates["expected_close_at"] = *req.ExpectedCloseAt
	}
	if req.Notes != nil {
		updates["notes"] = *req.Notes
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
	h.DB.First(&row, "id = ?", id)
	return c.JSON(http.StatusOK, opportunityJSON(row))
}

func (h Handler) AdminDeleteOpportunity(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid opportunity id"))
	}
	if err := h.DB.Delete(&models.Opportunity{}, "id = ?", id).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not delete opportunity"))
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "opportunity deleted"})
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
	kind := strings.TrimSpace(c.FormValue("kind"))

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
	if err := h.Store.Save(key, src, fileHeader.Size, contentType); err != nil {
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

func errResponse(message string) map[string]string { return map[string]string{"error": message} }

func publicUser(user models.User) map[string]any {
	return map[string]any{
		"id": user.ID, "email": user.Email, "full_name": user.FullName,
		"role": user.Role, "is_active": user.IsActive, "student_profile": user.StudentProfile,
	}
}


