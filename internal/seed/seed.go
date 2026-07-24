package seed

import (
	"arcusinvest/internal/config"
	"arcusinvest/internal/models"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// Admin provisions the single env-driven seed super-admin. Products and any
// additional admins are managed through the admin hub, never seeded here.
func Admin(db *gorm.DB, cfg *config.Config) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(cfg.SeedAdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	user := models.User{
		Email:        strings.ToLower(cfg.SeedAdminEmail),
		FullName:     cfg.SeedAdminName,
		PasswordHash: string(hash),
		Role:         models.RoleSuperAdmin,
		IsActive:     true,
	}
	return db.Where("email = ?", user.Email).FirstOrCreate(&user).Error
}
