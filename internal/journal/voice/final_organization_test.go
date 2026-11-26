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

func TestFinalOrganizationRetriesAnIncompleteDraftBeforePublishing(t *testing.T) {
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
		Metadata: &FinalOrganizationContext{Title: "原整理标题", BodyContext: "原整理的正文。", SuggestLibrary: true,
			SuggestIllustration: true, Books: []contracts.BookContext{book}}}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var originalInput atomic.Value
	const firstDraftOnly = "初稿专用的文字。"
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
		if strings.Contains(raw, plan.Metadata.Title) || strings.Contains(raw, plan.Metadata.BodyContext) {
			t.Error("the existing entry title and body must stay out of the writing input")
		}
		if calls.Load() == 1 {
			originalInput.Store(raw)
		} else if originalInput.Load() != raw {
			t.Error("retry must submit the same recording input without prior draft material")
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
				if target.ID == "T2" && calls.Load() == 1 {
					prose = []string{"这是没有来源的输出。"}
				}
				if target.ID == "T1" && calls.Load() == 1 {
					prose = append(prose, firstDraftOnly)
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
	service, store, now := syntheticFinalWorker(t, plan, model)
	if err := service.Process(t.Context(), store.job.OwnerID, store.job.ID); err != nil {
		t.Fatal(err)
	}
	if job := store.job; job.State != "queued" || job.Progress.InvalidResultRetries != 1 || len(job.Progress.Results) != 0 ||
		job.Progress.FinalMetadata != nil {
		t.Fatal("an incomplete draft must be retried without publishing any text or suggestions")
	}
	progress, err := json.Marshal(store.job.Progress)
	if err != nil {
		t.Fatal(err)
	}
	store.job.Progress = OrganizationState{}
	if err := json.Unmarshal(progress, &store.job.Progress); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Second)
	if err := service.Process(t.Context(), store.job.OwnerID, store.job.ID); err != nil {
		t.Fatal(err)
	}
	status, err := service.Get(t.Context(), store.job.OwnerID, store.job.ID)
	if err != nil || calls.Load() != 2 || status.State != "succeeded" || status.Progress.TotalFragments != 0 || len(status.Progress.Results) != 2 {
		t.Fatalf("calls=%d state=%s sections=%d err=%v", calls.Load(), status.State, len(status.Progress.Results), err)
	}
	first := status.Progress.Results[0]
	if status.Problem != "" || len(first.Targets) != 23 || len(status.Progress.Results[1].Targets) != 23 ||
		!strings.HasPrefix(first.Paragraphs[0].Text, "周五去图书馆。我还没有还书。") ||
		strings.Contains(first.Paragraphs[0].Text, firstDraftOnly) {
		t.Fatal("the retried draft must replace every target and retain the confirmed correction")
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

func TestFinalOrganizationRejectsMissingAndDuplicateGroups(t *testing.T) {
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
	for _, badID := range []string{"S1", "T2"} {
		result, err := decodeFinalOrganization(fmt.Sprintf(`{"groups":[
			{"target":%q,"paragraphs":[{"text":"早晨去河边看白鹭，三只白鹭在浅滩上觅食。","style":"body"}]},
			{"target":"T2","paragraphs":[{"text":"回程在图书馆还了画册。","style":"body"}]}],
			"title":"清晨沿河散步"}`, badID), plan, false)
		if result != nil || polishValidationCode(err) != "final_output_missing_group" {
			t.Fatalf("an invalid source reference must not publish an incomplete draft: %v", err)
		}
	}
}

func TestFinalOrganizationOmitsFillerBetweenProtectedSections(t *testing.T) {
	snapshot := comparisonSnapshot([]string{"早晨去河边观察白鹭。", "嗯。对呀。刚才没录上。啥意思？我看一下。为啥？然后还有一些，我看看，我看。好像。", "回程在图书馆还了画册。"}, false)
	targets := snapshot.Polish.Targets
	var sections []Snapshot
	for _, target := range targets {
		section := snapshot
		section.Polish = &PolishRequest{Targets: []PolishTarget{target}, Narrator: snapshot.Narrator}
		sections = append(sections, section)
	}
	plan := OrganizationPlan{Version: FinalOrganizationVersion, Sections: sections, Metadata: &FinalOrganizationContext{}}
	result, err := decodeFinalOrganization(`{"groups":[
		{"target":"T1","paragraphs":[{"text":"早晨去河边观察白鹭。","style":"body"}]},
		{"target":"T2","paragraphs":[]},
		{"target":"T3","paragraphs":[{"text":"回程在图书馆还了画册。","style":"body"}]}]}`, plan, false)
	if err != nil || len(result.Revisions) != 3 || result.PreservedSource {
		t.Fatalf("the complete draft should omit an isolated filler block: %v", err)
	}
	blank := result.Revisions[1]
	if len(blank.Targets) != 1 || len(blank.Paragraphs) != 0 || !slices.Equal(blank.Omissions, []PolishOmission{{
		SourceID: targets[1].Turns[0].ID, Text: targets[1].Turns[0].Text, Reason: "speechDisfluency",
	}}) {
		t.Fatal("the original source must remain linked to the omitted speech")
	}
}

func TestFinalOrganizationChecksEvidenceWithinEachProtectedSection(t *testing.T) {
	turns := make([]string, 11)
	for i := range turns[:9] {
		turns[i] = "午后我们沿着溪岸散步。"
	}
	turns[9] = "我们约了周五在文化馆练习合唱。"
	turns[10] = "朋友提议周日把花盆搬到阳台。"
	snapshot := comparisonSnapshot(turns, false)
	plan := OrganizationPlan{Version: FinalOrganizationVersion, Sections: []Snapshot{snapshot}, Metadata: &FinalOrganizationContext{}}
	const draft = `{"groups":[
		{"target":"T1","paragraphs":[{"text":"午后我们沿着溪岸散步。","style":"body"}]},
		{"target":"T2","paragraphs":[{"text":"我们约了周五在文化馆练习合唱。朋友提议周日把花盆搬到阳台。","style":"body"}]}]}`
	result, err := decodeFinalOrganization(draft, plan, false)
	if err != nil || len(result.Revisions[0].Targets) != len(turns) {
		t.Fatalf("a fact expressed in a neighboring group in the same section must count: %v", err)
	}
	first, second := snapshot, snapshot
	first.Polish = &PolishRequest{Targets: snapshot.Polish.Targets[:10]}
	second.Polish = &PolishRequest{Targets: snapshot.Polish.Targets[10:]}
	plan.Sections = []Snapshot{first, second}
	result, err = decodeFinalOrganization(draft, plan, false)
	if result != nil || polishValidationCode(err) != "final_output_unanchored_group" {
		t.Fatal("success across a hand-edited boundary must not hide an omission")
	}
}

func TestFinalOrganizationCoalescesRepeatedParagraphsInCompletedDraft(t *testing.T) {
	turns := []string{
		"上次我们在图书馆找到了那份修复过的旧地图。",
		"我们还问了下次怎么预约借阅。",
		"嗯。", "嗯。", "嗯。", "嗯。", "嗯。", "嗯。", "嗯。", "嗯。",
		"那套地图是从旧馆搬过来的。",
	}
	snapshot := comparisonSnapshot(turns, false)
	plan := OrganizationPlan{Version: FinalOrganizationVersion, Sections: []Snapshot{snapshot}, Metadata: &FinalOrganizationContext{}}
	lastID := snapshot.Polish.Targets[10].ID
	const complete = "我们上次在图书馆找到了那份修复过的旧地图。那套地图是从旧馆搬过来的。我们还问了下次怎么预约借阅。"
	body := fmt.Sprintf(`{"groups":[
		{"target":"T1","paragraphs":[{"style":"body","text":%q}]},
		{"target":"T2","paragraphs":[{"style":"body","text":%q}]}]}`, complete, turns[0]+turns[10])
	result, err := decodeFinalOrganization(body, plan, true)
	if err != nil || result == nil || len(result.Revisions) != 1 {
		t.Fatalf("could not combine the recording draft: %v", err)
	}
	revision := result.Revisions[0]
	if len(revision.Paragraphs) != 1 || len(revision.Targets) != len(turns) ||
		revision.Paragraphs[0].Text != complete || len(revision.Paragraphs[0].TargetIDs) != len(turns) ||
		!slices.Contains(revision.Paragraphs[0].TargetIDs, lastID) {
		t.Fatal("repeated prose should appear once, with the new detail and all attached sources")
	}
	first, second := snapshot, snapshot
	first.Polish = &PolishRequest{Targets: snapshot.Polish.Targets[:10]}
	second.Polish = &PolishRequest{Targets: snapshot.Polish.Targets[10:]}
	plan.Sections = []Snapshot{first, second}
	result, err = decodeFinalOrganization(body, plan, true)
	if err != nil || result == nil || len(result.Revisions) != 2 ||
		len(result.Revisions[0].Paragraphs) != 1 || len(result.Revisions[1].Paragraphs) != 1 {
		t.Fatal("paragraphs separated by protected manuscript content must remain independent")
	}
}

func TestFinalOrganizationKeepsSimilarParagraphsWithDifferentFacts(t *testing.T) {
	const first = "今天我们去图书馆查一份历史地图，借阅票价是4.5元，可以用到周五。"
	paragraphs := []PolishParagraph{
		{Text: first, Style: "body"},
		{Text: strings.Replace(first, "4.5", "45", 1), Style: "body"},
		{Text: strings.Replace(first, "可以", "不可以", 1), Style: "body"},
	}
	if result := coalesceFinalParagraphs(paragraphs); len(result) != len(paragraphs) {
		t.Fatal("similar statements with different quantities or qualifications must remain")
	}
}

func TestFinalOrganizationAcceptsReviewedParaphraseButRejectsMissingNarrative(t *testing.T) {
	snapshot := comparisonSnapshot([]string{
		"天刚亮，我们就带着装备走到排练室签到。",
		"后来我们把五把椅子摆成一排，接着又试了灯光。",
		"外头非常冷。",
	}, false)
	plan := OrganizationPlan{Version: FinalOrganizationVersion, Sections: []Snapshot{snapshot}, Metadata: &FinalOrganizationContext{}}
	const paraphrase = `{"groups":[{"target":"T1","paragraphs":[{"style":"body","text":"天刚亮，我们就带着装备走到排练室签到。我们把五把椅子摆成一排，然后试了灯光。街上的风吹得人直打哆嗦。"}]}]}`
	result, err := decodeFinalOrganization(paraphrase, plan, false)
	if result != nil || polishValidationCode(err) != "final_output_unanchored_group" {
		t.Fatal("ambiguous source coverage should trigger a complete retry")
	}
	result, err = decodeFinalOrganization(paraphrase, plan, true)
	if err != nil || result == nil || len(result.Revisions[0].Targets) != 3 {
		t.Fatalf("the reviewed draft should allow a natural paraphrase: %v", err)
	}
	const incomplete = `{"groups":[{"target":"T1","paragraphs":[{"style":"body","text":"天刚亮，我们就带着装备走到排练室签到。"}]}]}`
	result, err = decodeFinalOrganization(incomplete, plan, true)
	if result != nil || err == nil {
		t.Fatal("a repair that drops most recording facts must still fail")
	}
}

func TestFinalOrganizationKeepsSharedSourcesTogetherAndRewritesConversation(t *testing.T) {
	turns := []string{"周六准备去东码头。", "对呀。", "船票还没买，预算每人六十元。", "嗯。", "没录上，我去。刚才没录上，我重新说一遍。",
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
		  {"target":"T1","paragraphs":[{"text":"嗯。哼哼哼。","style":"body"},{"text":"我们准备周六到东码头，船票还没买，预算每人六十元。海边风大，要带厚外套。","style":"body"},{"text":"之前有段话没录上，我重新说：不过也不是非得周六出发。","style":"body"}]}
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
		result.Paragraphs[1].Text != "不过也不是非得周六出发。" {
		t.Fatal("the body should cover every original target and retain the substantive qualification")
	}
}

func TestFinalOrganizationFailedRetryNeverPublishesPartialDraft(t *testing.T) {
	source := comparisonSnapshot([]string{"早晨去河边观察白鹭。", "回程在图书馆还了画册。"}, false)
	first, second := source, source
	first.Polish = &PolishRequest{Targets: source.Polish.Targets[:1]}
	second.Polish = &PolishRequest{Targets: source.Polish.Targets[1:]}
	plan := OrganizationPlan{Version: FinalOrganizationVersion, Sections: []Snapshot{first, second}, Metadata: &FinalOrganizationContext{}}
	var calls atomic.Int32
	model := syntheticFinalModel(t, &calls, func(_ *http.Request, _ map[string]json.RawMessage) string {
		return `{"groups":[{"target":"T1","paragraphs":[{"text":"早晨去河边观察白鹭。","style":"body"}]}]}`
	})
	service, store, now := syntheticFinalWorker(t, plan, model)
	for range 3 {
		if err := service.Process(t.Context(), store.job.OwnerID, store.job.ID); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(2 * time.Second)
	}
	job := store.job
	if calls.Load() != 2 || job.State != "failed" || job.Problem != "voice_rewrite_invalid_result" ||
		len(job.Progress.Results) > 0 || job.Progress.FinalMetadata != nil || job.Plan != nil {
		t.Fatal("an incomplete repair must leave the saved recording and manuscript untouched")
	}
}

func TestFinalOrganizationStripsRecordingRestartsWithoutDroppingNarrative(t *testing.T) {
	for _, prefix := range []string{"之前有段话没录上，我重新说：", "刚才有一段没录上，我重新说："} {
		const fact = "不过也不是非得周六出发。"
		if got := finalNarrativeSpeech(prefix + fact); got != fact {
			t.Fatalf("recording restart was retained: %q", got)
		}
	}
	const event = "这场排练没录上，朋友晚点再发我一份。"
	if got := finalNarrativeSpeech(event); got != event {
		t.Fatalf("a fact about another recording was removed: %q", got)
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
