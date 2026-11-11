package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// Default ports for the two ways a relay accepts mail. 587 upgrades a plain
// connection with STARTTLS and is what every hosted provider documents; 465
// is TLS from the first byte.
const (
	smtpStartTLSPort = "587"
	smtpImplicitPort = "465"

	// smtpTimeout bounds the whole exchange when the caller's context carries
	// no deadline. A relay that has stopped answering must not hold a request
	// goroutine, and both callers send mail while a user waits on a response.
	smtpTimeout = 10 * time.Second
)

// SMTPMailer delivers mail through an SMTP relay. It holds no connection: each
// Send dials, delivers and quits, which costs a handshake per message and in
// exchange has no pool to go stale between the two messages this service sends
// in a day.
type SMTPMailer struct {
	addr     string
	host     string
	auth     smtp.Auth
	implicit bool
	from     string
}

// NewSMTPMailer builds a mailer from an SMTP URL:
//
//	smtp://user:password@smtp.example.com:587   STARTTLS
//	smtps://user:password@smtp.example.com:465  implicit TLS
//
// The password is URL-escaped like any other userinfo, so a password
// containing @ or / must be percent-encoded.
func NewSMTPMailer(rawURL, from string) (*SMTPMailer, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		// The parse error is dropped, not wrapped: url.Parse quotes the whole
		// input, password included, and this error ends up in the startup log.
		return nil, errors.New("smtp url is not a valid URL")
	}

	var implicit bool
	switch u.Scheme {
	case "smtp":
	case "smtps":
		implicit = true
	default:
		return nil, fmt.Errorf("smtp url: scheme must be smtp or smtps, got %q", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return nil, errors.New("smtp url: no host")
	}
	port := u.Port()
	if port == "" {
		port = smtpStartTLSPort
		if implicit {
			port = smtpImplicitPort
		}
	}

	if _, err := validAddress(from); err != nil {
		return nil, fmt.Errorf("smtp from address: %w", err)
	}

	m := &SMTPMailer{
		addr:     net.JoinHostPort(host, port),
		host:     host,
		implicit: implicit,
		from:     from,
	}
	if user := u.User; user != nil {
		password, _ := user.Password()
		// PlainAuth refuses to hand credentials to a server that is not using
		// TLS, unless the host is localhost. That check is the stdlib's, and
		// it is the reason this code does not need one of its own.
		m.auth = smtp.PlainAuth("", user.Username(), password, host)
	}
	return m, nil
}

// smtpReply reduces a server refusal to its numeric code. The reply text is the
// relay's own words and routinely quotes the recipient ("550 <a@b.c> does not
// exist"), which would put an address in the log.
func smtpReply(step string, err error) error {
	var tp *textproto.Error
	if errors.As(err, &tp) {
		return fmt.Errorf("%s: server replied %d", step, tp.Code)
	}
	return fmt.Errorf("%s: %w", step, err)
}

// dialFailure names what kind of dial failure happened without the net
// error's text, which carries the relay's host and address.
func dialFailure(err error) error {
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("dial smtp relay: %w", context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("dial smtp relay: %w", context.DeadlineExceeded)
	case errors.As(err, &dnsErr):
		return errors.New("dial smtp relay: host name lookup failed")
	case errors.As(err, &netErr) && netErr.Timeout():
		return errors.New("dial smtp relay: timed out")
	default:
		return errors.New("dial smtp relay: connection failed")
	}
}

// Send delivers one message.
func (m *SMTPMailer) Send(ctx context.Context, msg Message) error {
	body, err := buildMessage(m.from, msg)
	if err != nil {
		return err
	}
	recipient, err := validAddress(msg.To)
	if err != nil {
		return fmt.Errorf("smtp recipient: %w", err)
	}

	dialer := &net.Dialer{Timeout: smtpTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", m.addr)
	if err != nil {
		return dialFailure(err)
	}
	defer func() { _ = conn.Close() }()

	// One deadline for the whole exchange. net/smtp takes no context, so this
	// is what makes a wedged relay fail instead of hanging, and what honours
	// the caller's own deadline when it has one.
	deadline := time.Now().Add(smtpTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("set smtp deadline: %w", err)
	}

	if m.implicit {
		conn = tls.Client(conn, &tls.Config{ServerName: m.host})
	}

	client, err := smtp.NewClient(conn, m.host)
	if err != nil {
		return smtpReply("smtp greeting", err)
	}
	defer func() { _ = client.Close() }()

	if !m.implicit {
		ok, _ := client.Extension("STARTTLS")
		switch {
		case ok:
			if err := client.StartTLS(&tls.Config{ServerName: m.host}); err != nil {
				return fmt.Errorf("smtp starttls: %w", err)
			}
		case !isLoopbackHost(m.host):
			// A relay on the public internet that will not encrypt is refused
			// rather than used: the messages this service sends are a password
			// reset link and an address-confirmation link, both bearer secrets
			// for anyone on the path. A local test relay is allowed to be
			// plaintext because there is no path.
			return errors.New("smtp relay does not offer STARTTLS")
		}
	}

	if m.auth != nil {
		if err := client.Auth(m.auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := client.Mail(m.from); err != nil {
		return smtpReply("smtp mail from", err)
	}
	if err := client.Rcpt(recipient); err != nil {
		return smtpReply("smtp rcpt to", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("smtp write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close body: %w", err)
	}
	return client.Quit()
}

// buildMessage renders an RFC 5322 message. The Data writer net/smtp returns
// handles dot-stuffing and lone-newline conversion, so the body travels as
// written.
func buildMessage(from string, msg Message) ([]byte, error) {
	if _, err := validAddress(from); err != nil {
		return nil, fmt.Errorf("smtp from address: %w", err)
	}
	if _, err := validAddress(msg.To); err != nil {
		return nil, fmt.Errorf("smtp recipient: %w", err)
	}
	if strings.ContainsAny(msg.Subject, "\r\n") {
		// A newline in a header value ends the header and starts another, so
		// an unchecked subject is a free Bcc: on every message this service
		// sends. The subjects here are constants today, which is exactly why
		// this is cheap to assert now rather than after one of them is not.
		return nil, errors.New("smtp subject contains a line break")
	}

	var b strings.Builder
	write := func(name, value string) {
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(value)
		b.WriteString("\r\n")
	}
	write("From", from)
	write("To", msg.To)
	write("Subject", msg.Subject)
	write("Date", time.Now().UTC().Format(time.RFC1123Z))
	write("MIME-Version", "1.0")
	write("Content-Type", `text/plain; charset="utf-8"`)
	write("Content-Transfer-Encoding", "8bit")
	b.WriteString("\r\n")
	b.WriteString(msg.Body)
	return []byte(b.String()), nil
}

// validAddress accepts the addr-spec shapes this service actually sends to and
// refuses anything that could break out of a header. It is deliberately not a
// full RFC 5322 parser: the recipient has already been through validateEmail
// in the control plane, and the relay is the authority on deliverability.
func validAddress(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	switch {
	case addr == "":
		return "", errors.New("is empty")
	case strings.ContainsAny(addr, "\r\n"):
		return "", errors.New("contains a line break")
	case strings.Count(addr, "@") != 1:
		return "", fmt.Errorf("%q is not an email address", addr)
	}
	local, domain, _ := strings.Cut(addr, "@")
	if local == "" || domain == "" {
		return "", fmt.Errorf("%q is not an email address", addr)
	}
	return addr, nil
}

// isLoopbackHost reports whether a host names this machine, which is the one
// place a plaintext relay is acceptable.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
