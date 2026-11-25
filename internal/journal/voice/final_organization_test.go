package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tellyouwhat/backend/internal/journal/contracts"
	"github.com/tellyouwhat/backend/internal/promptconfig"
)

func TestFinalOrganizationSends46TurnsOnceAndKeepsRejectedGroupsLocal(t *testing.T) {
	turns := make([]string, 46)
	for i := range turns {
		turns[i] = fmt.Sprintf("第%d天，我们在河边观察到了白鹭。", i+1)
	}
	turns[0] = "周三去图书馆，记错了，周五去图书馆。我还没有还书。"
	snapshot := comparisonSnapshot(turns, true)
	left, right := snapshot, snapshot
	left.Polish, right.Polish = &PolishRequest{Narrator: snapshot.Narrator, Targets: snapshot.Polish.Targets[:23]},
		&PolishRequest{Narrator: snapshot.Narrator, Targets: snapshot.Polish.Targets[23:]}
	book := contracts.BookContext{ID: uuid.NewString(), Name: "沿河散步"}
	plan := OrganizationPlan{Version: FinalOrganizationVersion, Sections: []Snapshot{left, right},
		Metadata: &FinalOrganizationContext{SuggestLibrary: true, SuggestIllustration: true, Books: []contracts.BookContext{book}}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	model := syntheticFinalModel(t, &calls, func(req *http.Request, body map[string]json.RawMessage) string {
		if req.URL.Path != "/responses" {
			t.Error(req.URL.Path)
		}
		var raw string
		var input struct {
			Sources  []finalOrganizationSource
			Sections [][]finalOrganizationTarget
		}
		if json.Unmarshal(body["input"], &raw) != nil || json.Unmarshal([]byte(raw), &input) != nil || len(input.Sources) != 46 || len(input.Sections) != 2 {
			t.Error("the request did not include both sections and all 46 source turns")
		} else if input.Sources[0].Speaker == input.Sources[1].Speaker || input.Sources[1].Name != "妻子" {
			t.Error("speaker names or distinctions were removed")
		}
		groups := []finalOrganizationGroup{}
		for i, turn := range turns {
			if i == 0 {
				turn = "周五去图书馆。我还没有还书。"
			}
			if i == 2 {
				turn = "这是没有来源的输出。"
			}
			groups = append(groups, finalOrganizationGroup{i + 1, i + 1, []PolishParagraph{{Text: turn, Style: "body"}}})
		}
		result, _ := json.Marshal(map[string]any{"groups": groups, "title": "河边的白鹭", "alternativeTitles": []any{true, "河边观察"},
			"tags": "invalid optional field", "recommendedTags": []map[string]string{{"name": "白鹭", "type": "topic"}},
			"existingBookRecommendations": []map[string]string{{"bookID": "999", "reason": "不存在的书册"}, {"bookID": "1", "reason": "河边观察"}},
			"illustrationSuggestion": contracts.IllustrationSuggestion{Summary: "河边白鹭", Subject: "白鹭", Reason: "记录河边观察",
				SourceQuotes: []string{"我们在河边观察到了白鹭"}}})
		return string(result)
	})
	service, store, _ := syntheticFinalWorker(t, plan, model)
	if err := service.Process(t.Context(), store.job.OwnerID, store.job.ID); err != nil {
		t.Fatal(err)
	}
	status, err := service.Get(t.Context(), store.job.OwnerID, store.job.ID)
	if err != nil || calls.Load() != 1 || status.State != "succeeded" || status.Progress.TotalFragments != 0 || len(status.Progress.Results) != 2 {
		t.Fatalf("calls=%d state=%s sections=%d err=%v", calls.Load(), status.State, len(status.Progress.Results), err)
	}
	first := status.Progress.Results[0]
	if status.Problem != "voice_organization_preserved_source" || len(first.Targets) != 22 ||
		slices.ContainsFunc(first.Targets, func(target PolishTarget) bool { return target.ID == left.Polish.Targets[2].ID }) ||
		first.Paragraphs[0].Text != "周五去图书馆。我还没有还书。" {
		t.Fatal("the invalid group replaced its local source or the valid correction was rejected")
	}
	metadata := status.Progress.FinalMetadata
	if metadata == nil || metadata.Title != "河边的白鹭" || !slices.Equal(metadata.AlternativeTitles, []string{"河边观察"}) || len(metadata.Tags) != 0 ||
		len(metadata.RecommendedTags) != 1 || len(metadata.ExistingBookRecommendations) != 1 || metadata.ExistingBookRecommendations[0].BookID != book.ID ||
		metadata.IllustrationSuggestion == nil {
		t.Fatal("an invalid suggestion discarded valid fields or passed validation")
	}
	legacy := OrganizationPlan{Version: OrganizationVersion, Sections: plan.Sections}
	state := OrganizationState{}
	legacyCalls := 0
	for state.Section < len(legacy.Sections) && legacyCalls <= len(turns) {
		window, err := state.Work(legacy)
		if err != nil {
			t.Fatal(err)
		}
		if err := state.Advance(legacy, window, window.preservingSource()); err != nil {
			t.Fatal(err)
		}
		legacyCalls++
	}
	if legacyCalls <= int(calls.Load()) || state.Section != len(legacy.Sections) {
		t.Fatalf("legacy windows=%d final requests=%d", legacyCalls, calls.Load())
	}
	t.Logf("46 synthetic turns, legacy model windows=%d, whole-recording requests=%d", legacyCalls, calls.Load())
}

func TestFinalOrganizationRetriesOnlyDefiniteInvalidResults(t *testing.T) {
	plan := OrganizationPlan{Version: FinalOrganizationVersion, Sections: []Snapshot{comparisonSnapshot([]string{"下午去河边观察白鹭。"}, false)}, Metadata: &FinalOrganizationContext{}}
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			var calls atomic.Int32
			model := syntheticFinalModel(t, &calls, func(req *http.Request, _ map[string]json.RawMessage) string {
				return `{"groups":[]}`
			})
			if uncertain {
				model = finalRewriteFunc(func(context.Context, Snapshot, int) (RewriteResult, error) {
					calls.Add(1)
					return RewriteResult{Diagnostics: RewriteDiagnostics{Stage: "http_transport"}}, context.DeadlineExceeded
				})
			}
			service, store, now := syntheticFinalWorker(t, plan, model)
			for range 3 {
				if err := service.Process(t.Context(), store.job.OwnerID, store.job.ID); err != nil {
					t.Fatal(err)
				}
				*now = now.Add(2 * time.Second)
			}
			wantCalls, wantState := int32(2), "failed"
			if uncertain {
				wantCalls, wantState = 1, "uncertain"
			}
			if calls.Load() != wantCalls || store.job.State != wantState {
				t.Fatalf("calls=%d state=%s", calls.Load(), store.job.State)
			}
			if _, err := service.Retry(t.Context(), store.job.OwnerID, store.job.ID, store.job.Revision); err != ErrOrganizationConflict {
				t.Fatalf("final job could be resubmitted by client: %v", err)
			}
		})
	}
}

