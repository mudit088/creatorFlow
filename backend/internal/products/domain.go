package products

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Product lifecycle: draft -> published -> archived. Archived rather than
// deleted, because an order row references the product it sold, and destroying
// that row would make an old receipt unreadable.
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
	StatusArchived  = "archived"
)

type Product struct {
	ID          uuid.UUID
	ProfileID   uuid.UUID
	Slug        string
	Title       string
	Description *string
	// Minor units. 49900 is 499 rupees. The API speaks paise end to end so that
	// no layer is ever tempted to divide by 100 and keep the result in a float.
	PriceMinor  int64
	Currency    string
	Status      string
	PublishedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// File is one deliverable. UploadedAt is NULL between "we issued a presigned
// URL" and "we confirmed the object exists", which is the only window in which
// a row can exist without bytes behind it.
type File struct {
	ID           uuid.UUID
	ProductID    uuid.UUID
	S3Key        string
	OriginalName string
	ContentType  string
	SizeBytes    int64
	Checksum     *string
	UploadedAt   *time.Time
	CreatedAt    time.Time
}

func (f File) IsConfirmed() bool { return f.UploadedAt != nil }

var (
	ErrProductNotFound = errors.New("product not found")
	ErrSlugTaken       = errors.New("slug already used on this profile")
	ErrProfileRequired = errors.New("create a profile before adding products")
	ErrFileNotFound    = errors.New("file not found")
	ErrTooManyFiles    = errors.New("file limit reached")
	// ErrNoDeliverable blocks publishing a product nobody could be given after
	// paying for it.
	ErrNoDeliverable = errors.New("a published product needs at least one uploaded file")
	// ErrUploadMissing means the browser never completed the direct upload, or
	// uploaded something other than what it declared.
	ErrUploadMissing   = errors.New("no uploaded object found for this file")
	ErrUploadMismatch  = errors.New("uploaded object does not match what was declared")
	ErrAlreadyUploaded = errors.New("this file was already confirmed")
)

const (
	maxFilesPerProduct = 20

	// maxUploadBytes is the app's ceiling, well under the 5 GB a single PUT can
	// carry. It is enforced before signing: the signed Content-Length is what
	// stops the client exceeding it, so the number chosen here is the real limit.
	maxUploadBytes int64 = 2 * 1024 * 1024 * 1024

	// uploadURLTTL is deliberately short. The URL is a bearer credential to write
	// one object; anything long enough to be pasted into a chat and reused later
	// is too long. Fifteen minutes is enough to start a large upload, and S3
	// checks expiry at the start of the request, not throughout it, so a slow
	// upload that began in time still completes.
	uploadURLTTL = 15 * time.Minute
)

// allowedContentTypes is an allowlist, not a denylist of dangerous types. A
// denylist is a list you are permanently behind on: text/html is obvious,
// image/svg+xml carries script, and there is always one more. An allowlist fails
// closed, and the cost is that adding a new product format is a code change.
var allowedContentTypes = map[string]struct{}{
	"application/pdf":              {},
	"application/epub+zip":         {},
	"application/zip":              {},
	"application/x-zip-compressed": {},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   {},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         {},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {},
	"text/plain":               {},
	"text/csv":                 {},
	"text/markdown":            {},
	"image/png":                {},
	"image/jpeg":               {},
	"audio/mpeg":               {},
	"audio/mp4":                {},
	"video/mp4":                {},
	"application/octet-stream": {},
}
