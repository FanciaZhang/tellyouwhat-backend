package albums

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

const DailyLivePair = "daily_live_pair_v1"

var ErrLivePairNotReady = errors.New("album finished live pair is not ready")

// These are declarations bound to immutable component receipts. They are not
// proof of native Apple pairing. Clients must still parse both files and perform
// native playback/system-library readback; no original deletion is authorized.
type LivePairResource struct {
	SizeBytes int64  `json:"sizeBytes"`
	SHA256    string `json:"sha256"`
}
type LivePairPhotoSource struct {
	Preset      string `json:"preset"`
	SizeBytes   int64  `json:"sizeBytes"`
	SHA256      string `json:"sha256"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	ContentType string `json:"contentType"`
}
type LivePairVideoSource struct {
	Preset string `json:"preset"`
	LivePairResource
	VideoProfile
}
type LivePairInfo struct {
	Identifier  string  `json:"identifier"`
	PhotoTime   float64 `json:"photoTime"`
	Duration    float64 `json:"duration"`
	PhotoWidth  int     `json:"photoWidth"`
	PhotoHeight int     `json:"photoHeight"`
	HasAudio    bool    `json:"hasAudio"`
	HasHDRVideo bool    `json:"hasHDRVideo"`
}
type LivePairDescriptor struct {
	SourcePhoto LivePairPhotoSource `json:"sourcePhoto"`
	SourceVideo LivePairVideoSource `json:"sourceVideo"`
	Timing      LivePhotoTiming     `json:"timing"`
	Photo       LivePairResource    `json:"photo"`
	Movie       LivePairResource    `json:"movie"`
	Pair        LivePairInfo        `json:"pair"`
}

func (d LivePairDescriptor) validate() error {
	hash := func(r LivePairResource, limit int64) bool {
		return r.SizeBytes > 0 && r.SizeBytes <= limit && validSHA256(r.SHA256) && strings.ToLower(r.SHA256) == r.SHA256
	}
	if !hash(d.Photo, 16<<20) || !hash(d.Movie, 4<<30) || !hash(LivePairResource{d.SourcePhoto.SizeBytes, d.SourcePhoto.SHA256}, 16<<20) ||
		!hash(d.SourceVideo.LivePairResource, 4<<30) || d.SourcePhoto.Preset != string(PhotoDailyJPEG) || d.SourcePhoto.ContentType != "image/jpeg" || d.SourceVideo.Preset != string(VideoDaily1080) ||
		d.SourcePhoto.Width < 1 || d.SourcePhoto.Height < 1 || d.SourcePhoto.Width > 2560 || d.SourcePhoto.Height > 2560 ||
		!d.SourceVideo.VideoProfile.valid() || max(d.SourceVideo.Width, d.SourceVideo.Height) > 1920 || min(d.SourceVideo.Width, d.SourceVideo.Height) > 1080 || !d.Timing.valid() || !validOwner(d.Pair.Identifier) || d.Pair.PhotoWidth != d.SourcePhoto.Width || d.Pair.PhotoHeight != d.SourcePhoto.Height ||
		d.Pair.HasHDRVideo || d.Pair.HasAudio != (d.SourceVideo.AudioChannels > 0) || math.IsNaN(d.Pair.PhotoTime) || math.IsInf(d.Pair.PhotoTime, 0) ||
		math.IsNaN(d.Pair.Duration) || math.IsInf(d.Pair.Duration, 0) || d.Pair.PhotoTime < 0 || d.Pair.Duration <= d.Pair.PhotoTime ||
		d.Timing.PhotoTimeMicroseconds < 0 || d.Timing.DurationMicroseconds <= d.Timing.PhotoTimeMicroseconds || d.Timing.DurationMicroseconds > 3_600_000_000 ||
		!resourceIDPattern.MatchString(d.Timing.PhotoResourceID) || !resourceIDPattern.MatchString(d.Timing.VideoResourceID) ||
		math.Abs(d.Pair.PhotoTime-float64(d.Timing.PhotoTimeMicroseconds)/1e6) > .001 || math.Abs(d.Pair.Duration-float64(d.Timing.DurationMicroseconds)/1e6) > .15 ||
		math.Abs(float64(d.SourceVideo.DurationMilliseconds)-float64(d.Timing.DurationMicroseconds)/1000) > 150 {
		return ErrInvalidManifest
	}
	return nil
}

type LivePairUpload struct {
	ID             string             `json:"id"`
	OwnerID        string             `json:"-"`
	RequestID      string             `json:"requestID"`
	AssetID        string             `json:"assetID"`
	VersionID      string             `json:"versionID"`
	ManifestDigest string             `json:"manifestDigest"`
	Preset         string             `json:"preset"`
	Descriptor     LivePairDescriptor `json:"descriptor"`
	State          string             `json:"state"`
	ExpiresAt      time.Time          `json:"expiresAt"`
	SourcePhoto    SealedObject       `json:"-"`
	SourceVideo    SealedObject       `json:"-"`
	Objects        []SealedObject     `json:"-"`
	PhotoConsumed  bool               `json:"-"`
	MovieConsumed  bool               `json:"-"`
	LeaseToken     string             `json:"-"`
	LeaseUntil     time.Time          `json:"-"`
	Attempts       int                `json:"-"`
}

func (u LivePairUpload) resource(id string) (LivePairResource, error) {
	switch id {
	case "photo":
		return u.Descriptor.Photo, nil
	case "motion":
		return u.Descriptor.Movie, nil
	}
	return LivePairResource{}, ErrInvalidManifest
}
func (u LivePairUpload) key(id string, staging bool) (string, error) {
	if !validOwner(u.OwnerID) || !validOwner(u.ID) {
		return "", ErrInvalidManifest
	}
	if _, err := u.resource(id); err != nil {
		return "", err
	}
	area := "albums/live-pairs/"
	if staging {
		area = "albums/staging/live-pairs/"
	}
	return area + u.OwnerID + "/" + u.ID + "/" + id, nil
}
func (u LivePairUpload) validateReceipt(o SealedObject) error {
	k, err := u.key(o.ResourceID, false)
	r, e := u.resource(o.ResourceID)
	if err != nil || e != nil || o.Key != k || !concreteCOSVersion(o.VersionID) || o.SizeBytes != r.SizeBytes || o.SHA256 != r.SHA256 {
		return ErrConflict
	}
	return nil
}
func pairLease(a, b LivePairUpload, now time.Time) bool {
	return a.ID == b.ID && a.OwnerID == b.OwnerID && a.VersionID == b.VersionID && a.ManifestDigest == b.ManifestDigest && a.Descriptor == b.Descriptor &&
		a.State == "verifying" && validOwner(b.LeaseToken) && a.LeaseToken == b.LeaseToken && a.LeaseUntil.After(now)
}

type LivePairRepository interface {
	LivePair(context.Context, string, string) (LivePairUpload, error)
	LivePairByID(context.Context, string, string) (LivePairUpload, error)
	ReserveLivePair(context.Context, LivePairUpload, time.Time) (LivePairUpload, error)
	SubmitLivePair(context.Context, string, string, time.Time) (LivePairUpload, error)
	PendingLivePairs(context.Context, time.Time, int) ([]LivePairUpload, error)
	ClaimLivePair(context.Context, string, string, string, time.Time, time.Duration) (LivePairUpload, error)
	BeginLivePairSeal(context.Context, LivePairUpload, string, time.Time) (LivePairUpload, bool, error)
	CompleteLivePairPart(context.Context, LivePairUpload, SealedObject, time.Time) (LivePairUpload, error)
	CompleteLivePair(context.Context, LivePairUpload, time.Time) (LivePairUpload, error)
	DeferLivePair(context.Context, LivePairUpload, time.Time) error
}
type LivePairStorage interface {
	AuthorizeLivePairPut(context.Context, LivePairUpload, string, time.Time) (PutGrant, error)
	SealLivePairOnce(context.Context, LivePairUpload, string) (string, error)
	ReconcileLivePairSeal(context.Context, LivePairUpload, string) (string, error)
	OpenLivePairVersion(context.Context, LivePairUpload, string, string) (io.ReadCloser, error)
	AuthorizeLivePairRead(context.Context, LivePairUpload, SealedObject, time.Time) (string, error)
}

func livePairSources(u LivePairUpload, v AssetVersion, p PhotoWriteRecord, j VideoJob) error {
	po, err := p.Receipt()
	vs, e := videoSource(v)
	if err != nil || e != nil || !livePhotoPreviewEligible(v.Manifest) || u.VersionID != v.VersionID || u.AssetID != v.AssetID || u.ManifestDigest != v.ManifestDigest ||
		p.Spec.OwnerID != u.OwnerID || p.VersionID != u.VersionID || p.ManifestDigest != u.ManifestDigest || p.Spec.Preset != PhotoDailyJPEG ||
		j.OwnerID != u.OwnerID || j.VersionID != u.VersionID || j.ManifestDigest != u.ManifestDigest || j.State != "completed" || j.Spec == nil || j.Result == nil ||
		!j.RetirementStartedAt.IsZero() || !j.RetiredAt.IsZero() || j.validateResult(*j.Result) != nil || j.Spec.Source != vs || j.SubmittedAt.IsZero() || !ciJobPattern.MatchString(j.ProviderJobID) ||
		!sameLiveTiming(j.Spec.LivePhotoTiming, v.Manifest.LivePhotoTiming) || !livePhotoProfileMatches(v.Manifest, j.Result.Profile) ||
		u.SourcePhoto != po || u.SourceVideo != j.Result.Object || u.Descriptor.Timing != *v.Manifest.LivePhotoTiming ||
		u.Descriptor.SourcePhoto != (LivePairPhotoSource{string(PhotoDailyJPEG), po.SizeBytes, po.SHA256, p.Spec.Width, p.Spec.Height, "image/jpeg"}) ||
		u.Descriptor.SourceVideo != (LivePairVideoSource{string(VideoDaily1080), LivePairResource{j.Result.Object.SizeBytes, j.Result.Object.SHA256}, j.Result.Profile}) {
		return ErrLivePhotoNotReady
	}
	return u.Descriptor.validate()
}
func (s *UploadService) livePairRepository() (LivePairRepository, error) {
	r, ok := s.repository.(LivePairRepository)
	if !ok {
		return nil, ErrReadUnavailable
	}
	return r, nil
}
func (s *UploadService) CreateLivePair(ctx context.Context, owner, version, request string, descriptor LivePairDescriptor) (LivePairUpload, error) {
	if !validOwner(owner) || !validOwner(version) || !validOwner(request) || descriptor.validate() != nil {
		return LivePairUpload{}, ErrInvalidManifest
	}
	r, err := s.livePairRepository()
	if err != nil {
		return LivePairUpload{}, err
	}
	// A verified canonical pair wins a race between devices. No second write or
	// quota reservation is permitted, even if the locally generated UUID differs.
	if old, err := r.LivePair(ctx, owner, version); err == nil {
		if old.State == "verified" {
			return old, nil
		}
		if old.Descriptor != descriptor || old.RequestID != request {
			return LivePairUpload{}, ErrConflict
		}
		return old, nil
	} else if !errors.Is(err, ErrNotFound) {
		return LivePairUpload{}, err
	}
	v, err := s.verifiedAssetVersion(ctx, owner, version)
	if err != nil {
		return LivePairUpload{}, err
	}
	pr, ok := s.repository.(PhotoWriteRepository)
	vr, vok := s.repository.(VideoJobRepository)
	if !ok || !vok {
		return LivePairUpload{}, ErrReadUnavailable
	}
	p, err := pr.PhotoWrite(ctx, owner, version, PhotoDailyJPEG)
	if err != nil {
		return LivePairUpload{}, err
	}
	j, err := vr.VideoJob(ctx, owner, version, VideoDaily1080)
	if err != nil {
		return LivePairUpload{}, err
	}
	po, err := p.Receipt()
	if err != nil || j.Result == nil {
		return LivePairUpload{}, ErrLivePhotoNotReady
	}
	now := s.now().UTC()
	u := LivePairUpload{ID: uuid.NewString(), OwnerID: owner, RequestID: request, AssetID: v.AssetID, VersionID: version, ManifestDigest: v.ManifestDigest,
		Preset: DailyLivePair, Descriptor: descriptor, State: "uploading", ExpiresAt: now.Add(s.policy.UploadTTL), SourcePhoto: po, SourceVideo: j.Result.Object}
	if err := livePairSources(u, v, p, j); err != nil {
		return LivePairUpload{}, err
	}
	return r.ReserveLivePair(ctx, u, now)
}
func (s *UploadService) LivePair(ctx context.Context, owner, version string) (LivePairUpload, error) {
	if !validOwner(owner) || !validOwner(version) {
		return LivePairUpload{}, ErrInvalidManifest
	}
	r, err := s.livePairRepository()
	if err != nil {
		return LivePairUpload{}, err
	}
	return r.LivePair(ctx, owner, version)
}
func (s *UploadService) LivePairGrants(ctx context.Context, owner, id string) ([]PutGrant, error) {
	if !validOwner(owner) || !validOwner(id) {
		return nil, ErrInvalidManifest
	}
	r, err := s.livePairRepository()
	if err != nil {
		return nil, err
	}
	u, err := r.LivePairByID(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if u.State != "uploading" {
		return nil, ErrConflict
	}
	if !u.ExpiresAt.After(now) {
		return nil, ErrExpired
	}
	storage, ok := s.objects.(LivePairStorage)
	if !ok {
		return nil, ErrReadUnavailable
	}
	expiry := now.Add(10 * time.Minute)
	if u.ExpiresAt.Before(expiry) {
		expiry = u.ExpiresAt
	}
	expiry, err = cloudMediaGrantExpiry(ctx, s.repository, owner, now, expiry)
	if err != nil {
		return nil, err
	}
	grants := make([]PutGrant, 0, 2)
	for _, id := range []string{"photo", "motion"} {
		g, err := storage.AuthorizeLivePairPut(ctx, u, id, expiry)
		if err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, nil
}
func (s *UploadService) SubmitLivePair(ctx context.Context, owner, id string) (LivePairUpload, error) {
	if !validOwner(owner) || !validOwner(id) {
		return LivePairUpload{}, ErrInvalidManifest
	}
	r, err := s.livePairRepository()
	if err != nil {
		return LivePairUpload{}, err
	}
	return r.SubmitLivePair(ctx, owner, id, s.now().UTC())
}

// Only the trusted worker invokes this method. A consumed COPY intent never
// permits another COPY after restart or response loss. Recovery is HEAD/GET only.
func (s *UploadService) VerifyLivePair(ctx context.Context, owner, id string) (LivePairUpload, error) {
	r, err := s.livePairRepository()
	if err != nil {
		return LivePairUpload{}, err
	}
	storage, ok := s.objects.(LivePairStorage)
	if !ok {
		return LivePairUpload{}, ErrReadUnavailable
	}
	u, err := r.ClaimLivePair(ctx, owner, id, uuid.NewString(), s.now().UTC(), s.policy.VerificationLease)
	if err != nil {
		return LivePairUpload{}, err
	}
	claimed := u
	ctx, cancel := context.WithDeadline(ctx, u.LeaseUntil)
	defer cancel()
	success := false
	defer func() {
		if !success {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer stop()
			_ = r.DeferLivePair(cleanup, claimed, s.now().UTC())
		}
	}()
	for _, part := range []string{"photo", "motion"} {
		found := false
		for _, object := range u.Objects {
			if object.ResourceID == part {
				found = true
			}
		}
		if found {
			continue
		}
		var fresh bool
		u, fresh, err = r.BeginLivePairSeal(ctx, u, part, s.now().UTC())
		if err != nil {
			return LivePairUpload{}, err
		}
		var version string
		if fresh {
			version, err = storage.SealLivePairOnce(ctx, u, part)
		} else {
			version, err = storage.ReconcileLivePairSeal(ctx, u, part)
		}
		if err != nil || !concreteCOSVersion(version) {
			return LivePairUpload{}, ErrLivePairNotReady
		}
		body, err := storage.OpenLivePairVersion(ctx, u, part, version)
		if err != nil {
			return LivePairUpload{}, err
		}
		if body == nil {
			return LivePairUpload{}, ErrIncompleteBackup
		}
		expected, _ := u.resource(part)
		hash := sha256.New()
		count, copyErr := io.Copy(hash, io.LimitReader(body, expected.SizeBytes+1))
		closeErr := body.Close()
		if copyErr != nil || closeErr != nil || count != expected.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 || ctx.Err() != nil {
			return LivePairUpload{}, ErrIncompleteBackup
		}
		key, _ := u.key(part, false)
		u, err = r.CompleteLivePairPart(ctx, u, SealedObject{part, key, version, count, expected.SHA256}, s.now().UTC())
		if err != nil {
			return LivePairUpload{}, err
		}
	}
	u, err = r.CompleteLivePair(ctx, u, s.now().UTC())
	success = err == nil
	return u, err
}

type LivePairObjectReadGrant struct {
	ObjectVersionID string            `json:"objectVersionID"`
	SizeBytes       int64             `json:"sizeBytes"`
	SHA256          string            `json:"sha256"`
	ContentType     string            `json:"contentType"`
	URL             string            `json:"url"`
	Headers         map[string]string `json:"headers"`
	ExpiresAt       time.Time         `json:"expiresAt"`
}
type LivePairReadGrant struct {
	PairUploadID        string                  `json:"pairUploadID"`
	AssetID             string                  `json:"assetID"`
	VersionID           string                  `json:"versionID"`
	ManifestDigest      string                  `json:"manifestDigest"`
	Preset              string                  `json:"preset"`
	Descriptor          LivePairDescriptor      `json:"descriptor"`
	Photo               LivePairObjectReadGrant `json:"photo"`
	Movie               LivePairObjectReadGrant `json:"movie"`
	ReplacementEligible bool                    `json:"replacementEligible"`
	ExpiresAt           time.Time               `json:"expiresAt"`
}

func (s *UploadService) LivePairReadGrant(ctx context.Context, owner, version string) (LivePairReadGrant, error) {
	u, err := s.LivePair(ctx, owner, version)
	if errors.Is(err, ErrNotFound) {
		return LivePairReadGrant{}, ErrLivePairNotReady
	}
	if err != nil {
		return LivePairReadGrant{}, err
	}
	if u.State != "verified" || len(u.Objects) != 2 {
		return LivePairReadGrant{}, ErrLivePairNotReady
	}
	v, err := s.verifiedAssetVersion(ctx, owner, version)
	if err != nil {
		return LivePairReadGrant{}, err
	}
	if v.AssetID != u.AssetID || v.ManifestDigest != u.ManifestDigest || !livePhotoPreviewEligible(v.Manifest) || u.Descriptor.Timing != *v.Manifest.LivePhotoTiming {
		return LivePairReadGrant{}, ErrConflict
	}
	storage, ok := s.objects.(LivePairStorage)
	if !ok {
		return LivePairReadGrant{}, ErrReadUnavailable
	}
	expiry := s.now().UTC().Add(5 * time.Minute)
	result := LivePairReadGrant{PairUploadID: u.ID, AssetID: u.AssetID, VersionID: version, ManifestDigest: u.ManifestDigest, Preset: DailyLivePair, Descriptor: u.Descriptor, ExpiresAt: expiry}
	seen := map[string]bool{}
	for _, object := range u.Objects {
		if u.validateReceipt(object) != nil || seen[object.ResourceID] {
			return LivePairReadGrant{}, ErrConflict
		}
		seen[object.ResourceID] = true
		url, err := storage.AuthorizeLivePairRead(ctx, u, object, expiry)
		if err != nil {
			return LivePairReadGrant{}, err
		}
		g := LivePairObjectReadGrant{ObjectVersionID: object.VersionID, SizeBytes: object.SizeBytes, SHA256: object.SHA256, URL: url, Headers: map[string]string{}, ExpiresAt: expiry}
		if object.ResourceID == "photo" {
			g.ContentType = "image/jpeg"
			result.Photo = g
		} else {
			g.ContentType = "video/quicktime"
			result.Movie = g
		}
	}
	return result, nil
}