func syntheticFinalWorker(t *testing.T, plan OrganizationPlan, model Rewriter) (OrganizationService, *singleOrganizationJob, *time.Time) {
	t.Helper()
	now := time.Now()
	policy := promptconfig.Revision{Policy: promptconfig.Defaults("lite", "pro", "voice", 90)["journal"]}
	store := &singleOrganizationJob{job: OrganizationJob{ID: uuid.NewString(), OwnerID: "synthetic", State: "queued", Revision: 1,
		ExpiresAt: now.Add(time.Hour), AvailableAt: now, Plan: &plan, Policy: &policy}}
	service := OrganizationService{Store: store, Model: model, Now: func() time.Time { return now },
		Authorize: func(context.Context, string) bool { return true },
		Admit: func(context.Context, OrganizationJob, Snapshot) (func(RewriteResult, error), error) {
			return func(RewriteResult, error) {}, nil
		}}
	return service, store, &now
}

func syntheticFinalModel(t *testing.T, calls *atomic.Int32, respond func(*http.Request, map[string]json.RawMessage) string) Rewriter {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		var body map[string]json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		text, _ := json.Marshal(respond(req, body))
		_, _ = fmt.Fprintf(w, `{"status":"completed","usage":{"input_tokens":250,"output_tokens":200},"output":[{"content":[{"type":"output_text","text":%s}]}]}`, text)
	}))
	t.Cleanup(server.Close)
	return ArkRewriter{BaseURL: strings.TrimSuffix(server.URL, "/"), APIKey: "synthetic", Model: "voice", HTTP: server.Client()}
}

type finalRewriteFunc func(context.Context, Snapshot, int) (RewriteResult, error)

func (f finalRewriteFunc) Rewrite(ctx context.Context, snapshot Snapshot, attempt int) (RewriteResult, error) {
	return f(ctx, snapshot, attempt)
}
