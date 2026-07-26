package middleware

import (
	"arcusinvest/internal/authz"
	"arcusinvest/internal/config"
	"arcusinvest/internal/models"
	"arcusinvest/internal/services"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// Auth validates the bearer token and then re-checks the account against the
// database on every request.
//
// The token proves identity only; it is NOT trusted for authority. The role and
// active flag are read from the user row, so deactivating a user or changing
// their role takes effect on their next request instead of lingering until the
// token expires (JWT_TTL_HOURS, default 12).
func Auth(cfg *config.Config, db *gorm.DB) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			header := c.Request().Header.Get("Authorization")
			if !strings.HasPrefix(header, "Bearer ") {
				return echo.NewHTTPError(http.StatusUnauthorized, "missing bearer token")
			}
			tokenValue := strings.TrimPrefix(header, "Bearer ")
			claims := &services.Claims{}
			token, err := jwt.ParseWithClaims(tokenValue, claims, func(token *jwt.Token) (any, error) {
				return []byte(cfg.JWTSecret), nil
			})
			if err != nil || !token.Valid {
				return echo.NewHTTPError(http.StatusUnauthorized, "invalid token")
			}

			// Soft-deleted users are excluded by the default GORM scope, so a
			// deleted account fails here too.
			var user models.User
			if err := db.Select("id", "email", "role", "is_active").
				First(&user, "id = ?", claims.UserID).Error; err != nil {
				return echo.NewHTTPError(http.StatusUnauthorized, "account no longer exists")
			}
			if !user.IsActive {
				return echo.NewHTTPError(http.StatusForbidden, "account is disabled")
			}

			c.Set("user_id", claims.UserID)
			c.Set("role", user.Role) // authoritative: from the DB, not the claim
			c.Set("email", user.Email)
			return next(c)
		}
	}
}

// RequirePermission is the default-deny gate for the admin surface. It derives
// the resource from the MATCHED ROUTE PATH and the action from the HTTP method,
// then consults the authz matrix. A route that authz does not map is refused, so
// adding an admin endpoint without granting access fails closed rather than
// leaking. Attach it once to the admin group — never per route.
func RequirePermission(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		role, _ := c.Get("role").(models.Role)
		res, ok := authz.ResourceForPath(c.Path())
		if !ok {
			c.Logger().Errorf("authz: unmapped admin route %q — denying; add it to authz.pathResources", c.Path())
			return echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
		}
		if !authz.Can(role, res, authz.ActionForMethod(c.Request().Method)) {
			return echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
		}
		return next(c)
	}
}

func RequireRoles(roles ...models.Role) echo.MiddlewareFunc {
	allowed := map[models.Role]bool{}
	for _, role := range roles {
		allowed[role] = true
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			role, _ := c.Get("role").(models.Role)
			if !allowed[role] {
				return echo.NewHTTPError(http.StatusForbidden, "insufficient permissions")
			}
			return next(c)
		}
	}
}
