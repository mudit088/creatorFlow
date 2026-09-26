package orders

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Order lifecycle. Only the webhook may move an order to paid; nothing in this
// package ever writes that status, which is why there is no MarkPaid here yet.
const (
	StatusPending   = "pending"
	StatusPaid      = "paid"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	StatusRefunded  = "refunded"
)

type Order struct {
	ID        uuid.UUID
	ProfileID uuid.UUID
	// The buyer has no account. This email is the entire identity behind a
	// purchase and, from phase 9, the thing an entitlement is checked against.
	BuyerEmail string
	Status     string
	TotalMinor int64
	Currency   string
	// NULL until the Razorpay order is created and attached. Our row exists
	// first on purpose: a provider order we have no local record of is far worse
	// than a local order with no provider order, because only one of those two
	// can take a buyer's money.
	ProviderOrderID *string
	IdempotencyKey  *string
	PaidAt          *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Item is a line frozen at purchase time. TitleSnapshot and UnitPriceMinor are
// copies rather than joins so that a receipt keeps saying what the buyer agreed
// to after the creator renames the product or changes its price.
type Item struct {
	ID             uuid.UUID
	OrderID        uuid.UUID
	ProductID      uuid.UUID
	TitleSnapshot  string
	UnitPriceMinor int64
	Quantity       int
}

// purchasable is what the database says a product is worth right now. It is
// deliberately not products.Product: this package needs four fields to price an
// order, and depending on the whole type would couple the checkout to every
// future change in the product model.
type purchasable struct {
	ID         uuid.UUID
	ProfileID  uuid.UUID
	Title      string
	PriceMinor int64
	Currency   string
}

var (
	ErrOrderNotFound = errors.New("order not found")
	// ErrProductUnavailable covers every reason a product cannot be bought —
	// wrong id, draft, archived, or published with nothing to deliver. They are
	// one error on purpose: an unauthenticated caller must not be able to use
	// the difference between "does not exist" and "exists but is a draft" to
	// enumerate a creator's unpublished catalogue.
	ErrProductUnavailable = errors.New("one or more products cannot be purchased")
	// ErrMixedCreators rejects a cart spanning two creators. One order has one
	// profile_id because it settles into one creator's payout; splitting a
	// single Razorpay capture between two recipients is a different product with
	// different regulatory weight, not a bigger query.
	ErrMixedCreators = errors.New("an order cannot span two creators")
	// ErrFreeCheckout stops a zero-value order reaching a payment provider that
	// would reject it anyway. Free products need a delivery path that does not
	// involve a payment at all, which is phase 9's problem, not this one.
	ErrFreeCheckout  = errors.New("free products do not go through checkout")
	ErrMixedCurrency = errors.New("an order cannot mix currencies")
	// ErrPaymentProviderUnavailable means our order exists but Razorpay could
	// not be reached or refused the request. Deliberately distinct from an
	// internal error: the right client response is "try again", and the right
	// server response is to leave the pending order alone so the retry can
	// finish it rather than create a second one.
	ErrPaymentProviderUnavailable = errors.New("payment provider unavailable")
)

// maxItemsPerOrder is a guard on an endpoint anyone on the internet can call.
// Without it, a single request can ask us to price and insert an unbounded
// number of rows inside one transaction.
const maxItemsPerOrder = 10
