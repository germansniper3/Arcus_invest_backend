package services

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"arcusinvest/internal/config"
	"arcusinvest/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Errors a caller has to tell apart. A refused refresh means "sign in again";
// a lost race means "try once more with the cookie you now hold".
var (
	// ErrRefreshRejected covers every unusable token: unknown, expired, revoked,
	// or replayed. They are deliberately indistinguishable to the client — the
	// difference is only useful to someone probing.
	ErrRefreshRejected = errors.New("refresh token rejected")
	// ErrRefreshRaced means another request rotated this token a moment ago.
	// Not an attack, and specifically not grounds for killing the family.
	ErrRefreshRaced = errors.New("refresh token was rotated concurrently")
)

// refreshTokenBytes is the entropy in each token. 32 bytes is far past guessing,
// which matters because this value alone carries a session.
const refreshTokenBytes = 32

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func newRawToken() (string, error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// IssueRefreshToken starts a new chain. Called on a fresh sign-in, so each login
// gets its own family and revoking one device does not sign the others out.
func IssueRefreshToken(db *gorm.DB, userID uuid.UUID, ip, userAgent string) (string, error) {
	return appendToFamily(db, userID, uuid.New(), ip, userAgent)
}

func appendToFamily(db *gorm.DB, userID, familyID uuid.UUID, ip, userAgent string) (string, error) {
	raw, err := newRawToken()
	if err != nil {
		return "", err
	}
	row := models.RefreshToken{
		UserID:    userID,
		TokenHash: hashToken(raw),
		FamilyID:  familyID,
		ExpiresAt: time.Now().Add(config.RefreshTokenTTL()),
		IP:        ip,
		UserAgent: userAgent,
	}
	if err := db.Create(&row).Error; err != nil {
		return "", err
	}
	return raw, nil
}

// RotateRefreshToken consumes the presented token and returns its owner plus a
// successor. The presented value is never usable again.
//
// Rotation is what makes a stolen refresh token detectable: the thief and the
// real user cannot both keep using the chain, so whichever presents a spent
// token reveals the theft. That is why a replay revokes the whole family rather
// than only the token replayed — by then it is unknowable which party is which.
func RotateRefreshToken(db *gorm.DB, raw, ip, userAgent string) (models.User, string, error) {
	var empty models.User
	if raw == "" {
		return empty, "", ErrRefreshRejected
	}

	var row models.RefreshToken
	if err := db.Where("token_hash = ?", hashToken(raw)).First(&row).Error; err != nil {
		return empty, "", ErrRefreshRejected
	}
	if row.RevokedAt != nil || time.Now().After(row.ExpiresAt) {
		return empty, "", ErrRefreshRejected
	}
	// Already spent when we read it: this is a replay of an older link, so the
	// chain is compromised and every token in it dies.
	if row.UsedAt != nil {
		_ = RevokeRefreshFamily(db, row.FamilyID)
		return empty, "", ErrRefreshRejected
	}

	// Claim it. Conditional so that two requests arriving together cannot both
	// succeed and fork the chain into two live successors.
	now := time.Now()
	res := db.Model(&models.RefreshToken{}).
		Where("id = ? AND used_at IS NULL AND revoked_at IS NULL", row.ID).
		Update("used_at", now)
	if res.Error != nil {
		return empty, "", res.Error
	}
	if res.RowsAffected != 1 {
		// Lost the race rather than replayed a spent token. Revoking here would
		// sign a legitimate user out for double-clicking.
		return empty, "", ErrRefreshRaced
	}

	var user models.User
	if err := db.First(&user, "id = ?", row.UserID).Error; err != nil {
		return empty, "", ErrRefreshRejected
	}
	if !user.IsActive {
		_ = RevokeRefreshFamily(db, row.FamilyID)
		return empty, "", ErrRefreshRejected
	}

	next, err := appendToFamily(db, row.UserID, row.FamilyID, ip, userAgent)
	if err != nil {
		return empty, "", err
	}
	return user, next, nil
}

// RevokeRefreshFamily ends one chain — one device, or one compromised session.
func RevokeRefreshFamily(db *gorm.DB, familyID uuid.UUID) error {
	return db.Model(&models.RefreshToken{}).
		Where("family_id = ? AND revoked_at IS NULL", familyID).
		Update("revoked_at", time.Now()).Error
}

// RevokeAllRefreshTokens ends every chain a user has. Paired with
// RevokeAllSessions on a password reset: one kills the refresh tokens, the other
// the access tokens already in circulation.
func RevokeAllRefreshTokens(db *gorm.DB, userID uuid.UUID) error {
	return db.Model(&models.RefreshToken{}).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Update("revoked_at", time.Now()).Error
}

// FamilyForToken resolves the chain a raw token belongs to, for logout. It does
// not validate the token: signing out with a spent or expired one should still
// clear the session rather than fail.
func FamilyForToken(db *gorm.DB, raw string) (uuid.UUID, bool) {
	if raw == "" {
		return uuid.Nil, false
	}
	var row models.RefreshToken
	if err := db.Where("token_hash = ?", hashToken(raw)).First(&row).Error; err != nil {
		return uuid.Nil, false
	}
	return row.FamilyID, true
}
