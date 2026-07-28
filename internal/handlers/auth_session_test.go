package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"arcusinvest/internal/config"
	appmw "arcusinvest/internal/middleware"
	"arcusinvest/internal/models"
	"arcusinvest/internal/services"

	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// Exercises the real Auth middleware rather than the claim comparison in
// isolation: the check has to be wired into the request path to be worth
// anything, and a version stamped but never compared would pass a unit test.

func testCfg() *config.Config {
	return &config.Config{JWTSecret: "test-secret-not-used-anywhere-real"}
}

// authOnce runs one request through the Auth middleware with the given bearer
// token and reports the resulting status.
func authOnce(t *testing.T, db *gorm.DB, token string) int {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/opportunities", nil)
	req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := appmw.Auth(testCfg(), db)(func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})
	if err := handler(c); err != nil {
		// The middleware signals refusal with an echo.HTTPError rather than by
		// writing a response, so the status lives on the error.
		if he, ok := err.(*echo.HTTPError); ok {
			return he.Code
		}
		t.Fatalf("unexpected error: %v", err)
	}
	return rec.Code
}

// TestRevokeAllSessionsStrandsExistingTokens is the whole point of token
// versioning: without it a stolen token stays usable for its full lifetime and
// a password reset changes nothing for whoever already has one.
func TestRevokeAllSessionsStrandsExistingTokens(t *testing.T) {
	db := testDB(t)
	user := fixtureUser(t, db, models.RoleAdmin)

	// Re-read so the fixture carries the column default rather than a zero value.
	if err := db.First(&user, "id = ?", user.ID).Error; err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if user.TokenVersion < 1 {
		t.Fatalf("token_version = %d, want the column default of 1", user.TokenVersion)
	}

	token, err := services.IssueToken(testCfg(), user)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if code := authOnce(t, db, token); code != http.StatusOK {
		t.Fatalf("fresh token status = %d, want 200", code)
	}

	if err := services.RevokeAllSessions(db, user.ID); err != nil {
		t.Fatalf("RevokeAllSessions: %v", err)
	}
	if code := authOnce(t, db, token); code != http.StatusUnauthorized {
		t.Fatalf("revoked token status = %d, want 401 — the token outlived its revocation", code)
	}

	// A token minted after the bump works again, so revocation ends sessions
	// rather than locking the account out permanently.
	var refreshed models.User
	db.First(&refreshed, "id = ?", user.ID)
	next, err := services.IssueToken(testCfg(), refreshed)
	if err != nil {
		t.Fatalf("IssueToken after revoke: %v", err)
	}
	if code := authOnce(t, db, next); code != http.StatusOK {
		t.Fatalf("post-revocation token status = %d, want 200", code)
	}
}

// TestUnversionedTokenIsRefused pins the deliberate one-time sign-out on deploy.
// Accepting a token with no version as a legacy exception would be a revocation
// bypass with no expiry date.
func TestUnversionedTokenIsRefused(t *testing.T) {
	db := testDB(t)
	user := fixtureUser(t, db, models.RoleAdmin)
	db.First(&user, "id = ?", user.ID)

	legacy := user
	legacy.TokenVersion = 0 // what an old token decodes to
	token, err := services.IssueToken(testCfg(), legacy)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if code := authOnce(t, db, token); code != http.StatusUnauthorized {
		t.Errorf("unversioned token status = %d, want 401", code)
	}
}

// TestRevocationIncrementsRatherThanSets guards the concurrent case: two
// revocations landing together must both count, which a read-then-write would
// lose.
func TestRevocationIncrementsRatherThanSets(t *testing.T) {
	db := testDB(t)
	user := fixtureUser(t, db, models.RoleAdmin)
	db.First(&user, "id = ?", user.ID)
	start := user.TokenVersion

	for i := 0; i < 3; i++ {
		if err := services.RevokeAllSessions(db, user.ID); err != nil {
			t.Fatalf("revoke %d: %v", i, err)
		}
	}
	var after models.User
	db.First(&after, "id = ?", user.ID)
	if after.TokenVersion != start+3 {
		t.Errorf("token_version = %d, want %d", after.TokenVersion, start+3)
	}
}
