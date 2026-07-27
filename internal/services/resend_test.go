package services

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"arcusinvest/internal/config"
)

// fakeResend stands in for the provider API and records what was posted.
type fakeResend struct {
	srv      *httptest.Server
	requests []resendRequest
	auth     string
	status   int
	body     string
}

func startFakeResend(t *testing.T) *fakeResend {
	t.Helper()
	f := &fakeResend{status: http.StatusOK, body: `{"id":"abc-123"}`}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		var req resendRequest
		_ = json.Unmarshal(raw, &req)
		f.requests = append(f.requests, req)
		w.WriteHeader(f.status)
		io.WriteString(w, f.body)
	}))

	// Point the transport at the fake for the duration of the test.
	original := resendEndpoint
	resendEndpoint = f.srv.URL
	t.Cleanup(func() {
		resendEndpoint = original
		f.srv.Close()
	})
	return f
}

func resendCfg(key string) *config.Config {
	return &config.Config{ResendAPIKey: key, MailFrom: "ops@arcus.test"}
}

// TestResendIsPreferredOverSMTP pins the routing rule that makes this whole
// change work: with an API key set, mail must NOT touch SMTP. On Railway below
// the Pro plan the SMTP ports are blocked, so falling back would time out.
func TestResendIsPreferredOverSMTP(t *testing.T) {
	fake := startFakeResend(t)

	cfg := resendCfg("re_test_key")
	// Deliberately point SMTP at a dead port: if it is used at all, this fails.
	cfg.SMTPHost, cfg.SMTPPort = "127.0.0.1", "1"

	if err := sendMail(cfg, []string{"student@example.com"}, "student@example.com", "Subject", "Body"); err != nil {
		t.Fatalf("sendMail: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("expected 1 API call, got %d", len(fake.requests))
	}
	if fake.auth != "Bearer re_test_key" {
		t.Errorf("Authorization header = %q", fake.auth)
	}
	got := fake.requests[0]
	if got.From != "ops@arcus.test" || got.Subject != "Subject" || got.Text != "Body" {
		t.Errorf("payload = %+v", got)
	}
	if len(got.To) != 1 || got.To[0] != "student@example.com" {
		t.Errorf("To = %v, want the single recipient", got.To)
	}
	if len(got.Bcc) != 0 {
		t.Errorf("a direct message must not use bcc, got %v", got.Bcc)
	}
}

// TestResendBroadcastHidesRecipients is the privacy contract carried over from
// the SMTP path: students must never see each other's addresses.
func TestResendBroadcastHidesRecipients(t *testing.T) {
	fake := startFakeResend(t)
	cfg := resendCfg("re_test_key")

	recipients := []string{"a@example.com", "b@example.com", "c@example.com"}
	if err := SendBroadcastEmail(cfg, recipients, "Event", "Details"); err != nil {
		t.Fatalf("SendBroadcastEmail: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("expected 1 API call, got %d", len(fake.requests))
	}
	got := fake.requests[0]
	if len(got.To) != 1 || got.To[0] != cfg.MailFrom {
		t.Errorf("To = %v, want only the sender so the list stays hidden", got.To)
	}
	if len(got.Bcc) != 3 {
		t.Errorf("Bcc = %v, want all 3 recipients", got.Bcc)
	}
}

// TestResendBatchesLargeBroadcasts covers the provider's 50-address-per-request
// cap: a cohort bigger than that must still go out in full rather than erroring.
func TestResendBatchesLargeBroadcasts(t *testing.T) {
	fake := startFakeResend(t)
	cfg := resendCfg("re_test_key")

	recipients := make([]string, 120)
	for i := range recipients {
		recipients[i] = string(rune('a'+i%26)) + "-student@example.com"
	}
	if err := SendBroadcastEmail(cfg, recipients, "Event", "Details"); err != nil {
		t.Fatalf("SendBroadcastEmail: %v", err)
	}

	total := 0
	for _, req := range fake.requests {
		if n := len(req.To) + len(req.Bcc); n > resendMaxRecipients {
			t.Errorf("a batch carried %d addresses, over the %d cap", n, resendMaxRecipients)
		}
		total += len(req.Bcc)
	}
	if total != len(recipients) {
		t.Errorf("delivered to %d addresses, want %d", total, len(recipients))
	}
}

// TestResendErrorsAreActionable pins that a provider rejection names the fix.
// An unverified sending domain is the failure every new Resend account hits.
func TestResendErrorsAreActionable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"bad key", http.StatusUnauthorized, `{"message":"API key is invalid"}`, "RESEND_API_KEY"},
		{"unverified domain", http.StatusUnprocessableEntity,
			`{"message":"The arcus.test domain is not verified"}`, "resend.com/domains"},
		{"rate limited", http.StatusTooManyRequests, `{"message":"too many"}`, "rate limit"},
		{"provider down", http.StatusBadGateway, `{"message":"upstream"}`, "provider's side"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := startFakeResend(t)
			fake.status, fake.body = tc.status, tc.body

			err := sendMail(resendCfg("re_key"), []string{"a@b.c"}, "a@b.c", "s", "b")
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

// TestMailAdviceSkipsSMTPChecksOnResend guards against the admin portal showing
// "SMTP_HOST is not set" as a problem when mail is going out fine over HTTPS.
func TestMailAdviceSkipsSMTPChecksOnResend(t *testing.T) {
	cfg := resendCfg("re_key") // no SMTP settings at all
	if advice := MailAdvice(cfg); len(advice) != 0 {
		t.Errorf("a working Resend config should report no issues, got %v", advice)
	}

	// The testing sender is a real trap: it silently only reaches your own address.
	cfg.MailFrom = "onboarding@resend.dev"
	if advice := strings.Join(MailAdvice(cfg), " | "); !strings.Contains(advice, "resend.dev") {
		t.Errorf("resend.dev sender should be flagged, got %v", advice)
	}
}

// TestMailTransportSelection pins which transport each configuration resolves to.
func TestMailTransportSelection(t *testing.T) {
	smtpOnly := &config.Config{SMTPHost: "h", SMTPPort: "587", MailFrom: "a@b.c"}
	both := &config.Config{SMTPHost: "h", SMTPPort: "587", MailFrom: "a@b.c", ResendAPIKey: "re_key"}
	none := &config.Config{}
	// A key without a sender address cannot send: it must not claim the transport.
	keyOnly := &config.Config{ResendAPIKey: "re_key"}

	for _, tc := range []struct {
		cfg  *config.Config
		want string
	}{
		{smtpOnly, "smtp"}, {both, "resend"}, {none, "none"}, {keyOnly, "none"},
	} {
		if got := tc.cfg.MailTransport(); got != tc.want {
			t.Errorf("MailTransport() = %q, want %q for %+v", got, tc.want, tc.cfg)
		}
	}
}
