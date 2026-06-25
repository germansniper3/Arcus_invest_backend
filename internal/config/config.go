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
	SeedAdminName string
	SeedAdminEmail string
	SeedAdminPassword string
	AIProviderURL string
	AIAPIKey      string
	AIModel       string
}

func Load() (*Config, error) {
	_ = godotenv.Load()
	cfg := &Config{
		Port:              get("APP_PORT", get("PORT", "8032")),
		Env:               get("ENV", "development"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		JWTSecret:         os.Getenv("JWT_SECRET"),
		CORSOrigins:       split(get("CORS_ORIGINS", "http://localhost:5173,http://localhost:4173")),
		SeedAdminName:     get("SEED_ADMIN_NAME", "Arcus Administrator"),
		SeedAdminEmail:    get("SEED_ADMIN_EMAIL", "admin@arcusinvest-zm.com"),
		SeedAdminPassword: get("SEED_ADMIN_PASSWORD", ""),
		AIProviderURL:     get("AI_PROVIDER_URL", "https://api-inference.huggingface.co/models/mistralai/Mistral-7B-Instruct-v0.2"),
		AIAPIKey:          os.Getenv("AI_API_KEY"),
		AIModel:           get("AI_MODEL", "mistralai/Mistral-7B-Instruct-v0.2"),
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
	return cfg, nil
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
