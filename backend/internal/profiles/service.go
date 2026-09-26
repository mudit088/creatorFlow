package profiles

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Invalidator is the storefront cache, declared here as the one thing this
// package needs from it. Nothing on it returns an error: a failed invalidation
// means a page stays stale until its TTL expires, which is not a reason to fail
// the edit that succeeded.
type Invalidator interface {
	InvalidateProfile(ctx context.Context, username string)
}

type Service struct {
	repo  *Repository
	cache Invalidator
}

func NewService(repo *Repository, cache Invalidator) *Service {
	return &Service{repo: repo, cache: cache}
}

// invalidate drops the caller's public page from the cache.
//
// Called after a successful write, never before: invalidating first would let a
// concurrent read repopulate the cache from the pre-write state, which is the
// classic way a cache-aside system ends up permanently stale.
func (s *Service) invalidate(ctx context.Context, userID uuid.UUID) {
	if s.cache == nil {
		return
	}
	username, err := s.repo.UsernameForUser(ctx, userID)
	if err != nil || username == "" {
		return
	}
	s.cache.InvalidateProfile(ctx, username)
}

func (s *Service) Create(ctx context.Context, userID uuid.UUID, username, displayName string) (*Profile, error) {
	username = strings.TrimSpace(strings.ToLower(username))
	displayName = strings.TrimSpace(displayName)

	if err := validateUsername(username); err != nil {
		return nil, err
	}
	if err := validateDisplayName(displayName); err != nil {
		return nil, err
	}

	// No "is this username free?" query first. Between the check and the insert
	// another signup can take it; only the UNIQUE constraint decides, and it
	// decides atomically.
	return s.repo.CreateProfile(ctx, userID, username, displayName)
}

func (s *Service) Own(ctx context.Context, userID uuid.UUID) (*Profile, []Link, error) {
	profile, err := s.repo.FindByUserID(ctx, userID)
	if err != nil {
		return nil, nil, err
	}

	// The owner sees inactive links too. To them a hidden link is a draft, not a
	// deletion, and it has to be visible to be switched back on.
	links, err := s.repo.ListLinks(ctx, profile.ID)
	if err != nil {
		return nil, nil, err
	}
	return profile, links, nil
}

func (s *Service) Update(ctx context.Context, userID uuid.UUID, displayName, bio *string, isPublished *bool) (*Profile, error) {
	if displayName != nil {
		trimmed := strings.TrimSpace(*displayName)
		if err := validateDisplayName(trimmed); err != nil {
			return nil, err
		}
		displayName = &trimmed
	}
	if bio != nil {
		trimmed := strings.TrimSpace(*bio)
		if len([]rune(trimmed)) > 500 {
			return nil, &ValidationError{"bio", "Bio must be under 500 characters."}
		}
		bio = &trimmed
	}

	profile, err := s.repo.UpdateProfile(ctx, userID, displayName, bio, isPublished)
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, userID)
	return profile, nil
}

// AddLink locks the profile, counts, then inserts, all in one transaction.
//
// The lock is what makes the count trustworthy. Without it, two concurrent
// requests at 49 links both count 49, both pass the cap, and the profile ends up
// with 51. That same lock serialises the MAX(position) + 1 that CreateLink
// computes. One lock, two races closed.
func (s *Service) AddLink(ctx context.Context, userID uuid.UUID, title, linkURL string) (*Link, error) {
	title = strings.TrimSpace(title)
	linkURL = strings.TrimSpace(linkURL)

	if err := validateTitle(title); err != nil {
		return nil, err
	}
	if err := validateURL(linkURL); err != nil {
		return nil, err
	}

	var created *Link
	err := s.repo.InTx(ctx, func(r *Repository) error {
		profileID, err := r.LockProfile(ctx, userID)
		if err != nil {
			return err
		}

		count, err := r.CountLinks(ctx, profileID)
		if err != nil {
			return err
		}
		if count >= maxLinks {
			return ErrTooManyLinks
		}

		created, err = r.CreateLink(ctx, profileID, title, linkURL)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, userID)
	return created, nil
}

func (s *Service) UpdateLink(ctx context.Context, userID, linkID uuid.UUID, title, linkURL *string, isActive *bool) (*Link, error) {
	if title != nil {
		trimmed := strings.TrimSpace(*title)
		if err := validateTitle(trimmed); err != nil {
			return nil, err
		}
		title = &trimmed
	}
	if linkURL != nil {
		trimmed := strings.TrimSpace(*linkURL)
		if err := validateURL(trimmed); err != nil {
			return nil, err
		}
		linkURL = &trimmed
	}

	profile, err := s.repo.FindByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}
	link, err := s.repo.UpdateLink(ctx, profile.ID, linkID, title, linkURL, isActive)
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, userID)
	return link, nil
}

