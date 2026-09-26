package orders

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/mudit/creatorflow/backend/internal/razorpay"
)

// PaymentProvider is declared here, where it is consumed, rather than in the
// razorpay package. The checkout depends on the three things it needs, which is
// what lets a test drive this service without a Razorpay account — and what
// would make swapping Stripe in a change to one adapter rather than to this
// file.
type PaymentProvider interface {
	Enabled() bool
	KeyID() string
	CreateOrder(ctx context.Context, amountMinor int64, currency, receipt string, notes map[string]string) (*razorpay.Order, error)
}

type Service struct {
	repo     *Repository
	provider PaymentProvider
}

func NewService(repo *Repository, provider PaymentProvider) *Service {
	return &Service{repo: repo, provider: provider}
}

// Create turns "I want these products" into a pending order priced entirely
// from the database.
//
// What the caller supplies: which products, and where to send the goods. What
// the caller does not supply, and is never asked for: the amount. The total is
// summed from rows this service read itself, which is the difference between a
// checkout and a donation box.
//
// The replayed return value distinguishes "created" from "you already sent
// this". A double-clicked Buy button must not produce two orders, two Razorpay
// orders and two chances to be charged.
func (s *Service) Create(ctx context.Context, buyerEmail string, productIDs []string, idempotencyKey *string) (order *Order, items []Item, replayed bool, err error) {
	buyerEmail = strings.TrimSpace(buyerEmail)
	if err := validateEmail(buyerEmail); err != nil {
		return nil, nil, false, err
	}

	productIDs, err = normaliseProductIDs(productIDs)
	if err != nil {
		return nil, nil, false, err
	}

	if idempotencyKey != nil {
		key := strings.TrimSpace(*idempotencyKey)
		if key == "" {
			idempotencyKey = nil
		} else if len(key) > 255 {
			return nil, nil, false, &ValidationError{"idempotency_key", "Idempotency key must be at most 255 characters."}
		} else {
			idempotencyKey = &key
		}
	}

	err = s.repo.InTx(ctx, func(tx *Repository) error {
		// Priced inside the transaction, so the rows that set the total are the
		// same rows the snapshots are written from. Reading prices first and
		// inserting afterwards leaves a window where a creator's price change
		// lands in between and the order total no longer matches its items.
		available, err := tx.LoadPurchasable(ctx, productIDs)
		if err != nil {
			return err
		}

		// Fewer rows back than ids sent means at least one product is missing,
		// draft, archived or undeliverable. Which one it was is not disclosed.
		if len(available) != len(productIDs) {
			return ErrProductUnavailable
		}

		profileID := available[0].ProfileID
		currency := available[0].Currency
		var total int64
		for _, p := range available {
			if p.ProfileID != profileID {
				return ErrMixedCreators
			}
			if p.Currency != currency {
				return ErrMixedCurrency
			}
			total += p.PriceMinor
		}

		if total <= 0 {
			return ErrFreeCheckout
		}

		created, err := tx.CreateOrder(ctx, profileID, buyerEmail, total, currency, idempotencyKey)
		if err != nil {
			return err
		}

		for _, p := range available {
			if err := tx.AddItem(ctx, created.ID, p.ID, p.Title, p.PriceMinor); err != nil {
				return err
			}
		}

		order = created
		items, err = tx.ListItems(ctx, created.ID)
		return err
	})

	// The replay path. The unique index rejected the second insert, which is the
	// atomic version of this check — asking "does an order with this key exist?"
	// before inserting is a race that two concurrent clicks both win.
	if errors.Is(err, errIdempotencyConflict) && idempotencyKey != nil {
		existing, existingItems, ferr := s.findByKey(ctx, *idempotencyKey)
		if ferr != nil {
			return nil, nil, false, ferr
		}
		// The replay also goes through the provider step. That is what makes a
		// retry useful after Razorpay was unreachable: the first attempt left a
		// pending order with no provider id, and this one finishes the job
		// instead of handing back a permanently unpayable order.
		withProvider, perr := s.ensureProviderOrder(ctx, existing)
		if perr != nil {
			return nil, nil, false, perr
		}
		return withProvider, existingItems, true, nil
	}
	if err != nil {
		return nil, nil, false, err
	}

	// Razorpay is called only after our transaction has committed, and never
	// inside it. Two reasons, both learned the expensive way by other people:
	//
	// Holding a database transaction open across a third-party HTTP call means a
	// pgx connection is pinned for as long as that call takes, so a slow payment
	// provider becomes a database connection outage.
	//
	// And if the provider call succeeds but the commit then fails, Razorpay is
	// holding an order for a purchase we have no record of — money can be taken
	// for something we cannot deliver or even identify. The other way round, a
	// committed order with no provider id, is a row we can see, retry and expire.
	// Given a choice of which half to lose, lose the recoverable one.
	withProvider, perr := s.ensureProviderOrder(ctx, order)
	if perr != nil {
		return nil, nil, false, perr
	}
	return withProvider, items, false, nil
}

