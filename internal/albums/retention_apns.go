package albums

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/tellyouwhat/backend/internal/appstore"
	"golang.org/x/net/http2"
)

var errAlbumAPNS = errors.New("album notification request unavailable")

// APNs environment follows the app's signing entitlement, not StoreKit's environment.
type albumAPNSConfig struct {
	TeamID, KeyID, Environment string
	PrivateKeyPEM              []byte `json:"-"`
}

func (albumAPNSConfig) String() string   { return "[redacted album push configuration]" }
func (albumAPNSConfig) GoString() string { return "[redacted album push configuration]" }

type albumAPNSResult struct {
	// accepted means APNs accepted the request, never proof of device delivery.
	Status        string
	InvalidatedAt time.Time
	RetryAt       time.Time
}

type albumAPNS struct {
	host, team, keyID string
	key               *ecdsa.PrivateKey
	client            *http.Client
	now               func() time.Time
	mu                sync.Mutex
	token             string
	issuedAt          time.Time
}

func (*albumAPNS) String() string   { return "[redacted album push sender]" }
func (*albumAPNS) GoString() string { return "[redacted album push sender]" }

func newAlbumAPNS(c albumAPNSConfig, transport http.RoundTripper) (*albumAPNS, error) {
	if !appleIdentifierPattern.MatchString(c.TeamID) || !appleIdentifierPattern.MatchString(c.KeyID) || len(c.PrivateKeyPEM) > 16<<10 {
		return nil, errAlbumAPNS
	}
	key, err := appstore.ParseSigningKeyPEM(c.PrivateKeyPEM)
	if err != nil {
		return nil, errAlbumAPNS
	}
	host := "api.push.apple.com"
	if c.Environment == "development" {
		host = "api.sandbox.push.apple.com"
	} else if c.Environment != "production" {
		return nil, errAlbumAPNS
	}
	if transport == nil {
		transport = &http2.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ReadIdleTimeout: 30 * time.Second, PingTimeout: 15 * time.Second}
	}
	return &albumAPNS{host: host, team: c.TeamID, keyID: c.KeyID, key: key, now: time.Now,
		client: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (s *albumAPNS) providerToken(now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.issuedAt) || now.Unix() <= 0 {
		return "", errAlbumAPNS
	}
	if s.token != "" && now.Sub(s.issuedAt) < 50*time.Minute {
		return s.token, nil
	}
	t := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.RegisteredClaims{Issuer: s.team, IssuedAt: jwt.NewNumericDate(now)})
	t.Header["kid"] = s.keyID
	signed, err := t.SignedString(s.key)
	if err != nil {
		return "", errAlbumAPNS
	}
	s.token, s.issuedAt = signed, now
	return signed, nil
}

// The caller must reverify subscription and device ownership before sending.
// A live database lease cannot retract a push already accepted by APNs.
func (s *albumAPNS) sendRetention(ctx context.Context, deviceToken string, c albumRetentionReminder, requestID string) (albumAPNSResult, error) {
	now := s.now().UTC()
	due, expires, err := reminderWindow(c.Deadline, c.Days)
	if err != nil || !validOwner(requestID) || !validOwner(c.ID) || !validOwner(c.Owner) || !validOwner(c.Token) ||
		(c.Environment != "Sandbox" && c.Environment != "Production") || !c.Due.Equal(due) || !c.Expires.Equal(expires) ||
		c.State != "pending" || c.Attempts < 1 || now.Before(due) || !now.Before(expires) || !now.Before(c.Until) || c.Until.After(expires) ||
		len(deviceToken) == 0 || len(deviceToken) > 1024 || deviceToken != strings.ToLower(deviceToken) {
		return albumAPNSResult{}, errAlbumAPNS
	}
	if _, err := hex.DecodeString(deviceToken); err != nil {
		return albumAPNSResult{}, errAlbumAPNS
	}
	ctx, cancel := context.WithTimeout(ctx, min(15*time.Second, c.Until.Sub(now)))
	defer cancel()
	token, err := s.providerToken(now)
	if err != nil {
		return albumAPNSResult{}, err
	}
	// No account, filenames, media, links or precise deadline in the lock-screen payload.
	payload := []byte(`{"aps":{"alert":{"title":"云端照片保留提醒","body":"请打开告你相册查看保留期限，及时续费或下载需要的原件。"},"sound":"default"},"screen":"cloud-storage"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+s.host+"/3/device/"+deviceToken, bytes.NewReader(payload))
	if err != nil {
		return albumAPNSResult{}, errAlbumAPNS
	}
	req.Header.Set("authorization", "bearer "+token)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("apns-topic", albumAppleAudience)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "5")
	req.Header.Set("apns-id", requestID)
	req.Header.Set("apns-collapse-id", "album-retention")
	// Expiration limits APNs storage; Apple documents that late delivery remains possible.
	req.Header.Set("apns-expiration", strconv.FormatInt(minTime(now.Add(24*time.Hour), expires).Unix(), 10))
	response, err := s.client.Do(req)
	if err != nil {
		// Do not leak the device token embedded in url.Error, or retry an unknown outcome here.
		return albumAPNSResult{}, errAlbumAPNS
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(body) > 4096 || response.ProtoMajor != 2 || response.Header.Get("apns-id") != requestID {
		return albumAPNSResult{}, errAlbumAPNS
	}
	if response.StatusCode == http.StatusOK && len(body) == 0 {
		return albumAPNSResult{Status: "accepted"}, nil
	}
	var failure struct {
		Reason    string `json:"reason"`
		Timestamp *int64 `json:"timestamp"`
	}
	if err := json.Unmarshal(body, &failure); err != nil || failure.Reason == "" {
		return albumAPNSResult{}, errAlbumAPNS
	}
	if response.StatusCode == http.StatusGone && (failure.Reason == "Unregistered" || failure.Reason == "ExpiredToken") {
		if failure.Timestamp == nil || *failure.Timestamp <= 0 || *failure.Timestamp > now.Add(time.Minute).UnixMilli() {
			return albumAPNSResult{}, errAlbumAPNS
		}
		// Registration generation/time must still match before the caller disables this endpoint.
		return albumAPNSResult{Status: "unregistered", InvalidatedAt: time.UnixMilli(*failure.Timestamp).UTC()}, nil
	}
	if response.StatusCode >= 500 && response.StatusCode <= 599 || response.StatusCode == 429 && failure.Reason == "TooManyRequests" {
		retry := now.Add(15 * time.Minute)
		if delay, err := strconv.ParseInt(response.Header.Get("retry-after"), 10, 32); err == nil && delay > 0 {
			if candidate := now.Add(time.Duration(delay) * time.Second); candidate.After(retry) {
				retry = candidate
			}
		} else if date, err := http.ParseTime(response.Header.Get("retry-after")); err == nil {
			if date.After(retry) {
				retry = date
			}
		}
		return albumAPNSResult{Status: "retry", RetryAt: retry}, nil
	}
	if response.StatusCode >= 400 && response.StatusCode < 500 {
		return albumAPNSResult{Status: "rejected"}, nil
	}
	return albumAPNSResult{}, errAlbumAPNS
}
