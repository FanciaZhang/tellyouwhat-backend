package albums

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"
)

var (
	ErrShareUnavailable  = errors.New("album sharing unavailable")
	ErrShareNotAvailable = errors.New("album share not available")
	ErrSharePassword     = errors.New("album share password rejected")
	ErrShareLimit        = errors.New("album share claim limit reached")
	ErrShareLinksLimit   = errors.New("album active share link limit reached")
	ErrShareClaimExpired = errors.New("album share claim expired")
	ErrShareConflict     = errors.New("album share request conflict")
	ErrShareInput        = errors.New("invalid album share request")
)

const shareClaimLifetime = time.Hour
const shareMaxLifetime = 30 * 24 * time.Hour
const shareMaxActiveLinks = 32

type ShareCreateInput struct {
	RequestID       string `json:"requestID"`
	DurationSeconds int64  `json:"durationSeconds"`
	MaxClaims       int    `json:"maxClaims"`
	Password        string `json:"password,omitempty"`
}

func (ShareCreateInput) String() string   { return "[redacted album share request]" }
func (ShareCreateInput) GoString() string { return "[redacted album share request]" }

type ShareLink struct {
	ID               string     `json:"id"`
	VersionID        string     `json:"versionID"`
	URL              string     `json:"url,omitempty"`
	CreatedAt        time.Time  `json:"createdAt"`
	ExpiresAt        time.Time  `json:"expiresAt"`
	MaxClaims        int        `json:"maxClaims"`
	ClaimedCount     int        `json:"claimedCount"`
	RequiresPassword bool       `json:"requiresPassword"`
	RevokedAt        *time.Time `json:"revokedAt,omitempty"`
}

func (ShareLink) String() string   { return "[redacted album share link]" }
func (ShareLink) GoString() string { return "[redacted album share link]" }

type SharedResource struct {
	ResourceID string `json:"resourceID"`
	Role       string `json:"role"`
	Filename   string `json:"filename"`
	SizeBytes  int64  `json:"sizeBytes"`
}
type ShareLookup struct {
	Kind             string           `json:"kind"`
	Resources        []SharedResource `json:"resources"`
	ExpiresAt        time.Time        `json:"expiresAt"`
	RequiresPassword bool             `json:"requiresPassword"`
	RemainingClaims  int              `json:"remainingClaims"`
}
type ShareClaim struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (ShareClaim) String() string   { return "[redacted album share claim]" }
func (ShareClaim) GoString() string { return "[redacted album share claim]" }

