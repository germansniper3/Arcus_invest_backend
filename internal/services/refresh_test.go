package services

import (
	"errors"
	"os"
	"testing"

	"arcusinvest/internal/database"
	"arcusinvest/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

func refreshTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed refresh test")
	}
	db, err := database.Connect(dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Rolled back at the end, so a run leaves the shared dev database untouched.
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin: %v", tx.Error)
	}
	t.Cleanup(func() { tx.Rollback() })
	return tx
}

func refreshUser(t *testing.T, db *gorm.DB) models.User {
	t.Helper()
	u := models.User{
		Email:    "refresh-test-" + uuid.NewString() + "@example.invalid",
		FullName: "Refresh Fixture", Role: models.RoleAdmin, IsActive: true,
		TokenVersion: 1,
	}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

// TestRotationSpendsThePresentedToken is the property the whole design rests on:
// if a refresh token stayed usable, a thief and the real user could both hold a
// working credential indefinitely and nothing would ever reveal it.
func TestRotationSpendsThePresentedToken(t *testing.T) {
	db := refreshTestDB(t)
	user := refreshUser(t, db)

	first, err := IssueRefreshToken(db, user.ID, "1.2.3.4", "test-agent")
	if err != nil {
		t.Fatalf("IssueRefreshToken: %v", err)
	}

	got, second, err := RotateRefreshToken(db, first, "1.2.3.4", "test-agent")
	if err != nil {
		t.Fatalf("first rotation: %v", err)
	}
	if got.ID != user.ID {
		t.Errorf("rotation returned user %s, want %s", got.ID, user.ID)
	}
	if second == first {
		t.Fatal("rotation returned the same token — it was not rotated at all")
	}

	// The successor works.
	if _, _, err := RotateRefreshToken(db, second, "1.2.3.4", "test-agent"); err != nil {
		t.Fatalf("successor should be usable: %v", err)
	}
}

// TestReplayRevokesTheWholeFamily is the theft signal. Revoking only the
// replayed token would leave the thief's newer token working, which is exactly
// backwards — by the time a replay is seen there is no way to tell which party
// is legitimate, so both lose the session.
func TestReplayRevokesTheWholeFamily(t *testing.T) {
	db := refreshTestDB(t)
	user := refreshUser(t, db)

	first, _ := IssueRefreshToken(db, user.ID, "", "")
	_, second, err := RotateRefreshToken(db, first, "", "")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// Replay the spent token.
	if _, _, err := RotateRefreshToken(db, first, "", ""); !errors.Is(err, ErrRefreshRejected) {
		t.Fatalf("replay error = %v, want ErrRefreshRejected", err)
	}

	// The token the legitimate holder had must now be dead too.
	if _, _, err := RotateRefreshToken(db, second, "", ""); !errors.Is(err, ErrRefreshRejected) {
		t.Fatal("the live token still works after a replay — the family was not revoked")
	}

	var live int64
	db.Model(&models.RefreshToken{}).Where("user_id = ? AND revoked_at IS NULL", user.ID).Count(&live)
	if live != 0 {
		t.Errorf("%d tokens left unrevoked after a detected replay, want 0", live)
	}
}

// TestRevokedAndUnknownTokensAreRejected covers logout and garbage input.
func TestRevokedAndUnknownTokensAreRejected(t *testing.T) {
	db := refreshTestDB(t)
	user := refreshUser(t, db)

	raw, _ := IssueRefreshToken(db, user.ID, "", "")
	family, ok := FamilyForToken(db, raw)
	if !ok {
		t.Fatal("FamilyForToken did not resolve a token it had just issued")
	}
	if err := RevokeRefreshFamily(db, family); err != nil {
		t.Fatalf("RevokeRefreshFamily: %v", err)
	}
	if _, _, err := RotateRefreshToken(db, raw, "", ""); !errors.Is(err, ErrRefreshRejected) {
		t.Errorf("revoked token error = %v, want ErrRefreshRejected", err)
	}

	for _, bogus := range []string{"", "not-a-token", "deadbeef"} {
		if _, _, err := RotateRefreshToken(db, bogus, "", ""); !errors.Is(err, ErrRefreshRejected) {
			t.Errorf("bogus %q error = %v, want ErrRefreshRejected", bogus, err)
		}
	}
}

// TestDeactivatedUserCannotRefresh stops a disabled account from renewing itself
// back into a working session.
func TestDeactivatedUserCannotRefresh(t *testing.T) {
	db := refreshTestDB(t)
	user := refreshUser(t, db)
	raw, _ := IssueRefreshToken(db, user.ID, "", "")

	if err := db.Model(&models.User{}).Where("id = ?", user.ID).Update("is_active", false).Error; err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, _, err := RotateRefreshToken(db, raw, "", ""); !errors.Is(err, ErrRefreshRejected) {
		t.Errorf("deactivated user refresh error = %v, want ErrRefreshRejected", err)
	}
}

// TestRevokeAllRefreshTokensEndsEveryDevice backs the password-reset promise
// that resetting signs you out everywhere, not just where you reset from.
func TestRevokeAllRefreshTokensEndsEveryDevice(t *testing.T) {
	db := refreshTestDB(t)
	user := refreshUser(t, db)

	// Two independent sign-ins, so two families.
	laptop, _ := IssueRefreshToken(db, user.ID, "", "laptop")
	phone, _ := IssueRefreshToken(db, user.ID, "", "phone")

	if err := RevokeAllRefreshTokens(db, user.ID); err != nil {
		t.Fatalf("RevokeAllRefreshTokens: %v", err)
	}
	for name, raw := range map[string]string{"laptop": laptop, "phone": phone} {
		if _, _, err := RotateRefreshToken(db, raw, "", ""); !errors.Is(err, ErrRefreshRejected) {
			t.Errorf("%s still refreshes after a global revoke", name)
		}
	}
}

// TestTokenIsNotStoredInPlaintext pins the one property a database leak turns on.
func TestTokenIsNotStoredInPlaintext(t *testing.T) {
	db := refreshTestDB(t)
	user := refreshUser(t, db)
	raw, _ := IssueRefreshToken(db, user.ID, "", "")

	var found int64
	db.Model(&models.RefreshToken{}).Where("token_hash = ?", raw).Count(&found)
	if found != 0 {
		t.Error("the raw token is stored as-is — a database dump would hand over live sessions")
	}
	db.Model(&models.RefreshToken{}).Where("token_hash = ?", hashToken(raw)).Count(&found)
	if found != 1 {
		t.Errorf("hash lookup found %d rows, want 1", found)
	}
}
