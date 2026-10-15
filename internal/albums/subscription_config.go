package albums

import (
	"crypto/ecdsa"
	"crypto/x509"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/appstore"
)

type AlbumSubscriptionConfig struct {
	RebindingEnabled                       bool
	RecordErasureEnabled                   bool
	BundleID, Environment, IssuerID, KeyID string
	AppAppleID                             int64
	SigningKey                             *ecdsa.PrivateKey
	Roots                                  *x509.CertPool
}

func (AlbumSubscriptionConfig) String() string { return "[redacted album subscription configuration]" }
func (AlbumSubscriptionConfig) GoString() string {
	return "[redacted album subscription configuration]"
}

func albumStoreEnvironment(deployment string) string {
	if deployment == "test" {
		return "Sandbox"
	}
	if deployment == "production" {
		return "Production"
	}
	return ""
}

func loadAlbumSubscriptionConfig(getenv func(string) string, deployment string) (*AlbumSubscriptionConfig, error) {
	rebinding := strings.TrimSpace(getenv("ALBUM_SUBSCRIPTION_REBINDING_ENABLED"))
	if rebinding != "" && rebinding != "false" && rebinding != "true" {
		return nil, errors.New("invalid ALBUM_SUBSCRIPTION_REBINDING_ENABLED")
	}
	erasure := strings.TrimSpace(getenv("ALBUM_ACCOUNT_RECORD_ERASURE_ENABLED"))
	if erasure != "" && erasure != "false" && erasure != "true" {
		return nil, errors.New("invalid ALBUM_ACCOUNT_RECORD_ERASURE_ENABLED")
	}
	if erasure == "true" && rebinding != "true" {
		return nil, errors.New("album record erasure requires subscription rebinding")
	}
	enabled := strings.TrimSpace(getenv("ALBUM_SUBSCRIPTIONS_ENABLED"))
	if enabled == "" || enabled == "false" {
		if rebinding == "true" {
			return nil, errors.New("album subscription rebinding requires verification")
		}
		return nil, nil
	}
	if enabled != "true" {
		return nil, errors.New("invalid ALBUM_SUBSCRIPTIONS_ENABLED")
	}
	c := &AlbumSubscriptionConfig{BundleID: albumAudienceForDeployment(deployment), RebindingEnabled: rebinding == "true", RecordErasureEnabled: erasure == "true", Environment: albumStoreEnvironment(deployment), IssuerID: strings.TrimSpace(getenv("ALBUM_APP_STORE_ISSUER_ID")), KeyID: strings.TrimSpace(getenv("ALBUM_APP_STORE_KEY_ID"))}
	issuer, err := uuid.Parse(c.IssuerID)
	if err != nil || issuer.String() != c.IssuerID || !regexp.MustCompile(`^[A-Z0-9]{10}$`).MatchString(c.KeyID) || c.Environment == "" {
		return nil, errors.New("invalid album App Store identity")
	}
	if value := strings.TrimSpace(getenv("ALBUM_APP_STORE_APP_APPLE_ID")); value != "" {
		c.AppAppleID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || c.AppAppleID <= 0 {
			return nil, errors.New("invalid ALBUM_APP_STORE_APP_APPLE_ID")
		}
	}
	if c.Environment == "Production" && c.AppAppleID <= 0 {
		return nil, errors.New("production album App Store app ID required")
	}
	key, err := readAlbumSecretFile(getenv("ALBUM_APP_STORE_KEY_FILE"), 16<<10)
	if err != nil {
		return nil, errors.New("invalid ALBUM_APP_STORE_KEY_FILE")
	}
	c.SigningKey, err = appstore.ParseSigningKeyPEM(key)
	if err != nil {
		return nil, errors.New("invalid album App Store signing key")
	}
	roots, err := readAlbumSecretFile(getenv("ALBUM_APP_STORE_ROOTS_FILE"), 64<<10)
	if err != nil {
		return nil, errors.New("invalid ALBUM_APP_STORE_ROOTS_FILE")
	}
	c.Roots = x509.NewCertPool()
	if !c.Roots.AppendCertsFromPEM(roots) {
		return nil, errors.New("invalid album App Store trust roots")
	}
	return c, nil
}

func newConfiguredAlbumSubscriptions(repository *MySQLUploads, c *AlbumSubscriptionConfig, transport http.RoundTripper) (*AlbumSubscriptionService, error) {
	if c == nil {
		return nil, nil
	}
	if !validAlbumAudience(c.BundleID) || repository == nil || repository.billingEnvironment != c.Environment || c.Roots == nil || c.SigningKey == nil || (c.RecordErasureEnabled && !c.RebindingEnabled) ||
		c.IssuerID == "" || c.KeyID == "" || (c.Environment == "Production" && c.AppAppleID <= 0) {
		return nil, ErrSubscriptionUnavailable
	}
	base := "https://api.storekit.apple.com"
	if c.Environment == "Sandbox" {
		base = "https://api.storekit-sandbox.apple.com"
	} else if c.Environment != "Production" {
		return nil, ErrSubscriptionUnavailable
	}
	httpClient := &http.Client{Transport: transportOrDefault(transport), Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("album App Store redirect refused") }}
	verifier := appstore.NewTransactionVerifier(appstore.VerifierConfig{Roots: c.Roots, BundleID: c.BundleID, AppAppleID: c.AppAppleID,
		Environment: c.Environment, ProductIDs: []string{Cloud50GBAnnualProduct, Cloud500GBAnnualProduct}})
	client := appstore.NewAPIClient(appstore.APIClientConfig{BaseURL: base, KeyID: c.KeyID, IssuerID: c.IssuerID,
		BundleID: c.BundleID, AppAppleID: c.AppAppleID, Environment: c.Environment, SigningKey: c.SigningKey, HTTPClient: httpClient})
	resolver, err := NewAlbumSubscriptionResolver(verifier, client, c.Environment, time.Now)
	if err != nil {
		return nil, err
	}
	service, err := NewAlbumSubscriptionService(repository, resolver, time.Now)
	if err == nil {
		service.rebindingEnabled = c.RebindingEnabled
		service.recordErasureEnabled = c.RecordErasureEnabled
	}
	return service, err
}
