package auth

import (
	"context"
	"log/slog"
)

// Mailer delivers the two transactional messages this service sends. It is an
// interface with one method so a real provider can be dropped in without any
// handler changing; nothing here is coupled to SMTP.
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// Message is one outgoing email. Body is plain text.
type Message struct {
	To      string
	Subject string
	Body    string
}

// LogMailer writes messages to the log instead of sending them, which is how
// development and tests read a verification or reset link without an email
// provider.
//
// It logs the full link, which is a bearer secret. That is acceptable only
// because it is never the production mailer: config.Load refuses to start
// under APP_ENV=production unless SMTP_URL names a real relay, so the only way
// to reach this type is outside production. See SMTPMailer for the other half.
type LogMailer struct {
	Logger *slog.Logger
	From   string
}

// Send records the message.
func (m LogMailer) Send(ctx context.Context, msg Message) error {
	logger := m.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.WarnContext(ctx, "email not sent: no mail provider configured, logging instead",
		slog.String("from", m.From),
		slog.String("to", msg.To),
		slog.String("subject", msg.Subject),
		slog.String("body", msg.Body),
	)
	return nil
}
