package config

import (
	"os"
	"testing"
)

func TestSplitTrimsAndDropsEmpty(t *testing.T) {
	got := split("a, b ,,c,  ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("split len = %d (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("split[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTokenTTLHoursDefaultsAndParses(t *testing.T) {
	os.Unsetenv("JWT_TTL_HOURS")
	if got := TokenTTLHours(); got != 12 {
		t.Fatalf("default TTL = %d, want 12", got)
	}
	t.Setenv("JWT_TTL_HOURS", "48")
	if got := TokenTTLHours(); got != 48 {
		t.Fatalf("TTL = %d, want 48", got)
	}
	t.Setenv("JWT_TTL_HOURS", "not-a-number")
	if got := TokenTTLHours(); got != 12 {
		t.Fatalf("invalid TTL should fall back to 12, got %d", got)
	}
}

func TestGetFallback(t *testing.T) {
	os.Unsetenv("ARCUS_TEST_MISSING")
	if got := get("ARCUS_TEST_MISSING", "fallback"); got != "fallback" {
		t.Fatalf("get fallback = %q, want fallback", got)
	}
	t.Setenv("ARCUS_TEST_PRESENT", "value")
	if got := get("ARCUS_TEST_PRESENT", "fallback"); got != "value" {
		t.Fatalf("get = %q, want value", got)
	}
}

// TestMailFromFallsBackToSMTPFrom pins backwards compatibility: deployments that
// predate the transport split set SMTP_FROM, and must keep sending after upgrade
// without an env-var change.
func TestMailFromFallsBackToSMTPFrom(t *testing.T) {
	// Load() requires these two regardless of mail settings.
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SECRET", "secret")

	t.Setenv("SMTP_FROM", "legacy@arcus.test")
	t.Setenv("MAIL_FROM", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MailFrom != "legacy@arcus.test" {
		t.Errorf("MailFrom = %q, want the SMTP_FROM fallback", cfg.MailFrom)
	}

	// When both are set, the canonical name wins.
	t.Setenv("MAIL_FROM", "current@arcus.test")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MailFrom != "current@arcus.test" {
		t.Errorf("MailFrom = %q, want MAIL_FROM to take precedence", cfg.MailFrom)
	}
}

// TestResendEnvWiringSelectsTransport proves the real env-var names reach the
// transport decision — the struct-level tests in services/ cannot catch a typo
// in os.Getenv.
func TestResendEnvWiringSelectsTransport(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("MAIL_FROM", "ops@arcus.test")
	t.Setenv("SMTP_HOST", "smtp.gmail.com")
	t.Setenv("SMTP_PORT", "587")
	t.Setenv("RESEND_API_KEY", "re_live_key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ResendAPIKey != "re_live_key" {
		t.Fatalf("ResendAPIKey = %q — RESEND_API_KEY is not being read", cfg.ResendAPIKey)
	}
	if got := cfg.MailTransport(); got != "resend" {
		t.Errorf("MailTransport() = %q, want resend even with SMTP set", got)
	}
}
