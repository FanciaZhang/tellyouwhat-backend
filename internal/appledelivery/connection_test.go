package appledelivery

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func connectionFixture() []byte {
	data, _ := json.Marshal(map[string]any{"version": 1, "origin": "https://api-test.journal.tellyouwhat.cn",
		"serviceProtocol": "subscription-v2", "voiceProtocol": "journal-voice-v36", "token": strings.Repeat("s", 43)})
	return data
}

func TestJournalConnectionUsesSeparateEncryptedExpiringDownload(t *testing.T) {
	s, c, body, now := fixture(t)
	c.BundleID = "cn.tellyouwhat.journalapp.dev"
	install, err := s.Publish(c, bytes.NewReader(body), "https://install.example", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.JournalConnection(c, connectionFixture(), "https://install.example", MaxJournalConnectionTTL, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.URL, strings.Repeat("s", 43)) || strings.Contains(r.URL, "api-test.journal") || r.Source != c.Source || r.Digest != c.Digest {
		t.Fatal("connection link leaked plaintext or lost package binding")
	}
	u, _ := url.Parse(r.URL)
	h, _ := NewHandler(s, "https://install.example")
	h.now = func() time.Time { return now.Add(time.Second) }
	pageURL, _ := url.Parse(install.URL)
	page := request(t, h, pageURL.Path, "GET")
	if page.Code != 200 || strings.Contains(page.Body.String(), "connection.json") || strings.Contains(page.Body.String(), strings.Repeat("s", 43)) {
		t.Fatal("private connection appeared on the installation page")
	}
	// The HTTP service only receives a read-only mount of this operator-owned store.
	for _, name := range []string{".", "packages", "grants"} {
		if err := s.root.Chmod(name, 0500); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, name := range []string{".", "packages", "grants"} {
			_ = s.root.Chmod(name, 0700)
		}
	})
	got := request(t, h, u.Path, "GET")
	canonical, _ := normalizeConnection(connectionFixture())
	if got.Code != 200 || !bytes.Equal(got.Body.Bytes(), canonical) || got.Header().Get("Content-Type") != "application/json" ||
		got.Header().Get("Content-Disposition") != `attachment; filename="journal-development-connection.json"` ||
		got.Header().Get("Cache-Control") != "no-store" || got.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("private file transport or privacy headers failed")
	}
	head := request(t, h, u.Path, "HEAD")
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != got.Header().Get("Content-Length") {
		t.Fatal("invalid HEAD response")
	}
	for _, path := range []string{u.Path + "?download=1", strings.Replace(u.Path, "/connection.json", "/", 1),
		"/d/connection/" + strings.Repeat("A", 2049) + "/connection.json", "/d/connection/broken/connection.json"} {
		if request(t, h, path, "GET").Code != 404 {
			t.Fatal("malformed connection route accepted")
		}
	}
	for _, host := range []string{"INSTALL.EXAMPLE", "install.example:443", "other.example"} {
		req := httptest.NewRequest("GET", r.URL, nil)
		req.Host = host
		response := httptest.NewRecorder()
		h.ServeHTTP(response, req)
		want := 200
		if host == "other.example" {
			want = 404
		}
		if response.Code != want {
			t.Fatal("connection Host binding failed")
		}
	}
	parts := strings.Split(u.Path, "/")
	sealed := parts[3]
	if _, err := s.openConnection(sealed, "https://other.example", now); err == nil {
		t.Fatal("connection accepted on a different origin")
	}
	changed := []byte(sealed)
	if changed[40] == 'A' {
		changed[40] = 'B'
	} else {
		changed[40] = 'A'
	}
	if _, err := s.openConnection(string(changed), "https://install.example", now); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	h.now = func() time.Time { return now.Add(-time.Second) }
	if request(t, h, u.Path, "GET").Code != 404 {
		t.Fatal("future connection usable")
	}
	h.now = func() time.Time { return r.Expires }
	if request(t, h, u.Path, "GET").Code != 410 {
		t.Fatal("expired connection usable")
	}
}

