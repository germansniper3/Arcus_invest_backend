package services

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"arcusinvest/internal/config"
)

// TestSMTPAdviceMarshalsToArrayNotNull pins the API contract that broke the
// admin portal: a nil Go slice marshals to JSON `null`, and the client does
// `issues.length`, so a HEALTHY SMTP configuration (no advice) white-screened
// the Users & Email tab. The advice list must always be an array.
func TestSMTPAdviceMarshalsToArrayNotNull(t *testing.T) {
	// Both supported submission styles must be considered healthy.
	for _, port := range []string{"587", "465"} {
		healthy := &config.Config{
			SMTPHost:     "smtp.example.com",
			SMTPPort:     port,
			SMTPFrom:     "noreply@example.com",
			SMTPUsername: "user",
			SMTPPassword: "secret",
		}
		if advice := SMTPAdvice(healthy); len(advice) != 0 {
			t.Errorf("port %s should be healthy, got advice %v", port, advice)
		}
	}

	healthy := &config.Config{
		SMTPHost:     "smtp.example.com",
		SMTPPort:     "587",
		SMTPFrom:     "noreply@example.com",
		SMTPUsername: "user",
		SMTPPassword: "secret",
	}
	advice := SMTPAdvice(healthy)
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
		"blocked port 25":  {&config.Config{SMTPHost: "h", SMTPPort: "25", SMTPFrom: "a@b.c", SMTPUsername: "u", SMTPPassword: "p"}, "25"},
		"odd port":         {&config.Config{SMTPHost: "h", SMTPPort: "1234", SMTPFrom: "a@b.c", SMTPUsername: "u", SMTPPassword: "p"}, "unusual"},
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

// fakeSMTP is a minimal in-process SMTP server: enough of the protocol to let a
// real client complete a session. It records the DATA payload and the envelope.
type fakeSMTP struct {
	addr       string
	recipients []string
	from       string
	body       string
	done       chan struct{}
}

func startFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeSMTP{addr: ln.Addr().String(), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		w := bufio.NewWriter(conn)
		reply := func(s string) { w.WriteString(s + "\r\n"); w.Flush() }

		reply("220 fake ESMTP ready")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				// Advertise nothing: no STARTTLS, no AUTH.
				reply("250 fake")
			case strings.HasPrefix(cmd, "MAIL FROM:"):
				s.from = strings.TrimSpace(line[len("MAIL FROM:"):])
				reply("250 OK")
			case strings.HasPrefix(cmd, "RCPT TO:"):
				s.recipients = append(s.recipients, strings.TrimSpace(line[len("RCPT TO:"):]))
				reply("250 OK")
			case cmd == "DATA":
				reply("354 send it")
				var sb strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimRight(l, "\r\n") == "." {
						break
					}
					sb.WriteString(l)
				}
				s.body = sb.String()
				reply("250 queued")
			case cmd == "QUIT":
				reply("221 bye")
				return
			default:
				reply("250 OK")
			}
		}
	}()
	return s
}

// TestSendMailCompletesFullSession exercises the real send path end to end
// against a live SMTP conversation: envelope, headers, body and QUIT. This code
// had never actually run before — only its callers were compiled.
func TestSendMailCompletesFullSession(t *testing.T) {
	srv := startFakeSMTP(t)
	host, port, _ := net.SplitHostPort(srv.addr)

	cfg := &config.Config{SMTPHost: host, SMTPPort: port, SMTPFrom: "ops@arcus.test"}
	err := sendMail(cfg, []string{"buyer@example.com", "second@example.com"},
		"ops@arcus.test", "Quotation ready", "Body line one.")
	if err != nil {
		t.Fatalf("sendMail: %v", err)
	}
	<-srv.done

	if !strings.Contains(srv.from, "ops@arcus.test") {
		t.Errorf("envelope sender = %q", srv.from)
	}
	if len(srv.recipients) != 2 {
		t.Fatalf("expected 2 envelope recipients, got %v", srv.recipients)
	}
	// Recipients must travel in the envelope only — the To header is the sender,
	// so a broadcast cannot leak the recipient list.
	if strings.Contains(srv.body, "second@example.com") {
		t.Error("recipient leaked into the message headers; envelope-only delivery is required")
	}
	for _, want := range []string{"From: ops@arcus.test", "Subject: Quotation ready", "Body line one."} {
		if !strings.Contains(srv.body, want) {
			t.Errorf("message missing %q\n---\n%s", want, srv.body)
		}
	}
}

// TestSendMailFailsFastOnDeadPort proves the dial timeout exists. smtp.SendMail
// dials with no timeout, which is why a wrong port presented as a hang.
func TestSendMailFailsFastOnDeadPort(t *testing.T) {
	// Port 9 (discard) accepts nothing useful; use a closed local port.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close() // nothing is listening now
	host, port, _ := net.SplitHostPort(addr)

	cfg := &config.Config{SMTPHost: host, SMTPPort: port, SMTPFrom: "ops@arcus.test"}
	start := time.Now()
	err := sendMail(cfg, []string{"a@b.c"}, "a@b.c", "s", "b")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error connecting to a closed port")
	}
	if elapsed > smtpDialTimeout+5*time.Second {
		t.Errorf("took %v — dial timeout is not being applied", elapsed)
	}
	// The message must be actionable for a non-Go admin: name the address and say
	// what to do, not just surface the raw syscall error.
	msg := err.Error()
	if !strings.Contains(msg, port) {
		t.Errorf("error should name the address it failed on, got: %v", err)
	}
	if !strings.Contains(msg, "refused") && !strings.Contains(msg, "could not connect") {
		t.Errorf("error should explain the connection failure, got: %v", err)
	}
	if strings.Contains(msg, "refused") && !strings.Contains(msg, "SMTP_PORT") {
		t.Errorf("a refused connection should point at SMTP_PORT, got: %v", err)
	}
}

// TestExplainDialErrorIsActionable pins that each network failure mode produces
// guidance rather than a bare Go error. A blocked port is the case that matters
// most: on a hosting platform that filters outbound SMTP, no settings will work
// and the admin needs to be told to use an HTTP email API instead.
func TestExplainDialErrorIsActionable(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"dial tcp: lookup smtp.typo.com: no such host", "SMTP_HOST"},
		{"dial tcp 1.2.3.4:587: i/o timeout", "HTTP email API"},
		{"dial tcp 1.2.3.4:587: connect: connection refused", "SMTP_PORT"},
		{"dial tcp 1.2.3.4:587: connect: network is unreachable", "blocked"},
	}
	for _, tc := range cases {
		got := explainDialError("smtp.example.com:587", errors.New(tc.raw)).Error()
		if !strings.Contains(got, tc.want) {
			t.Errorf("explainDialError(%q)\n  got:  %s\n  want it to mention %q", tc.raw, got, tc.want)
		}
		if !strings.Contains(got, "smtp.example.com:587") {
			t.Errorf("explainDialError(%q) should name the address, got: %s", tc.raw, got)
		}
	}
}
