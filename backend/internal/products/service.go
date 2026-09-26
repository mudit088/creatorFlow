package products

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mudit/creatorflow/backend/internal/storage"
)

// ObjectStore is declared here, where it is consumed, rather than in the storage
// package. products depends on the behaviour it needs, not on a concrete client,
// which is what lets a test swap in a fake without a running MinIO.
type ObjectStore interface {
	PresignUpload(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (string, error)
	StatObject(ctx context.Context, key string) (*storage.ObjectInfo, bool, error)
	DeleteObject(ctx context.Context, key string) error
}

// Invalidator is the storefront cache. As in the profiles package, nothing on
// it returns an error: a stale page for a few minutes is not a reason to fail a
// write that already succeeded.
type Invalidator interface {
	InvalidateProduct(ctx context.Context, username, slug string)
}

type Service struct {
	repo  *Repository
	store ObjectStore
	cache Invalidator
}

func NewService(repo *Repository, store ObjectStore, cache Invalidator) *Service {
	return &Service{repo: repo, store: store, cache: cache}
}

// invalidate drops the product page and the creator page that lists it.
//
// After the write, never before: dropping the entry first leaves a window in
// which a concurrent reader repopulates the cache from the old row, and the
// stale value then survives until its TTL rather than until the next edit.
func (s *Service) invalidate(ctx context.Context, productID uuid.UUID) {
	if s.cache == nil {
		return
	}
	username, slug, err := s.repo.PublicCoords(ctx, productID)
	if err != nil || username == "" {
		return
	}
	s.cache.InvalidateProduct(ctx, username, slug)
}

func (s *Service) Create(ctx context.Context, userID uuid.UUID, slug, title string, description *string, priceMinor int64) (*Product, error) {
	slug = strings.TrimSpace(strings.ToLower(slug))
	title = strings.TrimSpace(title)

	if err := validateSlug(slug); err != nil {
		return nil, err
	}
	if err := validateTitle(title); err != nil {
		return nil, err
	}
	if err := validatePrice(priceMinor); err != nil {
		return nil, err
	}

	product, err := s.repo.Create(ctx, userID, slug, title, description, priceMinor)
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, product.ID)
	return product, nil
}

func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Product, error) {
	return s.repo.ListForUser(ctx, userID)
}

func (s *Service) Get(ctx context.Context, userID, productID uuid.UUID) (*Product, []File, error) {
	product, err := s.repo.FindForUser(ctx, userID, productID)
	if err != nil {
		return nil, nil, err
	}

	files, err := s.repo.ListFiles(ctx, product.ID)
	if err != nil {
		return nil, nil, err
	}
	return product, files, nil
}