type SharedReadGrant struct {
	SharedResource
	SHA256          string    `json:"sha256"`
	ObjectVersionID string    `json:"objectVersionID"`
	URL             string    `json:"url"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

func (SharedReadGrant) String() string   { return "[redacted album shared read grant]" }
func (SharedReadGrant) GoString() string { return "[redacted album shared read grant]" }

type sharedOriginalAuthorizer interface {
	AuthorizeSharedOriginalRead(context.Context, SealedObject, string, time.Time) (string, error)
}
type ShareService struct {
	repository  *MySQLUploads
	uploads     *UploadService
	siteOrigin  string
	keys        map[string][]byte
	activeKeyID string
	passwords   chan struct{}
	now         func() time.Time
}

func (ShareService) String() string   { return "[redacted album sharing service]" }
func (ShareService) GoString() string { return "[redacted album sharing service]" }
func shareKeyID(key []byte) string    { sum := sha256.Sum256(key); return hex.EncodeToString(sum[:8]) }
func validShareOrigin(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Port() == "" && u.RawQuery == "" && u.Fragment == "" && u.RawPath == "" && u.Path == "" && u.Host == u.Hostname() && albumAPIHostPattern.MatchString(u.Host)
}
func NewShareService(repository *MySQLUploads, uploads *UploadService, siteOrigin string, keys [][]byte, now func() time.Time) (*ShareService, error) {
	if repository == nil || repository.db == nil || uploads == nil || !validShareOrigin(siteOrigin) || len(keys) < 1 || len(keys) > 2 || (repository.billingEnvironment != "Sandbox" && repository.billingEnvironment != "Production") {
		return nil, ErrShareUnavailable
	}
	if _, ok := uploads.objects.(sharedOriginalAuthorizer); !ok {
		return nil, ErrShareUnavailable
	}
	s := &ShareService{repository: repository, uploads: uploads, siteOrigin: siteOrigin, keys: map[string][]byte{}, passwords: make(chan struct{}, 2), now: now}
	if now == nil {
		s.now = time.Now
	}
	for _, key := range keys {
		if len(key) != 32 {
			return nil, ErrShareUnavailable
		}
		id := shareKeyID(key)
		if s.keys[id] != nil {
			return nil, ErrShareUnavailable
		}
		s.keys[id] = append([]byte(nil), key...)
		if s.activeKeyID == "" {
			s.activeKeyID = id
		}
	}
	return s, nil
}
func shareMAC(key []byte, purpose string, values ...string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("albums:share:v1:" + purpose))
	for _, value := range values {
		mac.Write([]byte{0})
		mac.Write([]byte(value))
	}
	return mac.Sum(nil)
}
func sharedToken(id string, key []byte, purpose string, values ...string) string {
	return id + "." + base64.RawURLEncoding.EncodeToString(shareMAC(key, purpose, append([]string{id}, values...)...))
}
func parseSharedToken(token string) (string, [32]byte, error) {
	if len(token) != 80 || token[36] != '.' || !validOwner(token[:36]) {
		return "", [32]byte{}, ErrShareNotAvailable
	}
	secret, err := base64.RawURLEncoding.DecodeString(token[37:])
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != token[37:] {
		return "", [32]byte{}, ErrShareNotAvailable
	}
	return token[:36], sha256.Sum256([]byte(token)), nil
}
func validSharePassword(password string) bool {
	if password == "" {
		return true
	}
	if !utf8.ValidString(password) || len(password) > 256 || utf8.RuneCountInString(password) < 4 || utf8.RuneCountInString(password) > 64 || strings.TrimSpace(password) != password {
		return false
	}
	for _, r := range password {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func (s *ShareService) withPasswordBudget(ctx context.Context, work func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.passwords <- struct{}{}:
		defer func() { <-s.passwords }()
	default:
		return ErrShareUnavailable
	}
	err := work()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
func (s *ShareService) passwordProof(ctx context.Context, password string) ([]byte, error) {
	if password == "" {
		return nil, nil
	}
	var proof []byte
	err := s.withPasswordBudget(ctx, func() error {
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return ErrShareUnavailable
		}
		// Fixed RFC 9106 second-profile costs. Untrusted records cannot select costs.
		proof = append([]byte{1}, salt...)
		proof = append(proof, argon2.IDKey([]byte(password), salt, 3, 64*1024, 4, 32)...)
		return nil
	})
	return proof, err
}
func (s *ShareService) verifyPassword(ctx context.Context, proof []byte, password string) error {
	if len(proof) == 0 {
		if password != "" {
			return ErrSharePassword
		}
		return nil
	}
	if len(proof) != 49 || proof[0] != 1 {
		return ErrShareUnavailable
	}
	if !validSharePassword(password) || password == "" {
		return ErrSharePassword
	}
	return s.withPasswordBudget(ctx, func() error {
		actual := argon2.IDKey([]byte(password), proof[1:17], 3, 64*1024, 4, 32)
		if subtle.ConstantTimeCompare(actual, proof[17:]) != 1 {
			return ErrSharePassword
		}
		return nil
	})
}
func (s *ShareService) createDigest(key []byte, owner, version string, input ShareCreateInput) [32]byte {
	data, _ := json.Marshal(input)
	var result [32]byte
	copy(result[:], shareMAC(key, "create", owner, version, string(data)))
	return result
}
func (s *ShareService) link(row shareRow) ShareLink {
	result := ShareLink{ID: row.ID, VersionID: row.Version, CreatedAt: row.Created, ExpiresAt: row.Expiry, MaxClaims: row.MaxClaims, ClaimedCount: row.Claims, RequiresPassword: len(row.Password) > 0, RevokedAt: row.Revoked}
	if key := s.keys[row.KeyID]; key != nil {
		token := sharedToken(row.ID, key, "link", row.Owner)
		sum := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(sum[:], row.TokenDigest[:]) == 1 {
			result.URL = s.siteOrigin + "/share/#" + token
		}
	}
	return result
}
func (s *ShareService) confirmCreate(row shareRow, owner, version string, input ShareCreateInput) (ShareLink, error) {
	key := s.keys[row.KeyID]
	if key == nil {
		return ShareLink{}, ErrShareUnavailable
	}
	digest := s.createDigest(key, owner, version, input)
	if row.Owner != owner || row.Version != version || subtle.ConstantTimeCompare(row.RequestDigest[:], digest[:]) != 1 {
		return ShareLink{}, ErrShareConflict
	}
	link := s.link(row)
	if link.URL == "" {
		return ShareLink{}, ErrShareUnavailable
	}
	return link, nil
}
func (s *ShareService) Create(ctx context.Context, owner, version string, input ShareCreateInput) (ShareLink, error) {
	if !validOwner(owner) || !validOwner(version) || !validOwner(input.RequestID) || input.DurationSeconds < 3600 || input.DurationSeconds > int64(shareMaxLifetime/time.Second) || input.MaxClaims < 1 || input.MaxClaims > 100 || !validSharePassword(input.Password) {
		return ShareLink{}, ErrShareInput
	}
	old, err := s.repository.shareByRequest(ctx, owner, input.RequestID)
	if err == nil {
		return s.confirmCreate(old, owner, version, input)
	}
	if !errors.Is(err, ErrNotFound) {
		return ShareLink{}, err
	}
	if _, err := s.uploads.verifiedAssetVersion(ctx, owner, version); err != nil {
		return ShareLink{}, err
	}
	proof, err := s.passwordProof(ctx, input.Password)
	if err != nil {
		return ShareLink{}, err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	row := shareRow{ID: uuid.NewString(), Owner: owner, Version: version, Request: input.RequestID, KeyID: s.activeKeyID, Password: proof, MaxClaims: input.MaxClaims, Created: now, Expiry: now.Add(time.Duration(input.DurationSeconds) * time.Second)}
	row.RequestDigest = s.createDigest(s.keys[row.KeyID], owner, version, input)
	row.TokenDigest = sha256.Sum256([]byte(sharedToken(row.ID, s.keys[row.KeyID], "link", owner)))
	row, err = s.repository.createShare(ctx, row, now)
	if err != nil {
		return ShareLink{}, err
	}
	return s.confirmCreate(row, owner, version, input)
}
func (s *ShareService) List(ctx context.Context, owner, version string) ([]ShareLink, error) {
	if !validOwner(owner) || !validOwner(version) {
		return nil, ErrShareInput
	}
	if _, err := s.uploads.verifiedAssetVersion(ctx, owner, version); err != nil {
		return nil, err
	}
	rows, err := s.repository.ownerShares(ctx, owner, version, s.now().UTC())
	if err != nil {
		return nil, err
	}
	result := make([]ShareLink, 0, len(rows))
	for _, row := range rows {
		result = append(result, s.link(row))
	}
	return result, nil
}
func (s *ShareService) Revoke(ctx context.Context, owner, id string) error {
	if !validOwner(owner) || !validOwner(id) {
		return ErrShareInput
	}
	return s.repository.revokeShare(ctx, owner, id, s.now().UTC())
}
func sharedFilename(resource Resource) string {
	name := ""
	if resource.Descriptor != nil {
		name = path.Base(strings.ReplaceAll(resource.Descriptor.Filename, "\\", "/"))
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		name = resource.ID + ".bin"
	}
	runes := []rune(name)
	if len(runes) > 160 {
		ext := path.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = string(runes[:140]) + ext
	}
	return name
}
func sharedResources(version AssetVersion) []SharedResource {
	result := make([]SharedResource, 0, len(version.Manifest.Resources))
	for _, resource := range version.Manifest.Resources {
		result = append(result, SharedResource{resource.ID, string(resource.Role), sharedFilename(resource), resource.SizeBytes})
	}
	return result
}
func (s *ShareService) Lookup(ctx context.Context, token string) (ShareLookup, error) {
	id, digest, err := parseSharedToken(token)
	if err != nil {
		return ShareLookup{}, err
	}
	row, err := s.repository.activeShare(ctx, id, digest, s.now().UTC())
	if err != nil {
		return ShareLookup{}, err
	}
	version, err := s.uploads.verifiedAssetVersion(ctx, row.Owner, row.Version)
	if err != nil {
		return ShareLookup{}, ErrShareNotAvailable
	}
	return ShareLookup{Kind: string(version.Manifest.Kind), Resources: sharedResources(version), ExpiresAt: row.Expiry, RequiresPassword: len(row.Password) > 0, RemainingClaims: row.MaxClaims - row.Claims}, nil
}
func (s *ShareService) Claim(ctx context.Context, token, password, requestID string) (ShareClaim, error) {
	if !validOwner(requestID) {
		return ShareClaim{}, ErrShareInput
	}
	id, digest, err := parseSharedToken(token)
	if err != nil {
		return ShareClaim{}, err
	}
	row, err := s.repository.activeShare(ctx, id, digest, s.now().UTC())
	if err != nil {
		return ShareClaim{}, err
	}
	if err := s.verifyPassword(ctx, row.Password, password); err != nil {
		return ShareClaim{}, err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	candidate := shareClaimRow{ID: uuid.NewString(), Share: id, Request: requestID, KeyID: s.activeKeyID, Created: now, Expiry: now.Add(shareClaimLifetime)}
	candidate.TokenDigest = sha256.Sum256([]byte(sharedToken(candidate.ID, s.keys[candidate.KeyID], "claim", id, requestID)))
	result, err := s.repository.claimShare(ctx, row, digest, candidate, now)
	if err != nil {
		return ShareClaim{}, err
	}
	key := s.keys[result.KeyID]
	if key == nil {
		return ShareClaim{}, ErrShareUnavailable
	}
	claimToken := sharedToken(result.ID, key, "claim", result.Share, result.Request)
	sum := sha256.Sum256([]byte(claimToken))
	if subtle.ConstantTimeCompare(sum[:], result.TokenDigest[:]) != 1 {
		return ShareClaim{}, ErrShareUnavailable
	}
	return ShareClaim{Token: claimToken, ExpiresAt: result.Expiry}, nil
}
func (s *ShareService) ReadGrants(ctx context.Context, token string) ([]SharedReadGrant, error) {
	id, digest, err := parseSharedToken(token)
	if err != nil {
		return nil, err
	}
	var grants []SharedReadGrant
	err = s.repository.withShareClaim(ctx, id, digest, s.now().UTC(), func(row shareRow, version AssetVersion, deadline time.Time) error {
		expiry := minTime(s.now().UTC().Add(5*time.Minute), deadline)
		if !s.now().UTC().Before(expiry) {
			return ErrShareNotAvailable
		}
		signer := s.uploads.objects.(sharedOriginalAuthorizer)
		objects := map[string]SealedObject{}
		for _, object := range version.Objects {
			objects[object.ResourceID] = object
		}
		grants = make([]SharedReadGrant, 0, len(version.Manifest.Resources))
		for _, resource := range version.Manifest.Resources {
			name := sharedFilename(resource)
			signed, err := signer.AuthorizeSharedOriginalRead(ctx, objects[resource.ID], name, expiry)
			if err != nil {
				return ErrShareUnavailable
			}
			grants = append(grants, SharedReadGrant{SharedResource: SharedResource{resource.ID, string(resource.Role), name, resource.SizeBytes}, SHA256: strings.ToLower(resource.SHA256), ObjectVersionID: objects[resource.ID].VersionID, URL: signed, ExpiresAt: expiry})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return grants, nil
}
