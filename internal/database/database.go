package database

import (
	"arcusinvest/internal/models"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func Connect(databaseURL string) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	return db, nil
}

func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&models.User{},
		&models.StudentProfile{},
		&models.Enrollment{},
		&models.QuoteRequest{},
		&models.ChatMessage{},
		&models.OnboardingInvitation{},
		&models.Event{},
		&models.Reservation{},
		&models.CapstoneMilestone{},
		&models.CapstoneComment{},
		&models.ProgressReport{},
		&models.ExtensionRequest{},
		&models.Product{},
		&models.GalleryItem{},
		&models.Broadcast{},
		&models.Submission{},
		&models.Opportunity{},
		&models.OpportunityContact{},
		&models.OpportunityActivity{},
		&models.OpportunityLineItem{},
		&models.Payment{},
		&models.Contract{},
		&models.DocumentVersion{},
		&models.DocumentAccessLog{},
		&models.UserSignature{},
		&models.ContractSignature{},
		&models.Notification{},
		&models.NotificationPreference{},
		&models.AuditLog{},
		&models.CustomRole{},
		&models.CustomRolePermission{},
		&models.ApprovalRule{},
		&models.ApprovalRequest{},
		&models.ApprovalDecision{},
	)
}
