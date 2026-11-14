package voice

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/costcontrol"
	"golang.org/x/net/websocket"
)

type deniedSpeech struct {
	stage   string
	failure error
}

func (s deniedSpeech) Open(context.Context, []string) (SpeechConnection, error) {
	if s.stage == "open" {
		return nil, s.failure
	}
	return deniedSpeechConnection{s}, nil
}

type deniedSpeechConnection struct{ deniedSpeech }

func (s deniedSpeechConnection) Send([]byte, bool) error {
	if s.stage == "send" {
		return s.failure
	}
	return nil
}
func (s deniedSpeechConnection) Receive() (Transcript, error) { return Transcript{}, s.failure }
func (deniedSpeechConnection) Close() error                   { return nil }

func TestSpeechServiceFailuresPreserveBudgetProtectionAndConcurrency(t *testing.T) {
	cases := []struct {
		failure error
		code    string
	}{
		{costcontrol.ErrBudgetExceeded, "voice_service_budget_exhausted"},
		{costcontrol.ErrProtectionActive, "voice_service_protected"},
		{costcontrol.ErrPaused, "voice_service_protected"},
		{costcontrol.ErrConfigurationConflict, "voice_service_configuration_error"},
		{costcontrol.ErrConcurrencyExceeded, "voice_session_busy"},
	}
	for _, stage := range []string{"open", "send", "receive"} {
		for _, item := range cases {
			t.Run(stage+"/"+item.code+"/"+item.failure.Error(), func(t *testing.T) {
				failure := fmt.Errorf("wrapped admission: %w", item.failure)
				service := &Service{Store: NewMemoryStore(), Speech: deniedSpeech{stage, failure}, Secret: make([]byte, 32)}
				ws := voiceLifecycleSocket(t, service, Identity{Owner: uuid.NewString(), Anchor: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, uuid.NewString())
				defer ws.Close()
				if err := websocket.JSON.Send(ws, Frame{Type: "audio", RecognitionID: uuid.NewString(), SegmentID: uuid.NewString(), PCM: make([]byte, 6400)}); err != nil {
					t.Fatal(err)
				}
				var event Event
				if err := websocket.JSON.Receive(ws, &event); err != nil {
					t.Fatal(err)
				}
				if event.Code != item.code {
					t.Fatalf("%s failure became %s", stage, event.Code)
				}
				if rewriteClientCode(failure) != item.code {
					t.Fatal("rewrite lost the same service failure")
				}
			})
		}
	}
}

type deniedRewriter struct{ failure error }

func (m deniedRewriter) Rewrite(context.Context, Snapshot, int) (RewriteResult, error) {
	return RewriteResult{}, m.failure
}

func TestDictationBudgetFailureStopsStreamInsteadOfPolishRetry(t *testing.T) {
	service := &Service{Store: NewMemoryStore(), Model: deniedRewriter{costcontrol.ErrBudgetExceeded}, Secret: make([]byte, 32)}
	ws := voiceLifecycleSocket(t, service, Identity{Owner: uuid.NewString(), Anchor: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}, uuid.NewString())
	defer ws.Close()
	snapshot, _, _ := clarificationFixture()
	snapshot.DictationMode = true
	if err := websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot}); err != nil {
		t.Fatal(err)
	}
	for {
		var event Event
		if err := websocket.JSON.Receive(ws, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "rewrite_error" {
			t.Fatal("budget rejection remained an automatic polish retry")
		}
		if event.Type == "error" {
			if event.Code != "voice_service_budget_exhausted" {
				t.Fatal(event.Code)
			}
			return
		}
	}
}

type singleOrganizationJob struct {
	OrganizationStore
	job OrganizationJob
}

func (s *singleOrganizationJob) Get(_ context.Context, owner, id string) (OrganizationJob, error) {
	if owner != s.job.OwnerID || id != s.job.ID {
		return OrganizationJob{}, ErrOrganizationNotFound
	}
	return s.job, nil
}
func (s *singleOrganizationJob) Save(_ context.Context, job OrganizationJob, revision uint64) error {
	if revision != s.job.Revision {
		return ErrOrganizationConflict
	}
	s.job = job
	return nil
}

func TestDurableOrganizationBudgetFailureRequiresExplicitRetry(t *testing.T) {
	for _, stage := range []string{"admission", "model"} {
		for _, item := range []struct {
			failure     error
			state, code string
		}{
			{costcontrol.ErrBudgetExceeded, "failed", "voice_service_budget_exhausted"},
			{costcontrol.ErrProtectionActive, "failed", "voice_service_protected"},
			{costcontrol.ErrConfigurationConflict, "failed", "voice_service_configuration_error"},
			{costcontrol.ErrConcurrencyExceeded, "queued", ""},
		} {
			t.Run(stage+"/"+item.failure.Error(), func(t *testing.T) {
				now := time.Now()
				snapshot, _, _ := clarificationFixture()
				store := &singleOrganizationJob{job: OrganizationJob{ID: uuid.NewString(), OwnerID: "synthetic", State: "queued", Revision: 1,
					ExpiresAt: now.Add(time.Hour), AvailableAt: now,
					Plan: &OrganizationPlan{Version: OrganizationVersion, Sections: []Snapshot{snapshot}}}}
				service := OrganizationService{Store: store, Model: deniedRewriter{item.failure}, Now: func() time.Time { return now },
					Authorize: func(context.Context, string) bool { return true },
					Admit: func(context.Context, OrganizationJob, Snapshot) (func(RewriteResult, error), error) {
						if stage == "admission" {
							return nil, item.failure
						}
						return func(RewriteResult, error) {}, nil
					}}
				if err := service.Process(t.Context(), store.job.OwnerID, store.job.ID); err != nil {
					t.Fatal(err)
				}
				if store.job.State != item.state || !store.job.LeaseUntil.IsZero() {
					t.Fatal("failed budget admission remained automatic work", store.job.State)
				}
				if item.code != "" && store.job.Problem != item.code {
					t.Fatal(store.job.Problem)
				}
			})
		}
	}
}
