package auth

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestBuildMessageWritesHeadersAndBody(t *testing.T) {
	body, err := buildMessage("no-reply@example.com", Message{
		To:      "user@example.org",
		Subject: "Confirm your email address",
		Body:    "Use this link:\n\nhttps://gateway.example.com/verify-email?token=abc\n",
	})
	if err != nil {
		t.Fatalf("buildMessage() = %v, want no error", err)
	}

	got := string(body)
	for _, want := range []string{
		"From: no-reply@example.com\r\n",
		"To: user@example.org\r\n",
		"Subject: Confirm your email address\r\n",
		"MIME-Version: 1.0\r\n",
		`Content-Type: text/plain; charset="utf-8"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message is missing %q:\n%s", want, got)
		}
	}

	// Exactly one blank line separates the headers from the body; without it
	// the first body line is parsed as another header.
	headers, text, found := strings.Cut(got, "\r\n\r\n")
	if !found {
		t.Fatalf("no header/body separator:\n%s", got)
	}
	if strings.Contains(headers, "Use this link") {
		t.Error("body leaked into the headers")
	}
	if !strings.HasPrefix(text, "Use this link:") {
		t.Errorf("body = %q, want it to start with the link text", text)
	}
}

// A line break in a header value ends that header and begins another, so an
// unchecked field is a free Bcc: on every message. These are refused rather
// than stripped: silently sending a mangled header hides the bug.
func TestBuildMessageRefusesHeaderInjection(t *testing.T) {
	cases := []struct {
		name string
		msg  Message
		from string
	}{
		{
			name: "injection through the recipient",
			from: "no-reply@example.com",
			msg:  Message{To: "user@example.org\r\nBcc: attacker@evil.example", Subject: "hi", Body: "x"},
		},
		{
			name: "injection through the subject",
			from: "no-reply@example.com",
			msg:  Message{To: "user@example.org", Subject: "hi\r\nBcc: attacker@evil.example", Body: "x"},
		},
		{
			name: "injection through the sender",
			from: "no-reply@example.com\r\nBcc: attacker@evil.example",
			msg:  Message{To: "user@example.org", Subject: "hi", Body: "x"},
		},
		{
			name: "bare newline, not just crlf",
			from: "no-reply@example.com",
			msg:  Message{To: "user@example.org\nBcc: attacker@evil.example", Subject: "hi", Body: "x"},
		},
		{
			name: "empty recipient",
			from: "no-reply@example.com",
			msg:  Message{To: "", Subject: "hi", Body: "x"},
		},
		{
			name: "recipient is not an address",
			from: "no-reply@example.com",
			msg:  Message{To: "not-an-address", Subject: "hi", Body: "x"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildMessage(tc.from, tc.msg); err == nil {
				t.Fatal("buildMessage() = nil error, want a refusal")
			}
		})
	}
}

func TestNewSMTPMailerParsesURLs(t *testing.T) {
	cases := []struct {
		name         string
		url          string
		wantAddr     string
		wantImplicit bool
		wantAuth     bool
	}{
		{"starttls default port", "smtp://smtp.example.com", "smtp.example.com:587", false, false},
		{"implicit tls default port", "smtps://smtp.example.com", "smtp.example.com:465", true, false},
		{"explicit port wins", "smtp://smtp.example.com:2525", "smtp.example.com:2525", false, false},
		{"credentials become auth", "smtp://user:pass@smtp.example.com", "smtp.example.com:587", false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewSMTPMailer(tc.url, "no-reply@example.com")
			if err != nil {
				t.Fatalf("NewSMTPMailer(%q) = %v, want no error", tc.url, err)
			}
			if m.addr != tc.wantAddr {
				t.Errorf("addr = %q, want %q", m.addr, tc.wantAddr)
			}
			if m.implicit != tc.wantImplicit {
				t.Errorf("implicit = %v, want %v", m.implicit, tc.wantImplicit)
			}
			if (m.auth != nil) != tc.wantAuth {
				t.Errorf("auth set = %v, want %v", m.auth != nil, tc.wantAuth)
			}
		})
	}
}

func TestNewSMTPMailerRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		url  string
		from string
	}{
		{"wrong scheme", "https://smtp.example.com", "no-reply@example.com"},
		{"no scheme", "smtp.example.com:587", "no-reply@example.com"},
		{"no host", "smtp://", "no-reply@example.com"},
		{"sender is not an address", "smtp://smtp.example.com", "no-reply"},
		{"sender carries a line break", "smtp://smtp.example.com", "a@b.com\r\nBcc: c@d.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSMTPMailer(tc.url, tc.from); err == nil {
				t.Fatalf("NewSMTPMailer(%q, %q) = nil error, want a refusal", tc.url, tc.from)
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"localhost", "LOCALHOST", "127.0.0.1", "::1"} {
		if !isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"smtp.example.com", "10.0.0.1", "198.51.100.7"} {
		if isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = true, want false", host)
		}
	}
}

// The SMTP URL carries the relay password, and url.Parse echoes the whole
// string in its errors, so an error built around it would put the password in
// the startup log.
func TestNewSMTPMailerErrorsDoNotLeakCredentials(t *testing.T) {
	const password = "s3cretPW"
	for _, raw := range []string{
		"smtp://user:" + password + "@smtp.example.com:58x7", // invalid port
		"smtp://user:" + password + "@smtp.example.com/\x7f", // control character
		"smtp://user:" + password + "%zz@smtp.example.com",   // bad escape
		"https://user:" + password + "@smtp.example.com",     // wrong scheme
	} {
		_, err := NewSMTPMailer(raw, "no-reply@example.com")
		if err == nil {
			t.Fatalf("NewSMTPMailer(%q) = nil error, want a refusal", raw)
		}
		if strings.Contains(err.Error(), password) {
			t.Errorf("error leaks the password: %v", err)
		}
	}
}

// A relay's refusal usually quotes the recipient, and the caller logs the error.
func TestSendErrorsDoNotLeakRecipient(t *testing.T) {
	const recipient = "private.person@example.org"

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		_, _ = fmt.Fprint(conn, "220 localhost ready\r\n")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			reply := "250 OK\r\n"
			if strings.HasPrefix(line, "RCPT") {
				reply = "550 <" + recipient + "> mailbox does not exist\r\n"
			}
			_, _ = fmt.Fprint(conn, reply)
		}
	}()

	m, err := NewSMTPMailer("smtp://"+ln.Addr().String(), "no-reply@example.com")
	if err != nil {
		t.Fatal(err)
	}
	err = m.Send(context.Background(), Message{To: recipient, Subject: "hi", Body: "x"})
	if err == nil {
		t.Fatal("Send() = nil error, want the relay's refusal")
	}
	if strings.Contains(err.Error(), recipient) {
		t.Errorf("error leaks the recipient: %v", err)
	}
	if !strings.Contains(err.Error(), "550") {
		t.Errorf("error = %v, want the reply code kept for diagnosis", err)
	}
}

// A dial failure from net carries the relay's address ("dial tcp host:port"),
// and the relay host identifies the operator's mail provider and domain.
func TestSendDialErrorDoesNotLeakRelayAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens now, so the dial is refused

	m, err := NewSMTPMailer("smtp://"+addr, "no-reply@example.com")
	if err != nil {
		t.Fatal(err)
	}
	err = m.Send(context.Background(), Message{To: "user@example.org", Subject: "hi", Body: "x"})
	if err == nil {
		t.Fatal("Send() = nil error, want a dial failure")
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("error leaks the relay address: %v", err)
	}
}
