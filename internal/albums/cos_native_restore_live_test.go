//go:build albums_live

package albums

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Read-only bridge to the native PhotoKit test. Grants stay in the explicit
// private fixture directory; these synthetic manifests are never published.
func TestLiveCOSNativeOriginalGrants(t *testing.T) {
	s, root, ctx := liveRoundtripFixture(t)
	var receipt struct {
		Tests map[string]struct {
			Passed  bool         `json:"passed"`
			Source  SealedObject `json:"source"`
			Storage string       `json:"sourceStorageClass"`
		} `json:"tests"`
	}
	roundtripRead(t, "../../docs/evidence", "albums-low-frequency-2026-10-13.json", &receipt)
	var fixtures []map[string]any
	var verified []map[string]any
	for i, name := range []string{"jpeg", "oriented-jpeg", "transparent-png", "video"} {
		entry, ok := receipt.Tests[name]
		object := entry.Source
		parts := strings.Split(object.Key, "/")
		if !ok || !entry.Passed || entry.Storage != "MAZ_STANDARD_IA" || len(parts) != 5 || parts[0] != "albums" || parts[1] != "originals" || object.SizeBytes <= 0 || object.SizeBytes > 2<<20 {
			t.Fatal("invalid fixed synthetic receipt")
		}
		assertLiveStorageClass(t, ctx, s, object, "MAZ_STANDARD_IA")
		reader, err := s.OpenVersion(ctx, object.Key, object.VersionID)
		if err != nil {
			t.Fatal("fixed original unavailable")
		}
		data, err := io.ReadAll(io.LimitReader(reader, object.SizeBytes+1))
		reader.Close()
		if err != nil || int64(len(data)) != object.SizeBytes || roundtripHash(data) != object.SHA256 {
			t.Fatal("fixed original bytes changed")
		}
		kind, role, uti, ext, photoType := "photo", "original_photo", "public.jpeg", "jpg", 1
		if name == "transparent-png" {
			uti, ext = "public.png", "png"
		}
		if name == "video" {
			kind, role, uti, ext, photoType = "video", "original_video", "public.mpeg-4", "mp4", 2
		}
		assetID := uuid.NewString()
		manifest := map[string]any{"assetID": assetID, "sourceRevision": "controlled-cos-ia-fixture", "kind": kind, "edited": false,
			"resources": []any{map[string]any{"id": object.ResourceID, "role": role, "sizeBytes": object.SizeBytes, "sha256": object.SHA256,
				"descriptor": map[string]any{"photoKitType": photoType, "filename": "Albums-COS-Restore-" + name + "." + ext, "uniformTypeIdentifier": uti}}}}
		encoded, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal("manifest encoding failed")
		}
		now := time.Now().UTC().Truncate(time.Second)
		expires := now.Add(4 * time.Minute)
		url, err := s.AuthorizeOriginalRead(ctx, object, expires)
		if err != nil {
			t.Fatal("original grant failed")
		}
		fixtures = append(fixtures, map[string]any{"name": name, "ownerID": parts[2], "url": url, "expiresAt": expires, "objectVersionID": object.VersionID,
			"version": map[string]any{"sequence": i + 1, "assetID": assetID, "versionID": parts[3], "manifestDigest": roundtripHash(encoded),
				"manifest": manifest, "originalBytes": object.SizeBytes, "publishedAt": now}})
		verified = append(verified, map[string]any{"name": name, "source": object, "storageClass": "MAZ_STANDARD_IA", "bytesVerified": true})
	}
	roundtripJSON(t, root, "native-original-grants.json", fixtures)
	roundtripJSON(t, root, "native-original-source-evidence.json", map[string]any{"scope": "read-only existing synthetic IA originals; no database, uploads, deletion or account authentication", "sources": verified, "passed": true})
}
