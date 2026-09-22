package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

type Service struct {
	repo   *Repository
	tokens *TokenIssuer

	// dummyHash is verified against when no user matches, so the wrong-email and
	// wrong-password paths burn the same ~60ms of argon2. See Login.
	dummyHash string
}

func NewService(repo *Repository, tokens *TokenIssuer) (*Service, error) {
	// Hashed once at startup rather than per failed login: the point is to spend
	// the same time as a real verification, not to spend it twice. The input is
	// random so no attacker can learn anything from a hash of a known string.
	dummy, err := HashPassword(uuid.NewString())
	if err != nil {
		return nil, fmt.Errorf("precompute dummy hash: %w", err)
	}

	return &Service{repo: repo, tokens: tokens, dummyHash: dummy}, nil
}

// Session is everything a successful authentication produces. RefreshToken is
// the raw value and exists only in this struct and in the response — the
// database holds nothing but its SHA-256.
type Session struct {
	User             *User
	AccessToken      string
	AccessExpiresIn  time.Duration
	RefreshToken     string
	RefreshExpiresAt time.Time
}

func (s *Service) Register(ctx context.Context, email, password string) (*Session, error) {
	email = normalizeEmail(email)

	if err := validateEmail(email); err != nil {
		return nil, err
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}

	hash, err := HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	user, err := s.repo.CreateUser(ctx, email, hash)
	if err != nil {
		return nil, err
	}

	// Deliberately not in one transaction with the insert. If issuing the
	// session fails the account still exists and the user can simply log in,
	// which is a far better outcome than rolling back an account they believe
	// they just created.
	return s.issueSession(ctx, user)
}

// Login answers identically whether the email is unknown or the password is
// wrong — same error, and the same amount of work. Returning "no such account"
// turns the login form into an oracle for whether a given person uses the
// product, and skipping the hash when no user is found leaks the same fact
// through response time even when the message is identical.
func (s *Service) Login(ctx context.Context, email, password string) (*Session, error) {
	email = normalizeEmail(email)

	user, err := s.repo.FindUserByEmail(ctx, email)
	if errors.Is(err, ErrUserNotFound) {
		_, _ = VerifyPassword(password, s.dummyHash)
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}

	ok, err := VerifyPassword(password, user.PasswordHash)
	if err != nil {
		// A malformed stored hash is our bug, not the user's. It must never
		// present as "wrong password" or it will be debugged as one.
		return nil, fmt.Errorf("verify password for user %s: %w", user.ID, err)
	}
	if !ok {
		return nil, ErrInvalidCredentials
	}

	return s.issueSession(ctx, user)
}

// Refresh rotates: the presented token is consumed and a new one issued, so a
// stolen token is usable at most once and its theft becomes visible the moment
// the real owner refreshes.
//
// Consume and store are one transaction because a crash between them would
// leave the user with a revoked cookie and no replacement — logged out through
// no fault of their own.
func (s *Service) Refresh(ctx context.Context, rawToken string) (*Session, error) {
	var userID uuid.UUID
	var raw string
	var expiresAt time.Time

	err := s.repo.InTx(ctx, func(r *Repository) error {
		consumed, err := r.ConsumeRefreshToken(ctx, HashRefreshToken(rawToken))
		if err != nil {
			if consumed != nil {
				userID = consumed.UserID // needed for the revoke-all below
			}
			return err
		}
		userID = consumed.UserID

		newRaw, newHash, err := NewRefreshToken()
		if err != nil {
			return err
		}
		raw, expiresAt = newRaw, time.Now().Add(s.tokens.RefreshTTL())

		return r.StoreRefreshToken(ctx, userID, newHash, expiresAt)
	})

	// Reuse detection. The rollback above undid nothing — the UPDATE matched no
	// rows — so this revoke runs on its own, outside the failed transaction.
	// Someone holds a copy of a token that was already spent; we cannot tell the
	// thief from the victim, so every session for the account dies.
	if errors.Is(err, ErrTokenReused) {
		if revokeErr := s.repo.RevokeAllForUser(ctx, userID); revokeErr != nil {
			return nil, fmt.Errorf("revoke sessions after token reuse for user %s: %w", userID, revokeErr)
		}
		return nil, ErrTokenReused
	}
	if err != nil {
		return nil, err
	}

	user, err := s.repo.FindUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}

	access, err := s.tokens.NewAccessToken(user.ID, user.Role)
	if err != nil {
		return nil, fmt.Errorf("sign access token: %w", err)
	}

	return &Session{
		User:             user,
		AccessToken:      access,
		AccessExpiresIn:  s.tokens.AccessTTL(),
		RefreshToken:     raw,
		RefreshExpiresAt: expiresAt,
	}, nil
}

// Logout revokes one session, not all of them — signing out on a laptop must
// not sign you out on your phone. It reports no error for an unknown token:
// logout is idempotent, and a stale cookie has nothing to learn from us.
func (s *Service) Logout(ctx context.Context, rawToken string) error {
	if rawToken == "" {
		return nil
	}
	return s.repo.RevokeRefreshToken(ctx, HashRefreshToken(rawToken))
}

func (s *Service) UserByID(ctx context.Context, id uuid.UUID) (*User, error) {
	return s.repo.FindUserByID(ctx, id)
}

func (s *Service) issueSession(ctx context.Context, user *User) (*Session, error) {
	access, err := s.tokens.NewAccessToken(user.ID, user.Role)
	if err != nil {
		return nil, fmt.Errorf("sign access token: %w", err)
	}

	raw, hash, err := NewRefreshToken()
	if err != nil {
		return nil, err
	}

	expiresAt := time.Now().Add(s.tokens.RefreshTTL())
	if err := s.repo.StoreRefreshToken(ctx, user.ID, hash, expiresAt); err != nil {
		return nil, err
	}

	return &Session{
		User:             user,
		AccessToken:      access,
		AccessExpiresIn:  s.tokens.AccessTTL(),
		RefreshToken:     raw,
		RefreshExpiresAt: expiresAt,
	}, nil
}

// normalizeEmail exists so the same address always produces the same row. The
// column is citext, so Postgres already matches case-insensitively; lowercasing
// here keeps what we store predictable rather than "however they typed it".
func normalizeEmail(email string) string {
	return strings.TrimSpace(strings.ToLower(email))
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

func validateEmail(email string) error {
	if email == "" {
		return &ValidationError{"email", "Email is required."}
	}
	if len(email) > 254 {
		return &ValidationError{"email", "Email is too long."}
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return &ValidationError{"email", "Enter a valid email address."}
	}
	return nil
}

// Length beats composition rules. "Must contain a symbol" pushes people toward
// Password1! — long passphrases are stronger and easier to remember.
func validatePassword(password string) error {
	if len([]rune(password)) < 12 {
		return &ValidationError{"password", "Password must be at least 12 characters."}
	}
	if len(password) > 256 {
		return &ValidationError{"password", "Password must be under 256 characters."}
	}

	var hasLetter, hasOther bool
	for _, r := range password {
		if unicode.IsLetter(r) {
			hasLetter = true
		} else {
			hasOther = true
		}
	}
	if !hasLetter || !hasOther {
		return &ValidationError{"password", "Use a mix of letters and at least one number or symbol."}
	}
	return nil
}
