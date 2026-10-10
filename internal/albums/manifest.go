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

type Resource struct {
	ID        string       `json:"id"`
	Role      ResourceRole `json:"role"`
	SizeBytes int64        `json:"sizeBytes"`
	SHA256    string       `json:"sha256"`
}

type Manifest struct {
	AssetID        string     `json:"assetID"`
	SourceRevision string     `json:"sourceRevision"`
	Kind           AssetKind  `json:"kind"`
	Edited         bool       `json:"edited"`
	Resources      []Resource `json:"resources"`
}

var resourceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,96}$`)

func (m Manifest) Validate() error {
	if !resourceIDPattern.MatchString(m.AssetID) || len(m.SourceRevision) == 0 || len(m.SourceRevision) > 256 || len(m.Resources) == 0 || len(m.Resources) > 32 {
		return ErrInvalidManifest
	}
	roles := make(map[ResourceRole]int)
	ids := make(map[string]struct{})
	var total int64
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
		ids[resource.ID] = struct{}{}
		roles[resource.Role]++
		total += resource.SizeBytes
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
	return nil
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
