//go:build albums_live

package albums

import (
	"errors"
	"net/http"
	"testing"
)

// The transport allows only HEAD of four previously verified synthetic originals.
// It cannot download media, copy, transcode, delete, or access another namespace.
func TestLiveCOSOriginalDeletionObservationReadOnly(t *testing.T) {
	s, root, ctx := liveRoundtripFixture(t)
	var receipt struct {
		Tests map[string]struct {
			Passed bool         `json:"passed"`
			Source SealedObject `json:"source"`
		} `json:"tests"`
	}
	roundtripRead(t, "../../docs/evidence", "albums-low-frequency-2026-10-13.json", &receipt)
	names := []string{"jpeg", "oriented-jpeg", "transparent-png", "video"}
	expected := map[string]string{}
	for _, name := range names {
		entry, ok := receipt.Tests[name]
		if !ok || !entry.Passed || !albumObjectKey(entry.Source.Key, "originals") || !concreteCOSVersion(entry.Source.VersionID) {
			t.Fatal("synthetic fixture receipt invalid")
		}
		expected["/"+entry.Source.Key] = entry.Source.VersionID
	}
	if len(expected) != 4 {
		t.Fatal("duplicate synthetic fixture")
	}
	original := s.archiveHTTP.Transport
	calls := 0
	s.archiveHTTP.Transport = cosTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodHead || r.URL.Scheme != "https" || r.URL.Host != "tellyouwhat-albums-1251995834.cos.ap-guangzhou.myqcloud.com" || r.URL.RawQuery != "" || expected[r.URL.Path] == "" {
			return nil, errors.New("read-only original observation rejected request")
		}
		calls++
		return original.RoundTrip(r)
	})
	for _, name := range names {
		source := receipt.Tests[name].Source
		version, err := s.ReconcileSeal(ctx, source.Key)
		if err != nil || version != source.VersionID {
			t.Fatalf("%s original HEAD did not match recorded fixed version: %v", name, err)
		}
	}
	if calls != 4 {
		t.Fatal("unexpected request count", calls)
	}
	roundtripJSON(t, root, "original-deletion-observation-evidence.json", map[string]any{"passed": true, "requests": calls, "method": "HEAD", "fixtures": names, "all_versions_match_saved_receipts": true, "media_content_downloaded": false, "mutations_allowed": false, "database_receipts_written": false})
}
