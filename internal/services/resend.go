package services

import (
	"arcusinvest/internal/config"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// resendEndpoint is a var, not a const, so tests can point it at a local server.
var resendEndpoint = "https://api.resend.com/emails"

const (
	resendTimeout = 30 * time.Second
	// Resend rejects a request carrying more than 50 addresses across to+cc+bcc,
	// so broadcasts are split into batches rather than failing wholesale.
	resendMaxRecipients = 50
)

type resendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Bcc     []string `json:"bcc,omitempty"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
}

// resendError is the provider's error envelope. Both shapes seen in the wild are
// covered: {"message":...,"name":...} and {"error":{"message":...}}.
type resendError struct {
	Message string `json:"message"`
	Name    string `json:"name"`
	Error   struct {
		Message string `json:"message"`
	} `json:"error"`
}

// sendViaResend delivers one message over Resend's HTTPS API.
//
// It preserves the BCC semantics of the SMTP path: when the visible To header is
// not the recipient (a broadcast), the real recipients travel as bcc so the list
// never leaks between students.
func sendViaResend(cfg *config.Config, recipients []string, toHeader, subject, body string) error {
	// A message to exactly the person named in the To header needs no bcc.
	if len(recipients) == 1 && strings.EqualFold(recipients[0], toHeader) {
		return postResend(cfg, resendRequest{
			From: cfg.MailFrom, To: []string{toHeader}, Subject: subject, Text: body,
		})
	}

	// Broadcast: To is the sender, recipients are hidden in bcc. Batch so that a
	// cohort larger than the provider's per-request cap still goes out.
	const perBatch = resendMaxRecipients - 1 // one slot is taken by the To address
	for start := 0; start < len(recipients); start += perBatch {
		end := start + perBatch
		if end > len(recipients) {
			end = len(recipients)
		}
		err := postResend(cfg, resendRequest{
			From: cfg.MailFrom, To: []string{toHeader}, Bcc: recipients[start:end],
			Subject: subject, Text: body,
		})
		if err != nil {
			return fmt.Errorf("recipients %d-%d of %d: %w", start+1, end, len(recipients), err)
		}
	}
	return nil
}

func postResend(cfg *config.Config, payload resendRequest) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("could not encode the message: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, resendEndpoint, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("could not build the Resend request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.ResendAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: resendTimeout}).Do(req)
	if err != nil {
		return fmt.Errorf("could not reach the Resend API (check outbound HTTPS): %w", err)
	}
	defer resp.Body.Close()

	// Cap the read: an error page from a proxy could be arbitrarily large.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return explainResendError(cfg, resp.StatusCode, raw)
}

// explainResendError turns a provider rejection into guidance an admin can act
// on. The two that actually happen in practice are an unverified sending domain
// and a bad API key, and neither is obvious from the raw JSON.
func explainResendError(cfg *config.Config, status int, raw []byte) error {
	detail := strings.TrimSpace(string(raw))
	var parsed resendError
	if json.Unmarshal(raw, &parsed) == nil {
		if parsed.Message != "" {
			detail = parsed.Message
		} else if parsed.Error.Message != "" {
			detail = parsed.Error.Message
		}
	}

	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return fmt.Errorf("Resend rejected the API key (HTTP %d) — check RESEND_API_KEY: %s", status, detail)
	case status == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(detail), "domain"):
		return fmt.Errorf("Resend will not send from %s — verify that domain at resend.com/domains, "+
			"or set MAIL_FROM to onboarding@resend.dev for testing (which can only reach your own account address): %s",
			cfg.MailFrom, detail)
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("Resend rate limit reached — wait and retry: %s", detail)
	case status >= 500:
		return fmt.Errorf("Resend is failing (HTTP %d) — this is on the provider's side, retry shortly: %s", status, detail)
	}
	return fmt.Errorf("Resend refused the message (HTTP %d): %s", status, detail)
}
