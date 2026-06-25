package middleware

import (
	"arcusinvest/internal/config"
	"arcusinvest/internal/models"
	"arcusinvest/internal/services"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

func Auth(cfg *config.Config) echo.MiddlewareFunc {
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
			c.Set("user_id", claims.UserID)
			c.Set("role", claims.Role)
			c.Set("email", claims.Email)
			return next(c)
		}
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
