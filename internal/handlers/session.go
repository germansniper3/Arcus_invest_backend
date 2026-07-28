package handlers

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
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

// ForgotPassword emails a reset link, and says nothing about whether the address
// belongs to an account.
//
// The response is identical either way — same status, same body, and the email
// is sent in the background so the timing does not differ either. The invitation
// preview endpoint deliberately distinguishes 404 from 410, which is right there
// because the caller already holds the token; copying it here would turn this
// into a way to ask "does this person bank with you?" one address at a time.
func (h Handler) ForgotPassword(c echo.Context) error {
	var req struct {
		Email string `json:"email"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}

	sameAnswer := map[string]any{
		"message": "If that address has an account, a reset link is on its way.",
	}

	user, raw, created, err := services.CreatePasswordReset(h.DB, req.Email, c.RealIP())
	if err != nil {
		c.Logger().Errorf("could not create password reset: %v", err)
		return c.JSON(http.StatusOK, sameAnswer)
	}
	if !created {
		return c.JSON(http.StatusOK, sameAnswer)
	}

	resetURL := fmt.Sprintf("%s/reset-password?token=%s",
		strings.TrimRight(h.Cfg.FrontendURL, "/"), raw)
	expires := time.Now().Add(services.PasswordResetTTL)

	// Sent off the request path deliberately. Delivering inline would make the
	// known-address case measurably slower than the unknown one — the identical
	// body would still be handed over by a stopwatch. The goroutine takes only
	// values, nothing tied to the request, so it is safe after this returns.
	//
	// A delivery failure is logged rather than surfaced, for the same reason:
	// an error the caller can see is an answer the caller can use.
	go func(to, name string) {
		if err := services.SendPasswordResetEmail(h.Cfg, to, name, resetURL, expires); err != nil {
			log.Printf("WARN: could not send password reset email to %s: %v", to, err)
		}
	}(user.Email, user.FullName)

	return c.JSON(http.StatusOK, sameAnswer)
}

// ResetPassword redeems a reset link and signs the user out everywhere.
func (h Handler) ResetPassword(c echo.Context) error {
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errResponse("invalid request body"))
	}
	if _, err := services.ConsumePasswordReset(h.DB, req.Token, req.Password); err != nil {
		// A rejected link and a too-short password are both the caller's to fix,
		// and both messages are safe to show: neither reveals whether an account
		// exists, only whether this particular link is still good.
		return c.JSON(http.StatusBadRequest, errResponse(err.Error()))
	}
	// Whatever session this browser had is gone along with the rest.
	clearRefreshCookie(c)
	return c.JSON(http.StatusOK, map[string]any{
		"message": "Password updated. Sign in with your new password.",
	})
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
