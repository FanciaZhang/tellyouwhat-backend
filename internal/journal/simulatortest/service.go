// Package simulatortest authenticates explicitly provisioned, short-lived Journal
// integration clients. It never registers an Apple attestation key.
package simulatortest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/contracts"
	"github.com/tellyouwhat/backend/internal/entitlement"
	"github.com/tellyouwhat/backend/internal/journal/readaloud"
	"github.com/tellyouwhat/backend/internal/platform/appregistry"
	"github.com/tellyouwhat/backend/internal/privacy"
)

const (
	Scheme    = "journal-simulator-v1"
	Host      = "api-test.journal.tellyouwhat.cn"
	BundleID  = "cn.tellyouwhat.journalapp.dev"
	keyPrefix = "simulator_"
)

type Credential struct {
	KeyID                 string     `json:"keyID"`
	PublicKey             string     `json:"publicKey"`
	SubjectID             string     `json:"subjectID"`
	IssuedAt              time.Time  `json:"issuedAt"`
	ExpiresAt             time.Time  `json:"expiresAt"`
	Revoked               bool       `json:"revoked"`
	SubscriptionStartedAt *time.Time `json:"subscriptionStartedAt"`
	SubscriptionExpiresAt *time.Time `json:"subscriptionExpiresAt"`
	JournalCharacters     int        `json:"journalCharacters"`
	PreviewCharacters     int        `json:"previewCharacters"`
}

type Registry struct {
	Version     int          `json:"version"`
	Credentials []Credential `json:"credentials"`
}

type Service struct {
	consentMu        sync.Mutex
	voiceConsents    *privacy.MemoryRepository
	voiceConsentKeys map[string]bool
	path             string
	nonces           attestation.NonceStore
	now              func() time.Time
}

func New(path, environment string, app appregistry.App, nonces attestation.NonceStore, now func() time.Time) (*Service, error) {
	if environment != "development" || app.ID != appregistry.Journal || app.BundleID != BundleID ||
		len(app.Hosts) != 1 || app.Hosts[0] != Host || !filepath.IsAbs(path) || nonces == nil {
		return nil, errors.New("simulator read-aloud authentication requires the isolated Journal development gateway")
	}
	if now == nil {
		now = time.Now
	}
	s := &Service{path: path, nonces: nonces, now: now, voiceConsents: privacy.NewMemoryRepository(), voiceConsentKeys: map[string]bool{}}
	if _, err := s.load(); err != nil {
		return nil, err
	}
	if _, err := s.DevelopmentGrants(); err != nil {
		return nil, err
	}
	return s, nil
}

func IsKey(key string) bool {
	id, err := uuid.Parse(strings.TrimPrefix(key, keyPrefix))
	return err == nil && key == keyPrefix+id.String()
}

