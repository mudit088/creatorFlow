package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mudit/creatorflow/backend/internal/database"
)

type Repository struct {
	db   database.Querier // the pool, or a transaction when inside InTx
	pool *pgxpool.Pool    // only ever used to begin a transaction
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{db: pool, pool: pool}
}

// InTx runs fn against a repository bound to a single transaction. The service
// decides what belongs in one atomic unit and why; database.RunInTx owns the
// driver mechanics, so pgx never appears above the repository layer.
func (r *Repository) InTx(ctx context.Context, fn func(*Repository) error) error {
	return database.RunInTx(ctx, r.pool, func(q database.Querier) error {
		return fn(&Repository{db: q, pool: r.pool})
	})
}

// CreateUser inserts and relies on the UNIQUE constraint to reject duplicates.
// Checking "does this email exist?" first would be a race: two concurrent
// signups both see no row, both insert, one crashes. The constraint is the only
// thing that is actually atomic here.
func (r *Repository) CreateUser(ctx context.Context, email, passwordHash string) (*User, error) {
	const q = `
		INSERT INTO users (email, password_hash)
		VALUES ($1, $2)
		RETURNING id, email, password_hash, role, email_verified, created_at, updated_at`

	var u User
	err := r.db.QueryRow(ctx, q, email, passwordHash).Scan(
		&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.EmailVerified, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrEmailTaken
		}
		return nil, fmt.Errorf("insert user: %w", err)
	}

	return &u, nil
}

func (r *Repository) FindUserByEmail(ctx context.Context, email string) (*User, error) {
	const q = `
		SELECT id, email, password_hash, role, email_verified, created_at, updated_at
		FROM users WHERE email = $1`

	var u User
	err := r.db.QueryRow(ctx, q, email).Scan(
		&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.EmailVerified, &u.CreatedAt, &u.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find user by email: %w", err)
	}

	return &u, nil
}
