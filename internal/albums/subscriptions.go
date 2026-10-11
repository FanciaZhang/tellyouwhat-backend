package albums

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/appstore"
)

const (
	Cloud50GBAnnualProduct           = "albums.cloud.50gb.annual"
	Cloud500GBAnnualProduct          = "albums.cloud.500gb.annual"
	albumSubscriptionFreshness       = 15 * time.Minute
	albumSubscriptionRefreshInterval = 5 * time.Minute
)

var (
	ErrSubscriptionRequired     = errors.New("album cloud subscription required")
	ErrSubscriptionVerification = errors.New("album subscription verification failed")
	ErrSubscriptionUnavailable  = errors.New("album subscription verification unavailable")
	ErrSubscriptionBinding      = errors.New("album subscription account binding mismatch")
	ErrSubscriptionConflict     = errors.New("album subscription changed during verification")
	ErrSubscriptionStale        = errors.New("album subscription status needs revalidation")
)

func subscriptionCapacity(product string) int64 {
	switch product {
	case Cloud50GBAnnualProduct:
		return 50_000_000_000
	case Cloud500GBAnnualProduct:
		return 500_000_000_000
	default:
		return 0
	}
}

// Prices come from StoreKit. These are capacity identifiers, not live products
// or a client-selected quota. A server-verified current status supplies the SKU.
type VerifiedAlbumSubscription struct {
	OriginalTransactionID, TransactionID, ProductID, Environment string
	ExpiresAt, SignedAt                                          time.Time
	Active                                                       bool
}

type AlbumSubscriptionStatuses interface {
	AllSubscriptionStatuses(context.Context, string) ([]appstore.SubscriptionStatus, error)
}

type cloudMediaGrantAuthorizer interface {
	CloudMediaGrantDeadline(context.Context, string, time.Time) (time.Time, error)
}

func cloudMediaGrantExpiry(ctx context.Context, repository any, owner string, now, expiry time.Time) (time.Time, error) {
	if authorizer, ok := repository.(cloudMediaGrantAuthorizer); ok {
		deadline, err := authorizer.CloudMediaGrantDeadline(ctx, owner, now)
		if err != nil {
			return time.Time{}, err
		}
		if !deadline.IsZero() && deadline.Before(expiry) {
			expiry = deadline
		}
	}
	if !now.Before(expiry) {
		return time.Time{}, ErrSubscriptionRequired
	}
	return expiry, nil
}

type AlbumSubscriptionResolver struct {
	verifier    *appstore.TransactionVerifier
	client      AlbumSubscriptionStatuses
	environment string
	now         func() time.Time
}

func NewAlbumSubscriptionResolver(verifier *appstore.TransactionVerifier, client AlbumSubscriptionStatuses, environment string, now func() time.Time) (*AlbumSubscriptionResolver, error) {
	if verifier == nil || client == nil || (environment != "Sandbox" && environment != "Production") {
		return nil, ErrSubscriptionUnavailable
	}
	if now == nil {
		now = time.Now
	}
	return &AlbumSubscriptionResolver{verifier, client, environment, now}, nil
}

func transactionBelongsToAccount(t appstore.Transaction, owner, environment string) bool {
	token, err := uuid.Parse(t.AppAccountToken)
	return err == nil && token.String() == owner && strings.EqualFold(token.String(), t.AppAccountToken) &&
		t.Payment.OwnershipType == "PURCHASED" && strings.EqualFold(t.Environment, environment) &&
		subscriptionCapacity(t.ProductID) > 0 && t.SignedAt.UnixMilli() > 0 &&
		t.StartedAt.UnixMilli() > 0 && t.Payment.PurchasedAt.UnixMilli() > 0 &&
		!t.Payment.PurchasedAt.Before(t.StartedAt) && t.ExpiresAt.After(t.Payment.PurchasedAt) &&
		len(t.OriginalTransactionID) > 0 && len(t.OriginalTransactionID) <= 128 && len(t.TransactionID) > 0 && len(t.TransactionID) <= 128
}

