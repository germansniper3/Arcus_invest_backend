package seed

import (
	"arcusinvest/internal/config"
	"arcusinvest/internal/models"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

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
	if err := db.Where("email = ?", user.Email).FirstOrCreate(&user).Error; err != nil {
		return err
	}

	// Seed John Katepa as a SuperAdmin
	johnHash, err := bcrypt.GenerateFromPassword([]byte("ArcusAdmin#2026"), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	john := models.User{
		Email:        "john.katepa@arcusinvest-zm.com",
		FullName:     "John Katepa",
		PasswordHash: string(johnHash),
		Role:         models.RoleSuperAdmin,
		IsActive:     true,
	}
	return db.Where("email = ?", john.Email).FirstOrCreate(&john).Error
}

func Products(db *gorm.DB) error {
	products := []models.Product{
		{
			Name:        "Chofa E-Bike Standard",
			Slug:        "chofa-e-bike-standard",
			Description: "The flagship Chofa commuter features a durable steel frame, a high-torque rear hub motor, and maintainable local wiring. Built to handle daily commutes and cargo hauling on local Zambian roads.",
			Price:       18500,
			Stock:       8,
			ImageURL:    "/images/arcus/ebike.png",
			Specs:       "Range: 55-70 km | Motor: 350W Hub | Top Speed: 28 km/h | Charge Time: 4.5 hrs",
			IsPublished: true,
		},
		{
			Name:        "Chofa Heavy Cargo E-Bike",
			Slug:        "chofa-heavy-cargo-e-bike",
			Description: "Designed for commercial deliveries and heavy loads. Equipped with an extended heavy-duty rear rack, dual battery mount capabilities, and reinforced suspension wheels.",
			Price:       24500,
			Stock:       3,
			ImageURL:    "/images/arcus/ebike.png",
			Specs:       "Range: 80-110 km | Motor: 500W Mid-drive | Cargo Cap: 120 kg | Dual Battery Compatible",
			IsPublished: true,
		},
	}
	for _, p := range products {
		var count int64
		db.Model(&models.Product{}).Where("slug = ?", p.Slug).Count(&count)
		if count == 0 {
			if err := db.Create(&p).Error; err != nil {
				return err
			}
		}
	}
	return nil
}
