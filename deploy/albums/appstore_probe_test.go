//go:build ignore

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProbePrivateFileRejectsPublicAndIndirectFiles(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "private")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := privateFile(path); err != nil || string(data) != "fixture" {
		t.Fatalf("private regular file rejected: %v", err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{link, directory, filepath.Join(directory, "missing")} {
		if _, err := privateFile(invalid); err == nil {
			t.Fatal("accepted non-regular file")
		}
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := privateFile(path); err == nil {
		t.Fatal("accepted publicly readable file")
	}
}

func TestProbeIdentityRequiresExplicitSeparateAppAndState(t *testing.T) {
	for _, test := range []struct {
		profile, bundle, root string
		id                    int64
	}{
		{"development", "cn.tellyouwhat.albums.dev", "albums-development", 6824089160},
		{"formal", "cn.tellyouwhat.albums", "albums-test", 6822133304},
	} {
		bundle, id, root, err := probeIdentity(test.profile)
		if err != nil || bundle != test.bundle || id != test.id || root != test.root {
			t.Fatal("incorrect probe identity", test.profile, err)
		}
	}
	if _, _, _, err := probeIdentity("production"); err == nil {
		t.Fatal("unknown identity must not default to formal")
	}
}

func TestProbeInvalidArgumentsStopBeforeCredentialsOrNetwork(t *testing.T) {
	before := os.Args
	t.Cleanup(func() { os.Args = before })
	for _, args := range [][]string{
		{"probe", "request"}, {"probe", "delete", "development", "sandbox-notification-valid"},
		{"probe", "request", "other-app", "sandbox-notification-valid"},
		{"probe", "request", "development", "../albums-test"},
		{"probe", "request", "development", "sandbox-notification-../escape"},
	} {
		os.Args = args
		if err := run(); err == nil {
			t.Fatal("invalid probe arguments accepted")
		}
	}
}
