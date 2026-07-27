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

type ProgressReport struct {
	BaseModel
	StudentProfileID   uuid.UUID  `json:"student_profile_id" gorm:"type:uuid;index;not null"`
	PeriodStart        time.Time  `json:"period_start"`
	PeriodEnd          time.Time  `json:"period_end"`
	Accomplishments    string     `json:"accomplishments" gorm:"type:text"`
	Challenges         string     `json:"challenges" gorm:"type:text"`
	SupervisorFeedback string     `json:"supervisor_feedback" gorm:"type:text"`
	Status             string     `json:"status" gorm:"default:'submitted'"` // submitted, reviewed
	ReviewedAt         *time.Time `json:"reviewed_at"`
}

type ExtensionRequest struct {
	BaseModel
	StudentProfileID  uuid.UUID  `json:"student_profile_id" gorm:"type:uuid;index;not null"`
	ExtensionType     string     `json:"extension_type"`
	RequestedDeadline time.Time  `json:"requested_deadline"`
	Reason            string     `json:"reason" gorm:"type:text"`
	DecisionNote      string     `json:"decision_note" gorm:"type:text"`
	Status            string     `json:"status" gorm:"default:'pending'"` // pending, approved, denied
	DecidedAt         *time.Time `json:"decided_at"`
}

// Submission is a deliverable file a student uploads for mentor review. The
// actual bytes live in the storage layer; StoredKey is the opaque, server-
// generated storage key and is never exposed in JSON (json:"-").
type Submission struct {
	BaseModel
	StudentProfileID uuid.UUID  `json:"student_profile_id" gorm:"type:uuid;index;not null"`
	Title            string     `json:"title" gorm:"not null"`
	Kind             string     `json:"kind"` // proposal, report, final, other
	FileName         string     `json:"file_name"`     // original client filename, display only
	StoredKey        string     `json:"-" gorm:"not null"` // opaque storage key, never exposed
	ContentType      string     `json:"content_type"`
	Size             int64      `json:"size"` // bytes
	Status           string     `json:"status" gorm:"default:'submitted'"` // submitted, accepted, revise
	ReviewNote       string     `json:"review_note" gorm:"type:text"`
	ReviewedAt       *time.Time `json:"reviewed_at"`
}

