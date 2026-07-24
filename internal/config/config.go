package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	Port          string
	Env           string
	DatabaseURL   string
	JWTSecret     string
	CORSOrigins   []string
	FrontendURL   string
	SeedAdminName string
	SeedAdminEmail string
	SeedAdminPassword string
	AIProviderURL string
	AIAPIKey      string
	AIModel       string
	SMTPHost      string
	SMTPPort      string
	SMTPUsername  string
	SMTPPassword  string
	SMTPFrom      string
	StorageDriver string
	StorageDir    string
	S3Bucket      string
	S3Region      string
	S3Endpoint    string
	S3AccessKey   string
	S3SecretKey   string
}

func Load() (*Config, error) {
	_ = godotenv.Load()
	cfg := &Config{
		Port:              get("APP_PORT", get("PORT", "8032")),
		Env:               get("ENV", "development"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		JWTSecret:         os.Getenv("JWT_SECRET"),
		CORSOrigins:       split(get("CORS_ORIGINS", "http://localhost:5173,http://localhost:4173")),
		FrontendURL:       os.Getenv("FRONTEND_URL"),
		SeedAdminName:     get("SEED_ADMIN_NAME", "Arcus Administrator"),
		SeedAdminEmail:    get("SEED_ADMIN_EMAIL", "admin@arcusinvest-zm.com"),
		SeedAdminPassword: get("SEED_ADMIN_PASSWORD", ""),
		AIProviderURL:     get("AI_PROVIDER_URL", "https://api.anthropic.com"),
		AIAPIKey:          os.Getenv("AI_API_KEY"),
		AIModel:           get("AI_MODEL", "claude-sonnet-5"),
		SMTPHost:          os.Getenv("SMTP_HOST"),
		SMTPPort:          os.Getenv("SMTP_PORT"),
		SMTPUsername:      os.Getenv("SMTP_USERNAME"),
		SMTPPassword:      os.Getenv("SMTP_PASSWORD"),
		SMTPFrom:          os.Getenv("SMTP_FROM"),
		// Storage: local disk by default; S3_* are parsed but only consulted
		// when STORAGE_DRIVER=s3. None of these are production-required.
		StorageDriver: get("STORAGE_DRIVER", "local"),
		StorageDir:    get("STORAGE_DIR", "storage/uploads"),
		S3Bucket:      os.Getenv("S3_BUCKET"),
		S3Region:      os.Getenv("S3_REGION"),
		S3Endpoint:    os.Getenv("S3_ENDPOINT"),
		S3AccessKey:   os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:   os.Getenv("S3_SECRET_KEY"),
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("JWT_SECRET is required")
	}
	if cfg.Env != "production" && cfg.SeedAdminPassword == "" {
		cfg.SeedAdminPassword = "ArcusAdmin#2026"
	}
	if cfg.Env == "production" && cfg.SeedAdminPassword == "" {
		return nil, fmt.Errorf("SEED_ADMIN_PASSWORD is required in production")
	}
	if cfg.Env != "production" && cfg.FrontendURL == "" {
		cfg.FrontendURL = "http://localhost:5173"
	}
	if cfg.Env == "production" && cfg.FrontendURL == "" {
		return nil, fmt.Errorf("FRONTEND_URL is required in production")
	}
	return cfg, nil
}

// SMTPConfigured reports whether the minimum SMTP settings needed to send mail
// are present.
func (c *Config) SMTPConfigured() bool {
	return c.SMTPHost != "" && c.SMTPPort != "" && c.SMTPFrom != ""
}

func TokenTTLHours() int {
	raw := get("JWT_TTL_HOURS", "12")
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return 12
	}
	return v
}

func get(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func split(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}
