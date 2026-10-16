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