func (s *Service) DeleteLink(ctx context.Context, userID, linkID uuid.UUID) error {
	profile, err := s.repo.FindByUserID(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.repo.DeleteLink(ctx, profile.ID, linkID); err != nil {
		return err
	}
	s.invalidate(ctx, userID)
	return nil
}

// Reorder demands the complete list, not a subset. Renumbering only some links
// would leave the rest on positions that now collide. The deferred constraint
// would catch that at COMMIT, but as an opaque 500 rather than a message telling
// the client what it actually got wrong.
func (s *Service) Reorder(ctx context.Context, userID uuid.UUID, ids []uuid.UUID) ([]Link, error) {
	if len(ids) == 0 {
		return nil, &ValidationError{"link_ids", "Provide the full list of link ids in their new order."}
	}

	seen := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			return nil, &ValidationError{"link_ids", "The same link appears more than once."}
		}
		seen[id] = struct{}{}
	}

	var ordered []Link
	err := s.repo.InTx(ctx, func(r *Repository) error {
		profileID, err := r.LockProfile(ctx, userID)
		if err != nil {
			return err
		}

		count, err := r.CountLinks(ctx, profileID)
		if err != nil {
			return err
		}
		if count != len(ids) {
			return ErrOrderIncomplete
		}

		if err := r.ReorderLinks(ctx, profileID, ids); err != nil {
			return err
		}

		// Read back inside the transaction, so the caller is shown the order the
		// database actually committed rather than the one we asked it for.
		ordered, err = r.ListLinks(ctx, profileID)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, userID)
	return ordered, nil
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

// usernamePattern mirrors the profiles_username_shape CHECK constraint. The
// duplication is intentional and one-directional: this copy exists to produce a
// helpful message, the constraint exists to be true. If the two ever disagree
// the database wins and the request fails, which is the correct way round.
var usernamePattern = regexp.MustCompile(`^[a-z0-9_]{3,30}$`)

// reservedUsernames mirrors profiles_username_reserved. These are route names;
// letting a creator take /@admin is how a phishing page gets a trustworthy URL.
var reservedUsernames = map[string]struct{}{
	"api": {}, "admin": {}, "login": {}, "signup": {}, "dashboard": {},
	"settings": {}, "about": {}, "pricing": {}, "support": {},
}

func validateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return &ValidationError{"username", "Username must be 3 to 30 characters, using only lowercase letters, numbers and underscores."}
	}
	if _, reserved := reservedUsernames[username]; reserved {
		return &ValidationError{"username", "That username is reserved."}
	}
	return nil
}

func validateDisplayName(displayName string) error {
	n := len([]rune(displayName))
	if n < 1 || n > 50 {
		return &ValidationError{"display_name", "Display name must be between 1 and 50 characters."}
	}
	return nil
}

func validateTitle(title string) error {
	n := len([]rune(title))
	if n < 1 || n > 80 {
		return &ValidationError{"title", "Title must be between 1 and 80 characters."}
	}
	return nil
}

// validateURL is stricter than the links_url_scheme CHECK, which only tests the
// prefix. "https://" passes a regex and is not a URL. More importantly,
// rejecting everything that is not http or https is what keeps javascript: and
// data: URLs off a page that renders creator-supplied hrefs to the public.
func validateURL(raw string) error {
	if len(raw) > 2048 {
		return &ValidationError{"url", "URL is too long."}
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return &ValidationError{"url", "Enter a valid URL."}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return &ValidationError{"url", "Link must start with http:// or https://."}
	}
	if parsed.Host == "" {
		return &ValidationError{"url", "Enter a valid URL, including the domain."}
	}
	return nil
}
