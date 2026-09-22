package profiles

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Profile is a creator's public identity. It is one-to-one with a user, and the
// database enforces that with UNIQUE (user_id) rather than trusting the service
// to check first.
type Profile struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Username    string
	DisplayName string
	Bio         *string
	AvatarKey   *string
	IsPublished bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Link is one row on a creator's page. Position defines display order and is
// unique per profile; is_active hides a link without destroying it, which is
// what creators actually want when a campaign ends.
type Link struct {
	ID        uuid.UUID
	ProfileID uuid.UUID
	Title     string
	URL       string
	Position  int
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PublicProfile is what an anonymous visitor to /@username receives. It is a
// distinct type from Profile on purpose: UserID and IsPublished have no business
// leaving the server, and a separate struct makes that impossible rather than
// merely unlikely.
type PublicProfile struct {
	Username    string
	DisplayName string
	Bio         *string
	AvatarKey   *string
	Links       []Link
}

var (
	ErrProfileExists   = errors.New("user already has a profile")
	ErrProfileNotFound = errors.New("profile not found")
	ErrUsernameTaken   = errors.New("username already taken")
	ErrUsernameInvalid = errors.New("username is not allowed")
	ErrLinkNotFound    = errors.New("link not found")
	ErrTooManyLinks    = errors.New("link limit reached")
	// ErrOrderIncomplete guards a reorder that names only some of a profile's
	// links. A partial order would leave the untouched rows on stale positions.
	ErrOrderIncomplete = errors.New("reorder must list every link exactly once")
)

// maxLinks caps a page. Unbounded rows per profile is how one user turns a
// shared database into everyone's problem, and no real creator page needs more.
const maxLinks = 50