type Broadcast struct {
	BaseModel
	EventID        uuid.UUID  `json:"event_id" gorm:"type:uuid;index;not null"`
	Subject        string     `json:"subject"`
	Message        string     `json:"message" gorm:"type:text;not null"`
	RecipientCount int        `json:"recipient_count"`
	Status         string     `json:"status" gorm:"not null;default:'queued'"` // queued, sent
	SentAt         *time.Time `json:"sent_at"`
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

// OpportunityStage is the deal-qualification pipeline stage. The open stages
// (prospecting → negotiation) count toward the pipeline; won/lost are terminal.
type OpportunityStage string

const (
	StageProspecting OpportunityStage = "prospecting"
	StageQualified   OpportunityStage = "qualified"
	StageProposal    OpportunityStage = "proposal"
	StageNegotiation OpportunityStage = "negotiation"
	StageWon         OpportunityStage = "won"
	StageLost        OpportunityStage = "lost"
)

// OpportunityGrade is the account/deal maturity grading (Bronze → Platinum),
// an axis independent of the pipeline stage.
type OpportunityGrade string

const (
	GradeBronze   OpportunityGrade = "bronze"
	GradeSilver   OpportunityGrade = "silver"
	GradeGold     OpportunityGrade = "gold"
	GradePlatinum OpportunityGrade = "platinum"
)

// Opportunity is a B2B sales deal moving through the qualification pipeline.
// DealValue × Probability gives the weighted forecast value (computed in the
// API response, not stored, mirroring the progress-percentage pattern).
type Opportunity struct {
	BaseModel
	Name            string               `json:"name" gorm:"not null"`
	AccountName     string               `json:"account_name" gorm:"index"`
	ContactName     string               `json:"contact_name"`
	ContactEmail    string               `json:"contact_email"`
	Sector          string               `json:"sector" gorm:"index"`
	Segment         string               `json:"segment" gorm:"index;default:'standard'"` // strategic, growth, standard
	Stage           OpportunityStage     `json:"stage" gorm:"type:varchar(40);index;not null;default:'prospecting'"`
	Grade           OpportunityGrade     `json:"grade" gorm:"type:varchar(20);index;not null;default:'bronze'"`
	DealValue       float64              `json:"deal_value"`
	Probability     int                  `json:"probability"` // 0–100, defaults per stage
	OwnerID         *uuid.UUID           `json:"owner_id" gorm:"type:uuid;index"`
	SourceQuoteID   *uuid.UUID           `json:"source_quote_id" gorm:"type:uuid;index"` // lead it converted from
	ExpectedCloseAt *time.Time           `json:"expected_close_at"`
	Notes           string               `json:"notes" gorm:"type:text"`
	Contacts        []OpportunityContact `json:"contacts" gorm:"foreignKey:OpportunityID;constraint:OnDelete:CASCADE"`
	LineItems       []OpportunityLineItem `json:"line_items" gorm:"foreignKey:OpportunityID;constraint:OnDelete:CASCADE"`
}

// OpportunityLineItem is a priced good/service on a deal, used to itemise the
// generated quotations and invoices. Line total (quantity × unit_price) is
// computed in the API response, not stored. Embedded in the opportunity
// create/update payload and replaced wholesale on update, like Contacts.
type OpportunityLineItem struct {
	BaseModel
	OpportunityID uuid.UUID `json:"opportunity_id" gorm:"type:uuid;index;not null"`
	Description   string    `json:"description" gorm:"not null"`
	Quantity      float64   `json:"quantity"`
	UnitPrice     float64   `json:"unit_price"`
	Position      int       `json:"position"` // display order
}

// Payment records money received against a deal — the basis for a receipt and
// for the amount-paid / balance on an invoice. This records payments only; it
// does not process or transfer funds.
type Payment struct {
	BaseModel
	OpportunityID uuid.UUID  `json:"opportunity_id" gorm:"type:uuid;index;not null"`
	Amount        float64    `json:"amount"`
	Method        string     `json:"method"` // cash, bank_transfer, mobile_money, cheque, card, other
	Reference     string     `json:"reference"`
	PaidAt        time.Time  `json:"paid_at"`
	Note          string     `json:"note" gorm:"type:text"`
	RecordedByID  *uuid.UUID `json:"recorded_by_id" gorm:"type:uuid"`
	RecordedBy    string     `json:"recorded_by"`
}

// Contract is a stored agreement, optionally linked to a deal, with renewal
// tracking. The file bytes live in the storage layer; StoredKey is opaque and
// never exposed (a contract may exist as metadata before a file is attached).
type Contract struct {
	BaseModel
	OpportunityID *uuid.UUID `json:"opportunity_id" gorm:"type:uuid;index"`
	AccountName   string     `json:"account_name" gorm:"index"`
	Title         string     `json:"title" gorm:"not null"`
	Status        string     `json:"status" gorm:"index;default:'draft'"` // draft, sent, signed, active, expired
	Value         float64    `json:"value"`
	StartDate     *time.Time `json:"start_date"`
	RenewalDate   *time.Time `json:"renewal_date" gorm:"index"`
	Notes         string     `json:"notes" gorm:"type:text"`
	FileName      string     `json:"file_name"`
	StoredKey     string     `json:"-"`
	ContentType   string     `json:"content_type"`
	Size          int64      `json:"size"`
}

// OpportunityActivity is an attributed entry in a deal's engagement log — a
// logged call, meeting, email, note, or task. It records who logged it and when
// the engagement actually happened (OccurredAt), mirroring the CapstoneComment
// authorship pattern.
type OpportunityActivity struct {
	BaseModel
	OpportunityID uuid.UUID  `json:"opportunity_id" gorm:"type:uuid;index;not null"`
	ActorID       *uuid.UUID `json:"actor_id" gorm:"type:uuid;index"`
	ActorName     string     `json:"actor_name" gorm:"not null"`
	ActorRole     Role       `json:"actor_role"`
	Type          string     `json:"type" gorm:"index"` // call, meeting, email, note, task, other
	Body          string     `json:"body" gorm:"type:text;not null"`
	OccurredAt    time.Time  `json:"occurred_at"` // when the engagement happened (defaults to now)
}

// OpportunityContact is a member of the account's buying committee for a deal
// (decision maker, champion, influencer, …).
type OpportunityContact struct {
	BaseModel
	OpportunityID uuid.UUID `json:"opportunity_id" gorm:"type:uuid;index;not null"`
	Name          string    `json:"name" gorm:"not null"`
	Role          string    `json:"role"` // decision_maker, champion, influencer, technical, procurement, other
	Email         string    `json:"email"`
	Phone         string    `json:"phone"`
	IsPrimary     bool      `json:"is_primary"`
}

// AuditLog is an immutable record of an admin mutation: who did what to which
// entity, and the HTTP outcome. Written best-effort by audit middleware after a
// successful mutating request, so the trail never blocks or breaks the action.
type AuditLog struct {
	BaseModel
	ActorID   *uuid.UUID `json:"actor_id" gorm:"type:uuid;index"`
	ActorName string     `json:"actor_name"` // denormalised snapshot (survives user deletion)
	ActorRole Role       `json:"actor_role"`
	Action    string     `json:"action" gorm:"index"` // create, update, delete, convert, upload, ...
	Entity    string     `json:"entity" gorm:"index"` // opportunities, contracts, quotes, ...
	EntityID  string     `json:"entity_id" gorm:"index"`
	Method    string     `json:"method"`
	Path      string     `json:"path"`
	Status    int        `json:"status"`
}

type ChatMessage struct {
	BaseModel
	UserID   *uuid.UUID `json:"user_id" gorm:"type:uuid;index"`
	SessionID string    `json:"session_id" gorm:"index;not null"`
	Question string     `json:"question" gorm:"type:text;not null"`
	Answer   string     `json:"answer" gorm:"type:text;not null"`
	Source   string     `json:"source"`
}

// GalleryItem is a photo of Arcus's work shown on the public site. Images are
// uploaded through the same pipeline as product images and referenced by URL,
// so the bytes live in the storage layer rather than the database.
type GalleryItem struct {
	BaseModel
	Title       string `json:"title" gorm:"not null"`
	Caption     string `json:"caption" gorm:"type:text"`
	Category    string `json:"category" gorm:"index"` // e.g. Electronics, Fabrication, Software
	ImageURL    string `json:"image_url" gorm:"not null"`
	Position    int    `json:"position" gorm:"index"` // display order, ascending
	IsPublished bool   `json:"is_published" gorm:"default:true;index"`
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
