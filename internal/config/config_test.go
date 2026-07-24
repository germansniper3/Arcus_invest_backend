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
