package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

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
	// MailFrom is the sender address for every outbound message, whichever
	// transport carries it. It is not SMTP-specific.
	MailFrom     string
	ResendAPIKey string
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
		// MAIL_FROM is the canonical name; SMTP_FROM is still honoured so
		// existing deployments keep working after the transport split.
		MailFrom:     get("MAIL_FROM", os.Getenv("SMTP_FROM")),
		ResendAPIKey: os.Getenv("RESEND_API_KEY"),
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
	return c.SMTPHost != "" && c.SMTPPort != "" && c.MailFrom != ""
}

// ResendConfigured reports whether mail can go out over Resend's HTTPS API.
// This is the transport to use on hosts that block outbound SMTP ports (Railway
// blocks 25/465/587 below the Pro plan), since it needs nothing but port 443.
func (c *Config) ResendConfigured() bool {
	return c.ResendAPIKey != "" && c.MailFrom != ""
}

// MailConfigured reports whether ANY transport can deliver mail. Call sites that
// gate on "can we email this?" want this, not SMTPConfigured.
func (c *Config) MailConfigured() bool {
	return c.ResendConfigured() || c.SMTPConfigured()
}

// MailTransport names the transport that will actually be used. Resend wins when
// both are set: if an operator configured an API key, they did so to escape SMTP.
func (c *Config) MailTransport() string {
	switch {
	case c.ResendConfigured():
		return "resend"
	case c.SMTPConfigured():
		return "smtp"
	}
	return "none"
}

// TokenTTLHours is superseded by AccessTokenTTL and is no longer used to issue
// anything. It remains only so LegacyTokenTTLSet can warn an operator whose
// environment still sets JWT_TTL_HOURS that the value is being ignored.
//
// Deprecated: use AccessTokenTTL.
func TokenTTLHours() int {
	raw := get("JWT_TTL_HOURS", "12")
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return 12
	}
	return v
}

// LegacyTokenTTLSet reports whether JWT_TTL_HOURS is still configured. Silently
// ignoring a setting an operator deliberately chose is how a system develops a
// reputation for not doing what it is told.
func LegacyTokenTTLSet() bool { return os.Getenv("JWT_TTL_HOURS") != "" }

// AccessTokenTTL is how long a bearer token is good for.
//
// Short by design: with refresh tokens carrying the session, an access token
// only has to survive a burst of requests. Its lifetime is the window in which
// a stolen token is useful, and the window in which a revocation has not yet
// taken effect for a caller who is not making requests.
func AccessTokenTTL() time.Duration {
	return time.Duration(positiveInt("ACCESS_TOKEN_TTL_MINUTES", 30)) * time.Minute
}

// RefreshTokenTTL is the longest a session can survive without re-entering a
// password. Rotation means the stored token changes constantly; this bounds the
// chain, not any single token.
func RefreshTokenTTL() time.Duration {
	return time.Duration(positiveInt("REFRESH_TOKEN_TTL_DAYS", 30)) * 24 * time.Hour
}

func positiveInt(key string, fallback int) int {
	v, err := strconv.Atoi(get(key, ""))
	if err != nil || v <= 0 {
		return fallback
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
