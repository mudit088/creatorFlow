// Package analytics records what happened on a creator's public page.
//
// Two properties shape everything here. First, a visitor is a pseudonym that
// expires daily, never a person: the identifier is derived server-side and the
// inputs are never stored. Second, recording is lossy under pressure — counting
// views must never slow down or break serving them, so events are queued and
// dropped rather than allowed to hold up a response.
package analytics

import (
	"time"

	"github.com/google/uuid"
)

// The four things worth counting. These mirror the events_type_valid CHECK, and
// the database is the one that enforces the list — this copy exists so the code
// reads clearly, not so it can be trusted.
const (
	EventProfileView = "profile_view"
	EventProductView = "product_view"
	EventLinkClick   = "link_click"
	EventPurchase    = "purchase"
)

// Event is one thing that happened, already anonymised.
//
// Note what it does not carry: an IP address or a user agent. Those are hashed
// at the edge of the request, before anything is queued, so raw identifiers
// never sit in a buffer or reach this package's storage path.
type Event struct {
	ProfileID uuid.UUID
	Type      string

	// VisitorHash is empty for server-side events such as a purchase, which
	// have no browser behind them. The column is nullable for the same reason:
	// inventing a visitor for one would corrupt the visitor counts.
	VisitorHash string

	ProductID *uuid.UUID
	LinkID    *uuid.UUID
	OrderID   *uuid.UUID

	// ReferrerHost is the host only — never the full URL, which can carry a
	// search query or a private document title. "instagram.com" answers the
	// question a creator actually has.
	ReferrerHost *string

	OccurredAt time.Time
}

const (
	// queueSize is the number of events that may be waiting to be written. Past
	// this, new events are dropped. That is a deliberate choice: the alternative
	// is an unbounded buffer, which turns a slow database into memory growth and
	// eventually into a process that dies holding every event it has not yet
	// written.
	queueSize = 1024

	// writeTimeout bounds a single event's database work. An analytics insert
	// that cannot finish in this long is one the system is better off dropping
	// than retrying behind a backlog.
	writeTimeout = 3 * time.Second

	// shutdownTimeout is how long the worker gets to drain on the way out. Long
	// enough to flush a normal buffer, short enough not to hold a deployment.
	shutdownTimeout = 5 * time.Second
)
