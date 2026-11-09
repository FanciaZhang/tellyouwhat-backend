package adminauth

import (
	"context"
	"testing"
	"time"
)

func TestMemoryCeremonyIsOneTimeAndExpires(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStateStore(func() time.Time { return now })
	state := CeremonyState{Kind: "login", CreatedAt: now}
	if err := store.PutCeremony(context.Background(), "one", state, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.TakeCeremony(context.Background(), "one"); err != nil || !found {
		t.Fatalf("first take = %v, %v", found, err)
	}
	if _, found, err := store.TakeCeremony(context.Background(), "one"); err != nil || found {
		t.Fatalf("second take = %v, %v", found, err)
	}

	if err := store.PutCeremony(context.Background(), "expired", state, time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, found, err := store.TakeCeremony(context.Background(), "expired"); err != nil || found {
		t.Fatalf("expired take = %v, %v", found, err)
	}
}

func TestMemorySessionExpiresAndCanBeDeleted(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStateStore(func() time.Time { return now })
	hash := TokenHash("session-token")
	if err := store.PutSession(context.Background(), hash, Session{UserID: "owner"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if session, found, err := store.GetSession(context.Background(), hash); err != nil || !found || session.UserID != "owner" {
		t.Fatalf("get session = %#v, %v, %v", session, found, err)
	}
	if err := store.DeleteSession(context.Background(), hash); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.GetSession(context.Background(), hash); found {
		t.Fatal("deleted session remained available")
	}
	if err := store.PutSession(context.Background(), hash, Session{}, time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, found, _ := store.GetSession(context.Background(), hash); found {
		t.Fatal("expired session remained available")
	}
}
