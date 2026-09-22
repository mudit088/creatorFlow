package profiles

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const linkColumns = `id, profile_id, title, url, position, is_active, created_at, updated_at`

func scanLink(row pgx.Row) (*Link, error) {
	var l Link
	err := row.Scan(&l.ID, &l.ProfileID, &l.Title, &l.URL, &l.Position,
		&l.IsActive, &l.CreatedAt, &l.UpdatedAt)
	return &l, err
}

// CreateLink appends to the end of the page. The next position is computed in
// the same statement that inserts it, but that alone is not enough: two
// concurrent appends would both read MAX(position) = 4 and both write 5, and
// since migration 006 made (profile_id, position) unique, the second one now
// fails outright instead of silently corrupting the order.
//
// The caller therefore holds the profile's row lock (see LockProfile), which
// serialises appends for one creator while leaving every other creator's page
// completely unblocked. Locking the profile row rather than the links table is
// the difference between one slow page and a global bottleneck.
func (r *Repository) CreateLink(ctx context.Context, profileID uuid.UUID, title, url string) (*Link, error) {
	const q = `
		INSERT INTO links (profile_id, title, url, position)
		VALUES ($1, $2, $3, COALESCE((SELECT MAX(position) + 1 FROM links WHERE profile_id = $1), 0))
		RETURNING ` + linkColumns

	l, err := scanLink(r.db.QueryRow(ctx, q, profileID, title, url))
	if err != nil {
		return nil, translateLinkError(err)
	}
	return l, nil
}

func (r *Repository) CountLinks(ctx context.Context, profileID uuid.UUID) (int, error) {
	const q = `SELECT count(*) FROM links WHERE profile_id = $1`

	var n int
	if err := r.db.QueryRow(ctx, q, profileID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count links: %w", err)
	}
	return n, nil
}

func (r *Repository) ListLinks(ctx context.Context, profileID uuid.UUID) ([]Link, error) {
	const q = `SELECT ` + linkColumns + ` FROM links WHERE profile_id = $1 ORDER BY position`
	return r.queryLinks(ctx, q, profileID)
}

// ListActiveLinks is the public page's query. Deactivated links are filtered in
// SQL so a hidden link never reaches a response struct that might serialise it.
func (r *Repository) ListActiveLinks(ctx context.Context, profileID uuid.UUID) ([]Link, error) {
	const q = `SELECT ` + linkColumns + ` FROM links WHERE profile_id = $1 AND is_active ORDER BY position`
	return r.queryLinks(ctx, q, profileID)
}

func (r *Repository) queryLinks(ctx context.Context, q string, profileID uuid.UUID) ([]Link, error) {
	rows, err := r.db.Query(ctx, q, profileID)
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	defer rows.Close()

	// Non-nil so an empty page serialises as [] rather than null. A JSON null
	// where the client expects an array is a frontend crash, not a design choice.
	links := make([]Link, 0)
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, fmt.Errorf("scan link: %w", err)
		}
		links = append(links, *l)
	}
	return links, rows.Err()
}

// UpdateLink carries profile_id in the WHERE clause. That is the authorization
// check, and doing it in SQL rather than as a fetch-then-compare in Go means
// there is no window between the check and the write, and no code path where
// someone forgets the comparison. A link belonging to another creator simply
// matches no row.
func (r *Repository) UpdateLink(ctx context.Context, profileID, linkID uuid.UUID, title, url *string, isActive *bool) (*Link, error) {
	const q = `
		UPDATE links SET
			title     = COALESCE($3, title),
			url       = COALESCE($4, url),
			is_active = COALESCE($5, is_active)
		WHERE id = $2 AND profile_id = $1
		RETURNING ` + linkColumns

	l, err := scanLink(r.db.QueryRow(ctx, q, profileID, linkID, title, url, isActive))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLinkNotFound
	}
	if err != nil {
		return nil, translateLinkError(err)
	}
	return l, nil
}

// DeleteLink leaves a gap in the position sequence, deliberately. Position
// defines order, not array index — 0, 1, 3 sorts exactly like 0, 1, 2 — so
// renumbering every surviving row on each delete would be pure write
// amplification in service of a number nobody reads.
func (r *Repository) DeleteLink(ctx context.Context, profileID, linkID uuid.UUID) error {
	const q = `DELETE FROM links WHERE id = $1 AND profile_id = $2`

	tag, err := r.db.Exec(ctx, q, linkID, profileID)
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLinkNotFound
	}
	return nil
}

// ReorderLinks rewrites the whole order in a single statement, using the array's
// own ordinality as the new position.
//
// This is the payoff from migration 006's DEFERRABLE constraint. Swapping two
// links necessarily passes through a state where both hold the same position;
// with an immediate constraint that intermediate state is rejected and the only
// way through is to park every row at position + 1000 first and write each row
// twice. Deferred, Postgres checks uniqueness once at COMMIT, so a permutation
// is one UPDATE and the final state is either wholly valid or wholly rolled back.
//
// profile_id in the WHERE clause is again the authorization check: ids belonging
// to someone else update nothing, which RowsAffected then catches.
func (r *Repository) ReorderLinks(ctx context.Context, profileID uuid.UUID, ids []uuid.UUID) error {
	const q = `
		UPDATE links AS l
		SET position = o.pos
		FROM (
			SELECT id, ordinality - 1 AS pos
			FROM unnest($2::uuid[]) WITH ORDINALITY AS t(id, ordinality)
		) AS o
		WHERE l.id = o.id AND l.profile_id = $1`

	tag, err := r.db.Exec(ctx, q, profileID, ids)
	if err != nil {
		return fmt.Errorf("reorder links: %w", err)
	}
	if tag.RowsAffected() != int64(len(ids)) {
		// An id in the list is not on this profile. Rolling back is the only
		// correct response: a partially applied order is worse than none.
		return ErrLinkNotFound
	}
	return nil
}

func translateLinkError(err error) error {
	var validation *ValidationError
	if constraint := constraintName(err); constraint != "" {
		switch constraint {
		case "links_title_len":
			validation = &ValidationError{"title", "Title must be between 1 and 80 characters."}
		case "links_url_scheme":
			validation = &ValidationError{"url", "Link must start with http:// or https://."}
		}
	}
	if validation != nil {
		return validation
	}
	return fmt.Errorf("link write: %w", err)
}
