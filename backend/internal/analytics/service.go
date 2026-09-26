package analytics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Service records events without making anyone wait for them.
//
// Record() drops an event into a bounded queue and returns immediately; one
// worker goroutine drains it. Two consequences worth being explicit about:
//
//   - A page view is never slower because analytics is slow, and a database
//     hiccup shows up as missing counts rather than as failed page loads. For
//     this data that is the right trade — nobody has ever needed a view count to
//     be transactionally correct.
//   - Under sustained pressure events are dropped, deliberately and countably,
//     rather than buffered without limit until the process runs out of memory.
type Service struct {
	repo *Repository
	salt string

	queue   chan Event
	done    chan struct{}
	closeMu sync.Mutex
	closed  bool

	dropped atomic.Int64
}

func NewService(repo *Repository, salt string) *Service {
	s := &Service{
		repo:  repo,
		salt:  salt,
		queue: make(chan Event, queueSize),
		done:  make(chan struct{}),
	}
	go s.run()
	return s
}

// VisitorHash turns a request's identifying details into a daily pseudonym.
//
// Computed here, at the edge, so the IP and user agent are used and discarded
// rather than travelling through the queue: nothing downstream of this function
// can leak what it never receives.
//
// The UTC date in the input is the privacy mechanism. The same browser produces
// the same hash all day, which is what makes "unique visitors today" answerable,
// and a different one tomorrow, which is what makes following someone across
// weeks impossible — including for whoever holds the database.
//
// The salt defeats the obvious attack: without it, an adversary with the table
// could hash all four billion IPv4 addresses against today's date and recover
// every visitor's address in minutes.
func (s *Service) VisitorHash(ip, userAgent string, day time.Time) string {
	h := sha256.New()
	// Separated by a character that cannot appear in the parts, so that
	// ("1.2.3.4", "Mozilla") and ("1.2.3.4|Mozilla", "") cannot collide.
	h.Write([]byte(ip))
	h.Write([]byte{0})
	h.Write([]byte(userAgent))
	h.Write([]byte{0})
	h.Write([]byte(s.salt))
	h.Write([]byte{0})
	h.Write([]byte(day.UTC().Format("2006-01-02")))
	return hex.EncodeToString(h.Sum(nil))
}

// Record queues an event. It never blocks and never returns an error, because
// there is nothing a caller serving a page could usefully do about either.
func (s *Service) Record(ev Event) {
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now()
	}

	select {
	case s.queue <- ev:
	default:
		// Queue full: the writer is behind. Count it and move on — a dropped
		// view is a small inaccuracy, while blocking here would make analytics
		// an outage in the storefront.
		if n := s.dropped.Add(1); n == 1 || n%100 == 0 {
			slog.Warn("analytics queue full, dropping events", "dropped_total", n)
		}
	}
}

// Dropped reports how many events were discarded. Exposed so a test can assert
// the drop path, and so phase 16 can turn it into a metric rather than a log
// line nobody reads.
func (s *Service) Dropped() int64 { return s.dropped.Load() }

func (s *Service) run() {
	defer close(s.done)
	for ev := range s.queue {
		s.write(ev)
	}
}

func (s *Service) write(ev Event) {
	// A fresh context, not the request's. The request that produced this event
	// finished long ago and its context is already cancelled; inheriting it would
	// cancel every write before it started.
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()

	var visitorID *uuid.UUID
	if ev.VisitorHash != "" {
		id, err := s.repo.UpsertVisitor(ctx, ev.ProfileID, ev.VisitorHash, ev.OccurredAt.UTC())
		if err != nil {
			// Logged and dropped. Retrying would mean a queue that grows while
			// the database is unhappy, which is the failure mode the bounded
			// queue exists to avoid.
			slog.Warn("analytics: visitor upsert failed", "error", err, "profile_id", ev.ProfileID)
			return
		}
		visitorID = &id
	}

	if err := s.repo.InsertEvent(ctx, ev.ProfileID, visitorID, ev.Type,
		ev.ProductID, ev.LinkID, ev.OrderID, ev.ReferrerHost, ev.OccurredAt); err != nil {
		slog.Warn("analytics: event insert failed", "error", err, "type", ev.Type, "profile_id", ev.ProfileID)
	}
}

// Close drains what is queued and stops the worker. Called on shutdown, after
// the HTTP server has stopped accepting requests, so nothing new arrives while
// it drains.
func (s *Service) Close() {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return
	}
	s.closed = true
	close(s.queue)
	s.closeMu.Unlock()

	select {
	case <-s.done:
	case <-time.After(shutdownTimeout):
		slog.Warn("analytics: worker did not drain before shutdown", "queued", len(s.queue))
	}
}

// ReferrerHost extracts just the host from a Referer header.
//
// The full URL is deliberately discarded. A referrer can carry a search query,
// a session token in a path, or the title of a private document, and none of
// that is anyone's business here — while "instagram.com" is the entire answer a
// creator wants.
func ReferrerHost(referer string) *string {
	referer = strings.TrimSpace(referer)
	if referer == "" {
		return nil
	}

	u, err := url.Parse(referer)
	if err != nil || u.Host == "" {
		return nil
	}

	host := strings.ToLower(u.Hostname())
	if host == "" || len(host) > 253 {
		return nil
	}
	return &host
}
