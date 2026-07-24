package services

import (
	"arcusinvest/internal/config"
	"bytes"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

// SendBroadcastEmail sends a single message to many recipients without leaking
// the recipient list in the message headers: recipients are supplied only in
// the SMTP envelope (RCPT TO), i.e. BCC-style. The visible To header is the
// sender address itself.
//
// It returns an error if SMTP is not usable or delivery fails; callers decide
// how to reflect that in the broadcast status.
func SendBroadcastEmail(cfg *config.Config, recipients []string, subject, message string) error {
	if !cfg.SMTPConfigured() {
		return fmt.Errorf("smtp is not configured")
	}
	if len(recipients) == 0 {
		return fmt.Errorf("no recipients")
	}

	from := cfg.SMTPFrom
	// Strip CR/LF to prevent header injection via the subject line.
	safeSubject := strings.NewReplacer("\r", " ", "\n", " ").Replace(subject)

	var body bytes.Buffer
	fmt.Fprintf(&body, "From: %s\r\n", from)
	fmt.Fprintf(&body, "To: %s\r\n", from) // recipients are BCC via the envelope only
	fmt.Fprintf(&body, "Subject: %s\r\n", safeSubject)
	body.WriteString("MIME-Version: 1.0\r\n")
	body.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	body.WriteString("\r\n")
	body.WriteString(message)
	body.WriteString("\r\n")

	addr := net.JoinHostPort(cfg.SMTPHost, cfg.SMTPPort)
	var auth smtp.Auth
	if cfg.SMTPUsername != "" {
		auth = smtp.PlainAuth("", cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPHost)
	}
	return smtp.SendMail(addr, auth, from, recipients, body.Bytes())
}
