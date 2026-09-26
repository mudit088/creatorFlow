package storefront

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mudit/creatorflow/backend/internal/cache"
)

type Service struct {
	repo  *Repository
	cache *cache.Client
}

func NewService(repo *Repository, c *cache.Client) *Service {
	return &Service{repo: repo, cache: c}
}

// pageTTL is short on purpose. Invalidation on write is the fast path — an edit
// should show up immediately — and this is the safety net for the cases
// invalidation cannot cover: a Redis blip that swallowed a DELETE, a write path
// added later that nobody remembered to hook up, a row changed directly in the
// database. Five minutes bounds how wrong the public page can be, at the cost of
// one query per page per five minutes.
const pageTTL = 5 * time.Minute

// Keys carry a version. A change to the cached struct makes v1 entries
// undecodable, and rather than relying on every deploy to flush Redis, bumping
// this leaves the old entries to expire unread.
func pageKey(username string) string { return "storefront:v1:page:" + username }

func productKey(username, slug string) string {
	return "storefront:v1:product:" + username + ":" + slug
}

// InvalidateProfile drops everything cached for one creator.
//
// Only the page key is deleted by name. Product pages embed the creator, so a
// display-name change makes them stale too — but enumerating them would mean a
// SCAN or a secondary index, and the product entries expire within pageTTL
// anyway. The compromise is stated rather than hidden: the creator's own page
// updates instantly, their product pages within five minutes.
func (s *Service) InvalidateProfile(ctx context.Context, username string) {
	s.cache.Delete(ctx, pageKey(normalize(username)))
}

// InvalidateProduct drops a product page and the creator page that lists it,
// since a price or title change is visible on both.
func (s *Service) InvalidateProduct(ctx context.Context, username, slug string) {
	username, slug = normalize(username), normalize(slug)
	s.cache.Delete(ctx, productKey(username, slug), pageKey(username))
}

// Page assembles /@username in three queries rather than one.
//
// A single statement with json_agg would be one round trip, but it is markedly
// harder to read and to change, and the three here are all indexed lookups on a
// single profile id. More to the point, Phase 11 caches this whole struct under
// one key, so the steady-state cost of the public page is one Redis GET and the
// query count stops mattering. Optimising it now would be work aimed at a path
// that is about to stop being hot.
func (s *Service) Page(ctx context.Context, username string) (*Page, error) {
	username = normalize(username)

	// Cache-aside: ask the cache, fall through to the database on a miss, and
	// populate on the way back. Deliberately not write-through — the storefront
	// has many more readers than writers, and a write-through cache would put
	// Redis on the path of every profile edit for no benefit to the reader.
	var cached Page
	if s.cache.GetJSON(ctx, pageKey(username), &cached) {
		return &cached, nil
	}

	profileID, creator, err := s.repo.FindCreator(ctx, username)
	if err != nil {
		return nil, err
	}

	links, err := s.repo.ListLinks(ctx, profileID)
	if err != nil {
		return nil, err
	}

	products, err := s.repo.ListProducts(ctx, profileID)
	if err != nil {
		return nil, err
	}

	page := &Page{ProfileID: profileID, Creator: *creator, Links: links, Products: products}

	// Misses are not cached. A 404 for a username that does not exist is cheap
	// to compute, and caching negatives would let anyone fill Redis by walking
	// random handles.
	s.cache.SetJSON(ctx, pageKey(username), page, pageTTL)
	return page, nil
}

func (s *Service) Product(ctx context.Context, username, slug string) (*ProductDetail, error) {
	username, slug = normalize(username), normalize(slug)

	var cached ProductDetail
	if s.cache.GetJSON(ctx, productKey(username, slug), &cached) {
		return &cached, nil
	}

	productID, detail, err := s.repo.FindProduct(ctx, username, slug)
	if err != nil {
		return nil, err
	}

	includes, err := s.repo.ListDeliverables(ctx, productID)
	if err != nil {
		return nil, err
	}

	detail.ProductID = productID
	detail.Includes = includes

	s.cache.SetJSON(ctx, productKey(username, slug), detail, pageTTL)
	return detail, nil
}

// LinkTarget returns where a click should go, and whose page it was on.
func (s *Service) LinkTarget(ctx context.Context, username string, linkID uuid.UUID) (uuid.UUID, string, error) {
	return s.repo.FindLinkTarget(ctx, normalize(username), linkID)
}

// normalize lowercases and trims. username and slug are citext columns so
// Postgres already compares case-insensitively, but normalising here means the
// cache key for /@Rahul and /@rahul is the same string, which matters the moment
// Phase 11 starts keying on it.
func normalize(s string) string {
	return strings.TrimSpace(strings.ToLower(s))
}