func (s *Service) load() (Registry, error) {
	invalid := func() (Registry, error) { return Registry{}, attestation.ErrUnavailable }
	parent, err := os.Lstat(filepath.Dir(s.path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return invalid()
	}
	info, err := os.Lstat(s.path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 32768 {
		return invalid()
	}
	f, err := os.Open(s.path)
	if err != nil {
		return invalid()
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return invalid()
	}
	body, err := io.ReadAll(io.LimitReader(f, 32769))
	if err != nil || len(body) > 32768 {
		return invalid()
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var registry Registry
	if decoder.Decode(&registry) != nil || decoder.Decode(&struct{}{}) != io.EOF || registry.Version != 1 ||
		registry.Credentials == nil || len(registry.Credentials) > 16 {
		return invalid()
	}
	keys, subjects := map[string]bool{}, map[string]Credential{}
	for _, c := range registry.Credentials {
		pk, err := base64.StdEncoding.DecodeString(c.PublicKey)
		id, idErr := uuid.Parse(c.SubjectID)
		if !IsKey(c.KeyID) || keys[c.KeyID] || err != nil || len(pk) != ed25519.PublicKeySize || idErr != nil || id.String() != c.SubjectID ||
			c.IssuedAt.IsZero() || !c.IssuedAt.Before(c.ExpiresAt) || c.ExpiresAt.Sub(c.IssuedAt) > 24*time.Hour ||
			c.JournalCharacters < 1 || c.JournalCharacters > 100000 || c.PreviewCharacters < 1 || c.PreviewCharacters > 10000 ||
			(c.SubscriptionStartedAt == nil) != (c.SubscriptionExpiresAt == nil) {
			return invalid()
		}
		if c.SubscriptionStartedAt != nil && (c.SubscriptionStartedAt.IsZero() || !c.SubscriptionStartedAt.Before(*c.SubscriptionExpiresAt)) {
			return invalid()
		}
		if old, ok := subjects[c.SubjectID]; ok && (old.JournalCharacters != c.JournalCharacters || old.PreviewCharacters != c.PreviewCharacters ||
			!sameDate(old.SubscriptionStartedAt, c.SubscriptionStartedAt) || !sameDate(old.SubscriptionExpiresAt, c.SubscriptionExpiresAt)) {
			return invalid()
		}
		keys[c.KeyID] = true
		subjects[c.SubjectID] = c
	}
	return registry, nil
}

func sameDate(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

// Reloading is intentional: revocation must also affect existing audio streams.
func (s *Service) Lookup(key string) (Credential, error) {
	if s == nil || !IsKey(key) {
		return Credential{}, attestation.ErrAuthentication
	}
	r, err := s.load()
	if err != nil {
		return Credential{}, err
	}
	now := s.now()
	for _, c := range r.Credentials {
		if c.KeyID == key && !c.Revoked && !now.Before(c.IssuedAt) && now.Before(c.ExpiresAt) {
			return c, nil
		}
	}
	return Credential{}, attestation.ErrAuthentication
}

func Allowed(method, uri string) bool {
	u, err := url.ParseRequestURI(uri)
	if err != nil || u.Host != "" || u.Scheme != "" || u.Fragment != "" || u.ForceQuery || u.EscapedPath() != u.Path || strings.Contains(u.Path, "//") {
		return false
	}
	switch u.Path {
	case "/v1/ai/quota":
		return method == "GET" && u.RawQuery == ""
	case "/v1/dev/entitlements/activate", "/v1/journal/voice/sessions", "/v1/journal/organize":
		return method == "POST" && u.RawQuery == ""
	case "/v1/privacy/consents":
		return method == "POST" && u.RawQuery == ""
	case "/v1/journal/read-aloud/syntheses":
		return method == "POST" && u.RawQuery == ""
	case "/v1/journal/read-aloud/allowance":
		return method == "GET" && (u.RawQuery == "" || u.RawQuery == "purpose=journal" || u.RawQuery == "purpose=preview")
	}
	const prefix = "/v1/journal/read-aloud/syntheses/"
	if !strings.HasPrefix(u.Path, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, prefix), "/")
	id, err := uuid.Parse(parts[0])
	if err != nil || id.String() != parts[0] {
		return false
	}
	if len(parts) == 1 {
		return (method == "GET" || method == "DELETE") && u.RawQuery == ""
	}
	if len(parts) != 2 || parts[1] != "events" || method != "GET" {
		return false
	}
	if u.RawQuery == "" {
		return true
	}
	value := strings.TrimPrefix(u.RawQuery, "after=")
	n, err := strconv.ParseInt(value, 10, 64)
	return err == nil && n >= 0 && n <= readaloud.MaximumAudioBytes && n%2 == 0 && u.RawQuery == "after="+strconv.FormatInt(n, 10)
}

func Message(proof attestation.RequestProof) []byte {
	digest := contracts.RequestBindingDigest(contracts.RequestBinding{Method: proof.Method, Path: proof.Path,
		RequestID: proof.RequestID, Nonce: proof.Nonce, Timestamp: proof.Timestamp, BodySHA256: proof.BodySHA256})
	return []byte(strings.Join([]string{Scheme, BundleID, Host, proof.KeyID, digest}, "\n"))
}

func (s *Service) Authenticate(ctx context.Context, proof attestation.RequestProof) (attestation.Principal, error) {
	if s == nil || s.nonces == nil {
		return attestation.Principal{}, attestation.ErrUnavailable
	}
	id, err := uuid.Parse(proof.RequestID)
	timestamp, timeErr := time.Parse(time.RFC3339, proof.Timestamp)
	nonce, nonceErr := base64.RawURLEncoding.DecodeString(proof.Nonce)
	body, bodyErr := hex.DecodeString(proof.BodySHA256)
	if !Allowed(proof.Method, proof.Path) || err != nil || id.String() != proof.RequestID || timeErr != nil ||
		math.Abs(s.now().Sub(timestamp).Seconds()) > 300 || nonceErr != nil || len(nonce) != 32 || bodyErr != nil || len(body) != 32 {
		return attestation.Principal{}, attestation.ErrAuthentication
	}
	c, err := s.Lookup(proof.KeyID)
	if err != nil {
		return attestation.Principal{}, err
	}
	if !s.AllowsVoice(c.KeyID) && (proof.Path == "/v1/ai/quota" || proof.Path == "/v1/dev/entitlements/activate" || proof.Path == "/v1/journal/voice/sessions" || proof.Path == "/v1/journal/organize") {
		return attestation.Principal{}, attestation.ErrAuthentication
	}
	pk, _ := base64.StdEncoding.DecodeString(c.PublicKey)
	signature, err := base64.StdEncoding.DecodeString(proof.Assertion)
	if err != nil || !ed25519.Verify(pk, Message(proof), signature) {
		return attestation.Principal{}, attestation.ErrAuthentication
	}
	if err := s.nonces.Consume(ctx, proof.Nonce, proof.KeyID, s.now()); err != nil {
		if errors.Is(err, attestation.ErrAuthentication) || errors.Is(err, attestation.ErrReplay) {
			return attestation.Principal{}, err
		}
		return attestation.Principal{}, attestation.ErrUnavailable
	}
	return attestation.Principal{AppID: "journal", KeyID: c.KeyID, DeviceID: c.KeyID}, nil
}

func (s *Service) DevelopmentGrants() ([]entitlement.DevelopmentGrant, error) {
	parent, err := os.Lstat(filepath.Dir(s.path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return nil, attestation.ErrUnavailable
	}
	path := filepath.Join(filepath.Dir(s.path), "development-grants.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 8192 {
		return nil, attestation.ErrUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, attestation.ErrUnavailable
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, attestation.ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil {
		return nil, attestation.ErrUnavailable
	}
	return entitlement.ParseDevelopmentGrants(string(raw))
}

type GrantKeys struct {
	attestation.KeyStore
	Simulator *Service
}

func (keys GrantKeys) Get(ctx context.Context, key string) (attestation.RegisteredKey, error) {
	if !IsKey(key) {
		return keys.KeyStore.Get(ctx, key)
	}
	c, err := keys.Simulator.Lookup(key)
	if err != nil || !keys.Simulator.AllowsVoice(c.KeyID) {
		return attestation.RegisteredKey{}, attestation.ErrAuthentication
	}
	return attestation.RegisteredKey{AppID: "journal", KeyID: key, DeviceID: key}, nil
}

func (s *Service) AllowsVoice(key string) bool {
	grants, err := s.DevelopmentGrants()
	if err != nil {
		return false
	}
	for _, grant := range grants {
		if grant.KeyID == key && grant.DeviceID == key && !s.now().Before(grant.StartedAt) && s.now().Before(grant.ExpiresAt) {
			return true
		}
	}
	return false
}
