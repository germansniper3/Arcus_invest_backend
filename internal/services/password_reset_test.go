package services

import (
	"errors"
	"testing"
	"time"

	"arcusinvest/internal/models"

	"golang.org/x/crypto/bcrypt"
)

// TestResetEndsEverySession is the promise the feature makes: someone resetting
// because they think their account is compromised has to be able to evict
// whoever else is signed in. Changing the password alone would not do that.
func TestResetEndsEverySession(t *testing.T) {
	db := refreshTestDB(t)
	user := refreshUser(t, db)

	laptop, _ := IssueRefreshToken(db, user.ID, "", "laptop")
	phone, _ := IssueRefreshToken(db, user.ID, "", "phone")
	versionBefore := user.TokenVersion

	_, raw, created, err := CreatePasswordReset(db, user.Email, "1.2.3.4")
	if err != nil || !created {
		t.Fatalf("CreatePasswordReset: created=%v err=%v", created, err)
	}
	if _, err := ConsumePasswordReset(db, raw, "a-brand-new-password"); err != nil {
		t.Fatalf("ConsumePasswordReset: %v", err)
	}

	for name, rt := range map[string]string{"laptop": laptop, "phone": phone} {
		if _, _, err := RotateRefreshToken(db, rt, "", ""); !errors.Is(err, ErrRefreshRejected) {
			t.Errorf("%s session survived the reset", name)
		}
	}

	var after models.User
	db.First(&after, "id = ?", user.ID)
	if after.TokenVersion <= versionBefore {
		t.Errorf("token_version = %d, want greater than %d — access tokens already issued still work",
			after.TokenVersion, versionBefore)
	}
	if bcrypt.CompareHashAndPassword([]byte(after.PasswordHash), []byte("a-brand-new-password")) != nil {
		t.Error("the new password does not authenticate")
	}
}

// TestResetTokenIsSingleUse stops a link forwarded or left in an inbox from
// being redeemed a second time.
func TestResetTokenIsSingleUse(t *testing.T) {
	db := refreshTestDB(t)
	user := refreshUser(t, db)

	_, raw, _, _ := CreatePasswordReset(db, user.Email, "")
	if _, err := ConsumePasswordReset(db, raw, "first-password-here"); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if _, err := ConsumePasswordReset(db, raw, "second-password-here"); !errors.Is(err, ErrResetRejected) {
		t.Fatalf("second use error = %v, want ErrResetRejected", err)
	}

	var after models.User
	db.First(&after, "id = ?", user.ID)
	if bcrypt.CompareHashAndPassword([]byte(after.PasswordHash), []byte("second-password-here")) == nil {
		t.Error("the second redemption changed the password")
	}
}

// TestExpiredResetTokenIsRejected pins the hour limit.
func TestExpiredResetTokenIsRejected(t *testing.T) {
	db := refreshTestDB(t)
	user := refreshUser(t, db)

	_, raw, _, _ := CreatePasswordReset(db, user.Email, "")
	if err := db.Model(&models.PasswordResetToken{}).
		Where("user_id = ?", user.ID).
		Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("age the token: %v", err)
	}
	if _, err := ConsumePasswordReset(db, raw, "some-new-password"); !errors.Is(err, ErrResetRejected) {
		t.Errorf("expired token error = %v, want ErrResetRejected", err)
	}
}

// TestRequestingAgainSupersedesTheOldLink keeps only the newest link live, so a
// user who asks twice is not left guessing which email to open.
func TestRequestingAgainSupersedesTheOldLink(t *testing.T) {
	db := refreshTestDB(t)
	user := refreshUser(t, db)

	_, first, _, _ := CreatePasswordReset(db, user.Email, "")
	_, second, _, _ := CreatePasswordReset(db, user.Email, "")

	if _, err := ConsumePasswordReset(db, first, "old-link-password"); !errors.Is(err, ErrResetRejected) {
		t.Error("the superseded link still works")
	}
	if _, err := ConsumePasswordReset(db, second, "new-link-password"); err != nil {
		t.Errorf("the newest link should work: %v", err)
	}
}

// TestUnknownAddressCreatesNothing is the anti-enumeration property at the
// service layer; the handler's job is to answer identically either way.
func TestUnknownAddressCreatesNothing(t *testing.T) {
	db := refreshTestDB(t)

	_, raw, created, err := CreatePasswordReset(db, "nobody-here@example.invalid", "")
	if err != nil {
		t.Fatalf("CreatePasswordReset: %v", err)
	}
	if created || raw != "" {
		t.Error("a reset token was minted for an address with no account")
	}
}

// TestDeactivatedUserCannotReset stops a disabled account being reactivated by
// whoever still has access to its mailbox.
func TestDeactivatedUserCannotReset(t *testing.T) {
	db := refreshTestDB(t)
	user := refreshUser(t, db)
	_, raw, _, _ := CreatePasswordReset(db, user.Email, "")

	db.Model(&models.User{}).Where("id = ?", user.ID).Update("is_active", false)
	if _, err := ConsumePasswordReset(db, raw, "still-not-allowed-x"); !errors.Is(err, ErrResetRejected) {
		t.Errorf("deactivated reset error = %v, want ErrResetRejected", err)
	}
}

// TestResetEnforcesThePasswordRule confirms the shared validator is actually
// reached here rather than the check being forgotten on the newest path.
func TestResetEnforcesThePasswordRule(t *testing.T) {
	db := refreshTestDB(t)
	user := refreshUser(t, db)
	_, raw, _, _ := CreatePasswordReset(db, user.Email, "")

	if _, err := ConsumePasswordReset(db, raw, "short"); err == nil {
		t.Fatal("a 5-character password was accepted")
	}
	// The token must survive a rejected attempt, or a typo would burn the link.
	if _, err := ConsumePasswordReset(db, raw, "a-long-enough-password"); err != nil {
		t.Errorf("token was consumed by a failed attempt: %v", err)
	}
}
