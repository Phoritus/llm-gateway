package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"llm-gateway/internal/store"
)

// ErrInvalidToken means a verification or reset token was unknown, already
// spent or expired. The three are not distinguished: a client that submits a
// token either gets the action or does not, and splitting the failures apart
// would tell an attacker which guesses were once real.
var ErrInvalidToken = errors.New("auth: token is invalid or expired")

// IssueVerification mints an email-verification token and sends the link. Any
// outstanding token for the same user is replaced, so the newest link is the
// only one that works.
func (s *Service) IssueVerification(ctx context.Context, userID, email, dashboardURL string, mailer Mailer) error {
	token, hash, err := newSecret()
	if err != nil {
		return err
	}
	if err := s.store.CreateVerificationToken(ctx, userID, hash, time.Now().Add(s.cfg.VerificationTTL)); err != nil {
		return err
	}
	link := fmt.Sprintf("%s/verify-email?token=%s", dashboardURL, url.QueryEscape(token))
	return mailer.Send(ctx, Message{
		To:      email,
		Subject: "Confirm your email address",
		Body: fmt.Sprintf(
			"Confirm your address to finish setting up your gateway account:\n\n%s\n\n"+
				"The link expires in %s. If you did not create an account, ignore this message.",
			link, s.cfg.VerificationTTL),
	})
}

// ConsumeVerification spends a verification token and marks the address
// verified.
func (s *Service) ConsumeVerification(ctx context.Context, token string) error {
	if token == "" {
		return ErrInvalidToken
	}
	if _, err := s.store.ConsumeVerificationToken(ctx, hashSecret(token)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrInvalidToken
		}
		return err
	}
	return nil
}

// IssuePasswordReset mints a reset token and sends the link.
func (s *Service) IssuePasswordReset(ctx context.Context, userID, email, dashboardURL string, mailer Mailer) error {
	token, hash, err := newSecret()
	if err != nil {
		return err
	}
	if err := s.store.CreatePasswordResetToken(ctx, userID, hash, time.Now().Add(s.cfg.ResetTTL)); err != nil {
		return err
	}
	link := fmt.Sprintf("%s/reset-password?token=%s", dashboardURL, url.QueryEscape(token))
	return mailer.Send(ctx, Message{
		To:      email,
		Subject: "Reset your password",
		Body: fmt.Sprintf(
			"Use this link to choose a new password:\n\n%s\n\n"+
				"The link expires in %s and can be used once. "+
				"If you did not ask for a reset, ignore this message: your password has not changed.",
			link, s.cfg.ResetTTL),
	})
}

// ResetPassword spends a reset token, stores the new hash and revokes every
// session the user holds. The store does all three in one transaction, so a
// reset can never leave an old session alive.
//
// The password rules are not checked here: HashPassword applies them, and it is
// the only way to reach a stored hash.
func (s *Service) ResetPassword(ctx context.Context, token, newPassword string) error {
	if token == "" {
		return ErrInvalidToken
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	if _, err := s.store.ConsumePasswordResetToken(ctx, hashSecret(token), hash); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrInvalidToken
		}
		return err
	}
	return nil
}

// ChangePassword replaces a signed-in user's password after checking the
// current one, then revokes their other sessions and leaves the caller's own
// alive.
//
// The current password is checked before the new one is judged, so a caller who
// cannot prove the current password is told only that, and never which rules
// their proposed password broke. That order is decided here rather than by the
// handler, because the handler cannot sequence a check it does not perform.
func (s *Service) ChangePassword(ctx context.Context, userID, currentSessionID, currentPassword, newPassword string) error {
	user, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if err := VerifyPassword(user.PasswordHash, currentPassword); err != nil {
		return ErrPasswordMismatch
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	// Changing a password is often a response to suspicion, so every other
	// session goes with it -- in the same transaction as the new hash, so the
	// password cannot change without them dying too.
	_, err = s.store.ChangePasswordAndRevokeSessions(ctx, userID, hash, currentSessionID)
	return err
}

// Authenticate verifies an email and password pair and returns the user. It
// spends the same argon2 work whether or not the address exists, so the
// response time does not reveal which accounts are registered.
func (s *Service) Authenticate(ctx context.Context, email, password string) (*store.User, error) {
	user, err := s.store.UserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, VerifyPasswordDummy(password)
		}
		return nil, err
	}
	if err := VerifyPassword(user.PasswordHash, password); err != nil {
		return nil, ErrPasswordMismatch
	}
	return user, nil
}
