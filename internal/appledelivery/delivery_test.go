package appledelivery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Store, Candidate, []byte, time.Time) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	body := []byte("synthetic HTTP transport fixture; not a signed IPA")
	hash := sha256.Sum256(body)
	c := Candidate{BundleID: "cn.tellyouwhat.birthday.dev", Source: strings.Repeat("a", 40), Version: "1.0", Build: "2", Digest: hex.EncodeToString(hash[:]), Size: int64(len(body)), State: "verified-local-candidate"}
	return s, c, body, time.Now().UTC()
}

func request(t *testing.T, handler http.Handler, path, method string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "https://install.example"+path, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestPrivateManifestPageAndExpiry(t *testing.T) {
	s, c, body, now := fixture(t)
	r, err := s.Publish(c, bytes.NewReader(body), "https://install.example", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := NewHandler(s, "https://install.example")
	h.now = func() time.Time { return now.Add(time.Second) }
	u, _ := url.Parse(r.URL)
	page := request(t, h, u.Path, "GET")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "itms-services:") {
		t.Fatal(page.Code, page.Body.String())
	}
	if page.Header().Get("Cache-Control") != "no-store" || page.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("private headers missing")
	}
	manifestResponse := request(t, h, u.Path+"manifest.plist", "GET")
	if manifestResponse.Code != 200 || !strings.Contains(manifestResponse.Body.String(), c.BundleID) || !strings.Contains(manifestResponse.Body.String(), "https://install.example"+u.Path+"app.ipa") {
		t.Fatal("manifest does not bind package and identity")
	}
	for _, path := range []string{"/", "/d/", "/d/" + strings.Repeat("z", 43) + "/app.ipa", u.Path + "../candidate.json", u.Path + "app.ipa?x=1", u.Path + "%61pp.ipa"} {
		if got := request(t, h, path, "GET").Code; got != 404 {
			t.Fatalf("unsafe path accepted: %s status %d", path, got)
		}
	}
	for host, want := range map[string]int{"install.example": 200, "INSTALL.EXAMPLE": 200, "install.example:443": 200,
		"evil.example": 404, "install.example:80": 404, "install.example:444": 404} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", r.URL, nil)
		req.Host = host
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("host %s: status %d, want %d", host, w.Code, want)
		}
	}
	if request(t, h, u.Path, "POST").Code != 405 {
		t.Fatal("unexpected write API")
	}
	h.now = func() time.Time { return now.Add(time.Hour) }
	for _, suffix := range []string{"", "manifest.plist", "app.ipa"} {
		if request(t, h, u.Path+suffix, "GET").Code != 410 {
			t.Fatal("expired capability usable")
		}
	}
}

