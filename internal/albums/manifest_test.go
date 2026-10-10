package albums

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestResourceDescriptorsAreCompleteRoleBoundAndDigestBound(t *testing.T) {
	m := liveManifest()
	legacy, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	m.Resources[0].Descriptor = &ResourceDescriptor{PhotoKitType: 1, Filename: "照片.heic", UniformTypeIdentifier: "public.heic"}
	if !errors.Is(m.Validate(), ErrInvalidManifest) {
		t.Fatal("accepted partial reconstruction metadata")
	}
	m.Resources[1].Descriptor = &ResourceDescriptor{PhotoKitType: 9, Filename: "motion.mov", UniformTypeIdentifier: "com.apple.quicktime-movie"}
	full, err := m.Digest()
	if err != nil || full == legacy {
		t.Fatalf("descriptor not bound: %v", err)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var restored Manifest
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	digest, err := restored.Digest()
	if err != nil || digest != full || restored.Resources[0].Descriptor.Filename != "照片.heic" {
		t.Fatal("metadata lost on persistence roundtrip")
	}
	m.Resources[0].Descriptor.Filename = "changed.heic"
	changed, err := m.Digest()
	if err != nil || changed == full {
		t.Fatal("filename not bound")
	}
	m.Resources[1].Descriptor.PhotoKitType = 2
	if !errors.Is(m.Validate(), ErrInvalidManifest) {
		t.Fatal("paired resource accepted as regular video")
	}
}

func TestLegacyManifestDigestDoesNotChangeWhenMetadataAbsent(t *testing.T) {
	m := liveManifest()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "descriptor") {
		t.Fatal("legacy wire format changed")
	}
	// Canonical resource ordering and existing JSON fields are unchanged.
	m.Resources[0], m.Resources[1] = m.Resources[1], m.Resources[0]
	canonical, _ := json.Marshal(m)
	hash := sha256.Sum256(canonical)
	got, err := m.Digest()
	if err != nil || got != hex.EncodeToString(hash[:]) {
		t.Fatalf("legacy digest changed: %s %v", got, err)
	}
}

func TestDescriptorRejectsInvalidTypeSizeAndEncoding(t *testing.T) {
	for _, descriptor := range []ResourceDescriptor{
		{0, "a", "public.heic"}, {math.MaxInt32 + 1, "a", "public.heic"},
		{1, strings.Repeat("a", 4097), "public.heic"}, {1, "a\x00b", "public.heic"},
		{1, "\xff", "public.heic"}, {1, "a", ""}, {1, "a", strings.Repeat("x", 513)},
		{1, "a", "bad\x00type"}, {1, "a", "\xff"}, {9, "a", "public.heic"},
	} {
		if !errors.Is(descriptor.validate(OriginalPhoto), ErrInvalidManifest) {
			t.Fatal("accepted invalid descriptor")
		}
	}
	// Preserve unknown resource types as opaque companions, not as original photos.
	descriptor := ResourceDescriptor{100, "future", "public.data"}
	if err := descriptor.validate(Alternate); err != nil {
		t.Fatal(err)
	}
}
