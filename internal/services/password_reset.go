package services

import (
	"errors"
	"strings"
	"time"

	"arcusinvest/internal/models"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// PasswordResetTTL is short on purpose. A reset link is acted on within minutes
// of being asked for, unlike an invitation which is expected to wait in an inbox
// for days. Every extra hour is time the link sits in a mailbox someone else may
// later gain access to.
const PasswordResetTTL = time.Hour

// ErrResetRejected covers unknown, expired and already-used tokens alike. The
// differences would only help someone probing for valid links.
var ErrResetRejected = errors.New("reset link is invalid or has expired")

// CreatePasswordReset issues a reset token for an email address, if it belongs
// to an active account.
//
// The bool reports whether a token was actually created. Callers MUST NOT vary
// their response on it: answering differently for a known and an unknown address
// turns this endpoint into a way to enumerate who has an account.
func CreatePasswordReset(db *gorm.DB, email, ip string) (models.User, string, bool, error) {
	var user models.User
	err := db.Where("email = ?", strings.ToLower(strings.TrimSpace(email))).First(&user).Error
	if err != nil || !user.IsActive {
		return user, "", false, nil
	}

	// Supersede any outstanding request. Asking again should not leave two live
	// links, and the newest is the one the user is looking at.
	if err := db.Model(&models.PasswordResetToken{}).
		Where("user_id = ? AND used_at IS NULL", user.ID).
		Update("used_at", time.Now()).Error; err != nil {
		return user, "", false, err
	}

	raw, err := newRawToken()
	if err != nil {
		return user, "", false, err
	}
	row := models.PasswordResetToken{
		UserID:    user.ID,
		TokenHash: hashToken(raw),
		ExpiresAt: time.Now().Add(PasswordResetTTL),
		RequestIP: ip,
	}
	if err := db.Create(&row).Error; err != nil {
		return user, "", false, err
	}
	return user, raw, true, nil
}

// ConsumePasswordReset validates a token, sets the new password, and ends every
// existing session for that user.
//
// Ending the sessions is the point as much as changing the password: someone
// resetting because they believe their account is compromised has to be able to
// evict whoever else is signed in. Both halves are needed — revoking the refresh
// families stops renewal, and bumping the token version kills the access tokens
// already in circulation.
func ConsumePasswordReset(db *gorm.DB, raw, newPassword string) (models.User, error) {
	var empty models.User
	if err := ValidatePassword(newPassword); err != nil {
		return empty, err
	}
	if raw == "" {
		return empty, ErrResetRejected
	}

	var row models.PasswordResetToken
	if err := db.Where("token_hash = ?", hashToken(raw)).First(&row).Error; err != nil {
		return empty, ErrResetRejected
	}
	if row.UsedAt != nil || time.Now().After(row.ExpiresAt) {
		return empty, ErrResetRejected
	}

	var user models.User
	if err := db.First(&user, "id = ?", row.UserID).Error; err != nil {
		return empty, ErrResetRejected
	}
	if !user.IsActive {
		return empty, ErrResetRejected
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return empty, err
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		// Conditional so a token cannot be redeemed twice by two requests that
		// both read it as unused.
		res := tx.Model(&models.PasswordResetToken{}).
			Where("id = ? AND used_at IS NULL", row.ID).
			Update("used_at", time.Now())
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrResetRejected
		}
		if err := tx.Model(&models.User{}).Where("id = ?", user.ID).
			Update("password_hash", string(hash)).Error; err != nil {
			return err
		}
		if err := RevokeAllRefreshTokens(tx, user.ID); err != nil {
			return err
		}
		return RevokeAllSessions(tx, user.ID)
	})
	if err != nil {
		return empty, err
	}
	return user, nil
}
