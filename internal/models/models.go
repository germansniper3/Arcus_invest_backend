package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Role string

const (
	RoleSuperAdmin Role = "super_admin"
	RoleAdmin      Role = "admin"
	RoleAdmissions Role = "admissions"
	RoleStudent    Role = "student"
)

type EnrollmentStatus string

const (
	StatusSubmitted           EnrollmentStatus = "submitted"
	StatusPendingOrientation  EnrollmentStatus = "pending_orientation"
	StatusOrientationComplete EnrollmentStatus = "orientation_complete"
	StatusAccepted            EnrollmentStatus = "accepted"
	StatusArchived            EnrollmentStatus = "archived"
)

type BaseModel struct {
	ID        uuid.UUID      `json:"id" gorm:"type:uuid;primaryKey"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

func (m *BaseModel) BeforeCreate(tx *gorm.DB) error {
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	return nil
}

type User struct {
	BaseModel
	Email        string     `json:"email" gorm:"uniqueIndex;not null"`
	PasswordHash string     `json:"-"`
	FullName     string     `json:"full_name" gorm:"not null"`
	Role         Role       `json:"role" gorm:"type:varchar(40);index;not null"`
	IsActive     bool       `json:"is_active" gorm:"default:true"`
	LastLoginAt  *time.Time `json:"last_login_at"`
	StudentProfile *StudentProfile `json:"student_profile,omitempty"`
}

type StudentProfile struct {
	BaseModel
	UserID       uuid.UUID `json:"user_id" gorm:"type:uuid;uniqueIndex;not null"`
	User         User      `json:"-" gorm:"constraint:OnDelete:CASCADE"`
	EnrollmentID *uuid.UUID `json:"enrollment_id" gorm:"type:uuid;index"`
	Tier         string    `json:"tier" gorm:"index"`
	ProgressPct int       `json:"progress_pct" gorm:"default:0"`
	CapstoneTitle string  `json:"capstone_title"`
	CapstoneSummary string `json:"capstone_summary" gorm:"type:text"`
}

type Enrollment struct {
	BaseModel
	FullName      string           `json:"full_name" gorm:"not null"`
	Email         string           `json:"email" gorm:"index;not null"`
	Phone         string           `json:"phone" gorm:"index;not null"`
	AgeRange      string           `json:"age_range"`
	Location      string           `json:"location"`
	CurrentStatus string           `json:"current_status"`
	About         string           `json:"about" gorm:"type:text"`
	Interests     string           `json:"interests" gorm:"type:text"`
	HasProject    string           `json:"has_project"`
	ProjectIdea   string           `json:"project_idea" gorm:"type:text"`
	ContactPref   string           `json:"contact_pref"`
	NextOfKin     string           `json:"next_of_kin" gorm:"type:text"`
	Tier          string           `json:"tier" gorm:"index"`
	Status        EnrollmentStatus `json:"status" gorm:"type:varchar(40);index;not null;default:'submitted'"`
	Notes         string           `json:"notes" gorm:"type:text"`
	AssignedUserID *uuid.UUID      `json:"assigned_user_id" gorm:"type:uuid;index"`
	StudentUserID *uuid.UUID       `json:"student_user_id" gorm:"type:uuid;index"`
	OrientationAt *time.Time       `json:"orientation_at"`
}

type OnboardingInvitation struct {
	BaseModel
	EnrollmentID uuid.UUID `json:"enrollment_id" gorm:"type:uuid;index;uniqueIndex;not null"`
	Email        string    `json:"email" gorm:"not null"`
	Token        string    `json:"token" gorm:"uniqueIndex;not null"`
	ExpiresAt    time.Time `json:"expires_at" gorm:"not null"`
	Status       string    `json:"status" gorm:"not null;default:'pending'"` // pending, claimed, expired
}

type Event struct {
	BaseModel
	Title        string    `json:"title" gorm:"not null"`
	Slug         string    `json:"slug" gorm:"uniqueIndex;not null"`
	Description  string    `json:"description" gorm:"type:text;not null"`
	ImageURL     string    `json:"image_url"`
	Date         time.Time `json:"date" gorm:"not null"`
	Location     string    `json:"location"`
	Capacity     int       `json:"capacity"`
	IsPublished  bool      `json:"is_published" gorm:"default:false"`
}

type Reservation struct {
	BaseModel
	EventID   uuid.UUID  `json:"event_id" gorm:"type:uuid;index;not null"`
	Event     Event      `json:"event" gorm:"constraint:OnDelete:CASCADE"`
	UserID    *uuid.UUID `json:"user_id" gorm:"type:uuid;index"`
	FullName  string     `json:"full_name" gorm:"not null"`
	Email     string     `json:"email" gorm:"index;not null"`
	Phone     string     `json:"phone"`
	Notes     string     `json:"notes" gorm:"type:text"`
	Status    string     `json:"status" gorm:"default:'pending'"` // pending, confirmed, cancelled
}

type CapstoneMilestone struct {
	BaseModel
	StudentProfileID uuid.UUID  `json:"student_profile_id" gorm:"type:uuid;index;not null"`
	Title            string     `json:"title" gorm:"not null"`
	Description      string     `json:"description" gorm:"type:text"`
	Status           string     `json:"status" gorm:"default:'pending'"` // pending, in_progress, completed
	Feedback         string     `json:"feedback" gorm:"type:text"`
	CompletedAt      *time.Time `json:"completed_at"`
}

type CapstoneComment struct {
	BaseModel
	StudentProfileID uuid.UUID `json:"student_profile_id" gorm:"type:uuid;index;not null"`
	AuthorName       string    `json:"author_name" gorm:"not null"`
	AuthorRole       Role      `json:"author_role" gorm:"not null"`
	Message          string    `json:"message" gorm:"type:text;not null"`
}

type QuoteRequest struct {
	BaseModel
	Name       string `json:"name" gorm:"not null"`
	Email      string `json:"email" gorm:"index;not null"`
	Phone      string `json:"phone"`
	Company    string `json:"company"`
	Service    string `json:"service" gorm:"index"`
	BudgetRange string `json:"budget_range"`
	Message    string `json:"message" gorm:"type:text;not null"`
	Status     string `json:"status" gorm:"index;not null;default:'new'"`
	AdminNotes string `json:"admin_notes" gorm:"type:text"`
}

type ChatMessage struct {
	BaseModel
	UserID   *uuid.UUID `json:"user_id" gorm:"type:uuid;index"`
	SessionID string    `json:"session_id" gorm:"index;not null"`
	Question string     `json:"question" gorm:"type:text;not null"`
	Answer   string     `json:"answer" gorm:"type:text;not null"`
	Source   string     `json:"source"`
}

type Product struct {
	BaseModel
	Name        string  `json:"name" gorm:"not null"`
	Slug        string  `json:"slug" gorm:"uniqueIndex;not null"`
	Description string  `json:"description" gorm:"type:text;not null"`
	Price       float64 `json:"price"`
	Stock       int     `json:"stock"`
	ImageURL    string  `json:"image_url"`
	Specs       string  `json:"specs" gorm:"type:text"` // e.g. "Range: 60km | Battery: 48V"
	IsPublished bool    `json:"is_published" gorm:"default:true"`
}
