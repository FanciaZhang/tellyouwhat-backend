//go:build albums_live

package albums

import (
	"errors"
	"net/http"
	"testing"
)

func TestLiveCOSOriginalAvailabilityReadOnly(t *testing.T) {
	s, root, ctx := liveRoundtripFixture(t)
	var receipt struct {
		Tests map[string]struct {
			Passed  bool         `json:"passed"`
			Source  SealedObject `json:"source"`
			Storage string       `json:"sourceStorageClass"`
		} `json:"tests"`
	}
	roundtripRead(t, "../../docs/evidence", "albums-low-frequency-2026-10-13.json", &receipt)
	names := []string{"jpeg", "oriented-jpeg", "transparent-png", "video"}
	expected := map[string]string{}
	for _, name := range names {
		entry, ok := receipt.Tests[name]
		if !ok || !entry.Passed || entry.Storage != "MAZ_STANDARD_IA" || !albumObjectKey(entry.Source.Key, "originals") || !concreteCOSVersion(entry.Source.VersionID) {
			t.Fatal("invalid synthetic receipt")
		}
		expected["/"+entry.Source.Key] = entry.Source.VersionID
	}
	if len(expected) != len(names) {
		t.Fatal("duplicate fixture")
	}
	original := s.archiveHTTP.Transport
	calls := 0
	s.archiveHTTP.Transport = cosTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodHead || r.URL.Scheme != "https" || r.URL.Host != s.host ||
			expected[r.URL.Path] == "" || r.URL.Query().Get("versionId") != expected[r.URL.Path] || len(r.URL.Query()) != 1 {
			return nil, errors.New("availability probe rejected non-scoped HEAD")
		}
		calls++
		return original.RoundTrip(r)
	})
	for _, name := range names {
		if err := s.CheckOriginalAvailability(ctx, receipt.Tests[name].Source); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if calls != len(names) {
		t.Fatal("unexpected request count", calls)
	}
	roundtripJSON(t, root, "original-availability-live.json", map[string]any{
		"passed": true, "head_requests": calls, "fixed_version_query": true, "fixtures": names,
		"storage_class": "MAZ_STANDARD_IA", "media_downloaded": false, "mutations": false,
		"scope": "COS adapter only; fixed previously byte-verified synthetic originals; not system-original deletion authority",
	})
}