// PublicKeyID is what the browser's checkout script needs. It is the public half
// of the key pair; the secret never leaves this process.
func (s *Service) PublicKeyID() string {
	if s.provider == nil || !s.provider.Enabled() {
		return ""
	}
	return s.provider.KeyID()
}

// ensureProviderOrder gives an order a Razorpay order id, if it has none.
//
// Idempotent by design: called on a fresh order and on every replay, and it does
// nothing when the id is already there. That is what makes "retry the same
// request" a complete recovery story rather than a way to accumulate abandoned
// Razorpay orders.
func (s *Service) ensureProviderOrder(ctx context.Context, order *Order) (*Order, error) {
	// No keys configured — local development. The order is real and the flow is
	// testable; only the payment itself is unavailable. Failing here instead
	// would make every developer without a Razorpay account unable to exercise
	// checkout at all.
	if s.provider == nil || !s.provider.Enabled() {
		return order, nil
	}
	if order.ProviderOrderID != nil {
		return order, nil
	}

	providerOrder, err := s.provider.CreateOrder(ctx, order.TotalMinor, order.Currency, order.ID.String(), map[string]string{
		// Notes are visible in Razorpay's dashboard. When someone asks about a
		// payment six months from now, this is what turns their reference into
		// one of our rows without a human matching timestamps.
		"order_id":   order.ID.String(),
		"profile_id": order.ProfileID.String(),
	})
	if err != nil {
		// The local order survives, pending. The buyer sees a failure, the
		// client can retry with the same idempotency key, and the pending row is
		// visible to the expiry job either way.
		return nil, fmt.Errorf("%w: %v", ErrPaymentProviderUnavailable, err)
	}

	updated, err := s.repo.AttachProviderOrder(ctx, order.ID, providerOrder.ID)
	if errors.Is(err, errProviderOrderAlreadySet) {
		// A concurrent request attached one first. Theirs is the id the buyer's
		// checkout window is using, so read and return that rather than ours.
		return s.repo.FindByID(ctx, order.ID)
	}
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Service) findByKey(ctx context.Context, key string) (*Order, []Item, error) {
	order, err := s.repo.FindByIdempotencyKey(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	items, err := s.repo.ListItems(ctx, order.ID)
	if err != nil {
		return nil, nil, err
	}
	return order, items, nil
}

// normaliseProductIDs deduplicates before counting. order_items has a UNIQUE
// (order_id, product_id), so sending the same id twice would otherwise be a
// constraint violation surfacing as a 500 instead of the harmless "you asked
// for it twice, you get it once" it actually is.
func normaliseProductIDs(ids []string) ([]string, error) {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))

	for _, raw := range ids {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			return nil, &ValidationError{"product_ids", "Every product id must be a valid identifier."}
		}
		key := id.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}

	if len(out) == 0 {
		return nil, &ValidationError{"product_ids", "Choose at least one product."}
	}
	if len(out) > maxItemsPerOrder {
		return nil, &ValidationError{"product_ids", "An order can contain at most 10 products."}
	}
	return out, nil
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

// emailPattern is intentionally loose. Strict RFC 5322 validation rejects
// addresses that genuinely deliver, and the only real proof an address works is
// sending to it — which phase 11 does with the receipt. This check exists to
// catch a typo before it becomes an undeliverable purchase, and to match the
// orders_email_shape constraint that will reject the rest.
var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func validateEmail(email string) error {
	if email == "" {
		return &ValidationError{"buyer_email", "Enter the email the purchase should be delivered to."}
	}
	if len(email) > 254 || !emailPattern.MatchString(email) {
		return &ValidationError{"buyer_email", "Enter a valid email address."}
	}
	return nil
}
