package storefront

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	db *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{db: pool}
}

// FindCreator resolves a username to a published profile.
//
// Every filter that decides visibility lives in the WHERE clause, not in Go. A
// draft profile never enters the process, so no later code path can accidentally
// serialise it, and there is no branch anyone can forget to write.
func (r *Repository) FindCreator(ctx context.Context, username string) (uuid.UUID, *Creator, error) {
	const q = `
		SELECT id, username, display_name, bio
		FROM profiles
		WHERE username = $1 AND is_published`

	var id uuid.UUID
	var c Creator
	err := r.db.QueryRow(ctx, q, username).Scan(&id, &c.Username, &c.DisplayName, &c.Bio)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("find public creator: %w", err)
	}
	return id, &c, nil
}

func (r *Repository) ListLinks(ctx context.Context, profileID uuid.UUID) ([]Link, error) {
	const q = `
		SELECT title, url FROM links
		WHERE profile_id = $1 AND is_active
		ORDER BY position`

	rows, err := r.db.Query(ctx, q, profileID)
	if err != nil {
		return nil, fmt.Errorf("list public links: %w", err)
	}
	defer rows.Close()

	out := make([]Link, 0)
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.Title, &l.URL); err != nil {
			return nil, fmt.Errorf("scan public link: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ListProducts reads only published rows, which is exactly what
// products_profile_published_idx is a partial index for.
func (r *Repository) ListProducts(ctx context.Context, profileID uuid.UUID) ([]ProductSummary, error) {
	const q = `
		SELECT slug, title, price_minor, currency
		FROM products
		WHERE profile_id = $1 AND status = 'published'
		ORDER BY published_at DESC`

	rows, err := r.db.Query(ctx, q, profileID)
	if err != nil {
		return nil, fmt.Errorf("list public products: %w", err)
	}
	defer rows.Close()

	out := make([]ProductSummary, 0)
	for rows.Next() {
		var p ProductSummary
		if err := rows.Scan(&p.Slug, &p.Title, &p.PriceMinor, &p.Currency); err != nil {
			return nil, fmt.Errorf("scan public product: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FindProduct resolves /@username/:slug in one statement.
//
// Both visibility conditions are in the same WHERE: a published product on an
// unpublished profile must stay invisible, and joining first then checking in Go
// would make that two decisions instead of one, with the usual risk that a later
// edit keeps only one of them.
func (r *Repository) FindProduct(ctx context.Context, username, slug string) (uuid.UUID, *ProductDetail, error) {
	const q = `
		SELECT pr.id, pr.username, pr.display_name, pr.bio,
		       p.id, p.slug, p.title, p.description, p.price_minor, p.currency
		FROM products p
		JOIN profiles pr ON pr.id = p.profile_id
		WHERE pr.username = $1
		  AND pr.is_published
		  AND p.slug = $2
		  AND p.status = 'published'`

	var profileID, productID uuid.UUID
	var d ProductDetail
	err := r.db.QueryRow(ctx, q, username, slug).Scan(
		&profileID, &d.Creator.Username, &d.Creator.DisplayName, &d.Creator.Bio,
		&productID, &d.Slug, &d.Title, &d.Description, &d.PriceMinor, &d.Currency,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("find public product: %w", err)
	}
	return productID, &d, nil
}

// ListDeliverables lists only confirmed files. A pending upload is a row without
// bytes behind it, and advertising it would promise the buyer something that
// does not exist.
func (r *Repository) ListDeliverables(ctx context.Context, productID uuid.UUID) ([]Deliverable, error) {
	const q = `
		SELECT original_name, content_type, size_bytes
		FROM product_files
		WHERE product_id = $1 AND uploaded_at IS NOT NULL
		ORDER BY created_at`

	rows, err := r.db.Query(ctx, q, productID)
	if err != nil {
		return nil, fmt.Errorf("list deliverables: %w", err)
	}
	defer rows.Close()

	out := make([]Deliverable, 0)
	for rows.Next() {
		var d Deliverable
		if err := rows.Scan(&d.Name, &d.ContentType, &d.SizeBytes); err != nil {
			return nil, fmt.Errorf("scan deliverable: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
