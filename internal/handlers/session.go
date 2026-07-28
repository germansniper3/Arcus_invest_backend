package handlers

import (
	"errors"
	"net/http"
	"time"

	"arcusinvest/internal/config"
	"arcusinvest/internal/models"
	"arcusinvest/internal/services"

	"github.com/labstack/echo/v4"
)

// refreshCookieName is the only place the refresh token is ever written. It is
// never returned in a response body: the whole reason for the cookie is that
// JavaScript — and therefore any XSS — cannot read it.
const refreshCookieName = "arcus_refresh"

// refreshCookiePath scopes the cookie to the auth endpoints, so it is not
// attached to the hundred other API calls that have no use for it.
const refreshCookiePath = "/api/v1/auth"

func setRefreshCookie(c echo.Context, raw string) {
	c.SetCookie(&http.Cookie{
		Name:     refreshCookieName,
		Value:    raw,
		Path:     refreshCookiePath,
		HttpOnly: true,
		// The frontend and API are separate origins in every environment, so the
		// cookie has to be SameSite=None — which browsers only honour together
		// with Secure. Secure is permitted on http://localhost, so development
		// behaves the same as production rather than needing a special case.
		Secure:   true,
		SameSite: http.SameSiteNoneMode,
		Expires:  time.Now().Add(config.RefreshTokenTTL()),
	})
}

func clearRefreshCookie(c echo.Context) {
	c.SetCookie(&http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteNoneMode,
		MaxAge:   -1,
	})
}

func readRefreshCookie(c echo.Context) string {
	ck, err := c.Cookie(refreshCookieName)
	if err != nil || ck == nil {
		return ""
	}
	return ck.Value
}

// Refresh exchanges the refresh cookie for a new access token and rotates the
// cookie. The response body carries only the access token and user; the refresh
// value goes out solely in the Set-Cookie header.
func (h Handler) Refresh(c echo.Context) error {
	raw := readRefreshCookie(c)
	user, next, err := services.RotateRefreshToken(h.DB, raw, c.RealIP(), c.Request().UserAgent())
	switch {
	case errors.Is(err, services.ErrRefreshRaced):
		// Another request rotated a moment ago and the caller now holds a newer
		// cookie. 409 rather than 401 so the client retries instead of tearing
		// the session down.
		return c.JSON(http.StatusConflict, errResponse("this session was refreshed concurrently — retry"))
	case err != nil:
		// Every other reason — unknown, expired, revoked, replayed — answers the
		// same way. The distinctions are only useful to someone probing.
		clearRefreshCookie(c)
		return c.JSON(http.StatusUnauthorized, errResponse("session expired — sign in again"))
	}

	access, err := services.IssueToken(h.Cfg, user)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not issue a token"))
	}
	setRefreshCookie(c, next)
	return c.JSON(http.StatusOK, map[string]any{"token": access, "user": userWithPermissions(user)})
}

// Logout ends this session server-side, which localStorage-clearing alone never
// did: before this, a copied token stayed valid until it expired.
//
// Always succeeds. Signing out with an already-dead cookie should still leave
// the caller signed out, not hand them an error they can do nothing about.
func (h Handler) Logout(c echo.Context) error {
	if family, ok := services.FamilyForToken(h.DB, readRefreshCookie(c)); ok {
		if err := services.RevokeRefreshFamily(h.DB, family); err != nil {
			c.Logger().Errorf("could not revoke refresh family on logout: %v", err)
		}
	}
	clearRefreshCookie(c)
	return c.JSON(http.StatusOK, map[string]any{"message": "signed out"})
}

// LogoutEverywhere revokes every session this user has, on every device, and
// invalidates the access tokens already in circulation.
func (h Handler) LogoutEverywhere(c echo.Context) error {
	var user models.User
	if err := h.DB.First(&user, "id = ?", c.Get("user_id")).Error; err != nil {
		return c.JSON(http.StatusUnauthorized, errResponse("could not identify the caller"))
	}
	if err := services.RevokeAllRefreshTokens(h.DB, user.ID); err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not revoke sessions"))
	}
	if err := services.RevokeAllSessions(h.DB, user.ID); err != nil {
		return c.JSON(http.StatusInternalServerError, errResponse("could not revoke sessions"))
	}
	clearRefreshCookie(c)
	return c.JSON(http.StatusOK, map[string]any{"message": "signed out everywhere"})
}