func (r *AlbumSubscriptionResolver) Resolve(ctx context.Context, owner, signed string) (VerifiedAlbumSubscription, error) {
	if r == nil || !validOwner(owner) || signed == "" || len(signed) > 1<<20 {
		return VerifiedAlbumSubscription{}, ErrSubscriptionVerification
	}
	submitted, err := r.verifier.VerifyTransaction(signed)
	if err != nil {
		return VerifiedAlbumSubscription{}, ErrSubscriptionVerification
	}
	if !transactionBelongsToAccount(submitted, owner, r.environment) {
		return VerifiedAlbumSubscription{}, ErrSubscriptionBinding
	}
	return r.resolveCurrent(ctx, owner, submitted.OriginalTransactionID, submitted.TransactionID)
}

// Only server-owned anchors or a separately verified incoming JWS reach this
// method. An HTTP caller cannot choose the owner or original subscription chain.
func (r *AlbumSubscriptionResolver) resolveCurrent(ctx context.Context, owner, originalID, transactionID string) (VerifiedAlbumSubscription, error) {
	if r == nil || !validOwner(owner) || originalID == "" || len(originalID) > 128 || transactionID == "" || len(transactionID) > 128 {
		return VerifiedAlbumSubscription{}, ErrSubscriptionVerification
	}
	statuses, err := r.client.AllSubscriptionStatuses(ctx, transactionID)
	if err != nil {
		return VerifiedAlbumSubscription{}, ErrSubscriptionUnavailable
	}
	var latest VerifiedAlbumSubscription
	now := r.now().UTC()
	for _, status := range statuses {
		t, err := r.verifier.VerifyTransaction(status.SignedTransaction)
		if err != nil {
			return VerifiedAlbumSubscription{}, ErrSubscriptionVerification
		}
		if t.OriginalTransactionID != originalID {
			continue
		}
		if !transactionBelongsToAccount(t, owner, r.environment) {
			return VerifiedAlbumSubscription{}, ErrSubscriptionBinding
		}
		if t.IsUpgraded {
			continue
		}
		if status.Status < 1 || status.Status > 5 {
			return VerifiedAlbumSubscription{}, ErrSubscriptionUnavailable
		}
		expiry, stamp := t.ExpiresAt.UTC(), t.SignedAt.UTC()
		if status.SignedRenewal != "" {
			renewal, err := r.verifier.VerifyRenewal(status.SignedRenewal)
			if err != nil || renewal.OriginalTransactionID != t.OriginalTransactionID || !strings.EqualFold(renewal.Environment, r.environment) {
				return VerifiedAlbumSubscription{}, ErrSubscriptionVerification
			}
			if renewal.SignedAt.After(stamp) {
				stamp = renewal.SignedAt.UTC()
			}
			if status.Status == 4 {
				if !renewal.GracePeriodExpiresAt.After(expiry) {
					return VerifiedAlbumSubscription{}, ErrSubscriptionUnavailable
				}
				expiry = renewal.GracePeriodExpiresAt.UTC()
			} else if (status.Status == 2 || status.Status == 3) && renewal.GracePeriodExpiresAt.After(expiry) {
				expiry = renewal.GracePeriodExpiresAt.UTC()
			}
		} else if status.Status == 4 {
			return VerifiedAlbumSubscription{}, ErrSubscriptionVerification
		}
		if status.Status == 5 {
			if t.RevokedAt == nil || t.RevokedAt.UnixMilli() <= 0 || t.RevokedAt.After(now.Add(5*time.Minute)) {
				return VerifiedAlbumSubscription{}, ErrSubscriptionUnavailable
			}
			if t.RevokedAt.Before(expiry) {
				expiry = t.RevokedAt.UTC()
			}
		} else if t.RevokedAt != nil {
			return VerifiedAlbumSubscription{}, ErrSubscriptionUnavailable
		}
		if (status.Status == 2 || status.Status == 3 || status.Status == 5) && expiry.After(now) {
			return VerifiedAlbumSubscription{}, ErrSubscriptionUnavailable
		}
		v := VerifiedAlbumSubscription{t.OriginalTransactionID, t.TransactionID, t.ProductID, r.environment, expiry, stamp,
			(status.Status == 1 || status.Status == 4) && now.Before(expiry)}
		if latest.OriginalTransactionID == "" || v.SignedAt.After(latest.SignedAt) {
			latest = v
		} else if v.SignedAt.Equal(latest.SignedAt) && (v.TransactionID != latest.TransactionID || v.ProductID != latest.ProductID || !v.ExpiresAt.Equal(latest.ExpiresAt) || v.Active != latest.Active) {
			return VerifiedAlbumSubscription{}, ErrSubscriptionUnavailable
		}
	}
	if latest.OriginalTransactionID == "" {
		return VerifiedAlbumSubscription{}, ErrSubscriptionUnavailable
	}
	return latest, nil
}

