package profiles

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mudit/creatorflow/backend/internal/database"
)

type Repository struct {
	db   database.Querier
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{db: pool, pool: pool}
}

func (r *Repository) InTx(ctx context.Context, fn func(*Repository) error) error {
	return database.RunInTx(ctx, r.pool, func(q database.Querier) error {
		return fn(&Repository{db: q, pool: r.pool})
	})
}

const profileColumns = `id, user_id, username, display_name, bio, avatar_key, is_published, created_at, updated_at`

func scanProfile(row pgx.Row) (*Profile, error) {
	var p Profile
	err := row.Scan(&p.ID, &p.UserID, &p.Username, &p.DisplayName, &p.Bio,
		&p.AvatarKey, &p.IsPublished, &p.CreatedAt, &p.UpdatedAt)
	return &p, err
}

// CreateProfile inserts and lets the constraints decide. Two UNIQUE constraints
// can reject this row for completely different reasons, so the error must be
// resolved by constraint NAME, not by the 23505 code alone. Mapping every 23505
// to "username taken" would tell a creator their handle is unavailable when the
// truth is that they already have a profile — a genuinely confusing bug to chase.
func (r *Repository) CreateProfile(ctx context.Context, userID uuid.UUID, username, displayName string) (*Profile, error) {
	const q = `
		INSERT INTO profiles (user_id, username, display_name)
		VALUES ($1, $2, $3)
		RETURNING ` + profileColumns

	p, err := scanProfile(r.db.QueryRow(ctx, q, userID, username, displayName))
	if err != nil {
		return nil, translateProfileError(err)
	}
	return p, nil
}

// constraintName reports which named constraint rejected a write, or "" if the
// error was not a constraint violation at all. The error CODE is not enough on
// its own: one table can have several UNIQUE or CHECK constraints, and 23505
// says only that one of them fired.
func constraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

func translateProfileError(err error) error {
	switch constraintName(err) {
	case "profiles_user_id_key":
		return ErrProfileExists
	case "profiles_username_key":
		return ErrUsernameTaken
	// The service validates these too, for a message that says what is wrong.
	// These arms exist because the service is not the only writer forever, and a
	// constraint that can fire must have a defined translation.
	case "profiles_username_shape", "profiles_username_reserved":
		return ErrUsernameInvalid
	default:
		return fmt.Errorf("profile write: %w", err)
	}
}

func (r *Repository) FindByUserID(ctx context.Context, userID uuid.UUID) (*Profile, error) {
	const q = `SELECT ` + profileColumns + ` FROM profiles WHERE user_id = $1`

	p, err := scanProfile(r.db.QueryRow(ctx, q, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProfileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find profile by user: %w", err)
	}
	return p, nil
}

// FindPublishedByUsername filters on is_published in SQL rather than fetching
// and checking in Go. An unpublished profile must be indistinguishable from a
// nonexistent one, and the surest way to guarantee that is for the draft row to
// never enter the process in the first place.
func (r *Repository) FindPublishedByUsername(ctx context.Context, username string) (*Profile, error) {
	const q = `SELECT ` + profileColumns + ` FROM profiles WHERE username = $1 AND is_published`

	p, err := scanProfile(r.db.QueryRow(ctx, q, username))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProfileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find published profile: %w", err)
	}
	return p, nil
}

// UpdateProfile applies a partial update in one statement. COALESCE($n, column)
// leaves a column untouched when the parameter is NULL, which is what a nil
// pointer becomes on the wire — so "field absent from the PATCH body" and
// "column unchanged" are the same thing, with no read-modify-write cycle and no
// window for a concurrent update to be clobbered.
//
// Username is deliberately not updatable here. Changing it would break every
// /@username link already shared and would release the old handle for immediate
// impersonation; doing it safely needs a redirect table and a cooldown, which is
// a feature, not a column in an UPDATE.
func (r *Repository) UpdateProfile(ctx context.Context, userID uuid.UUID, displayName, bio *string, isPublished *bool) (*Profile, error) {
	const q = `
		UPDATE profiles SET
			display_name = COALESCE($2, display_name),
			bio          = COALESCE($3, bio),
			is_published = COALESCE($4, is_published)
		WHERE user_id = $1
		RETURNING ` + profileColumns

	p, err := scanProfile(r.db.QueryRow(ctx, q, userID, displayName, bio, isPublished))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProfileNotFound
	}
	if err != nil {
		return nil, translateProfileError(err)
	}
	return p, nil
}

// LockProfile takes a row-level lock on the caller's profile and returns its id.
// Every link mutation goes through here first, which serialises them per profile
// without blocking anyone else's page. See CreateLink for why that matters.
func (r *Repository) LockProfile(ctx context.Context, userID uuid.UUID) (uuid.UUID, error) {
	const q = `SELECT id FROM profiles WHERE user_id = $1 FOR UPDATE`

	var id uuid.UUID
	err := r.db.QueryRow(ctx, q, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrProfileNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("lock profile: %w", err)
	}
	return id, nil
}