func TestConnectionPublicationRejectsWrongScopeAndInvalidProfiles(t *testing.T) {
	s, c, body, now := fixture(t)
	if _, err := s.JournalConnection(c, connectionFixture(), "https://install.example", time.Minute, now); err == nil {
		t.Fatal("non-Journal candidate accepted")
	}
	c.BundleID = "cn.tellyouwhat.journalapp.dev"
	if _, err := s.JournalConnection(c, connectionFixture(), "https://install.example", time.Minute, now); err == nil {
		t.Fatal("unpublished package accepted")
	}
	if _, err := s.Publish(c, bytes.NewReader(body), "https://install.example", time.Hour, now); err != nil {
		t.Fatal(err)
	}
	for _, ttl := range []time.Duration{0, 59 * time.Second, MaxJournalConnectionTTL + time.Nanosecond} {
		if _, err := s.JournalConnection(c, connectionFixture(), "https://install.example", ttl, now); err == nil {
			t.Fatal("invalid private lease accepted")
		}
	}
	for _, origin := range []string{"http://install.example", "https://user@install.example", "https://install.example:443"} {
		if _, err := s.JournalConnection(c, connectionFixture(), origin, time.Minute, now); err == nil {
			t.Fatal("invalid origin accepted")
		}
	}
	for key, value := range map[string]any{"version": 2, "origin": "https://api.journal.tellyouwhat.cn", "serviceProtocol": "other", "voiceProtocol": "other", "token": "bad\r\nheader", "unknown": true} {
		var profile map[string]any
		_ = json.Unmarshal(connectionFixture(), &profile)
		profile[key] = value
		invalid, _ := json.Marshal(profile)
		if _, err := normalizeConnection(invalid); err == nil {
			t.Fatal("invalid private profile accepted", key)
		}
	}
	for _, data := range [][]byte{nil, append(connectionFixture(), []byte(" {}")...), bytes.Repeat([]byte(" "), 1025)} {
		if _, err := normalizeConnection(data); err == nil {
			t.Fatal("invalid private payload accepted")
		}
	}
	path := filepath.Join(t.TempDir(), "connection.json")
	if err := os.WriteFile(path, connectionFixture(), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadJournalConnection(path); err == nil {
		t.Fatal("public connection file accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadJournalConnection(path); err != nil {
		t.Fatal(err)
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadJournalConnection(link); err == nil {
		t.Fatal("symlink connection accepted")
	}
}

func TestConnectionKeyPersistsWithoutSavingProfileOrWeakeningCleanup(t *testing.T) {
	s, c, body, now := fixture(t)
	c.BundleID = "cn.tellyouwhat.journalapp.dev"
	if _, err := s.Publish(c, bytes.NewReader(body), "https://install.example", time.Hour, now); err != nil {
		t.Fatal(err)
	}
	r, err := s.JournalConnection(c, connectionFixture(), "https://install.example", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.root.ReadFile(connectionKeyName)
	if err != nil || len(key) != 32 {
		t.Fatal("transport key was not saved")
	}
	info, _ := s.root.Lstat(connectionKeyName)
	if info.Mode().Perm() != 0600 {
		t.Fatal("transport key permissions")
	}
	entries, _ := fs.ReadDir(s.root.FS(), ".")
	if len(entries) != 4 {
		t.Fatal("unexpected profile storage")
	}
	for _, directory := range []string{"packages", "grants"} {
		files, _ := fs.ReadDir(s.root.FS(), directory)
		for _, file := range files {
			data, _ := s.root.ReadFile(directory + "/" + file.Name())
			if bytes.Contains(data, []byte(strings.Repeat("s", 43))) {
				t.Fatal("plaintext private profile persisted")
			}
		}
	}
	result, err := s.Cleanup(now)
	if err != nil || result.Grants != 0 || result.Packages != 0 {
		t.Fatal("connection key broke ordinary cleanup", err)
	}
	u, _ := url.Parse(r.URL)
	token := strings.Split(u.Path, "/")[3]
	if _, err := s.openConnection(token, "https://install.example", now); err != nil {
		t.Fatal("connection key did not survive cleanup")
	}
	if err := s.root.WriteFile(connectionKeyName, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JournalConnection(c, connectionFixture(), "https://install.example", time.Minute, now); err == nil {
		t.Fatal("corrupt key silently replaced")
	}
	data, _ := s.root.ReadFile(connectionKeyName)
	if string(data) != "broken" {
		t.Fatal("corrupt key changed")
	}
	if err := s.root.Remove(connectionKeyName); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(outside, key, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.root.Symlink(outside, connectionKeyName); err != nil {
		t.Fatal(err)
	}
	if _, err := s.openConnection(token, "https://install.example", now); err == nil {
		t.Fatal("symlink key followed")
	}
}
