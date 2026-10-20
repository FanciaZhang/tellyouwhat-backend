package readaloud

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMySQLReadAloudSessionsKeepPaidIdentityAcrossRestartsAndDevices(t *testing.T) {
	catalog, m, now := catalogTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v, _, err := VerifyVoice(ctx, draftVoice(), &catalogSynthesizer{}, now)
	if err != nil {
		t.Fatal(err)
	}
	v.Enabled = true
	probe := anotherMutation(m)
	if _, err := catalog.ClaimProbe(ctx, probe, v, now); err != nil {
		t.Fatal(err)
	}
	if err := catalog.FinishProbe(ctx, probe, v, true, now); err != nil {
		t.Fatal(err)
	}
	draft, err := catalog.Draft(ctx, CatalogDraft{DefaultVoiceID: v.ID, Voices: []VoiceConfiguration{Configuration(v)}}, anotherMutation(m), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Publish(ctx, CatalogPublication{Revision: draft.Version}, anotherMutation(m), now); err != nil {
		t.Fatal(err)
	}
	ledger := QuotaStore{DB: catalog.DB}
	provider := &sessionProvider{started: make(chan struct{}), release: make(chan struct{})}
	close(provider.release)
	service, err := NewSessionService(ctx, ledger, provider, catalog, func(context.Context, Subject, string) error { return nil }, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { service.Run(); close(done) }()
	defer func() { service.Close(); <-done }()
	grant := Grant{Subject: Subject{"journal", "purchase:shared"}, Purpose: "journal", Start: now.Add(-time.Hour), End: now.Add(time.Hour), Limit: 3}
	command := SessionCommand{Synthesis: validSynthesis(), VoiceRevision: v.policyHash(), Purpose: "journal", AccessKey: "device-one"}
	command.Text = "测试"
	if _, err := service.Create(ctx, grant, command); err != nil {
		t.Fatal(err)
	}
	waitSession(t, service, grant.Subject, command.RequestID, "succeeded")
	service.Close()
	restarted, err := NewSessionService(ctx, ledger, provider, catalog, func(context.Context, Subject, string) error { return nil }, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	command.AccessKey = "device-two"
	info, err := restarted.Create(ctx, grant, command)
	if err != nil || info.BufferAvailable || info.State != "succeeded" || provider.calls.Load() != 1 {
		t.Fatal("restart repeated paid synthesis", info, err)
	}
	command.RequestID = uuid.NewString()
	if _, err := restarted.Create(ctx, grant, command); !errors.Is(err, ErrQuotaExceeded) || provider.calls.Load() != 1 {
		t.Fatal("second device received a fresh allowance", err)
	}
	usage, err := ledger.Usage(ctx, grant, time.Now())
	if err != nil || usage.Used != 2 || usage.Reserved != 0 || usage.Remaining != 1 {
		t.Fatal("incorrect durable session accounting", usage, err)
	}
}
