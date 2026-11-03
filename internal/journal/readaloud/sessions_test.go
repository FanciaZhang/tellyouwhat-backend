package readaloud

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type sessionLedger struct {
	mu      sync.Mutex
	rows    map[string]RequestRecord
	claims  int
	updated chan struct{}
}

func newSessionLedger() *sessionLedger {
	return &sessionLedger{rows: map[string]RequestRecord{}, updated: make(chan struct{}, 100)}
}
func (r *sessionLedger) Begin(ctx context.Context, g Grant, u UnitRequest, now time.Time) (RequestRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return RequestRecord{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.claims++
	if row, ok := r.rows[u.ID]; ok {
		if !sameRequest(row, g, u) {
			return row, true, ErrRequestConflict
		}
		return row, true, nil
	}
	if u.Characters > g.Limit {
		return RequestRecord{}, false, ErrQuotaExceeded
	}
	row := RequestRecord{UnitRequest: u, Subject: g.Subject, State: "reserved", CreatedAt: now, ExpiresAt: now.Add(2 * time.Minute)}
	r.rows[u.ID] = row
	return row, false, nil
}
func (r *sessionLedger) MarkRunning(ctx context.Context, s Subject, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[id]
	if !ok || row.Subject != s {
		return ErrRequestNotFound
	}
	row.State = "running"
	r.rows[id] = row
	return nil
}
func (r *sessionLedger) Finish(_ context.Context, s Subject, id, state string, actual int, known bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[id]
	if !ok || row.Subject != s {
		return ErrRequestNotFound
	}
	row.State = state
	row.ActualCharacters = actual
	row.UsageKnown = known
	r.rows[id] = row
	r.updated <- struct{}{}
	return nil
}
func (r *sessionLedger) Get(_ context.Context, s Subject, id string) (RequestRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[id]
	if !ok || row.Subject != s {
		return row, ErrRequestNotFound
	}
	return row, nil
}
func (r *sessionLedger) Usage(_ context.Context, g Grant, _ time.Time) (CharacterUsage, error) {
	return CharacterUsage{Limit: g.Limit, Remaining: g.Limit}, nil
}

type sessionCatalog struct{ value CatalogRevision }

func (c sessionCatalog) Current(context.Context) (CatalogRevision, error) { return c.value, nil }

type sessionProvider struct {
	calls        atomic.Int32
	started      chan struct{}
	release      chan struct{}
	once         sync.Once
	ignoreCancel bool
	textMu       sync.Mutex
	text         string
}

func (p *sessionProvider) Synthesize(ctx context.Context, input Synthesis, emit func([]byte) error) (SynthesisUsage, error) {
	p.calls.Add(1)
	p.textMu.Lock()
	p.text = input.Text
	p.textMu.Unlock()
	err := emit([]byte{1, 2})
	p.once.Do(func() { close(p.started) })
	if err != nil {
		return SynthesisUsage{}, err
	}
	if p.ignoreCancel {
		<-p.release
	} else {
		select {
		case <-ctx.Done():
			return SynthesisUsage{}, ctx.Err()
		case <-p.release:
		}
	}
	if err := emit([]byte{3, 4}); err != nil {
		return SynthesisUsage{}, err
	}
	return SynthesisUsage{Characters: 2, UsageKnown: true, AudioBytes: 4}, nil
}

type sessionFixture struct {
	s        *SessionService
	ledger   *sessionLedger
	provider *sessionProvider
	grant    Grant
	command  SessionCommand
	clock    atomic.Int64
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	f := &sessionFixture{ledger: newSessionLedger(), provider: &sessionProvider{started: make(chan struct{}), release: make(chan struct{})}}
	now := time.Now().UTC().Truncate(time.Microsecond)
	f.clock.Store(now.UnixNano())
	clock := func() time.Time { return time.Unix(0, f.clock.Load()) }
	v, _, err := VerifyVoice(context.Background(), draftVoice(), &catalogSynthesizer{}, now)
	if err != nil {
		t.Fatal(err)
	}
	v.Enabled = true
	value := sessionCatalog{value: CatalogRevision{Catalog: Catalog{Version: uuid.NewString(), DefaultVoiceID: v.ID, Voices: []Voice{v}}}}
	f.grant = quotaGrant(now)
	f.command = SessionCommand{Synthesis: validSynthesis(), Purpose: "journal", VoiceRevision: v.policyHash()}
	f.s, err = NewSessionService(context.Background(), f.ledger, f.provider, value, func(context.Context, Subject, string) error { return nil }, clock)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { f.s.Run(); close(done) }()
	t.Cleanup(func() {
		f.provider.once.Do(func() { close(f.provider.started) })
		select {
		case <-f.provider.release:
		default:
			close(f.provider.release)
		}
		f.s.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("session workers did not stop")
		}
	})
	return f
}
func waitSession(t *testing.T, s *SessionService, subject Subject, id, state string) SessionInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		info, _, changed, err := s.Read(ctx, subject, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if info.State == state {
			return info
		}
		select {
		case <-ctx.Done():
			t.Fatal("session did not reach state", state, info)
		case <-changed:
		}
	}
}
func TestReadAloudSessionsCreateOnceAndResumeImmutableOrderedFrames(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	failures := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := f.s.Create(ctx, f.grant, f.command); failures <- err }()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	<-f.provider.started
	if f.provider.calls.Load() != 1 || f.ledger.claims != 1 {
		t.Fatal("duplicate purchase", f.provider.calls.Load(), f.ledger.claims)
	}
	_, frames, _, err := f.s.Read(ctx, f.grant.Subject, f.command.RequestID, 0)
	if err != nil || len(frames) != 1 || frames[0].Sequence != 2 || frames[0].Offset != 0 {
		t.Fatal("bad initial frame", frames, err)
	}
	frames[0].PCM[0] = 99
	_, again, _, _ := f.s.Read(ctx, f.grant.Subject, f.command.RequestID, 0)
	if again[0].PCM[0] != 1 {
		t.Fatal("consumer corrupted protected audio")
	}
	close(f.provider.release)
	info := waitSession(t, f.s, f.grant.Subject, f.command.RequestID, "succeeded")
	if info.AudioBytes != 4 || info.LastSequence != 4 {
		t.Fatal(info)
	}
	_, tail, _, err := f.s.Read(ctx, f.grant.Subject, f.command.RequestID, 2)
	if err != nil || len(tail) != 1 || tail[0].Offset != 2 || !slices.Equal(tail[0].PCM, []byte{3, 4}) {
		t.Fatal("resume repeated or omitted PCM", tail, err)
	}
	if _, _, _, err := f.s.Read(ctx, f.grant.Subject, f.command.RequestID, 3); err != ErrInput {
		t.Fatal("invalid cursor accepted", err)
	}
}
func TestReadAloudSessionsStopWipesAudioAndDiscardsLateProvider(t *testing.T) {
	f := newSessionFixture(t)
	f.provider.ignoreCancel = true
	ctx := context.Background()
	if _, err := f.s.Create(ctx, f.grant, f.command); err != nil {
		t.Fatal(err)
	}
	<-f.provider.started
	f.s.mu.Lock()
	raw := f.s.sessions[f.command.RequestID].pcm
	f.s.mu.Unlock()
	if _, err := f.s.Cancel(ctx, f.grant.Subject, f.command.RequestID); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(raw, []byte{0, 0}) {
		t.Fatal("private PCM was not wiped")
	}
	close(f.provider.release)
	select {
	case <-f.ledger.updated:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled accounting did not settle")
	}
	if _, _, _, err := f.s.Read(ctx, f.grant.Subject, f.command.RequestID, 0); err != ErrAudioUnavailable {
		t.Fatal("late audio resurfaced", err)
	}
	replayed, err := f.s.Create(ctx, f.grant, f.command)
	if err != nil || replayed.BufferAvailable || f.provider.calls.Load() != 1 {
		t.Fatal("stop replay bought audio again", replayed, err)
	}
	other := f.grant.Subject
	other.ID = "other"
	if _, err := f.s.Get(ctx, other, f.command.RequestID); err != ErrRequestNotFound {
		t.Fatal("cross-owner metadata", err)
	}
}
func TestReadAloudSessionsBoundOwnersAndRetainDurableIdentityAfterExpiry(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	for range 2 {
		c := f.command
		c.RequestID = uuid.NewString()
		if _, err := f.s.Create(ctx, f.grant, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.Create(ctx, f.grant, f.command); err != ErrSessionBusy {
		t.Fatal("unbounded owner prefetch", err)
	}
	f.s.CancelSubject(f.grant.Subject)
	if _, err := f.s.Create(ctx, f.grant, f.command); err != nil {
		t.Fatal(err)
	}
	close(f.provider.release)
	waitSession(t, f.s, f.grant.Subject, f.command.RequestID, "succeeded")
	f.clock.Add(int64(4 * time.Minute))
	if _, _, _, err := f.s.Read(ctx, f.grant.Subject, f.command.RequestID, 0); err != ErrAudioUnavailable {
		t.Fatal("expired audio retained", err)
	}
	info, err := f.s.Get(ctx, f.grant.Subject, f.command.RequestID)
	if err != nil || info.BufferAvailable || info.State != "succeeded" {
		t.Fatal("durable outcome lost after expiry", info, err)
	}
	before := f.provider.calls.Load()
	if _, err := f.s.Create(ctx, f.grant, f.command); err != nil || f.provider.calls.Load() != before {
		t.Fatal("expired replay invoked provider", err)
	}
}
func TestReadAloudSessionsPreviewCannotCarryPrivateTextAndRestartCannotRepeat(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	command := f.command
	command.Purpose = "preview"
	grant := f.grant
	grant.Purpose = "preview"
	if _, err := f.s.Create(ctx, grant, command); err != ErrInput {
		t.Fatal("preview accepted caller text", err)
	}
	command.Text = ""
	if _, err := f.s.Create(ctx, grant, command); err != nil {
		t.Fatal(err)
	}
	<-f.provider.started
	close(f.provider.release)
	waitSession(t, f.s, grant.Subject, command.RequestID, "succeeded")
	f.provider.textMu.Lock()
	text := f.provider.text
	f.provider.textMu.Unlock()
	if text != draftVoice().ProbeText {
		t.Fatal("preview did not use server public sample")
	}
	f.s.Close()
	restarted, err := NewSessionService(ctx, f.ledger, f.provider, f.s.catalog, f.s.authorize, f.s.now)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	result, err := restarted.Create(ctx, grant, command)
	if err != nil || result.BufferAvailable || f.provider.calls.Load() != 1 {
		t.Fatal("restart repeated paid request", result, err)
	}
}
func TestReadAloudSessionsRejectChangedVoicesConsentAndQuotaBeforeProvider(t *testing.T) {
	for _, kind := range []string{"voice", "consent", "quota"} {
		t.Run(kind, func(t *testing.T) {
			f := newSessionFixture(t)
			command := f.command
			if kind == "voice" {
				command.VoiceRevision = "wrong"
			}
			if kind == "consent" {
				f.s.authorize = func(context.Context, Subject, string) error { return ErrInput }
			}
			if kind == "quota" {
				f.grant.Limit = 0
			}
			_, err := f.s.Create(context.Background(), f.grant, command)
			if err == nil || f.provider.calls.Load() != 0 {
				t.Fatal("unapproved synthesis started", kind, err)
			}
		})
	}
}

func TestReadAloudSessionsRecheckConsentBeforePaidCallAndReleaseUnusedQuota(t *testing.T) {
	f := newSessionFixture(t)
	var checks atomic.Int32
	f.s.authorize = func(context.Context, Subject, string) error {
		if checks.Add(1) > 1 {
			return ErrConsentRequired
		}
		return nil
	}
	if _, err := f.s.Create(context.Background(), f.grant, f.command); err != nil {
		t.Fatal(err)
	}
	info := waitSession(t, f.s, f.grant.Subject, f.command.RequestID, "failed")
	row, err := f.ledger.Get(context.Background(), f.grant.Subject, f.command.RequestID)
	if err != nil || f.provider.calls.Load() != 0 || !row.UsageKnown || row.ActualCharacters != 0 || info.Problem != "consent_required" {
		t.Fatal("revoked request consumed quota", row, info, err)
	}
}

func TestReadAloudSessionsCoalesceTinyChunksAndOnlyExposeCompleteSamples(t *testing.T) {
	f := newSessionFixture(t)
	ready := make(chan struct{})
	f.s.provider = synthesisFunc(func(ctx context.Context, _ Synthesis, emit func([]byte) error) (SynthesisUsage, error) {
		if err := emit([]byte{1}); err != nil {
			return SynthesisUsage{}, err
		}
		close(ready)
		select {
		case <-ctx.Done():
			return SynthesisUsage{}, ctx.Err()
		case <-f.provider.release:
		}
		for range 4097 {
			if err := emit([]byte{2}); err != nil {
				return SynthesisUsage{}, err
			}
		}
		return SynthesisUsage{Characters: 2, UsageKnown: true, AudioBytes: 4098}, nil
	})
	ctx := context.Background()
	if _, err := f.s.Create(ctx, f.grant, f.command); err != nil {
		t.Fatal(err)
	}
	<-ready
	info, frames, _, err := f.s.Read(ctx, f.grant.Subject, f.command.RequestID, 0)
	if err != nil || len(frames) != 0 || info.LastSequence != 0 {
		t.Fatal("half a PCM16 sample escaped", info, frames, err)
	}
	close(f.provider.release)
	waitSession(t, f.s, f.grant.Subject, f.command.RequestID, "succeeded")
	info, frames, _, err = f.s.Read(ctx, f.grant.Subject, f.command.RequestID, 0)
	if err != nil || len(frames) != 1 || frames[0].Sequence != 4098 || len(frames[0].PCM) != 4098 || info.LastSequence != 4098 {
		t.Fatal("upstream chunk count leaked into retained framing", info, len(frames), err)
	}
}

func TestReadAloudSessionsRejectOversizedProviderAudioWithoutRetainingIt(t *testing.T) {
	f := newSessionFixture(t)
	f.s.provider = synthesisFunc(func(_ context.Context, _ Synthesis, emit func([]byte) error) (SynthesisUsage, error) {
		return SynthesisUsage{}, emit(make([]byte, MaximumAudioBytes+2))
	})
	if _, err := f.s.Create(context.Background(), f.grant, f.command); err != nil {
		t.Fatal(err)
	}
	info := waitSession(t, f.s, f.grant.Subject, f.command.RequestID, "failed")
	if info.AudioBytes != 0 || info.LastSequence != 0 || f.s.bytes != 0 || info.Problem != "service_busy" {
		t.Fatal("oversized provider output retained", info)
	}
}

func TestReadAloudSessionsWithdrawOnlyTheOriginatingDeviceConsent(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	one := f.command
	one.AccessKey = "device-one"
	two := one
	two.RequestID = uuid.NewString()
	two.AccessKey = "device-two"
	if _, err := f.s.Create(ctx, f.grant, one); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Create(ctx, f.grant, two); err != nil {
		t.Fatal(err)
	}
	f.s.CancelAccessKey("device-one")
	if _, _, _, err := f.s.Read(ctx, f.grant.Subject, one.RequestID, 0); err != ErrAudioUnavailable {
		t.Fatal("revoked device retained audio", err)
	}
	if info, err := f.s.Get(ctx, f.grant.Subject, two.RequestID); err != nil || !info.BufferAvailable {
		t.Fatal("another device lost its independent session", info, err)
	}
}

func TestReadAloudSessionsRecheckVoiceBeforePaidCall(t *testing.T) {
	f := newSessionFixture(t)
	var checks atomic.Int32
	f.s.authorize = func(context.Context, Subject, string) error {
		if checks.Add(1) == 2 {
			catalog := f.s.catalog.(sessionCatalog)
			catalog.value.Voices = slices.Clone(catalog.value.Voices)
			catalog.value.Voices[0].Enabled = false
			f.s.catalog = catalog
		}
		return nil
	}
	if _, err := f.s.Create(context.Background(), f.grant, f.command); err != nil {
		t.Fatal(err)
	}
	info := waitSession(t, f.s, f.grant.Subject, f.command.RequestID, "failed")
	row, err := f.ledger.Get(context.Background(), f.grant.Subject, f.command.RequestID)
	if err != nil || f.provider.calls.Load() != 0 || !row.UsageKnown || row.ActualCharacters != 0 || info.Problem != "voice_changed" {
		t.Fatal("disabled voice started a paid call", row, info, err)
	}
}
