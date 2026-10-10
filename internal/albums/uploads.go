package albums

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"time"

	"github.com/google/uuid"
)

var (
	ErrOwner    = errors.New("stable album account required")
	ErrQuota    = errors.New("album storage quota exceeded")
	ErrNotFound = errors.New("album upload not found")
	ErrConflict = errors.New("album upload conflicts with existing state")
	ErrExpired  = errors.New("album upload expired")
	ErrLease    = errors.New("album verification lease lost")
)

type UploadState string

const (
	Uploading         UploadState = "uploading"
	Queued            UploadState = "queued"
	Verifying         UploadState = "verifying"
	OriginalsVerified UploadState = "originals_verified"
)

// OwnerID is a server-issued stable account UUID, never an App Attest key or device ID.
type Upload struct {
	ID            string         `json:"id"`
	OwnerID       string         `json:"-"`
	RequestID     string         `json:"requestID"`
	Manifest      Manifest       `json:"manifest"`
	Digest        string         `json:"manifestDigest"`
	State         UploadState    `json:"state"`
	OriginalBytes int64          `json:"originalBytes"`
	DerivedBudget int64          `json:"derivedBudget"`
	ExpiresAt     time.Time      `json:"expiresAt"`
	LeaseToken    string         `json:"-"`
	LeaseUntil    time.Time      `json:"-"`
	Objects       []SealedObject `json:"-"`
}

type SealedObject struct {
	ResourceID string `json:"resourceID"`
	Key        string `json:"key"`
	VersionID  string `json:"versionID"`
	SizeBytes  int64  `json:"sizeBytes"`
	SHA256     string `json:"sha256"`
}

type UploadRepository interface {
	Reserve(context.Context, Upload) (Upload, error)
	Get(context.Context, string, string) (Upload, error)
	Submit(context.Context, string, string, time.Time) (Upload, error)
	Claim(context.Context, string, string, string, time.Time, time.Time) (Upload, error)
	Complete(context.Context, string, string, string, []SealedObject, time.Time) (Upload, error)
}

type PutGrant struct {
	ResourceID string            `json:"resourceID"`
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers"`
	ExpiresAt  time.Time         `json:"expiresAt"`
}

// Client writes are limited to staging. Seal must create an immutable server-only
// destination/version, and OpenVersion must read precisely that version.
// A production provider must enforce these guarantees, not just attach metadata.
type AlbumObjects interface {
	AuthorizePut(context.Context, string, Resource, time.Time) (PutGrant, error)
	Seal(context.Context, string, string) (versionID string, err error)
	OpenVersion(context.Context, string, string) (io.ReadCloser, error)
}

type UploadPolicy struct {
	MaxOriginalBytes  int64
	MaxDerivedBytes   int64
	UploadTTL         time.Duration
	VerificationLease time.Duration
}

type UploadService struct {
	repository UploadRepository
	objects    AlbumObjects
	policy     UploadPolicy
	now        func() time.Time
}

func NewUploadService(repository UploadRepository, objects AlbumObjects, policy UploadPolicy, now func() time.Time) (*UploadService, error) {
	if repository == nil || objects == nil || policy.MaxOriginalBytes <= 0 || policy.MaxDerivedBytes <= 0 ||
		policy.MaxOriginalBytes > math.MaxInt64-policy.MaxDerivedBytes || policy.UploadTTL <= 0 ||
		policy.UploadTTL > 24*time.Hour || policy.VerificationLease <= 0 || policy.VerificationLease > time.Hour {
		return nil, errors.New("invalid album upload configuration")
	}
	if now == nil {
		now = time.Now
	}
	return &UploadService{repository, objects, policy, now}, nil
}

func validOwner(owner string) bool {
	id, err := uuid.Parse(owner)
	return err == nil && id != uuid.Nil && id.String() == owner
}

func (s *UploadService) Create(ctx context.Context, owner, requestID string, manifest Manifest) (Upload, error) {
	if !validOwner(owner) {
		return Upload{}, ErrOwner
	}
	if !validOwner(requestID) {
		return Upload{}, ErrInvalidManifest
	}
	digest, err := manifest.Digest()
	if err != nil {
		return Upload{}, err
	}
	var total int64
	for _, r := range manifest.Resources {
		total += r.SizeBytes
	}
	if total > s.policy.MaxOriginalBytes {
		return Upload{}, ErrQuota
	}
	budget := min(total, s.policy.MaxDerivedBytes)
	return s.repository.Reserve(ctx, Upload{
		ID: uuid.NewString(), OwnerID: owner, RequestID: requestID, Manifest: manifest, Digest: digest,
		State: Uploading, OriginalBytes: total, DerivedBudget: budget,
		ExpiresAt: s.now().UTC().Add(s.policy.UploadTTL),
	})
}

