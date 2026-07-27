package handlers

import (
	"net/http"
	"time"

	"arcusinvest/internal/models"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

func notificationJSON(n models.Notification) map[string]any {
	return map[string]any{
		"id":          n.ID,
		"created_at":  n.CreatedAt,
		"kind":        n.Kind,
		"title":       n.Title,
		"body":        n.Body,
		"entity_type": n.EntityType,
		"entity_id":   n.EntityID,
		"read_at":     n.ReadAt,
	}
}

// AdminListNotifications returns the caller's own inbox, newest first, with the
// unread count alongside so the bell badge needs no second request.
//
// Every query here is scoped to the caller's user_id. An inbox is personal, and
// a notification id is never enough on its own to reach someone else's.
func (h Handler) AdminListNotifications(c echo.Context) error {
	userID := c.Get("user_id")

	var rows []models.Notification
	q := h.DB.Where("user_id = ?", userID)
	if c.QueryParam("unread") == "true" {
		q = q.Where("read_at IS NULL")
	}
	if err := q.Order("created_at DESC").Limit(100).Find(&rows).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not load notifications"))
	}

	var unread int64
	h.DB.Model(&models.Notification{}).Where("user_id = ? AND read_at IS NULL", userID).Count(&unread)

	// Must be an empty slice, never nil: a nil slice marshals to `null` and a
	// client doing `items.length` would crash on an empty inbox.
	items := []map[string]any{}
	for _, n := range rows {
		items = append(items, notificationJSON(n))
	}
	return c.JSON(http.StatusOK, map[string]any{"items": items, "unread": unread})
}

// AdminMarkNotificationRead marks one of the caller's notifications as read.
func (h Handler) AdminMarkNotificationRead(c echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid notification id"))
	}
	now := time.Now().UTC()
	// Scoped by user_id as well as id, so one user cannot mark another's inbox.
	// read_at IS NULL keeps the original timestamp if it is already read.
	res := h.DB.Model(&models.Notification{}).
		Where("id = ? AND user_id = ? AND read_at IS NULL", id, c.Get("user_id")).
		Update("read_at", now)
	if res.Error != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update the notification"))
	}
	return c.JSON(http.StatusOK, map[string]any{"message": "marked read"})
}

// AdminMarkAllNotificationsRead clears the caller's unread badge.
func (h Handler) AdminMarkAllNotificationsRead(c echo.Context) error {
	now := time.Now().UTC()
	if err := h.DB.Model(&models.Notification{}).
		Where("user_id = ? AND read_at IS NULL", c.Get("user_id")).
		Update("read_at", now).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not update notifications"))
	}
	return c.JSON(http.StatusOK, map[string]any{"message": "all marked read"})
}

var validEmailModes = map[string]bool{
	models.EmailModePerEvent: true,
	models.EmailModeDigest:   true,
	models.EmailModeNone:     true,
}

// AdminGetNotificationPreference returns the caller's email mode, defaulting to
// digest for a user who has never set one.
func (h Handler) AdminGetNotificationPreference(c echo.Context) error {
	var pref models.NotificationPreference
	if err := h.DB.Where("user_id = ?", c.Get("user_id")).First(&pref).Error; err != nil {
		return c.JSON(http.StatusOK, map[string]any{"email_mode": models.EmailModeDigest})
	}
	return c.JSON(http.StatusOK, map[string]any{"email_mode": pref.EmailMode})
}

// AdminSetNotificationPreference changes how much mail the caller receives.
func (h Handler) AdminSetNotificationPreference(c echo.Context) error {
	var req struct {
		EmailMode string `json:"email_mode"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if !validEmailModes[req.EmailMode] {
		return c.JSON(http.StatusBadRequest, errResponse("invalid email mode — use per_event, digest or none"))
	}
	userID, err := uuid.Parse(c.Get("user_id").(string))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid user"))
	}

	var pref models.NotificationPreference
	if err := h.DB.Where("user_id = ?", userID).First(&pref).Error; err != nil {
		pref = models.NotificationPreference{UserID: userID, EmailMode: req.EmailMode}
		if err := h.DB.Create(&pref).Error; err != nil {
			return c.JSON(http.StatusInternalServerError, errResponse("could not save the preference"))
		}
	} else if err := h.DB.Model(&pref).Update("email_mode", req.EmailMode).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not save the preference"))
	}
	return c.JSON(http.StatusOK, map[string]any{"email_mode": req.EmailMode})
}
