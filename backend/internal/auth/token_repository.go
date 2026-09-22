package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type RefreshToken struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	ExpiresAt time.Time
}

func (r *Repository) StoreRefreshToken(ctx context.Context, userID uuid.UUID, hash string, expiresAt time.Time) error {
	const q = `INSERT INTO refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`
	if _, err := r.db.Exec(ctx, q, userID, hash, expiresAt); err != nil {
		return fmt.Errorf("store refresh token: %w", err)
	}
	return nil
}

// ConsumeRefreshToken revokes a token and returns it in the same statement.
//
// The revoke IS the check. A SELECT-then-UPDATE would let two concurrent
// refreshes both observe revoked_at IS NULL and both succeed, handing out two
// live sessions from one token — the same check-then-act race that CreateUser
// avoids by leaning on the UNIQUE constraint. Here the WHERE clause plays that
// role: exactly one caller can ever match revoked_at IS NULL, and everyone else
// gets zero rows.
//
// Zero rows therefore means one of two very different things, and the follow-up
// SELECT tells them apart:
//
//	no row at all  → a token we never issued          → ErrInvalidToken
//	row, revoked   → someone replayed a used token    → ErrTokenReused
//
// In the reuse case the returned RefreshToken carries UserID so the service can
// revoke that user's entire session family. Nothing else on it is populated.
//
// Expiry is deliberately not in the WHERE clause. An expired token is just
// stale, not evidence of theft, and folding it in would make an honest
// long-absent user look like an attacker and log out all their devices.
func (r *Repository) ConsumeRefreshToken(ctx context.Context, hash string) (*RefreshToken, error) {
	const consume = `
		UPDATE refresh_tokens SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
		RETURNING id, user_id, expires_at`

	var t RefreshToken
	err := r.db.QueryRow(ctx, consume, hash).Scan(&t.ID, &t.UserID, &t.ExpiresAt)
	if err == nil {
		if time.Now().After(t.ExpiresAt) {
			return nil, ErrInvalidToken
		}
		return &t, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("consume refresh token: %w", err)
	}

	const whose = `SELECT user_id FROM refresh_tokens WHERE token_hash = $1`

	var userID uuid.UUID
	switch err := r.db.QueryRow(ctx, whose, hash).Scan(&userID); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrInvalidToken
	case err != nil:
		return nil, fmt.Errorf("lookup reused refresh token: %w", err)
	default:
		return &RefreshToken{UserID: userID}, ErrTokenReused
	}
}

// RevokeRefreshToken is logout: idempotent, and silent about whether the token
// existed. A caller with a stale cookie has nothing to learn from the response.
func (r *Repository) RevokeRefreshToken(ctx context.Context, hash string) error {
	const q = `UPDATE refresh_tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`
	if _, err := r.db.Exec(ctx, q, hash); err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	return nil
}

// RevokeAllForUser is the panic button: a reused token means someone has a copy,
// so every session for that user dies and they must log in again.
func (r *Repository) RevokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	const q = `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`
	if _, err := r.db.Exec(ctx, q, userID); err != nil {
		return fmt.Errorf("revoke user tokens: %w", err)
	}
	return nil
}

func (r *Repository) FindUserByID(ctx context.Context, id uuid.UUID) (*User, error) {
	const q = `
		SELECT id, email, password_hash, role, email_verified, created_at, updated_at
		FROM users WHERE id = $1`

	var u User
	err := r.db.QueryRow(ctx, q, id).Scan(
		&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.EmailVerified, &u.CreatedAt, &u.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find user by id: %w", err)
	}
	return &u, nil
}