// Calendar months in UTC, clamping the day to the target month's last day.
// August 31 + six months is February 28/29, not March or a fixed 180 days.
func albumRetentionDeadline(expiry time.Time) time.Time {
	expiry = expiry.UTC()
	year, month, day := expiry.Date()
	target := time.Date(year, month+6, 1, expiry.Hour(), expiry.Minute(), expiry.Second(), expiry.Nanosecond(), time.UTC)
	last := time.Date(target.Year(), target.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if day > last {
		day = last
	}
	return time.Date(target.Year(), target.Month(), day, expiry.Hour(), expiry.Minute(), expiry.Second(), expiry.Nanosecond(), time.UTC)
}

type AlbumCloudAccount struct {
	AccountID             string     `json:"accountID"`
	State                 string     `json:"state"`
	ProductID             string     `json:"productID,omitempty"`
	QuotaBytes            int64      `json:"quotaBytes"`
	UsedBytes             int64      `json:"usedBytes"`
	ReservedBytes         int64      `json:"reservedBytes"`
	AvailableBytes        int64      `json:"availableBytes"`
	Revision              int64      `json:"revision"`
	ExpiresAt             *time.Time `json:"expiresAt,omitempty"`
	RetentionDeadline     *time.Time `json:"retentionDeadline,omitempty"`
	PurchasesEnabled      bool       `json:"purchasesEnabled"`
	VerificationExpiresAt *time.Time `json:"verificationExpiresAt,omitempty"`
	VerificationPending   bool       `json:"verificationPending"`
}

type AlbumSubscriptionRepository interface {
	CloudAccount(context.Context, string, time.Time) (AlbumCloudAccount, error)
	ApplySubscription(context.Context, string, int64, VerifiedAlbumSubscription, time.Time) (AlbumCloudAccount, error)
}

type AlbumSubscriptionService struct {
	repository AlbumSubscriptionRepository
	resolver   *AlbumSubscriptionResolver
	now        func() time.Time
}

func NewAlbumSubscriptionService(repository AlbumSubscriptionRepository, resolver *AlbumSubscriptionResolver, now func() time.Time) (*AlbumSubscriptionService, error) {
	if repository == nil || resolver == nil {
		return nil, ErrSubscriptionUnavailable
	}
	if now == nil {
		now = time.Now
	}
	return &AlbumSubscriptionService{repository, resolver, now}, nil
}

func (s *AlbumSubscriptionService) Sync(ctx context.Context, owner, signed string) (AlbumCloudAccount, error) {
	if s == nil {
		return AlbumCloudAccount{}, ErrSubscriptionUnavailable
	}
	before, err := s.repository.CloudAccount(ctx, owner, s.now().UTC())
	if err != nil {
		return AlbumCloudAccount{}, err
	}
	v, err := s.resolver.Resolve(ctx, owner, signed)
	if err != nil {
		return AlbumCloudAccount{}, err
	}
	return s.repository.ApplySubscription(ctx, owner, before.Revision, v, s.now().UTC())
}
