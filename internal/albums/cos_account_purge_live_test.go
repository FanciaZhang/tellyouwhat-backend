//go:build albums_live

package albums

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type accountInventoryReadOnlyTransport struct {
	next       http.RoundTripper
	calls      int
	lastStatus int
	lastCode   string
}

func (p *accountInventoryReadOnlyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	q := r.URL.Query()
	if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "tellyouwhat-albums-1251995834.cos.ap-guangzhou.myqcloud.com" || r.URL.Path != "/" || (!q.Has("versions") && !q.Has("uploads")) {
		return nil, errors.New("read-only account inventory probe rejected request")
	}
	p.calls++
	response, err := p.next.RoundTrip(r)
	if response != nil {
		p.lastStatus = response.StatusCode
		if response.StatusCode != http.StatusOK && response.Body != nil {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
			response.Body.Close()
			response.Body = io.NopCloser(bytes.NewReader(data))
			var failure struct{ Code string }
			if readErr == nil && len(data) <= 4096 && xml.Unmarshal(data, &failure) == nil && failure.Code == "AccessDenied" {
				p.lastCode = "AccessDenied"
			}
		}
	}
	return response, err
}

// Only inventories owners from the committed synthetic IA fixture receipt.
// It never uploads, signs media URLs, downloads content, or invokes a purge.
func TestLiveCOSAccountDeletionInventoryReadOnly(t *testing.T) {
	s, root, ctx := liveRoundtripFixture(t)
	guard := &accountInventoryReadOnlyTransport{next: s.archiveHTTP.Transport}
	s.archiveHTTP.Transport = guard
	defer func() {
		roundtripJSON(t, root, "account-inventory-probe-status.json", map[string]any{"passed": !t.Failed(), "requests": guard.calls, "lastStatus": guard.lastStatus, "lastCode": guard.lastCode, "mutationsAllowed": false})
	}()
	var receipt struct {
		Tests map[string]struct {
			Passed  bool           `json:"passed"`
			Source  SealedObject   `json:"source"`
			Outputs []SealedObject `json:"outputs"`
		} `json:"tests"`
	}
	roundtripRead(t, "../../docs/evidence", "albums-low-frequency-2026-10-13.json", &receipt)
	var evidence []map[string]any
	for _, name := range []string{"jpeg", "oriented-jpeg", "transparent-png", "video"} {
		entry, ok := receipt.Tests[name]
		parts := strings.Split(entry.Source.Key, "/")
		if !ok || !entry.Passed || len(parts) != 5 || parts[0] != "albums" || parts[1] != "originals" || !validOwner(parts[2]) {
			t.Fatal("fixed synthetic receipt invalid")
		}
		owner := parts[2]
		found := map[string]bool{}
		counts := map[string]map[string]int{}
		for _, area := range accountPurgeAreas {
			versions, err := s.listAccountPurgeVersions(ctx, owner, area)
			if err != nil {
				t.Fatalf("%s %s versions: status=%d code=%s %v", name, area, guard.lastStatus, guard.lastCode, err)
			}
			uploads, err := s.listAccountPurgeUploads(ctx, owner, area)
			if err != nil {
				t.Fatalf("%s %s multipart: %v", name, area, err)
			}
			for _, v := range versions {
				if !v.Version.DeleteMarker {
					found[v.Target.Key+"\x00"+v.Version.VersionID] = true
				}
			}
			counts[area] = map[string]int{"versionsAndMarkers": len(versions), "multipartUploads": len(uploads)}
		}
		for _, expected := range append([]SealedObject{entry.Source}, entry.Outputs...) {
			if !found[expected.Key+"\x00"+expected.VersionID] {
				t.Fatal("previously verified fixture version absent from account inventory")
			}
		}
		evidence = append(evidence, map[string]any{"fixture": name, "counts": counts, "expectedVersionsFound": len(entry.Outputs) + 1})
	}
	if guard.calls != 48 {
		t.Fatal("unexpected read count", guard.calls)
	}
	roundtripJSON(t, root, "account-inventory-evidence.json", map[string]any{"passed": true, "scope": "read-only known synthetic fixture account prefixes; no media download, upload or deletion", "inventoryRequests": guard.calls, "fixtures": evidence})
}

func TestLiveCOSAccountMultipartInventoryReadOnly(t *testing.T) {
	s, root, ctx := liveRoundtripFixture(t)
	guard := &accountInventoryReadOnlyTransport{next: s.archiveHTTP.Transport}
	s.archiveHTTP.Transport = guard
	var receipt struct {
		Tests map[string]struct {
			Passed bool         `json:"passed"`
			Source SealedObject `json:"source"`
		} `json:"tests"`
	}
	roundtripRead(t, "../../docs/evidence", "albums-low-frequency-2026-10-13.json", &receipt)
	entry := receipt.Tests["jpeg"]
	parts := strings.Split(entry.Source.Key, "/")
	if !entry.Passed || len(parts) != 5 || parts[0] != "albums" || parts[1] != "originals" || !validOwner(parts[2]) {
		t.Fatal("fixed synthetic account invalid")
	}
	counts := map[string]int{}
	defer func() {
		roundtripJSON(t, root, "account-multipart-inventory-evidence.json", map[string]any{"passed": !t.Failed(), "scope": "read-only six prefixes of the synthetic JPEG fixture account; no mutations", "requests": guard.calls, "lastStatus": guard.lastStatus, "lastCode": guard.lastCode, "counts": counts})
	}()
	for _, area := range accountPurgeAreas {
		items, err := s.listAccountPurgeUploads(ctx, parts[2], area)
		if err != nil {
			t.Fatalf("%s multipart status=%d code=%s %v", area, guard.lastStatus, guard.lastCode, err)
		}
		counts[area] = len(items)
	}
}
