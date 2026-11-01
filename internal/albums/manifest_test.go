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

func TestSystemCloudIdentifierIsValidatedAndDoesNotReplaceBackupProof(t *testing.T) {
	m := liveManifest()
	before, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	identifier := "opaque-system-cloud-id"
	m.SystemCloudIdentifier = &identifier
	after, err := m.Digest()
	if err != nil || before != after {
		t.Fatal("discovery hint changed the original content proof", err)
	}
	data, err := json.Marshal(m)
	var restored Manifest
	if err != nil || json.Unmarshal(data, &restored) != nil || restored.SystemCloudIdentifier == nil || *restored.SystemCloudIdentifier != identifier {
		t.Fatal("cloud identifier lost on persistence")
	}
	for _, invalid := range []string{"", "line\nbreak", strings.Repeat("x", 4097), strings.Repeat("图", 1366)} {
		m.SystemCloudIdentifier = &invalid
		if !errors.Is(m.Validate(), ErrInvalidManifest) {
			t.Fatal("unsafe identifier accepted")
		}
	}
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

func TestAssetMetadataIsDigestBoundAndValidatesDomain(t *testing.T) {
	m := liveManifest()
	legacy, _ := m.Digest()
	timestamp := int64(-1)
	m.Metadata = &AssetMetadata{CreationTimeMilliseconds: &timestamp, PixelWidth: 4000, PixelHeight: 3000, Favorite: true, Location: &AssetLocation{Latitude: 0, Longitude: 0}}
	digest, err := m.Digest()
	if err != nil || digest == legacy {
		t.Fatal("metadata not bound")
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var restored Manifest
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	got, err := restored.Digest()
	if err != nil || got != digest || restored.Metadata.Location.Latitude != 0 || *restored.Metadata.CreationTimeMilliseconds != -1 {
		t.Fatal("metadata lost")
	}
	m.Metadata.Favorite = false
	changed, _ := m.Digest()
	if changed == digest {
		t.Fatal("favorite not bound")
	}
	for _, coordinate := range []float64{91, -91, math.NaN(), math.Inf(1)} {
		m.Metadata.Location.Latitude = coordinate
		if !errors.Is(m.Validate(), ErrInvalidManifest) {
			t.Fatal("invalid latitude")
		}
	}
	m.Metadata.Location = nil
	for _, date := range []int64{-62135596800001, 253402300800000} {
		m.Metadata.CreationTimeMilliseconds = &date
		if !errors.Is(m.Validate(), ErrInvalidManifest) {
			t.Fatal("invalid capture date")
		}
	}
	m.Metadata.CreationTimeMilliseconds = nil
	m.Metadata.DurationMilliseconds = -1
	if !errors.Is(m.Validate(), ErrInvalidManifest) {
		t.Fatal("negative duration")
	}
	m.Metadata.DurationMilliseconds = 0
	m.Metadata.PixelWidth = 0
	if !errors.Is(m.Validate(), ErrInvalidManifest) {
		t.Fatal("missing dimensions")
	}
}

func nativeSlowMotionManifest() Manifest {
	return Manifest{AssetID: "native-video", SourceRevision: "r1", Kind: Video, Edited: true,
		VideoRecipe: "apple_slow_motion_1_1",
		Metadata:    &AssetMetadata{PixelWidth: 320, PixelHeight: 240, DurationMilliseconds: 1000, MediaSubtypes: 131072},
		Resources: []Resource{
			{ID: "original", Role: OriginalVideo, SizeBytes: 1024, SHA256: strings.Repeat("a", 64), Descriptor: &ResourceDescriptor{2, "source.mov", "com.apple.quicktime-movie"}},
			{ID: "adjustment", Role: Adjustment, SizeBytes: 200, SHA256: strings.Repeat("b", 64), Descriptor: &ResourceDescriptor{7, "adjustment.plist", "com.apple.property-list"}},
		},
	}
}

func TestNativeSlowMotionRequiresExactQualifiedResources(t *testing.T) {
	m := nativeSlowMotionManifest()
	digest, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	observations := []Observation{{"original", 1024, strings.Repeat("a", 64)}, {"adjustment", 200, strings.Repeat("b", 64)}}
	if got, err := VerifyBackup(m, observations); err != nil || got != digest {
		t.Fatal("qualified proof rejected", err)
	}
	if _, err := VerifyBackup(m, observations[:1]); !errors.Is(err, ErrIncompleteBackup) {
		t.Fatal("missing recipe bytes accepted")
	}
	data, _ := json.Marshal(m)
	var restored Manifest
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if got, err := restored.Digest(); err != nil || got != digest || restored.VideoRecipe != m.VideoRecipe {
		t.Fatal("recipe lost on persistence", err)
	}
	for name, change := range map[string]func(*Manifest){
		"absent": func(m *Manifest) { m.VideoRecipe = "" }, "unknown": func(m *Manifest) { m.VideoRecipe = "other" },
		"plain": func(m *Manifest) { m.Edited = false }, "photo": func(m *Manifest) { m.Kind = Photo },
		"metadata": func(m *Manifest) { m.Metadata = nil }, "subtype": func(m *Manifest) { m.Metadata.MediaSubtypes = 0 },
		"descriptor": func(m *Manifest) { m.Resources[1].Descriptor = nil },
		"render": func(m *Manifest) {
			m.Resources = append(m.Resources, Resource{ID: "current", Role: CurrentVideo, SizeBytes: 100, SHA256: strings.Repeat("c", 64), Descriptor: &ResourceDescriptor{6, "current.mov", "com.apple.quicktime-movie"}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := nativeSlowMotionManifest()
			change(&bad)
			if !errors.Is(bad.Validate(), ErrInvalidManifest) {
				t.Fatal("invalid native recipe accepted")
			}
		})
	}
	// Digest must bind the recipe field itself, as well as its resource hashes.
	canonical, _ := json.Marshal(m)
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) == digest {
		t.Fatal("expected resource canonical ordering before digest")
	}
	m.Resources[0], m.Resources[1] = m.Resources[1], m.Resources[0]
	canonical, _ = json.Marshal(m)
	sum = sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != digest {
		t.Fatal("canonical proof omitted recipe")
	}
}
