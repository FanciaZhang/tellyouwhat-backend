//go:build albums_live

package albums

import (
	"errors"
	"net/http"
	"testing"
)

// Only read metadata for the two archived components of an existing synthetic pair.
func TestLiveCOSLivePairDeletionObservationReadOnly(t *testing.T) {
	s, root, ctx := liveRoundtripFixture(t)
	var evidence struct {
		Pair struct {
			Passed     bool               `json:"passed"`
			ID         string             `json:"pairUploadID"`
			Owner      string             `json:"ownerID"`
			Descriptor LivePairDescriptor `json:"descriptor"`
			Objects    []SealedObject     `json:"objects"`
		} `json:"pair"`
	}
	roundtripRead(t, "../../docs/evidence", "albums-ci-live-roundtrip-2026-10-13.json", &evidence)
	pair := evidence.Pair
	if !pair.Passed || len(pair.Objects) != 2 || pair.Descriptor.validate() != nil {
		t.Fatal("successful synthetic pair receipt required")
	}
	u := LivePairUpload{ID: pair.ID, OwnerID: pair.Owner, Descriptor: pair.Descriptor, State: "queued", PhotoConsumed: true, MovieConsumed: true}
	expected := map[string]string{}
	for _, o := range pair.Objects {
		if u.validateReceipt(o) != nil {
			t.Fatal("invalid synthetic pair receipt")
		}
		expected["/"+o.Key] = o.VersionID
	}
	if len(expected) != 2 {
		t.Fatal("duplicate component receipt")
	}
	original := s.archiveHTTP.Transport
	calls := 0
	s.archiveHTTP.Transport = cosTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodHead || req.URL.Scheme != "https" || req.URL.Host != "tellyouwhat-albums-1251995834.cos.ap-guangzhou.myqcloud.com" || req.URL.RawQuery != "" || expected[req.URL.Path] == "" {
			return nil, errors.New("read-only pair observation rejected request")
		}
		calls++
		return original.RoundTrip(req)
	})
	for _, o := range pair.Objects {
		version, err := s.observeLivePairCopy(ctx, u, o.ResourceID)
		if err != nil || version != o.VersionID {
			t.Fatalf("%s archived version not confirmed: %v", o.ResourceID, err)
		}
	}
	if calls != 2 {
		t.Fatal("unexpected request count", calls)
	}
	roundtripJSON(t, root, "live-pair-deletion-observation-evidence.json", map[string]any{"passed": true, "requests": calls, "method": "HEAD", "all_versions_and_sizes_match_saved_receipts": true, "media_content_downloaded": false, "mutations_allowed": false, "database_receipts_written": false})
}
