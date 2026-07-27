package services

import (
	"encoding/json"
	"strings"
	"testing"

	"arcusinvest/internal/config"
)

// TestSMTPAdviceMarshalsToArrayNotNull pins the API contract that broke the
// admin portal: a nil Go slice marshals to JSON `null`, and the client does
// `issues.length`, so a HEALTHY SMTP configuration (no advice) white-screened
// the Users & Email tab. The advice list must always be an array.
func TestSMTPAdviceMarshalsToArrayNotNull(t *testing.T) {
	healthy := &config.Config{
		SMTPHost:     "smtp.example.com",
		SMTPPort:     "587",
		SMTPFrom:     "noreply@example.com",
		SMTPUsername: "user",
		SMTPPassword: "secret",
	}
	advice := SMTPAdvice(healthy)
	if len(advice) != 0 {
		t.Fatalf("a fully configured SMTP should produce no advice, got %v", advice)
	}
	if advice == nil {
		t.Error("advice is nil — it will marshal to JSON null and crash clients doing issues.length")
	}

	payload, err := json.Marshal(map[string]any{"issues": advice})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(payload); !strings.Contains(got, `"issues":[]`) {
		t.Errorf("issues must serialise as [], got %s", got)
	}
}

// TestSMTPAdviceFlagsMisconfiguration covers the other direction: real problems
// are still reported, including the port 465 case this server cannot use.
func TestSMTPAdviceFlagsMisconfiguration(t *testing.T) {
	cases := map[string]struct {
		cfg  *config.Config
		want string
	}{
		"missing host":     {&config.Config{SMTPPort: "587", SMTPFrom: "a@b.c"}, "SMTP_HOST"},
		"missing from":     {&config.Config{SMTPHost: "h", SMTPPort: "587"}, "SMTP_FROM"},
		"implicit tls 465": {&config.Config{SMTPHost: "h", SMTPPort: "465", SMTPFrom: "a@b.c", SMTPUsername: "u", SMTPPassword: "p"}, "465"},
		"password missing": {&config.Config{SMTPHost: "h", SMTPPort: "587", SMTPFrom: "a@b.c", SMTPUsername: "u"}, "SMTP_PASSWORD"},
	}
	for name, tc := range cases {
		advice := SMTPAdvice(tc.cfg)
		if len(advice) == 0 {
			t.Errorf("%s: expected advice, got none", name)
			continue
		}
		if !strings.Contains(strings.Join(advice, " | "), tc.want) {
			t.Errorf("%s: advice %v should mention %q", name, advice, tc.want)
		}
	}
}
