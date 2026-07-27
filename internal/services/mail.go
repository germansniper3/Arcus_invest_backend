package services

import (
	"arcusinvest/internal/config"
	"arcusinvest/internal/models"
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

const (
	// Fail fast and legibly. smtp.SendMail dials with no timeout at all, so a
	// blocked port or a TLS-mode mismatch hangs until the OS gives up — which is
	// what "it timed out" looks like from the admin portal.
	smtpDialTimeout    = 15 * time.Second
	smtpSessionTimeout = 45 * time.Second
)

// dialSMTP opens an authenticated-capable SMTP client, handling BOTH submission
// styles:
//
//   - port 465  — implicit TLS: the connection is TLS from the first byte.
//   - otherwise — plaintext connect, then upgrade with STARTTLS (port 587).
//
// Getting this wrong deadlocks rather than erroring: on 465 a plaintext client
// waits for a greeting while the server waits for a TLS handshake, so the
// symptom is a timeout, not a rejection.
// explainDialError turns a Go network error into something a non-Go admin can
// act on. Connection failures at this layer are almost never the mail settings
// themselves — they are DNS, a typo, or the platform blocking outbound SMTP.
func explainDialError(addr string, err error) error {
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "no such host"):
		return fmt.Errorf("cannot resolve the mail server hostname (%s) — check SMTP_HOST for a typo: %w", addr, err)
	case strings.Contains(text, "timeout"), strings.Contains(text, "i/o timeout"), strings.Contains(text, "deadline exceeded"):
		return fmt.Errorf("timed out connecting to %s — the connection is being blocked, "+
			"most often because the hosting platform blocks outbound SMTP. Try port 587 or 465, "+
			"and if both time out use an HTTP email API (Resend/SendGrid/Mailgun) instead of SMTP: %w", addr, err)
	case strings.Contains(text, "refused"):
		return fmt.Errorf("connection refused by %s — nothing is accepting mail on that port; check SMTP_PORT (587 or 465): %w", addr, err)
	case strings.Contains(text, "network is unreachable"), strings.Contains(text, "no route to host"):
		return fmt.Errorf("no network route to %s — outbound access to this host is blocked: %w", addr, err)
	}
	return fmt.Errorf("could not connect to %s: %w", addr, err)
}

func dialSMTP(cfg *config.Config) (*smtp.Client, error) {
	addr := net.JoinHostPort(cfg.SMTPHost, cfg.SMTPPort)
	dialer := &net.Dialer{Timeout: smtpDialTimeout}
	tlsCfg := &tls.Config{ServerName: cfg.SMTPHost, MinVersion: tls.VersionTLS12}

	if cfg.SMTPPort == "465" {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
		if err != nil {
			return nil, explainDialError(addr, err)
		}
		_ = conn.SetDeadline(time.Now().Add(smtpSessionTimeout))
		client, err := smtp.NewClient(conn, cfg.SMTPHost)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("smtp handshake failed on %s: %w", addr, err)
		}
		return client, nil
	}

	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, explainDialError(addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(smtpSessionTimeout))
	client, err := smtp.NewClient(conn, cfg.SMTPHost)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("smtp handshake failed on %s: %w", addr, err)
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(tlsCfg); err != nil {
			client.Close()
			return nil, fmt.Errorf("STARTTLS failed on %s: %w", addr, err)
		}
	}
	return client, nil
}