// Update refuses to publish a product with nothing behind it.
//
// This is the one invariant in this package that cannot be a database
// constraint: it spans two tables, and a CHECK sees only one row. A trigger
// could do it, but that buries a business rule where nobody reading the service
// would find it. So it lives here, inside the same transaction as the status
// change, with the count taken after the row is locked by the UPDATE.
func (s *Service) Update(ctx context.Context, userID, productID uuid.UUID, title, description, slug *string, priceMinor *int64, status *string) (*Product, error) {
	if title != nil {
		trimmed := strings.TrimSpace(*title)
		if err := validateTitle(trimmed); err != nil {
			return nil, err
		}
		title = &trimmed
	}
	if slug != nil {
		trimmed := strings.TrimSpace(strings.ToLower(*slug))
		if err := validateSlug(trimmed); err != nil {
			return nil, err
		}
		slug = &trimmed
	}
	if priceMinor != nil {
		if err := validatePrice(*priceMinor); err != nil {
			return nil, err
		}
	}
	if status != nil {
		switch *status {
		case StatusDraft, StatusPublished, StatusArchived:
		default:
			return nil, &ValidationError{"status", "Status must be draft, published or archived."}
		}
	}

	var updated *Product
	err := s.repo.InTx(ctx, func(r *Repository) error {
		p, err := r.Update(ctx, userID, productID, title, description, slug, priceMinor, status)
		if err != nil {
			return err
		}

		if p.Status == StatusPublished {
			confirmed, err := r.CountConfirmedFiles(ctx, p.ID)
			if err != nil {
				return err
			}
			if confirmed == 0 {
				// Rolls back the status change. A published product with no
				// deliverable is a page that takes money and hands over nothing.
				return ErrNoDeliverable
			}
		}

		updated = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Invalidated by id rather than by the slug from the request, because the
	// update may have changed the slug — and the entry that needs dropping is
	// the one under the OLD one too. PublicCoords reads the row after the write,
	// so the new page is refreshed; the old slug's entry expires on its TTL,
	// which is correct since that URL now 404s anyway.
	s.invalidate(ctx, updated.ID)
	return updated, nil
}

// UploadIntent is what the client needs to perform the upload itself.
type UploadIntent struct {
	File      *File
	UploadURL string
	ExpiresIn time.Duration
}

// RequestUpload validates, records the pending row, then signs a URL.
//
// The order matters. The row is written first so an upload that is started and
// abandoned leaves a trace we can find and clean up; signing first would let a
// client create objects the database knows nothing about. The row and the
// signature are not in one transaction because signing is pure local
// computation that cannot fail for external reasons, and holding a database
// transaction open across it would buy nothing.
//
// Note what is NOT trusted here: the client's declared size and content type are
// validated and then baked into the signature, so a lie is not merely detected
// later, it makes the upload impossible.
func (s *Service) RequestUpload(ctx context.Context, userID, productID uuid.UUID, filename, contentType string, size int64) (*UploadIntent, error) {
	filename = strings.TrimSpace(filename)
	contentType = strings.TrimSpace(strings.ToLower(contentType))

	if filename == "" {
		return nil, &ValidationError{"filename", "A filename is required."}
	}
	if size <= 0 {
		return nil, &ValidationError{"size_bytes", "File size must be greater than zero."}
	}
	if size > maxUploadBytes {
		return nil, &ValidationError{"size_bytes", fmt.Sprintf("Files must be %d GB or smaller.", maxUploadBytes/(1024*1024*1024))}
	}
	if _, ok := allowedContentTypes[contentType]; !ok {
		return nil, &ValidationError{"content_type", "That file type is not supported."}
	}

	product, err := s.repo.FindForUser(ctx, userID, productID)
	if err != nil {
		return nil, err
	}

	var file *File
	err = s.repo.InTx(ctx, func(r *Repository) error {
		count, err := r.CountFiles(ctx, product.ID)
		if err != nil {
			return err
		}
		if count >= maxFilesPerProduct {
			return ErrTooManyFiles
		}

		key := storage.ProductFileKey(product.ID, filename)
		file, err = r.CreateFile(ctx, product.ID, key, storage.SanitizeFilename(filename), contentType, size)
		return err
	})
	if err != nil {
		return nil, err
	}

	url, err := s.store.PresignUpload(ctx, file.S3Key, contentType, size, uploadURLTTL)
	if err != nil {
		return nil, err
	}

	return &UploadIntent{File: file, UploadURL: url, ExpiresIn: uploadURLTTL}, nil
}

// ConfirmUpload asks S3 whether the object is really there before believing the
// client. The browser uploads directly, so this process never observes the
// transfer; without this call, "I uploaded it" would be an unverified claim and
// a buyer would be the one to discover it was false.
func (s *Service) ConfirmUpload(ctx context.Context, userID, productID, fileID uuid.UUID) (*File, error) {
	file, err := s.repo.FindFileForUser(ctx, userID, productID, fileID)
	if err != nil {
		return nil, err
	}
	if file.IsConfirmed() {
		return file, nil // idempotent: a retried confirm is not an error
	}

	info, found, err := s.store.StatObject(ctx, file.S3Key)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrUploadMissing
	}

	// The signature already bound Content-Length, so a mismatch here should be
	// impossible. Checking anyway costs one comparison and turns a silent
	// assumption into an assertion that will fire loudly if a future change
	// stops signing the length.
	if info.Size != file.SizeBytes {
		return nil, ErrUploadMismatch
	}

	confirmed, err := s.repo.ConfirmFile(ctx, file.ID, info.Size, strings.Trim(info.ETag, `"`))
	if err != nil {
		return nil, err
	}
	// A confirmed file changes what the public product page lists as included.
	s.invalidate(ctx, productID)
	return confirmed, nil
}

// DeleteFile removes the row first and the object second.
//
// If the object delete fails, the result is an unreferenced object costing a
// little storage, which a cleanup job can sweep. The other order risks the
// opposite: a row that claims a deliverable whose bytes are already gone, which
// a buyer discovers at download time. When two systems cannot be updated
// atomically, choose the failure mode that costs money over the one that breaks
// a promise.
// DeleteFile removes a file, unless someone has already bought it.
//
// Two invariants live here, both of which span tables and so cannot be CHECK
// constraints:
//
//  1. A file behind a live entitlement cannot be deleted. Someone paid for it,
//     their entitlement stays valid, and deleting the object would leave them
//     holding a receipt for a 404. Deletion is refused rather than cascaded.
//  2. Removing the last deliverable un-publishes the product. Otherwise the
//     storefront keeps offering something checkout would then refuse to sell,
//     which reads to a creator as a broken Buy button rather than as their own
//     earlier deletion.
//
// The S3 object is deleted after the transaction commits, never before. If it
// went first and the transaction then rolled back, the row would survive
// pointing at bytes that no longer exist — a file that looks deliverable and is
// not. This way the worst case is an orphaned object, which costs storage and
// can be swept up, rather than a lie in the database.
func (s *Service) DeleteFile(ctx context.Context, userID, productID, fileID uuid.UUID) error {
	var key string

	err := s.repo.InTx(ctx, func(r *Repository) error {
		file, err := r.FindFileForUser(ctx, userID, productID, fileID)
		if err != nil {
			return err
		}

		sold, err := r.HasLiveEntitlements(ctx, productID)
		if err != nil {
			return err
		}
		if sold {
			return ErrFileSold
		}

		if err := r.DeleteFile(ctx, file.ID); err != nil {
			return err
		}

		remaining, err := r.CountConfirmedFiles(ctx, productID)
		if err != nil {
			return err
		}
		if remaining == 0 {
			// A no-op unless the product was published, so a draft losing its
			// last file simply stays a draft.
			if err := r.Unpublish(ctx, productID); err != nil {
				return err
			}
		}

		key = file.S3Key
		return nil
	})
	if err != nil {
		return err
	}

	s.invalidate(ctx, productID)
	return s.store.DeleteObject(ctx, key)
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

// slugPattern mirrors products_slug_shape. As with usernames, this copy exists
// for the error message; the constraint exists to be true.
var slugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{1,58}[a-z0-9])?$`)

func validateSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return &ValidationError{"slug", "Slug must be 2 to 60 characters of lowercase letters, numbers and hyphens, and cannot start or end with a hyphen."}
	}
	return nil
}

func validateTitle(title string) error {
	n := len([]rune(title))
	if n < 1 || n > 120 {
		return &ValidationError{"title", "Title must be between 1 and 120 characters."}
	}
	return nil
}

// validatePrice speaks minor units. The ceiling mirrors products_price_max and
// exists to catch a client that sends 499 meaning rupees where the API expects
// 49900 paise, which would otherwise quietly sell a product for 4.99.
func validatePrice(priceMinor int64) error {
	if priceMinor < 0 {
		return &ValidationError{"price_minor", "Price cannot be negative."}
	}
	if priceMinor > 100000000 {
		return &ValidationError{"price_minor", "Price is above the maximum. Remember this field is in paise, not rupees."}
	}
	return nil
}
