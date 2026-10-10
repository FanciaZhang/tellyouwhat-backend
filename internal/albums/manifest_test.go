package albums

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func liveManifest() Manifest {
	return Manifest{AssetID: "asset-1", SourceRevision: "revision-1", Kind: LivePhoto, Resources: []Resource{
		{ID: "photo", Role: OriginalPhoto, SizeBytes: 100, SHA256: strings.Repeat("a", 64)},
		{ID: "motion", Role: PairedVideo, SizeBytes: 200, SHA256: strings.Repeat("b", 64)},
	}}
}

func TestMissingLiveResourceCannotConfirmBackup(t *testing.T) {
	m := liveManifest()
	_, err := VerifyBackup(m, []Observation{{ResourceID: "photo", SizeBytes: 100, ComputedSHA256: m.Resources[0].SHA256}})
	if !errors.Is(err, ErrIncompleteBackup) {
		t.Fatalf("got %v", err)
	}
	m.Resources = m.Resources[:1]
	if !errors.Is(m.Validate(), ErrInvalidManifest) {
		t.Fatal("accepted incomplete live photo manifest")
	}
}

func TestEditedPhotoRequiresCurrentDisplayResource(t *testing.T) {
	m := Manifest{AssetID: "asset-1", SourceRevision: "r", Kind: Photo, Edited: true, Resources: liveManifest().Resources[:1]}
	if !errors.Is(m.Validate(), ErrInvalidManifest) {
		t.Fatal("lost edited display state")
	}
	m.Resources = append(m.Resources, Resource{ID: "current", Role: CurrentPhoto, SizeBytes: 90, SHA256: strings.Repeat("c", 64)})
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDigestIsCanonicalAndVersionBound(t *testing.T) {
	m := liveManifest()
	first, _ := m.Digest()
	m.Resources[0], m.Resources[1] = m.Resources[1], m.Resources[0]
	second, _ := m.Digest()
	if first != second {
		t.Fatal("order changed identity")
	}
	m.SourceRevision = "revision-2"
	third, _ := m.Digest()
	if first == third {
		t.Fatal("revision not bound to backup")
	}
}

func TestSizeOrActualHashMismatchCannotConfirmBackup(t *testing.T) {
	m := liveManifest()
	observations := []Observation{{"photo", 100, m.Resources[0].SHA256}, {"motion", 200, m.Resources[1].SHA256}}
	if _, err := VerifyBackup(m, observations); err != nil {
		t.Fatal(err)
	}
	observations[1].ComputedSHA256 = strings.Repeat("c", 64)
	if _, err := VerifyBackup(m, observations); !errors.Is(err, ErrIncompleteBackup) {
		t.Fatalf("got %v", err)
	}
	observations[1] = observations[0]
	if _, err := VerifyBackup(m, observations); !errors.Is(err, ErrIncompleteBackup) {
		t.Fatal("accepted duplicate verification")
	}
}

func TestManifestRejectsSizeOverflowAndUnsafeIdentifiers(t *testing.T) {
	m := liveManifest()
	m.Resources[0].SizeBytes = math.MaxInt64
	if !errors.Is(m.Validate(), ErrInvalidManifest) {
		t.Fatal("accepted size overflow")
	}
	m = liveManifest()
	m.AssetID = "../another-user"
	if !errors.Is(m.Validate(), ErrInvalidManifest) {
		t.Fatal("accepted unsafe identity")
	}
}
