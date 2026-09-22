package products

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const fileColumns = `id, product_id, s3_key, original_name, content_type, size_bytes, checksum, uploaded_at, created_at`

func scanFile(row pgx.Row) (*File, error) {
	var f File
	err := row.Scan(&f.ID, &f.ProductID, &f.S3Key, &f.OriginalName, &f.ContentType,
		&f.SizeBytes, &f.Checksum, &f.UploadedAt, &f.CreatedAt)
	return &f, err
}

// CreateFile records the intent to upload, before the presigned URL is handed
// out. Writing the row first is what makes an abandoned upload visible: if the
// browser never completes the PUT, the row sits with uploaded_at NULL and the
// pending index finds it. Signing first and inserting on confirmation instead
// would leave orphaned objects nothing in the database knows about.
func (r *Repository) CreateFile(ctx context.Context, productID uuid.UUID, key, originalName, contentType string, size int64) (*File, error) {
	const q = `
		INSERT INTO product_files (product_id, s3_key, original_name, content_type, size_bytes)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING ` + fileColumns

	f, err := scanFile(r.db.QueryRow(ctx, q, productID, key, originalName, contentType, size))
	if err != nil {
		return nil, fmt.Errorf("create product file: %w", err)
	}
	return f, nil
}

// FindFileForUser joins back up to profiles so a file can only be reached by the
// creator who owns the product that owns it. Three levels of ownership, one
// statement, no chance of checking two of them and forgetting the third.
func (r *Repository) FindFileForUser(ctx context.Context, userID, productID, fileID uuid.UUID) (*File, error) {
	const q = `
		SELECT f.id, f.product_id, f.s3_key, f.original_name, f.content_type,
		       f.size_bytes, f.checksum, f.uploaded_at, f.created_at
		FROM product_files f
		JOIN products p ON p.id = f.product_id
		WHERE f.id = $3
		  AND f.product_id = $2
		  AND p.profile_id = (SELECT id FROM profiles WHERE user_id = $1)`

	f, err := scanFile(r.db.QueryRow(ctx, q, userID, productID, fileID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrFileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find product file: %w", err)
	}
	return f, nil
}

func (r *Repository) ListFiles(ctx context.Context, productID uuid.UUID) ([]File, error) {
	const q = `SELECT ` + fileColumns + ` FROM product_files WHERE product_id = $1 ORDER BY created_at`

	rows, err := r.db.Query(ctx, q, productID)
	if err != nil {
		return nil, fmt.Errorf("list product files: %w", err)
	}
	defer rows.Close()

	out := make([]File, 0)
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("scan product file: %w", err)
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func (r *Repository) CountFiles(ctx context.Context, productID uuid.UUID) (int, error) {
	const q = `SELECT count(*) FROM product_files WHERE product_id = $1`

	var n int
	if err := r.db.QueryRow(ctx, q, productID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count product files: %w", err)
	}
	return n, nil
}

// CountConfirmedFiles backs the publish check. Pending rows do not count: a
// product whose only file was never actually uploaded has nothing to deliver.
func (r *Repository) CountConfirmedFiles(ctx context.Context, productID uuid.UUID) (int, error) {
	const q = `SELECT count(*) FROM product_files WHERE product_id = $1 AND uploaded_at IS NOT NULL`

	var n int
	if err := r.db.QueryRow(ctx, q, productID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count confirmed files: %w", err)
	}
	return n, nil
}

// ConfirmFile marks an upload complete, but only if it is still pending. The
// uploaded_at IS NULL clause makes this idempotent under a double-click and,
// more importantly, stops a second confirmation overwriting the recorded size
// and checksum of a file that was already accepted.
func (r *Repository) ConfirmFile(ctx context.Context, fileID uuid.UUID, size int64, checksum string) (*File, error) {
	const q = `
		UPDATE product_files
		SET uploaded_at = now(), size_bytes = $2, checksum = $3
		WHERE id = $1 AND uploaded_at IS NULL
		RETURNING ` + fileColumns

	f, err := scanFile(r.db.QueryRow(ctx, q, fileID, size, checksum))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAlreadyUploaded
	}
	if err != nil {
		return nil, fmt.Errorf("confirm product file: %w", err)
	}
	return f, nil
}

func (r *Repository) DeleteFile(ctx context.Context, fileID uuid.UUID) error {
	const q = `DELETE FROM product_files WHERE id = $1`

	if _, err := r.db.Exec(ctx, q, fileID); err != nil {
		return fmt.Errorf("delete product file: %w", err)
	}
	return nil
}
