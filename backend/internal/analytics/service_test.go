package analytics

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The hashing tests need no database: they are about what the identifier
// guarantees, which is the privacy claim this package makes.

func testService(t *testing.T, repo *Repository) *Service {
	t.Helper()
	s := NewService(repo, "test-salt")
	t.Cleanup(s.Close)
	return s
}

func TestVisitorHashIsStableWithinADay(t *testing.T) {
	s := testService(t, nil)
	day := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	first := s.VisitorHash("1.2.3.4", "Mozilla/5.0", day)
	later := s.VisitorHash("1.2.3.4", "Mozilla/5.0", day.Add(6*time.Hour))

	if first != later {
		t.Error("hash changed within the same day; unique-visitor counts would be inflated")
	}
	if len(first) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars to satisfy visitors_hash_shape", len(first))
	}
}

// The privacy property: yesterday's pseudonym cannot be matched to today's.
func TestVisitorHashRotatesDaily(t *testing.T) {
	s := testService(t, nil)
	day := time.Date(2026, 9, 26, 23, 59, 0, 0, time.UTC)

	today := s.VisitorHash("1.2.3.4", "Mozilla/5.0", day)
	tomorrow := s.VisitorHash("1.2.3.4", "Mozilla/5.0", day.Add(2*time.Minute))

	if today == tomorrow {
		t.Error("hash survived midnight; the same visitor would be linkable across days")
	}
}

// Without this, anyone holding the table could hash the whole IPv4 space against
// a known date and recover every visitor's address.
func TestVisitorHashDependsOnSalt(t *testing.T) {
	day := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	a := NewService(nil, "salt-one")
	defer a.Close()
	b := NewService(nil, "salt-two")
	defer b.Close()

	if a.VisitorHash("1.2.3.4", "UA", day) == b.VisitorHash("1.2.3.4", "UA", day) {
		t.Error("hash ignores the salt; visitor hashes would be brute-forceable")
	}
}

// The separator matters: without it, ("1.2.3.4", "Mozilla") and
// ("1.2.3.4Mozilla", "") would hash identically and two visitors could merge.
func TestVisitorHashFieldsCannotBeConfused(t *testing.T) {
	s := testService(t, nil)
	day := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)

	if s.VisitorHash("1.2.3.4", "Mozilla", day) == s.VisitorHash("1.2.3.4Mozilla", "", day) {
		t.Error("field boundaries are not encoded in the hash input")
	}
}

func TestReferrerHostKeepsOnlyTheHost(t *testing.T) {
	cases := []struct {
		in   string
		want string // "" means nil
	}{
		// The whole point: a query string can carry a search term or a token.
		{"https://www.google.com/search?q=private+thing", "www.google.com"},
		{"https://Instagram.com/rahul", "instagram.com"},
		{"http://localhost:3000/page", "localhost"},
		{"", ""},
		{"not a url", ""},
		{"/relative/path", ""},
	}

	for _, tc := range cases {
		got := ReferrerHost(tc.in)
		if tc.want == "" {
			if got != nil {
				t.Errorf("ReferrerHost(%q) = %q, want nil", tc.in, *got)
			}
			continue
		}
		if got == nil || *got != tc.want {
			t.Errorf("ReferrerHost(%q) = %v, want %q", tc.in, got, tc.want)
		}
	}
}

// Recording must not block a page render, even when nothing is draining the
// queue. This is the behaviour that keeps analytics off the critical path.
func TestRecordDropsRatherThanBlocking(t *testing.T) {
	// No repository and no worker: a Service built by hand so the queue is
	// never drained, which is the worst case a real one can reach.
	s := &Service{queue: make(chan Event, 2), done: make(chan struct{})}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			s.Record(Event{ProfileID: uuid.New(), Type: EventProfileView})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked when the queue was full; a slow writer would stall page loads")
	}

	if s.Dropped() != 48 {
		t.Errorf("dropped = %d, want 48 (50 sent, 2 buffered)", s.Dropped())
	}
}

// --- database-backed -------------------------------------------------------

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping database-backed test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("database unreachable (%v); skipping", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newProfile(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	var userID, profileID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		fmt.Sprintf("an-test-%s@test.local", suffix)).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO profiles (user_id, username, display_name) VALUES ($1, $2, 'A') RETURNING id`,
		userID, "antest"+suffix).Scan(&profileID); err != nil {
		t.Fatalf("insert profile: %v", err)
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM events WHERE profile_id = $1`, profileID)
		_, _ = pool.Exec(c, `DELETE FROM visitors WHERE profile_id = $1`, profileID)
		_, _ = pool.Exec(c, `DELETE FROM profiles WHERE id = $1`, profileID)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id = $1`, userID)
	})
	return profileID
}

// Two views from one browser are one visitor and two events — the shape every
// "unique visitors" number depends on.
func TestRepeatVisitIsOneVisitorTwoEvents(t *testing.T) {
	pool := testPool(t)
	profileID := newProfile(t, pool)
	repo := NewRepository(pool)
	svc := NewService(repo, "test-salt")

	hash := svc.VisitorHash("1.2.3.4", "Mozilla/5.0", time.Now().UTC())
	for i := 0; i < 2; i++ {
		svc.Record(Event{ProfileID: profileID, Type: EventProfileView, VisitorHash: hash})
	}
	svc.Close() // drains the queue

	ctx := context.Background()
	var visitors, events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM visitors WHERE profile_id = $1`, profileID).Scan(&visitors); err != nil {
		t.Fatalf("count visitors: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE profile_id = $1`, profileID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}

	if visitors != 1 {
		t.Errorf("visitors = %d, want 1", visitors)
	}
	if events != 2 {
		t.Errorf("events = %d, want 2", events)
	}
}

// A purchase has no browser behind it. Inventing a visitor would corrupt the
// visitor counts, so the column stays NULL.
func TestServerSideEventHasNoVisitor(t *testing.T) {
	pool := testPool(t)
	profileID := newProfile(t, pool)
	svc := NewService(NewRepository(pool), "test-salt")

	svc.Record(Event{ProfileID: profileID, Type: EventProfileView})
	svc.Close()

	var visitorID *uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT visitor_id FROM events WHERE profile_id = $1`, profileID).Scan(&visitorID); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if visitorID != nil {
		t.Errorf("visitor_id = %v, want NULL for an event with no visitor hash", visitorID)
	}
}
