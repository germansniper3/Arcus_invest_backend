package services

import (
	"arcusinvest/internal/config"
	"bytes"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// sendMail delivers one plain-text message. Recipients are supplied only in the
// SMTP envelope; the caller decides what the visible To header says.
//
// NOTE: net/smtp speaks STARTTLS, which is port 587. Implicit-TLS submission
// (port 465) is NOT supported and will hang or fail — SMTPAdvice() surfaces that.
func sendMail(cfg *config.Config, recipients []string, toHeader, subject, body string) error {
	if !cfg.SMTPConfigured() {
		return fmt.Errorf("smtp is not configured")
	}
	if len(recipients) == 0 {
		return fmt.Errorf("no recipients")
	}
	// Strip CR/LF from header values to prevent header injection.
	scrub := strings.NewReplacer("\r", " ", "\n", " ")

	var msg bytes.Buffer
	fmt.Fprintf(&msg, "From: %s\r\n", scrub.Replace(cfg.SMTPFrom))
	fmt.Fprintf(&msg, "To: %s\r\n", scrub.Replace(toHeader))
	fmt.Fprintf(&msg, "Subject: %s\r\n", scrub.Replace(subject))
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(body)
	msg.WriteString("\r\n")

	addr := net.JoinHostPort(cfg.SMTPHost, cfg.SMTPPort)
	var auth smtp.Auth
	if cfg.SMTPUsername != "" {
		auth = smtp.PlainAuth("", cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPHost)
	}
	return smtp.SendMail(addr, auth, cfg.SMTPFrom, recipients, msg.Bytes())
}

// SMTPAdvice returns human-readable problems with the current SMTP settings:
// which required fields are missing, and configuration that cannot work with
// this STARTTLS-only implementation. An empty slice means the settings look sane
// (it does not prove delivery works — use SendTestEmail for that).
func SMTPAdvice(cfg *config.Config) []string {
	var advice []string
	if cfg.SMTPHost == "" {
		advice = append(advice, "SMTP_HOST is not set")
	}
	if cfg.SMTPPort == "" {
		advice = append(advice, "SMTP_PORT is not set (use 587)")
	}
	if cfg.SMTPFrom == "" {
		advice = append(advice, "SMTP_FROM is not set — it must be a full address the provider allows you to send from")
	}
	if cfg.SMTPPort == "465" {
		advice = append(advice, "SMTP_PORT is 465 (implicit TLS), which this server cannot use — switch to 587 (STARTTLS)")
	}
	if cfg.SMTPHost != "" && cfg.SMTPUsername == "" {
		advice = append(advice, "SMTP_USERNAME is empty, so the connection is unauthenticated — most providers reject that")
	}
	if cfg.SMTPUsername != "" && cfg.SMTPPassword == "" {
		advice = append(advice, "SMTP_USERNAME is set but SMTP_PASSWORD is empty")
	}
	return advice
}

// SendTestEmail sends a diagnostic message to a single address (the requesting
// admin's own), so SMTP can be verified without emailing students.
func SendTestEmail(cfg *config.Config, to string) error {
	body := "This is a test message from your Arcus Investments admin portal.\r\n\r\n" +
		"If you are reading this, SMTP delivery is working: invitation emails and event broadcasts will be sent.\r\n"
	return sendMail(cfg, []string{to}, to, "Arcus Investments — SMTP test", body)
}

// SendInvitationEmail emails a student their onboarding claim link.
func SendInvitationEmail(cfg *config.Config, to, fullName, tier, claimURL string, expires time.Time) error {
	greeting := "Hello"
	if n := strings.TrimSpace(fullName); n != "" {
		greeting = "Hello " + n
	}
	tierLine := ""
	if t := strings.TrimSpace(tier); t != "" {
		tierLine = fmt.Sprintf("Your enrollment for the %s tier has been accepted.\r\n", t)
	}
	body := fmt.Sprintf(
		"%s,\r\n\r\n%s\r\nUse the link below to set your password and activate your Arcus Innovation Hub student portal:\r\n\r\n%s\r\n\r\n"+
			"This link is personal to you, can be used once, and expires on %s.\r\n\r\n"+
			"If you were not expecting this, you can ignore this message.\r\n\r\n— Arcus Investments\r\n",
		greeting, tierLine, claimURL, expires.Format("2 January 2006"),
	)
	return sendMail(cfg, []string{to}, to, "Your Arcus Innovation Hub onboarding link", body)
}

// SendBroadcastEmail sends a single message to many recipients without leaking
// the recipient list in the message headers: recipients are supplied only in
// the SMTP envelope (RCPT TO), i.e. BCC-style. The visible To header is the
// sender address itself.
//
// It returns an error if SMTP is not usable or delivery fails; callers decide
// how to reflect that in the broadcast status.
func SendBroadcastEmail(cfg *config.Config, recipients []string, subject, message string) error {
	// The visible To header is the sender itself; real recipients stay in the
	// envelope only, i.e. BCC-style.
	return sendMail(cfg, recipients, cfg.SMTPFrom, subject, message)
}
