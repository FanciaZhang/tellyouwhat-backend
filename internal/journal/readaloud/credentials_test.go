package readaloud

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAloudCredentialsRejectPublicLinksOversizeAndHeaderInjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tts-key")
	for _, text := range []string{"", "key\r\nX-Injected: yes", strings.Repeat("a", 9000)} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewProviderFromFile(path); err != ErrCredentials {
			t.Fatal("unsafe key accepted", err)
		}
	}
	if err := os.WriteFile(path, []byte("fixture-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := NewProviderFromFile(path)
	if err != nil || p.endpoint != Endpoint || p.apiKey != "fixture-secret" {
		t.Fatal("private key not configured", err)
	}
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewProviderFromFile(link); err != ErrCredentials {
		t.Fatal("symlink admitted", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewProviderFromFile(path); err != ErrCredentials {
		t.Fatal("public key file admitted", err)
	}
}
