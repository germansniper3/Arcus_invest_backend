package handlers

import (
	"arcusinvest/internal/config"
	"arcusinvest/internal/models"
	"arcusinvest/internal/services"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type Handler struct {
	DB  *gorm.DB
	Cfg *config.Config
}

func (h Handler) Health(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]any{"status": "ok", "service": "arcusinvest-api", "time": time.Now().UTC()})
}

func (h Handler) Login(c echo.Context) error {
	var req struct{ Email, Password string }
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
	req.Status = models.StatusSubmitted
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
	claimURL := fmt.Sprintf("%s/claim-invitation?token=%s", c.Request().Header.Get("Origin"), invite.Token)
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
	return c.JSON(http.StatusOK, map[string]any{
		"profile":    profile,
		"enrollment": enrollment,
		"milestones": milestones,
		"comments":   comments,
	})
}

func (h Handler) UpdateCapstone(c echo.Context) error {
	var req struct{ Title, Summary string }
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

func (h Handler) UpdateMilestone(c echo.Context) error {
	milestoneID, err := uuid.Parse(c.Param("mid"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid milestone id"))
	}
	var req struct {
		Status   string `json:"status"`
		Feedback string `json:"feedback"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	var milestone models.CapstoneMilestone
	if err := h.DB.First(&milestone, "id = ?", milestoneID).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("milestone not found"))
	}
	updates := map[string]any{}
	if req.Status != "" { updates["status"] = req.Status }
	if req.Feedback != "" { updates["feedback"] = req.Feedback }
	if req.Status == "completed" { now := time.Now(); updates["completed_at"] = &now }
	h.DB.Model(&milestone).Updates(updates)
	h.DB.First(&milestone, "id = ?", milestoneID)
	// Update profile progress based on completed milestones
	var total, completed int64
	h.DB.Model(&models.CapstoneMilestone{}).Where("student_profile_id = ?", milestone.StudentProfileID).Count(&total)
	h.DB.Model(&models.CapstoneMilestone{}).Where("student_profile_id = ? AND status = ?", milestone.StudentProfileID, "completed").Count(&completed)
	if total > 0 {
		pct := int((completed * 100) / total)
		h.DB.Model(&models.StudentProfile{}).Where("id = ?", milestone.StudentProfileID).Update("progress_pct", pct)
	}
	return c.JSON(http.StatusOK, milestone)
}

func (h Handler) PostComment(c echo.Context) error {
	var req struct{ Message string }
	if err := c.Bind(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		return c.JSON(http.StatusBadRequest, errResponse("message is required"))
	}
	var user models.User
	if err := h.DB.Preload("StudentProfile").First(&user, "id = ?", c.Get("user_id")).Error; err != nil {
		return c.JSON(http.StatusNotFound, errResponse("user not found"))
	}
	profileID := user.StudentProfile.ID
	comment := models.CapstoneComment{
		StudentProfileID: profileID,
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
	profile := user.StudentProfile
	var milestones []models.CapstoneMilestone
	h.DB.Where("student_profile_id = ?", profile.ID).Order("created_at asc").Find(&milestones)
	var comments []models.CapstoneComment
	h.DB.Where("student_profile_id = ?", profile.ID).Order("created_at asc").Find(&comments)
	return c.JSON(http.StatusOK, map[string]any{
		"user": publicUser(user), "profile": profile,
		"milestones": milestones, "comments": comments,
	})
}

// Admin can post a comment/feedback on a student's capstone
func (h Handler) AdminPostComment(c echo.Context) error {
	sid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid student id"))
	}
	var req struct{ Message string }
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

// Admin milestone update with feedback
func (h Handler) AdminUpdateMilestone(c echo.Context) error {
	return h.UpdateMilestone(c)
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
		Title       string `json:"title"`
		Description string `json:"description"`
		Date        string `json:"date"`
		Location    string `json:"location"`
		Capacity    int    `json:"capacity"`
		IsPublished bool   `json:"is_published"`
		ImageURL    string `json:"image_url"`
		Slug        string `json:"slug"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid body"))
	}
	if req.Title != "" { event.Title = req.Title }
	if req.Description != "" { event.Description = req.Description }
	if req.Location != "" { event.Location = req.Location }
	if req.ImageURL != "" { event.ImageURL = req.ImageURL }
	if req.Slug != "" { event.Slug = req.Slug }
	if req.Capacity > 0 { event.Capacity = req.Capacity }
	event.IsPublished = req.IsPublished
	if req.Date != "" {
		parsed, err := time.Parse("2006-01-02T15:04", req.Date)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, req.Date)
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
	var req struct{ Subject, Message string }
	if err := c.Bind(&req); err != nil || req.Message == "" {
		return c.JSON(http.StatusBadRequest, errResponse("message is required"))
	}
	var reservations []models.Reservation
	h.DB.Where("event_id = ? AND status = ?", id, "confirmed").Find(&reservations)
	// Log the broadcast (SMTP integration can be added later)
	fmt.Printf("[BROADCAST] Event %s | Subject: %s | Recipients: %d\n", id, req.Subject, len(reservations))
	return c.JSON(http.StatusOK, map[string]any{
		"message":    "broadcast queued",
		"recipients": len(reservations),
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
	req.Status = "new"
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
	var req struct{ SessionID, Question string }
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
	var req struct{ Email, FullName, Password string; Role models.Role }
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil || len(req.Password) < 10 {
		return c.JSON(http.StatusBadRequest, errResponse("password must be at least 10 characters"))
	}
	user := models.User{Email: strings.ToLower(req.Email), FullName: req.FullName, PasswordHash: string(hash), Role: req.Role, IsActive: true}
	if user.Role == "" { user.Role = models.RoleAdmissions }
	if err := h.DB.Create(&user).Error; err != nil {
		return c.JSON(http.StatusBadRequest, errResponse(err.Error()))
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
		Name        string  `json:"name"`
		Description string  `json:"description"`
		Price       float64 `json:"price"`
		Stock       int     `json:"stock"`
		ImageURL    string  `json:"image_url"`
		Specs       string  `json:"specs"`
		IsPublished bool    `json:"is_published"`
		Slug        string  `json:"slug"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid body"))
	}
	if req.Name != "" { row.Name = req.Name }
	if req.Description != "" { row.Description = req.Description }
	if req.Specs != "" { row.Specs = req.Specs }
	if req.ImageURL != "" { row.ImageURL = req.ImageURL }
	if req.Slug != "" { row.Slug = req.Slug }
	row.Price = req.Price
	row.Stock = req.Stock
	row.IsPublished = req.IsPublished

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

func errResponse(message string) map[string]string { return map[string]string{"error": message} }

func publicUser(user models.User) map[string]any {
	return map[string]any{
		"id": user.ID, "email": user.Email, "full_name": user.FullName,
		"role": user.Role, "is_active": user.IsActive, "student_profile": user.StudentProfile,
	}
}


