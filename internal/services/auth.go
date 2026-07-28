package services

import (
	"arcusinvest/internal/config"
	"arcusinvest/internal/models"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type Claims struct {
	UserID string      `json:"user_id"`
	Email  string      `json:"email"`
	Role   models.Role `json:"role"`
	jwt.RegisteredClaims
}

func Login(db *gorm.DB, cfg *config.Config, email string, password string) (string, models.User, error) {
	var user models.User
	if err := db.Preload("StudentProfile").Where("email = ?", strings.ToLower(email)).First(&user).Error; err != nil {
		return "", user, errors.New("invalid email or password")
	}
	if !user.IsActive {
		return "", user, errors.New("account is disabled")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return "", user, errors.New("invalid email or password")
	}
	now := time.Now().UTC()
	db.Model(&user).Update("last_login_at", now)
	token, err := IssueToken(cfg, user)
	return token, user, err
}

func IssueToken(cfg *config.Config, user models.User) (string, error) {
	claims := Claims{
		UserID: user.ID.String(),
		Email:  user.Email,
		Role:   user.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   user.ID.String(),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Duration(config.TokenTTLHours()) * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(cfg.JWTSecret))
}

func GenerateInvitation(db *gorm.DB, enrollmentID uuid.UUID) (*models.OnboardingInvitation, error) {
	var enrollment models.Enrollment
	if err := db.First(&enrollment, "id = ?", enrollmentID).Error; err != nil {
		return nil, err
	}

	// Remove any existing invitation for this enrollment. This MUST be a hard
	// delete: OnboardingInvitation soft-deletes (BaseModel carries DeletedAt) but
	// EnrollmentID has a unique index, which soft-deleted rows still occupy — a
	// plain Delete here would make every re-invite fail on a duplicate key.
	db.Unscoped().Where("enrollment_id = ?", enrollmentID).Delete(&models.OnboardingInvitation{})

	token := uuid.NewString()
	invite := models.OnboardingInvitation{
		EnrollmentID: enrollmentID,
		Email:        enrollment.Email,
		Token:        token,
		ExpiresAt:    time.Now().Add(7 * 24 * time.Hour), // 7 days expiration
		Status:       "pending",
	}

	if err := db.Create(&invite).Error; err != nil {
		return nil, err
	}

	return &invite, nil
}

func ClaimInvitation(db *gorm.DB, token string, password string, capstoneTitle string, capstoneSummary string) (*models.User, error) {
	var invite models.OnboardingInvitation
	if err := db.First(&invite, "token = ? AND status = ?", token, "pending").Error; err != nil {
		return nil, errors.New("invalid or already claimed invitation token")
	}

	if time.Now().After(invite.ExpiresAt) {
		invite.Status = "expired"
		db.Save(&invite)
		return nil, errors.New("invitation token has expired")
	}

	var enrollment models.Enrollment
	if err := db.First(&enrollment, "id = ?", invite.EnrollmentID).Error; err != nil {
		return nil, err
	}

	email := strings.ToLower(strings.TrimSpace(enrollment.Email))

	// An account may already exist for this email (a previous claim, or a
	// soft-deleted user still holding the unique email index). Report that
	// clearly instead of surfacing a raw duplicate-key error from Postgres.
	var existing int64
	db.Unscoped().Model(&models.User{}).Where("lower(email) = ?", email).Count(&existing)
	if existing > 0 {
		return nil, errors.New("an account already exists for " + email + " — sign in instead, or ask an administrator to reset the password")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	user := models.User{
		Email:        email,
		FullName:     enrollment.FullName,
		PasswordHash: string(hash),
		Role:         models.RoleStudent,
		IsActive:     true,
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&user).Error; err != nil {
			return err
		}

		profile := models.StudentProfile{
			UserID:          user.ID,
			EnrollmentID:    &enrollment.ID,
			Tier:            enrollment.Tier,
			ProgressPct:     15, // Starts at 15% after orientation/signup
			CapstoneTitle:   capstoneTitle,
			CapstoneSummary: capstoneSummary,
		}
		if err := tx.Create(&profile).Error; err != nil {
			return err
		}

		// Create default onboarding milestones
		defaultMilestones := []models.CapstoneMilestone{
			{StudentProfileID: profile.ID, Title: "Orientation Complete", Description: "Attend the Hub tour and orientation session.", Status: "completed", CompletedAt: func() *time.Time { t := time.Now(); return &t }()},
			{StudentProfileID: profile.ID, Title: "Capstone Proposal Approved", Description: "Submit the capstone project brief for review.", Status: "in_progress"},
			{StudentProfileID: profile.ID, Title: "Schematic & Design Checkpoint", Description: "Complete PCB design or mechanical CAD models.", Status: "pending"},
			{StudentProfileID: profile.ID, Title: "Prototype Assembly", Description: "Populate electronics, weld chassis, or deploy baseline software.", Status: "pending"},
			{StudentProfileID: profile.ID, Title: "Functional Demonstration", Description: "Live testing and validation of the working build.", Status: "pending"},
			{StudentProfileID: profile.ID, Title: "Hub Demo Day Presentation", Description: "Pitch and display the project to mentors and partners.", Status: "pending"},
		}

		for _, milestone := range defaultMilestones {
			if err := tx.Create(&milestone).Error; err != nil {
				return err
			}
		}

		// Add initial welcome comment
		welcomeComment := models.CapstoneComment{
			StudentProfileID: profile.ID,
			AuthorName:       "Arcus Hub Coordinator",
			AuthorRole:       models.RoleAdmissions,
			Message:          "Welcome to the Arcus Innovation Hub! We've provisioned your dashboard with standard engineering milestones. Please share updates here as you build.",
		}
		if err := tx.Create(&welcomeComment).Error; err != nil {
			return err
		}

		// Update invitation status
		invite.Status = "claimed"
		if err := tx.Save(&invite).Error; err != nil {
			return err
		}

		// Update enrollment status
		return tx.Model(&enrollment).Updates(map[string]any{"student_user_id": user.ID, "status": models.StatusAccepted}).Error
	})

	if err != nil {
		return nil, err
	}

	return &user, nil
}

func UserIDFromString(value string) uuid.UUID {
	id, _ := uuid.Parse(value)
	return id
}

// MinPasswordLength is the shortest password the system accepts anywhere.
const MinPasswordLength = 10

// ValidatePassword is the single rule for what a password may be.
//
// It exists because the length check was duplicated at every place a password
// could be set, and a rule copied per call site is a rule that eventually
// differs per call site — the weakest copy then decides what the system
// actually enforces. The error text is returned to the user, so it says what to
// do rather than what went wrong.
func ValidatePassword(password string) error {
	if len(password) < MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	return nil
}
