package delivery

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Deliverable is one file a buyer has the right to download, joined to the
// product it belongs to so the response can say what they are downloading.
type Deliverable struct {
	FileID       uuid.UUID
	ProductID    uuid.UUID
	ProductTitle string
	S3Key        string
	OriginalName string
	ContentType  string
	SizeBytes    int64
}

// ErrNotEntitled covers every reason a download is refused: no such order, wrong
// email, a revoked entitlement, or an order that was never paid.
//
// They are one error deliberately. This endpoint is unauthenticated, so
// distinguishing "no such order" from "that order exists but the email is wrong"
// would turn it into an oracle: an attacker holding a leaked order id could
// confirm whether a guessed address was the buyer's.
var ErrNotEntitled = errors.New("no downloads are available for that order and email")

// downloadTTL is deliberately short. A presigned URL is a bearer credential —
// anyone holding it can fetch the object, with no further checks — so its
// usefulness has to expire quickly. Five minutes is enough to start a download,
// because S3 evaluates expiry when the request begins rather than throughout,
// so a slow transfer that started in time still completes.
const downloadTTL = 5 * time.Minute
