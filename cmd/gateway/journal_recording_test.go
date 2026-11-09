package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/attestation"
	"github.com/tellyouwhat/backend/internal/journal/voice"
)

type followingPrivacyCleaner struct {
	calls int
	err   error
}

func (c *followingPrivacyCleaner) DeletePrincipal(context.Context, attestation.Principal) error {
	c.calls++
	return c.err
}
func TestJournalRecordingPrivacyFencesBeforeFollowingCleanerAndPropagatesFailure(t *testing.T) {
	store, err := voice.NewRecordingJobStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	next := &followingPrivacyCleaner{err: errors.New("synthetic cache failure")}
	cleaner := &journalRecordingPrivacy{store: store, next: next}
	p := attestation.Principal{AppID: "journal", KeyID: "owner", DeviceID: "device"}
	if err = cleaner.DeletePrincipal(context.Background(), p); !errors.Is(err, next.err) {
		t.Fatal("cleanup failure hidden", err)
	}
	if _, err = store.Upload(context.Background(), "owner", uuid.NewString(), bytes.NewReader(nil)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owner not fenced before cache failure", err)
	}
	next.err = nil
	if err = cleaner.DeletePrincipal(context.Background(), p); err != nil || next.calls != 2 {
		t.Fatal("failed cleanup not retryable", next.calls, err)
	}
	p.AppID = "health"
	if err = cleaner.DeletePrincipal(context.Background(), p); err == nil || next.calls != 2 {
		t.Fatal("another app reached recording deletion", err)
	}
}
