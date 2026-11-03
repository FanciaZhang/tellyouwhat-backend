package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tellyouwhat/backend/internal/promptconfig"
)

func TestIndependentPromptRetainsLastValidRevisionAndNeverFallsBackToDatabase(t *testing.T) {
	ctx, now := context.Background(), time.Now().UTC()
	file := filepath.Join(t.TempDir(), "prompt.json")
	cache := promptconfig.NewJournalCache(journalPromptFile{path: file})
	if cache.Refresh(ctx, now) == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := cache.Current("journal"); err == nil {
		t.Fatal("missing policy replaced by defaults")
	}
	revision := promptconfig.Revision{ID: "independent-test", Scope: "journal", CreatedAt: now, PublishedAt: &now,
		Policy: promptconfig.Defaults("lite", "pro", "voice", 90)["journal"]}
	save := func(r promptconfig.Revision) {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, raw, 0640); err != nil {
			t.Fatal(err)
		}
	}
	refresh := func() error { now = now.Add(promptconfig.RefreshInterval); return cache.Refresh(ctx, now) }
	save(revision)
	if err := refresh(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []promptconfig.Revision{
		{ID: "wrong-scope", Scope: "health", PublishedAt: &now, Policy: revision.Policy},
		{ID: "draft", Scope: "journal", Policy: revision.Policy},
		{ID: "invalid-policy", Scope: "journal", PublishedAt: &now},
	} {
		save(invalid)
		if refresh() == nil {
			t.Fatal("invalid revision accepted")
		}
		current, err := cache.Current("journal")
		if err != nil || current.ID != revision.ID {
			t.Fatal("invalid refresh lost the last good policy")
		}
	}
	if err := os.WriteFile(file, make([]byte, 512*1024+1), 0640); err != nil {
		t.Fatal(err)
	}
	if refresh() == nil {
		t.Fatal("oversized file accepted")
	}
	save(revision)
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if refresh() == nil {
		t.Fatal("publicly readable file accepted")
	}
	link := file + ".link"
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (journalPromptFile{path: link}).Published(ctx); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Chmod(file, 0640); err != nil {
		t.Fatal(err)
	}
	revision.ID = "next-independent-test"
	save(revision)
	if err := refresh(); err != nil {
		t.Fatal(err)
	}
	current, err := cache.Current("journal")
	if err != nil || current.ID != revision.ID {
		t.Fatal("valid revision did not replace the snapshot")
	}
}