func (s *UploadService) Get(ctx context.Context, owner, id string) (Upload, error) {
	if !validOwner(owner) {
		return Upload{}, ErrOwner
	}
	if !validOwner(id) {
		return Upload{}, ErrNotFound
	}
	return s.repository.Get(ctx, owner, id)
}

func (s *UploadService) Grants(ctx context.Context, owner, id string) ([]PutGrant, error) {
	upload, err := s.Get(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	if !s.now().Before(upload.ExpiresAt) {
		return nil, ErrExpired
	}
	if upload.State != Uploading {
		return nil, ErrConflict
	}
	expiry := minTime(upload.ExpiresAt, s.now().Add(10*time.Minute))
	grants := make([]PutGrant, 0, len(upload.Manifest.Resources))
	for _, resource := range upload.Manifest.Resources {
		grant, err := s.objects.AuthorizePut(ctx, stagingKey(upload, resource.ID), resource, expiry)
		if err != nil {
			return nil, err
		}
		grant.ResourceID = resource.ID
		grant.ExpiresAt = expiry
		if grant.Headers == nil {
			grant.Headers = map[string]string{}
		}
		grants = append(grants, grant)
	}
	return grants, nil
}

// Submit records a durable worker job. It does not trust client upload receipts.
func (s *UploadService) Submit(ctx context.Context, owner, id string) (Upload, error) {
	if !validOwner(owner) {
		return Upload{}, ErrOwner
	}
	if !validOwner(id) {
		return Upload{}, ErrNotFound
	}
	return s.repository.Submit(ctx, owner, id, s.now().UTC())
}

// Verify is a trusted worker operation and is never exposed as a client proof endpoint.
func (s *UploadService) Verify(ctx context.Context, owner, id string) (Upload, error) {
	if !validOwner(owner) {
		return Upload{}, ErrOwner
	}
	if !validOwner(id) {
		return Upload{}, ErrNotFound
	}
	lease := uuid.NewString()
	now := s.now().UTC()
	upload, err := s.repository.Claim(ctx, owner, id, lease, now, now.Add(s.policy.VerificationLease))
	if err != nil {
		return Upload{}, err
	}
	if upload.State == OriginalsVerified {
		return upload, nil
	}
	workerContext, cancel := context.WithTimeout(ctx, s.policy.VerificationLease)
	defer cancel()
	observations := make([]Observation, 0, len(upload.Manifest.Resources))
	objects := make([]SealedObject, 0, len(upload.Manifest.Resources))
	for _, resource := range upload.Manifest.Resources {
		if err := workerContext.Err(); err != nil {
			return Upload{}, err
		}
		key := sealedKey(upload, resource.ID)
		version, err := s.objects.Seal(workerContext, stagingKey(upload, resource.ID), key)
		if err != nil {
			return Upload{}, err
		}
		if version == "" {
			return Upload{}, ErrIncompleteBackup
		}
		body, err := s.objects.OpenVersion(workerContext, key, version)
		if err != nil {
			return Upload{}, err
		}
		hash := sha256.New()
		read, readErr := io.Copy(hash, io.LimitReader(body, resource.SizeBytes+1))
		closeErr := body.Close()
		if readErr != nil {
			return Upload{}, readErr
		}
		if closeErr != nil {
			return Upload{}, closeErr
		}
		if read != resource.SizeBytes {
			return Upload{}, ErrIncompleteBackup
		}
		computed := hex.EncodeToString(hash.Sum(nil))
		observations = append(observations, Observation{resource.ID, read, computed})
		objects = append(objects, SealedObject{resource.ID, key, version, read, computed})
	}
	if _, err := VerifyBackup(upload.Manifest, observations); err != nil {
		return Upload{}, err
	}
	return s.repository.Complete(ctx, owner, id, lease, objects, s.now().UTC())
}

func stagingKey(u Upload, resource string) string {
	return "albums/staging/" + u.OwnerID + "/" + u.ID + "/" + resource
}
func sealedKey(u Upload, resource string) string {
	return "albums/originals/" + u.OwnerID + "/" + u.ID + "/" + resource
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
