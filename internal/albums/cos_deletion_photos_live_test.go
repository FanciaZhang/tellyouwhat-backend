//go:build albums_live

package albums

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestLiveCOSPhotoDeletionObservationReadOnly(t *testing.T) {
	s, root, ctx := liveRoundtripFixture(t)
	var evidence struct {
		Components struct {
			Passed bool         `json:"passed"`
			Photo  SealedObject `json:"photo"`
		} `json:"components"`
		Pair struct {
			Passed     bool               `json:"passed"`
			Descriptor LivePairDescriptor `json:"descriptor"`
		} `json:"pair"`
	}
	roundtripRead(t, "../../docs/evidence", "albums-ci-live-roundtrip-2026-10-13.json", &evidence)
	object, source := evidence.Components.Photo, evidence.Pair.Descriptor.SourcePhoto
	parts := strings.Split(object.Key, "/")
	if !evidence.Components.Passed || !evidence.Pair.Passed || len(parts) != 5 || parts[0] != "albums" || parts[1] != "derivatives" || object.ResourceID != source.Preset || object.SizeBytes != source.SizeBytes || object.SHA256 != source.SHA256 || !concreteCOSVersion(object.VersionID) {
		t.Fatal("recorded synthetic preview required")
	}
	spec := PhotoWriteSpec{OwnerID: parts[2], AttemptID: parts[3], Preset: PhotoPreset(source.Preset), SizeBytes: source.SizeBytes, SHA256: source.SHA256, Width: source.Width, Height: source.Height}
	key, err := spec.key()
	if err != nil || key != object.Key {
		t.Fatal("preview binding invalid")
	}
	original := s.archiveHTTP.Transport
	calls := 0
	s.archiveHTTP.Transport = cosTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodHead || req.URL.Scheme != "https" || req.URL.Host != "tellyouwhat-albums-1251995834.cos.ap-guangzhou.myqcloud.com" || req.URL.Path != "/"+key || req.URL.RawQuery != "" {
			return nil, errors.New("read-only photo observation rejected request")
		}
		calls++
		return original.RoundTrip(req)
	})
	version, err := s.observePhotoDeletion(ctx, spec)
	if err != nil || version != object.VersionID || calls != 1 {
		t.Fatal("photo HEAD did not match recorded version", err, calls)
	}
	roundtripJSON(t, root, "photo-deletion-observation-evidence.json", map[string]any{"passed": true, "requests": calls, "method": "HEAD", "version_matches_saved_receipt": true, "media_content_downloaded": false, "mutations_allowed": false, "database_receipts_written": false})
}
