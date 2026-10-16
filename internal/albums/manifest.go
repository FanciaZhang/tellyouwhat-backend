// Package albums owns durable album assets. It deliberately does not reuse
// internal/media's expiring AI upload records or device identity as an owner.
package albums

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var ErrInvalidManifest = errors.New("invalid album resource manifest")
var ErrIncompleteBackup = errors.New("album backup is not verified")

type AssetKind string

const (
	Photo     AssetKind = "photo"
	Video     AssetKind = "video"
	LivePhoto AssetKind = "live_photo"
)

type ResourceRole string

const (
	OriginalPhoto ResourceRole = "original_photo"
	OriginalVideo ResourceRole = "original_video"
	PairedVideo   ResourceRole = "paired_video"
	CurrentPhoto  ResourceRole = "current_photo"
	CurrentVideo  ResourceRole = "current_video"
	Adjustment    ResourceRole = "adjustment"
	Alternate     ResourceRole = "alternate"
)

// ResourceDescriptor preserves PhotoKit reconstruction metadata. Filename is
// descriptive, untrusted metadata, never an object key or filesystem path.
// Nil descriptors identify legacy manifests; those do not establish full
// system-library restoration capability.
type ResourceDescriptor struct {
	PhotoKitType          int    `json:"photoKitType"`
	Filename              string `json:"filename"`
	UniformTypeIdentifier string `json:"uniformTypeIdentifier"`
}

func (d ResourceDescriptor) validate(role ResourceRole) error {
	if d.PhotoKitType <= 0 || d.PhotoKitType > math.MaxInt32 || len(d.Filename) > 4096 || !utf8.ValidString(d.Filename) ||
		strings.ContainsRune(d.Filename, 0) || len(d.UniformTypeIdentifier) == 0 || len(d.UniformTypeIdentifier) > 512 ||
		!utf8.ValidString(d.UniformTypeIdentifier) || strings.ContainsRune(d.UniformTypeIdentifier, 0) {
		return ErrInvalidManifest
	}
	expected := Alternate
	switch d.PhotoKitType {
	case 1:
		expected = OriginalPhoto
	case 2:
		expected = OriginalVideo
	case 5:
		expected = CurrentPhoto
	case 6, 10:
		expected = CurrentVideo
	case 7:
		expected = Adjustment
	case 9:
		expected = PairedVideo
	}
	if role != expected {
		return ErrInvalidManifest
	}
	return nil
}

type Resource struct {
	ID         string              `json:"id"`
	Role       ResourceRole        `json:"role"`
	SizeBytes  int64               `json:"sizeBytes"`
	SHA256     string              `json:"sha256"`
	Descriptor *ResourceDescriptor `json:"descriptor,omitempty"`
}

// AssetMetadata is the capture/display snapshot, not an authorization claim.
// Times use integer UTC milliseconds, independent of client date encoders.
type AssetLocation struct {
	Latitude  float64  `json:"latitude"`
	Longitude float64  `json:"longitude"`
	Altitude  *float64 `json:"altitude,omitempty"`
}
type AssetMetadata struct {
	CreationTimeMilliseconds *int64         `json:"creationTimeMilliseconds,omitempty"`
	PixelWidth               int            `json:"pixelWidth"`
	PixelHeight              int            `json:"pixelHeight"`
	DurationMilliseconds     int64          `json:"durationMilliseconds"`
	MediaSubtypes            int64          `json:"mediaSubtypes"`
	Favorite                 bool           `json:"favorite"`
	Location                 *AssetLocation `json:"location,omitempty"`
}