// sendMail delivers one plain-text message. Recipients are supplied only in the
// SMTP envelope; the caller decides what the visible To header says.
func sendMail(cfg *config.Config, recipients []string, toHeader, subject, body string) error {
	if !cfg.MailConfigured() {
		return fmt.Errorf("no email transport is configured")
	}
	if len(recipients) == 0 {
		return fmt.Errorf("no recipients")
	}

	// Prefer the HTTPS API when it is available: it is the only transport that
	// works on hosts that block outbound SMTP ports. It builds its own payload
	// rather than a wire message, so it returns before the header assembly below.
	if cfg.ResendConfigured() {
		return sendViaResend(cfg, recipients, toHeader, subject, body)
	}

	// Strip CR/LF from header values to prevent header injection.
	scrub := strings.NewReplacer("\r", " ", "\n", " ")

	var msg bytes.Buffer
	fmt.Fprintf(&msg, "From: %s\r\n", scrub.Replace(cfg.MailFrom))
	fmt.Fprintf(&msg, "To: %s\r\n", scrub.Replace(toHeader))
	fmt.Fprintf(&msg, "Subject: %s\r\n", scrub.Replace(subject))
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(body)
	msg.WriteString("\r\n")

	client, err := dialSMTP(cfg)
	if err != nil {
		return err
	}
	defer client.Close()

	if cfg.SMTPUsername != "" {
		if ok, _ := client.Extension("AUTH"); !ok {
			return fmt.Errorf("server at %s:%s does not offer AUTH (is the port or TLS mode right?)", cfg.SMTPHost, cfg.SMTPPort)
		}
		if err := client.Auth(smtp.PlainAuth("", cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPHost)); err != nil {
			return fmt.Errorf("authentication failed for %s: %w", cfg.SMTPUsername, err)
		}
	}
	if err := client.Mail(cfg.MailFrom); err != nil {
		return fmt.Errorf("sender %s rejected: %w", cfg.MailFrom, err)
	}
	for _, rcpt := range recipients {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("recipient %s rejected: %w", rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("server refused the message body: %w", err)
	}
	if _, err := w.Write(msg.Bytes()); err != nil {
		return fmt.Errorf("writing message failed: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("server rejected the message: %w", err)
	}
	return client.Quit()
}

// MailAdvice returns human-readable problems with the current email settings,
// checked against whichever transport is actually in play. An empty slice means
// the settings look sane (it does not prove delivery works — use SendTestEmail
// for that).
func MailAdvice(cfg *config.Config) []string {
	// Must be an empty slice, never nil: a nil slice marshals to JSON `null`,
	// and clients that do `issues.length` would crash on a healthy configuration.
	advice := []string{}

	// With an API key present, SMTP settings are inert — auditing them would
	// report problems that cannot affect delivery.
	if cfg.ResendAPIKey != "" {
		if cfg.MailFrom == "" {
			advice = append(advice, "MAIL_FROM is not set — it must be an address on a domain you have verified with Resend")
		}
		if !strings.Contains(cfg.MailFrom, "@") && cfg.MailFrom != "" {
			advice = append(advice, "MAIL_FROM is not a full email address")
		}
		if strings.HasSuffix(strings.ToLower(cfg.MailFrom), "@resend.dev") {
			advice = append(advice, "MAIL_FROM uses resend.dev, which can only deliver to your own Resend account address — "+
				"verify your domain at resend.com/domains before inviting students")
		}
		return advice
	}

	if cfg.SMTPHost == "" {
		advice = append(advice, "SMTP_HOST is not set")
	}
	if cfg.SMTPPort == "" {
		advice = append(advice, "SMTP_PORT is not set (use 587)")
	}
	if cfg.MailFrom == "" {
		advice = append(advice, "MAIL_FROM (or SMTP_FROM) is not set — it must be a full address the provider allows you to send from")
	}
	// 465 (implicit TLS) and 587 (STARTTLS) are both supported. Port 25 is
	// outbound-blocked by most hosting providers, which presents as a timeout
	// rather than a rejection.
	if cfg.SMTPPort == "25" {
		advice = append(advice, "SMTP_PORT is 25, which most hosts block outbound — use 587 (STARTTLS) or 465 (implicit TLS)")
	}
	if p := cfg.SMTPPort; p != "" && p != "25" && p != "465" && p != "587" && p != "2525" {
		advice = append(advice, "SMTP_PORT is "+p+", an unusual submission port — 587 or 465 are expected")
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
	return sendMail(cfg, recipients, cfg.MailFrom, subject, message)
}

// SendNotificationEmail sends one notification to one person.
func SendNotificationEmail(cfg *config.Config, to, title, body string) error {
	message := fmt.Sprintf(
		"%s\r\n\r\n%s\r\n\r\nYou can change how often you get these in the admin portal.\r\n\r\n— Arcus Investments\r\n",
		title, body,
	)
	return sendMail(cfg, []string{to}, to, "Arcus: "+title, message)
}

// SendNotificationDigestEmail sends one message covering everything currently
// outstanding for a person, which is the default because a message per event
// gets muted within a week.
func SendNotificationDigestEmail(cfg *config.Config, to, fullName string, items []models.Notification) error {
	if len(items) == 0 {
		return nil
	}
	greeting := "Hello"
	if strings.TrimSpace(fullName) != "" {
		greeting = "Hi " + strings.TrimSpace(fullName)
	}

	var lines strings.Builder
	for _, n := range items {
		fmt.Fprintf(&lines, "· %s\r\n  %s\r\n\r\n", n.Title, n.Body)
	}

	noun := "items need"
	if len(items) == 1 {
		noun = "item needs"
	}
	message := fmt.Sprintf(
		"%s,\r\n\r\n%d %s your attention:\r\n\r\n%sOpen the admin portal to act on these.\r\n\r\n"+
			"You can switch to a message per event, or turn these off, in the admin portal.\r\n\r\n— Arcus Investments\r\n",
		greeting, len(items), noun, lines.String(),
	)
	return sendMail(cfg, []string{to}, to, fmt.Sprintf("Arcus: %d %s your attention", len(items), noun), message)
}