func TestPackageHeadRangeAndIntegrity(t *testing.T) {
	s, c, body, now := fixture(t)
	receipt, err := s.Publish(c, bytes.NewReader(body), "https://install.example", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := NewHandler(s, "https://install.example")
	server := httptest.NewServer(h)
	defer server.Close()
	u, _ := url.Parse(receipt.URL)
	get := func(method, rangeHeader string) (int, []byte, http.Header) {
		t.Helper()
		r, _ := http.NewRequest(method, server.URL+u.Path+"app.ipa", nil)
		r.Host = "install.example"
		r.Header.Set("Range", rangeHeader)
		resp, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b, resp.Header
	}
	status, b, headers := get("GET", "")
	if status != 200 || !bytes.Equal(b, body) || headers.Get("ETag") != `"`+c.Digest+`"` {
		t.Fatal("package mismatch")
	}
	status, b, _ = get("HEAD", "")
	if status != 200 || len(b) != 0 {
		t.Fatal("HEAD included data")
	}
	status, b, _ = get("GET", "bytes=2-6")
	if status != 206 || !bytes.Equal(b, body[2:7]) {
		t.Fatal("range mismatch")
	}
	if err := s.root.WriteFile("packages/"+c.Digest+".ipa", []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	status, _, _ = get("GET", "")
	if status != 503 {
		t.Fatal("corrupted package served")
	}
}

func TestPublishRejectsWrongIdentityHashOriginAndTTL(t *testing.T) {
	s, c, body, now := fixture(t)
	for _, origin := range []string{"http://install.example", "https://user:password@install.example", "https://install.example/", "https://install.example?x=1", "https://install.example:443", "https://install.example:", "https://INSTALL.example"} {
		if _, err := s.Publish(c, bytes.NewReader(body), origin, time.Hour, now); err == nil {
			t.Fatal("invalid origin accepted")
		}
	}
	for _, ttl := range []time.Duration{0, 30 * time.Second, MaxTTL + time.Second} {
		if _, err := s.Publish(c, bytes.NewReader(body), "https://install.example", ttl, now); err == nil {
			t.Fatal("invalid TTL accepted")
		}
	}
	wrong := c
	wrong.BundleID = "cn.tellyouwhat.birthday"
	if _, err := s.Publish(wrong, bytes.NewReader(body), "https://install.example", time.Hour, now); err == nil {
		t.Fatal("formal app accepted")
	}
	if _, err := s.Publish(c, strings.NewReader("wrong"), "https://install.example", time.Hour, now); err == nil {
		t.Fatal("wrong package accepted")
	}
	entries, _ := fs.ReadDir(s.root.FS(), "grants")
	if len(entries) != 0 {
		t.Fatal("invalid package received a lease")
	}
}

func TestCleanupProtectsActiveLeasesAndRejectsUnknownArtifacts(t *testing.T) {
	s, c, body, now := fixture(t)
	receipt, err := s.Publish(c, bytes.NewReader(body), "https://install.example", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	name := "packages/" + c.Digest + ".ipa"
	old := now.Add(-3 * time.Hour)
	if err := s.root.Chtimes(name, old, old); err != nil {
		t.Fatal(err)
	}
	result, err := s.Cleanup(now)
	if err != nil || result.Packages != 0 {
		t.Fatal("active package removed", result, err)
	}
	if err := s.root.WriteFile("packages/user-document.txt", []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cleanup(now.Add(2 * time.Hour)); err == nil {
		t.Fatal("unknown file ignored")
	}
	grants, _ := fs.ReadDir(s.root.FS(), "grants")
	if len(grants) != 1 {
		t.Fatal("cleanup mutated a partially validated inventory")
	}
	_ = s.root.Remove("packages/user-document.txt")
	result, err = s.Cleanup(now.Add(2 * time.Hour))
	if err != nil || result.Grants != 1 || result.Packages != 1 {
		t.Fatal(result, err)
	}
	h, _ := NewHandler(s, "https://install.example")
	u, _ := url.Parse(receipt.URL)
	if request(t, h, u.Path, "GET").Code != 404 {
		t.Fatal("revoked grant still available")
	}
}

func TestStoreDoesNotFollowSymlinksOrExposeRawCapability(t *testing.T) {
	s, c, body, now := fixture(t)
	r, err := s.Publish(c, bytes.NewReader(body), "https://install.example", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(r.URL)
	token := strings.Split(u.Path, "/")[2]
	entries, _ := fs.ReadDir(s.root.FS(), "grants")
	data, _ := s.root.ReadFile("grants/" + entries[0].Name())
	if bytes.Contains(data, []byte(token)) {
		t.Fatal("capability persisted in grant metadata")
	}
	outside := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(outside, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.root.Remove("packages/" + c.Digest + ".ipa"); err != nil {
		t.Fatal(err)
	}
	if err := s.root.Symlink(outside, "packages/"+c.Digest+".ipa"); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyPackage(c); err == nil {
		t.Fatal("symlink package accepted")
	}
	var g grant
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
}

func TestBoundedMetadataAndSharedPackageLeases(t *testing.T) {
	s, c, body, now := fixture(t)
	data, _ := json.Marshal(c)
	for _, input := range []string{string(data) + ` {}`, string(data) + strings.Repeat(" ", 16*1024), `{"unknown":true}`} {
		var candidate Candidate
		if err := decode(strings.NewReader(input), &candidate); err == nil {
			t.Fatal("invalid metadata accepted")
		}
	}
	if _, err := s.Publish(c, bytes.NewReader(body), "https://install.example", time.Hour, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(c, bytes.NewReader(body), "https://install.example", 3*time.Hour, now); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-3 * time.Hour)
	_ = s.root.Chtimes("packages/"+c.Digest+".ipa", old, old)
	result, err := s.Cleanup(now.Add(2 * time.Hour))
	if err != nil || result.Grants != 1 || result.Packages != 0 {
		t.Fatal("a second lease must protect the shared package", result, err)
	}
}

func TestConcurrentPublicationAndCapacity(t *testing.T) {
	s, c, body, now := fixture(t)
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := s.Publish(c, bytes.NewReader(body), "https://install.example", time.Hour, now)
			errors <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errors; err != nil {
			t.Error(err)
		}
	}
	if t.Failed() {
		return
	}
	entries, _ := fs.ReadDir(s.root.FS(), "grants")
	if len(entries) != 8 {
		t.Fatal("concurrent publications lost a lease")
	}
	// Sparse inventory verifies the quota without allocating gigabytes in the test.
	f, err := s.root.OpenFile("packages/"+strings.Repeat("b", 64)+".ipa", os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxStoreBytes); err != nil {
		t.Fatal(err)
	}
	f.Close()
	other := c
	other.Digest = strings.Repeat("c", 64)
	if _, err := s.Publish(other, bytes.NewReader(body), "https://install.example", time.Hour, now); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatal("storage capacity not enforced", err)
	}
}

func TestInitializedStoreCanReopenWithoutWriting(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "data")
	s, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, name := range []string{"packages", "grants", ""} {
		if err := os.Chmod(filepath.Join(directory, name), 0500); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(directory, ".lock"), 0400); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, name := range []string{"packages", "grants", ""} {
			_ = os.Chmod(filepath.Join(directory, name), 0700)
		}
	}()
	s, err = Open(directory)
	if err != nil {
		t.Fatal("read-only server cannot open initialized store", err)
	}
	s.Close()
}