func (m AssetMetadata) validate() error {
	if m.PixelWidth <= 0 || m.PixelWidth > math.MaxInt32 || m.PixelHeight <= 0 || m.PixelHeight > math.MaxInt32 || m.DurationMilliseconds < 0 || m.MediaSubtypes < 0 {
		return ErrInvalidManifest
	}
	if m.CreationTimeMilliseconds != nil && (*m.CreationTimeMilliseconds < -62135596800000 || *m.CreationTimeMilliseconds > 253402300799999) {
		return ErrInvalidManifest
	}
	if m.Location != nil {
		l := m.Location
		if math.IsNaN(l.Latitude) || math.IsInf(l.Latitude, 0) || l.Latitude < -90 || l.Latitude > 90 || math.IsNaN(l.Longitude) || math.IsInf(l.Longitude, 0) || l.Longitude < -180 || l.Longitude > 180 {
			return ErrInvalidManifest
		}
		if l.Altitude != nil && (math.IsNaN(*l.Altitude) || math.IsInf(*l.Altitude, 0)) {
			return ErrInvalidManifest
		}
	}
	return nil
}

type Manifest struct {
	AssetID         string           `json:"assetID"`
	SourceRevision  string           `json:"sourceRevision"`
	Kind            AssetKind        `json:"kind"`
	Edited          bool             `json:"edited"`
	Resources       []Resource       `json:"resources"`
	Metadata        *AssetMetadata   `json:"metadata,omitempty"`
	LivePhotoTiming *LivePhotoTiming `json:"livePhotoTiming,omitempty"`
}

// Timing belongs to the current resource pair, not PHAsset.duration (which may
// be zero for a Live Photo). It is digest-bound reconstruction metadata only.
// Legacy backups without timing remain valid, but cannot produce a daily pair.
type LivePhotoTiming struct {
	PhotoResourceID       string `json:"photoResourceID"`
	VideoResourceID       string `json:"videoResourceID"`
	PhotoTimeMicroseconds int64  `json:"photoTimeMicroseconds"`
	DurationMicroseconds  int64  `json:"durationMicroseconds"`
}

func (t LivePhotoTiming) valid() bool {
	return t.DurationMicroseconds > 0 && t.DurationMicroseconds <= 3_600_000_000 &&
		t.PhotoTimeMicroseconds >= 0 && t.PhotoTimeMicroseconds < t.DurationMicroseconds &&
		resourceIDPattern.MatchString(t.PhotoResourceID) && resourceIDPattern.MatchString(t.VideoResourceID) && t.PhotoResourceID != t.VideoResourceID
}
func sameLiveTiming(a, b *LivePhotoTiming) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func (m Manifest) validateLivePhotoTiming() error {
	t := m.LivePhotoTiming
	if t == nil {
		return nil
	}
	if m.Kind != LivePhoto || m.Metadata == nil || m.Metadata.MediaSubtypes&8 == 0 ||
		!t.valid() {
		return ErrInvalidManifest
	}
	photoRole, videoRole, photoType, videoType := OriginalPhoto, PairedVideo, 1, 9
	if m.Edited {
		photoRole, videoRole, photoType, videoType = CurrentPhoto, CurrentVideo, 5, 10
	}
	photo, video := false, false
	for _, r := range m.Resources {
		if r.Descriptor == nil {
			return ErrInvalidManifest
		}
		if r.ID == t.PhotoResourceID {
			photo = r.Role == photoRole && r.Descriptor.PhotoKitType == photoType
		}
		if r.ID == t.VideoResourceID {
			video = r.Role == videoRole && r.Descriptor.PhotoKitType == videoType
		}
	}
	if !photo || !video {
		return ErrInvalidManifest
	}
	return nil
}

var resourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,96}$`)

func (m Manifest) Validate() error {
	if m.Metadata != nil {
		if err := m.Metadata.validate(); err != nil {
			return err
		}
	}
	if !resourceIDPattern.MatchString(m.AssetID) || len(m.SourceRevision) == 0 || len(m.SourceRevision) > 256 || len(m.Resources) == 0 || len(m.Resources) > 32 {
		return ErrInvalidManifest
	}
	roles := make(map[ResourceRole]int)
	ids := make(map[string]struct{})
	var total int64
	described := 0
	for _, resource := range m.Resources {
		if !resourceIDPattern.MatchString(resource.ID) || resource.SizeBytes <= 0 || resource.SizeBytes > math.MaxInt64-total {
			return fmt.Errorf("%w: invalid resource size or identity", ErrInvalidManifest)
		}
		digest, err := hex.DecodeString(resource.SHA256)
		if err != nil || len(digest) != sha256.Size {
			return fmt.Errorf("%w: invalid sha256", ErrInvalidManifest)
		}
		if _, duplicate := ids[resource.ID]; duplicate {
			return fmt.Errorf("%w: duplicate resource", ErrInvalidManifest)
		}
		switch resource.Role {
		case OriginalPhoto, OriginalVideo, PairedVideo, CurrentPhoto, CurrentVideo, Adjustment, Alternate:
		default:
			return fmt.Errorf("%w: unknown resource role", ErrInvalidManifest)
		}
		if resource.Descriptor != nil {
			if err := resource.Descriptor.validate(resource.Role); err != nil {
				return err
			}
			described++
		}
		ids[resource.ID] = struct{}{}
		roles[resource.Role]++
		total += resource.SizeBytes
	}
	if described != 0 && described != len(m.Resources) {
		return ErrInvalidManifest
	}
	for role, count := range roles {
		if role != Alternate && count > 1 {
			return fmt.Errorf("%w: ambiguous resource role", ErrInvalidManifest)
		}
	}
	switch m.Kind {
	case Photo:
		if roles[OriginalPhoto] != 1 || roles[OriginalVideo] != 0 || roles[PairedVideo] != 0 || roles[CurrentVideo] != 0 || (m.Edited && roles[CurrentPhoto] != 1) {
			return fmt.Errorf("%w: photo resources incomplete", ErrInvalidManifest)
		}
	case Video:
		if roles[OriginalVideo] != 1 || roles[OriginalPhoto] != 0 || roles[PairedVideo] != 0 || roles[CurrentPhoto] != 0 || (m.Edited && roles[CurrentVideo] != 1) {
			return fmt.Errorf("%w: video resources incomplete", ErrInvalidManifest)
		}
	case LivePhoto:
		if roles[OriginalPhoto] != 1 || roles[PairedVideo] != 1 || roles[OriginalVideo] != 0 || (m.Edited && (roles[CurrentPhoto] != 1 || roles[CurrentVideo] != 1)) {
			return fmt.Errorf("%w: live photo resources incomplete", ErrInvalidManifest)
		}
	default:
		return ErrInvalidManifest
	}
	return m.validateLivePhotoTiming()
}

// Digest binds a backup to all its resources and the exact source revision.
// Resource order is immaterial; the caller's manifest is never mutated.
func (m Manifest) Digest() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	m.Resources = append([]Resource(nil), m.Resources...)
	sort.Slice(m.Resources, func(i, j int) bool { return m.Resources[i].ID < m.Resources[j].ID })
	for index := range m.Resources {
		bytes, _ := hex.DecodeString(m.Resources[index].SHA256)
		m.Resources[index].SHA256 = hex.EncodeToString(bytes)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// Observation comes only from a trusted object-reading verifier, never from
// client metadata, upload success responses or multipart ETags.
type Observation struct {
	ResourceID     string
	SizeBytes      int64
	ComputedSHA256 string
}

func VerifyBackup(m Manifest, observations []Observation) (string, error) {
	digest, err := m.Digest()
	if err != nil {
		return "", err
	}
	if len(observations) != len(m.Resources) {
		return "", ErrIncompleteBackup
	}
	byID := make(map[string]Observation, len(observations))
	for _, observation := range observations {
		if _, duplicate := byID[observation.ResourceID]; duplicate {
			return "", ErrIncompleteBackup
		}
		byID[observation.ResourceID] = observation
	}
	for _, resource := range m.Resources {
		observation, ok := byID[resource.ID]
		if !ok || observation.SizeBytes != resource.SizeBytes {
			return "", ErrIncompleteBackup
		}
		expected, _ := hex.DecodeString(resource.SHA256)
		actual, err := hex.DecodeString(observation.ComputedSHA256)
		if err != nil || len(actual) != sha256.Size || hex.EncodeToString(expected) != hex.EncodeToString(actual) {
			return "", ErrIncompleteBackup
		}
	}
	return digest, nil
}
