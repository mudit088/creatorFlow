package storefront

import (
	"context"
	"strings"
)

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
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
	profileID, creator, err := s.repo.FindCreator(ctx, normalize(username))
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

	return &Page{Creator: *creator, Links: links, Products: products}, nil
}

func (s *Service) Product(ctx context.Context, username, slug string) (*ProductDetail, error) {
	productID, detail, err := s.repo.FindProduct(ctx, normalize(username), normalize(slug))
	if err != nil {
		return nil, err
	}

	includes, err := s.repo.ListDeliverables(ctx, productID)
	if err != nil {
		return nil, err
	}

	detail.Includes = includes
	return detail, nil
}

// normalize lowercases and trims. username and slug are citext columns so
// Postgres already compares case-insensitively, but normalising here means the
// cache key for /@Rahul and /@rahul is the same string, which matters the moment
// Phase 11 starts keying on it.
func normalize(s string) string {
	return strings.TrimSpace(strings.ToLower(s))
}
