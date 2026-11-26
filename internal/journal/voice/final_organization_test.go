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
			Sections [][]finalOrganizationTarget
		}
		if json.Unmarshal(body["input"], &raw) != nil || json.Unmarshal([]byte(raw), &input) != nil ||
			len(input.Sections) != 2 || len(input.Sections[0]) != 3 || len(input.Sections[1]) != 3 ||
			len(input.Sections[0][0].Sources) != 10 || len(input.Sections[1][2].Sources) != 3 {
			t.Error("the request did not include both sections with predefined editing groups")
			return ""
		}
		first := input.Sections[0][0]
		if first.ID != "T1" || first.Sources[0].Speaker == first.Sources[1].Speaker || first.Sources[1].Name != "妻子" {
			t.Error("speaker names or distinctions were removed")
		}
		groups := []finalOrganizationGroup{}
		for _, section := range input.Sections {
			for _, target := range section {
				prose := []string{}
				for _, source := range target.Sources {
					if source.Text == turns[0] {
						source.Text = "周五去图书馆。我还没有还书。"
					}
					prose = append(prose, source.Text)
				}
				if target.ID == "T2" {
					prose = []string{"这是没有来源的输出。"}
				}
				groups = append(groups, finalOrganizationGroup{target.ID, []PolishParagraph{{Text: strings.Join(prose, ""), Style: "body"}}})
			}
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
	if status.Problem != "voice_organization_preserved_source" || len(first.Targets) != 13 ||
		slices.ContainsFunc(first.Targets, func(target PolishTarget) bool { return target.ID == left.Polish.Targets[10].ID }) ||
		!strings.HasPrefix(first.Paragraphs[0].Text, "周五去图书馆。我还没有还书。") {
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

func TestFinalOrganizationKeepsValidEditsWhenModelUsesSourceLabels(t *testing.T) {
	snapshot := comparisonSnapshot([]string{"早晨我们去河边观察白鹭。", "三只白鹭在浅滩上觅食。", "回程在图书馆还了画册。"}, false)
	first := snapshot.Polish.Targets[0]
	second := snapshot.Polish.Targets[1]
	first.Turns = append(first.Turns, second.Turns...)
	first.SourceIDs = append(first.SourceIDs, second.SourceIDs...)
	last := snapshot
	last.Polish = &PolishRequest{Targets: []PolishTarget{snapshot.Polish.Targets[2]}}
	snapshot.Polish.Targets = []PolishTarget{first}
	plan := OrganizationPlan{Version: FinalOrganizationVersion, Sections: []Snapshot{snapshot, last}, Metadata: &FinalOrganizationContext{}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	result, err := decodeFinalOrganization(`{"groups":[
		{"target":"S1","paragraphs":[{"text":"这是转写编号，不能当作正文目标。","style":"body"}]},
		{"target":"T2","paragraphs":[{"text":"回程在图书馆还了画册。","style":"body"}]}],
		"title":"清晨沿河散步"}`, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !result.PreservedSource || len(result.Revisions) != 1 || len(result.Revisions[0].Targets) != 1 ||
		result.Revisions[0].Targets[0].ID != last.Polish.Targets[0].ID || result.Metadata.Title != "清晨沿河散步" {
		t.Fatal("an invalid source reference replaced original text or discarded an independent edit")
	}
}

func TestFinalOrganizationKeepsSharedSourcesTogetherAndRewritesConversation(t *testing.T) {
	turns := []string{"周六准备去东码头。", "对呀。", "船票还没买，预算每人六十元。", "嗯。", "刚才没录上，我重新说一遍。",
		"海边风大，要带厚外套。", "倒也不是非得周六出发。"}
	snapshot := comparisonSnapshot(turns, true)
	targets := snapshot.Polish.Targets
	targets[5].Turns = append(targets[5].Turns, targets[2].Turns...)
	targets[5].SourceIDs = append(targets[5].SourceIDs, targets[2].SourceIDs...)
	plan := OrganizationPlan{Version: FinalOrganizationVersion, Sections: []Snapshot{snapshot}, Metadata: &FinalOrganizationContext{}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	model := syntheticFinalModel(t, &calls, func(_ *http.Request, body map[string]json.RawMessage) string {
		var raw string
		var input struct {
			Sections [][]finalOrganizationTarget
		}
		if json.Unmarshal(body["input"], &raw) != nil || json.Unmarshal([]byte(raw), &input) != nil ||
			len(input.Sections) != 1 || len(input.Sections[0]) != 1 || input.Sections[0][0].ID != "T1" ||
			len(input.Sections[0][0].Sources) != len(turns) {
			t.Error("related turns and isolated filler must share one editable unit")
		}
		return `{"groups":[
		  {"target":"T1","paragraphs":[{"text":"我们准备周六到东码头，船票还没买，预算每人六十元。海边风大，要带厚外套。","style":"body"},{"text":"不过也不是非得周六出发。","style":"body"}]}
		],"title":"周末去码头"}`
	})
	service, store, _ := syntheticFinalWorker(t, plan, model)
	if err := service.Process(t.Context(), store.job.OwnerID, store.job.ID); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || store.job.State != "succeeded" || store.job.Progress.PreservedSource || len(store.job.Progress.Results) != 1 {
		t.Fatalf("calls=%d state=%s preserved=%t", calls.Load(), store.job.State, store.job.Progress.PreservedSource)
	}
	result := store.job.Progress.Results[0]
	if len(result.Targets) != len(turns) || len(result.Paragraphs) != 2 || len(result.Paragraphs[0].TargetIDs) != len(turns) ||
		!strings.Contains(result.Paragraphs[1].Text, "不是非得周六") {
		t.Fatal("the body should cover every original target and retain the substantive qualification")
	}
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
