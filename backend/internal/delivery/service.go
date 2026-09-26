package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ObjectStore is declared here, where it is consumed. Delivery needs exactly one
// capability — turn a key into a short-lived URL — and depending on that rather
// than on a concrete S3 client is what lets the tests below run with no MinIO.
type ObjectStore interface {
	PresignDownload(ctx context.Context, key, downloadAs string, ttl time.Duration) (string, error)
}

type Service struct {
	repo  *Repository
	store ObjectStore
}

func NewService(repo *Repository, store ObjectStore) *Service {
	return &Service{repo: repo, store: store}
}

// Link is one downloadable file, with the URL the browser will fetch directly.
type Link struct {
	ProductID    uuid.UUID
	ProductTitle string
	Filename     string
	ContentType  string
	SizeBytes    int64
	URL          string
	ExpiresIn    time.Duration
}

// Download authorises a buyer and hands back presigned URLs.
//
// The bytes never pass through this process: the API decides who may have the
// file and then steps out of the way, exactly as the upload path does in
// reverse. A 2 GB course download costs this endpoint one query and one
// signature, not 2 GB of memory and a held connection.
func (s *Service) Download(ctx context.Context, orderID uuid.UUID, buyerEmail string) ([]Link, error) {
	buyerEmail = strings.TrimSpace(buyerEmail)
	if !emailPattern.MatchString(buyerEmail) || len(buyerEmail) > 254 {
		// Same error as a genuine miss, so a malformed address cannot be used to
		// tell "this order exists" from "this order does not".
		return nil, ErrNotEntitled
	}

	deliverables, err := s.repo.FindDeliverables(ctx, orderID, buyerEmail)
	if err != nil {
		return nil, err
	}
	if len(deliverables) == 0 {
		// Logged, not returned. A burst of these against one order id is someone
		// guessing a buyer's email, which is worth seeing in the logs — while the
		// caller still learns nothing.
		slog.Info("download refused", "order_id", orderID)
		return nil, ErrNotEntitled
	}

	links := make([]Link, 0, len(deliverables))
	for _, d := range deliverables {
		// downloadAs sets Content-Disposition, so the buyer's browser saves
		// "12-week-plan.pdf" rather than the uuid the object is stored under.
		// The storage key stays an internal detail they never see.
		url, err := s.store.PresignDownload(ctx, d.S3Key, d.OriginalName, downloadTTL)
		if err != nil {
			return nil, fmt.Errorf("presign %s: %w", d.S3Key, err)
		}

		links = append(links, Link{
			ProductID:    d.ProductID,
			ProductTitle: d.ProductTitle,
			Filename:     d.OriginalName,
			ContentType:  d.ContentType,
			SizeBytes:    d.SizeBytes,
			URL:          url,
			ExpiresIn:    downloadTTL,
		})
	}

	slog.Info("download authorised", "order_id", orderID, "files", len(links))
	return links, nil
}

// emailPattern matches the loose check used at checkout. The two must agree:
// an address accepted when buying and rejected when downloading would take
// someone's money and then refuse them the thing they bought.
var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
