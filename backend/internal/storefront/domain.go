// Package storefront is the public read model: what an anonymous visitor to
// /@username sees. It deliberately does not reuse the profiles or products
// response types.
//
// Those are owner-facing shapes, and every field added to one of them would
// otherwise silently become public the day someone reuses the struct. Keeping a
// separate model means exposing something new to the world is an explicit edit
// to a type that exists for that purpose, and it is also the natural cache unit
// for Phase 11: one page, one key, one entry.
package storefront

import "errors"

// Creator is the public view of a profile. No id, no user_id, no is_published,
// no timestamps. A visitor needs none of them.
type Creator struct {
	Username    string
	DisplayName string
	Bio         *string
}

type Link struct {
	Title string
	URL   string
}

// ProductSummary is a card on the creator's page.
type ProductSummary struct {
	Slug       string
	Title      string
	PriceMinor int64
	Currency   string
}

// ProductDetail is the product's own page. It carries the creator so the page
// can be rendered from one request rather than two.
type ProductDetail struct {
	Creator     Creator
	Slug        string
	Title       string
	Description *string
	PriceMinor  int64
	Currency    string
	// Includes is what the buyer gets. Names and sizes only: the s3_key is an
	// internal address and publishing the bucket layout helps nobody but someone
	// probing it.
	Includes []Deliverable
}

type Deliverable struct {
	Name        string
	ContentType string
	SizeBytes   int64
}

type Page struct {
	Creator  Creator
	Links    []Link
	Products []ProductSummary
}

// ErrNotFound covers every miss this package can produce: no such creator, an
// unpublished creator, no such product, and an unpublished product.
//
// One error on purpose. Distinguishing them would answer "does this handle
// exist?" for anyone who asks, turning the public endpoint into a directory of
// drafts and reserved names. A visitor cannot tell the difference, and that is
// the point.
var ErrNotFound = errors.New("not found")
