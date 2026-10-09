package voice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/websocket"
)

func identityFixture(texts ...string) IdentityRequest {
	r := IdentityRequest{Fingerprint: strings.Repeat("a", 64), Turns: []SourceUtterance{}, Speakers: []IdentitySpeaker{}}
	for i, text := range texts {
		key := uuid.NewString() + ":voice"
		r.Speakers = append(r.Speakers, IdentitySpeaker{Key: key})
		r.Turns = append(r.Turns, SourceUtterance{ID: uuid.NewString(), Speaker: key, Text: text})
		if i == 0 {
			r.Speakers[0].PersonID = uuid.NewString()
			r.Speakers[0].Name = "我"
			r.NarratorPersonID = r.Speakers[0].PersonID
			r.Turns[0].PersonID, r.Turns[0].Person = r.NarratorPersonID, "我"
		}
	}
	return r
}

func TestUnchangedNarratorEchoWithEvidenceDoesNotCreateAnOperation(t *testing.T) {
	r := identityFixture("我和我老婆宝在河边散步，很开心。", "我和我老公宝也觉得今天很好。")
	body, _ := json.Marshal(map[string]any{"assignments": []IdentityAssignment{}, "narratorSourceID": r.Turns[0].ID,
		"narratorEvidenceIDs": []string{r.Turns[0].ID}, "commands": []IdentityCommand{}})
	got, err := decodeIdentity(string(body), r)
	if err != nil || got.NarratorSourceID != "" || len(got.NarratorEvidenceIDs) != 0 {
		t.Fatal("a default-author echo became a spoken operation", got, err)
	}
	r.Turns[0].Text = "这篇手记按我的视角写。今天去了河边。"
	body, _ = json.Marshal(map[string]any{"assignments": []IdentityAssignment{}, "narratorSourceID": r.Turns[0].ID,
		"narratorEvidenceIDs": []string{r.Turns[0].ID}, "commands": []IdentityCommand{{SourceID: r.Turns[0].ID, Text: "这篇手记按我的视角写。"}}})
	got, err = decodeIdentity(string(body), r)
	if err != nil || got.NarratorSourceID != r.Turns[0].ID || len(got.Commands) != 1 {
		t.Fatal("a real explicit author instruction was incorrectly removed", got, err)
	}
}

func TestUnreliableAcousticKeysAllowGroundedSourcesButRejectVoiceDefaults(t *testing.T) {
	r := identityFixture("我叫小林。", "今天去河边很开心。")
	key := r.Speakers[0].Key
	r.Speakers = []IdentitySpeaker{{Key: key}}
	r.NarratorPersonID = ""
	for i := range r.Turns {
		r.Turns[i].Speaker = key
		r.Turns[i].PersonID = ""
		r.Turns[i].Person = ""
	}
	r.UnreliableSpeakerKeys = []string{key}
	assignment := IdentityAssignment{SpeakerKey: key, Scope: "voice", SourceIDs: []string{r.Turns[0].ID, r.Turns[1].ID},
		Name: "小林", Kind: "introduction", EvidenceIDs: []string{r.Turns[0].ID}}
	encode := func() string {
		b, _ := json.Marshal(map[string]any{"assignments": []IdentityAssignment{assignment}, "narratorSourceID": "", "narratorEvidenceIDs": []string{}, "commands": []IdentityCommand{}})
		return string(b)
	}
	if _, err := decodeIdentity(encode(), r); err == nil {
		t.Fatal("unreliable acoustic key acquired a future voice default")
	}
	assignment.Scope = "sources"
	assignment.SourceIDs = []string{r.Turns[0].ID}
	if out, err := decodeIdentity(encode(), r); err != nil || len(out.Assignments) != 1 {
		t.Fatal("grounded introduction must still be usable", err)
	}
	r.UnreliableSpeakerKeys = []string{key, key}
	if r.Validate() == nil {
		t.Fatal("duplicate unreliable key accepted")
	}
	r.UnreliableSpeakerKeys = []string{"invented-key"}
	if r.Validate() == nil {
		t.Fatal("unknown unreliable key accepted")
	}
}

func TestManualSourceEchoCannotDiscardAnIndependentSpokenNarratorCommand(t *testing.T) {
	wife, author := uuid.NewString(), uuid.NewString()
	intro, packing, command := uuid.NewString(), uuid.NewString(), uuid.NewString()
	request := IdentityRequest{NarratorPersonID: author, Speakers: []IdentitySpeaker{{Key: "wife", Name: "老婆宝", PersonID: wife}, {Key: "author", Name: "老公宝", PersonID: author}},
		Turns: []SourceUtterance{{ID: intro, Speaker: "wife", Text: "我是老婆宝。", Person: "老婆宝", PersonID: wife},
			{ID: packing, Speaker: "wife", Text: "我把桂花糕装进蓝色袋子。", Person: "老婆宝", PersonID: wife},
			{ID: command, Speaker: "author", Text: "这篇日记请用老婆宝的视角来写。", Person: "老公宝", PersonID: author}}, ExplicitSourceIDs: []string{packing}}
	// A real model repeated the existing manual claim as kind=command, even
	// though it only had a UI assignment and no spoken identity command.
	raw, _ := json.Marshal(map[string]any{"assignments": []IdentityAssignment{{SpeakerKey: "wife", Scope: "sources", SourceIDs: []string{packing}, Name: "老婆宝", PersonID: wife, Kind: "command", EvidenceIDs: []string{packing}}},
		"narratorSourceID": intro, "narratorEvidenceIDs": []string{command}, "commands": []IdentityCommand{{SourceID: command, Text: request.Turns[2].Text}}})
	result, err := decodeIdentity(string(raw), request)
	if err != nil {
		t.Fatal("an unchanged manual source echo blocked the independently grounded narrator operation", err)
	}
	if len(result.Assignments) != 0 || result.NarratorSourceID != intro || len(result.Commands) != 1 {
		t.Fatal("echo must have no identity effect; the real spoken operation remains", result)
	}
	request.Turns[1].PersonID = author
	if _, err := decodeIdentity(string(raw), request); err == nil {
		t.Fatal("a conflicting identity is not an unchanged echo")
	}
}

func TestIdentityRejectsConflictingVoiceAndIncompleteAuthorOperation(t *testing.T) {
	r := identityFixture("我和老婆宝沿着河边散步。", "我是老婆宝。", "我是小林，从杭州坐火车过来。", "这篇日记请用老婆宝的视角写。今天三个人一起散步。")
	// This reproduces a real ASR collision without inventing another voice.
	r.Turns[2].Speaker = r.Speakers[0].Key
	r.Speakers = r.Speakers[:2]
	r.Turns[3].Speaker = r.Speakers[0].Key
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	assignments := []IdentityAssignment{
		{SpeakerKey: r.Speakers[0].Key, Scope: "voice", SourceIDs: []string{r.Turns[0].ID, r.Turns[2].ID, r.Turns[3].ID}, Name: "老婆宝", Kind: "context", EvidenceIDs: []string{r.Turns[0].ID}},
		{SpeakerKey: r.Speakers[0].Key, Scope: "sources", SourceIDs: []string{r.Turns[2].ID}, Name: "小林", Kind: "introduction", EvidenceIDs: []string{r.Turns[2].ID}},
	}
	encode := func(assignments []IdentityAssignment, commands []IdentityCommand) string {
		body, _ := json.Marshal(map[string]any{"assignments": assignments, "narratorSourceID": r.Turns[1].ID,
			"narratorEvidenceIDs": []string{r.Turns[3].ID}, "commands": commands})
		return string(body)
	}
	command := IdentityCommand{SourceID: r.Turns[3].ID, Text: "这篇日记请用老婆宝的视角写。"}
	if _, err := decodeIdentity(encode(assignments, []IdentityCommand{command}), r); err == nil {
		t.Fatal("one voice was assigned two contradictory people")
	}
	if _, err := decodeIdentity(encode([]IdentityAssignment{}, []IdentityCommand{}), r); err == nil {
		t.Fatal("author operation lost its exact removable source span")
	}
	if got, err := decodeIdentity(encode([]IdentityAssignment{}, []IdentityCommand{command}), r); err != nil || got.NarratorSourceID != r.Turns[1].ID {
		t.Fatal("an independent grounded author operation should remain available", got, err)
	}
}

type identityThenPolishRewriter struct{ failIdentity bool }

func TestSourceIdentitySeparatesPeopleSharingOneAcousticVoice(t *testing.T) {
	r := identityFixture("我和我老婆宝今天沿河散步。", "对，我和我老公宝出来很开心。", "我是小林，从杭州坐火车过来。", "我买桂花糕排了二十分钟。")
	r.Turns[2].Speaker = r.Speakers[0].Key
	r.Turns[3].Speaker = r.Speakers[0].Key
	r.Speakers = r.Speakers[:2]
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	assignments := []IdentityAssignment{
		{SpeakerKey: r.Speakers[0].Key, Scope: "sources", SourceIDs: []string{r.Turns[0].ID}, Name: "老公宝", Kind: "context", EvidenceIDs: []string{r.Turns[0].ID, r.Turns[1].ID}},
		{SpeakerKey: r.Speakers[0].Key, Scope: "sources", SourceIDs: []string{r.Turns[2].ID, r.Turns[3].ID}, Name: "小林", Kind: "introduction", EvidenceIDs: []string{r.Turns[2].ID}},
	}
	encode := func() string {
		b, _ := json.Marshal(map[string]any{"assignments": assignments, "narratorSourceID": "", "narratorEvidenceIDs": []string{}, "commands": []IdentityCommand{}})
		return string(b)
	}
	if got, err := decodeIdentity(encode(), r); err != nil || len(got.Assignments) != 2 {
		t.Fatal("grounded source identities must coexist", got, err)
	}
	r.ExplicitSourceIDs = []string{r.Turns[3].ID}
	r.Turns[3].Person = "老公宝"
	r.Turns[3].PersonID = r.Speakers[0].PersonID
	if _, err := decodeIdentity(encode(), r); err == nil {
		t.Fatal("automatic source assignment overwrote a manual claim")
	}
	r.ExplicitSourceIDs = nil
	assignments[1].Scope = "voice"
	if _, err := decodeIdentity(encode(), r); err == nil {
		t.Fatal("partial evidence renamed a whole merged voice")
	}
	assignments[1].Scope = "sources"
	assignments[1].SourceIDs = append(assignments[1].SourceIDs, r.Turns[0].ID)
	if _, err := decodeIdentity(encode(), r); err == nil {
		t.Fatal("two people claimed the same source")
	}
}

func TestNarratorSourceSelectsDifferentPeopleWithTheSameAcousticKey(t *testing.T) {
	r := identityFixture("今天和小林一起散步。", "我是小林，从杭州过来。", "这篇请以小林的视角写。")
	r.Turns[1].Speaker, r.Turns[2].Speaker = r.Speakers[0].Key, r.Speakers[0].Key
	r.Speakers = r.Speakers[:1]
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	assignment := IdentityAssignment{SpeakerKey: r.Speakers[0].Key, Scope: "sources", SourceIDs: []string{r.Turns[1].ID}, Name: "小林", Kind: "introduction", EvidenceIDs: []string{r.Turns[1].ID}}
	command := IdentityCommand{SourceID: r.Turns[2].ID, Text: r.Turns[2].Text}
	encode := func(sourceID string, commands []IdentityCommand) string {
		body, _ := json.Marshal(map[string]any{"assignments": []IdentityAssignment{assignment}, "narratorSourceID": sourceID,
			"narratorEvidenceIDs": []string{r.Turns[2].ID}, "commands": commands})
		return string(body)
	}
	if result, err := decodeIdentity(encode(r.Turns[1].ID, []IdentityCommand{command}), r); err != nil || result.NarratorSourceID != r.Turns[1].ID {
		t.Fatal("source-selected author must not fall back to the first person of this voice", result, err)
	}
	for _, invalid := range []string{r.Speakers[0].Key, uuid.NewString()} {
		if _, err := decodeIdentity(encode(invalid, []IdentityCommand{command}), r); err == nil {
			t.Fatal("accepted an invented author source")
		}
	}
	if _, err := decodeIdentity(encode(r.Turns[1].ID, []IdentityCommand{}), r); err == nil {
		t.Fatal("author change lost its exact command")
	}
}

func TestKnownIdentityEchoCannotBlockGroundedNewIntroduction(t *testing.T) {
	r := identityFixture("今天在河边散步很舒服。", "我叫小林，天气很好。")
	a := []IdentityAssignment{
		{SpeakerKey: r.Speakers[0].Key, Scope: "voice", SourceIDs: []string{r.Turns[0].ID}, Name: "我", PersonID: r.NarratorPersonID, Kind: "context", EvidenceIDs: []string{r.Turns[0].ID}},
		{SpeakerKey: r.Speakers[1].Key, Scope: "voice", SourceIDs: []string{r.Turns[1].ID}, Name: "小林", Kind: "introduction", EvidenceIDs: []string{r.Turns[1].ID}},
	}
	encode := func() string {
		data, _ := json.Marshal(map[string]any{"assignments": a, "narratorSourceID": "", "narratorEvidenceIDs": []string{}, "commands": []IdentityCommand{}})
		return string(data)
	}
	if result, err := decodeIdentity(encode(), r); err != nil || len(result.Assignments) != 1 || result.Assignments[0].Name != "小林" {
		t.Fatal("known no-op blocked grounded introduction", result, err)
	}
	a[0].Name = "小周"
	if _, err := decodeIdentity(encode(), r); err == nil {
		t.Fatal("an actual rename still requires literal evidence")
	}
	a[0].Name = "我"
	a[0].SourceIDs = []string{uuid.NewString()}
	if _, err := decodeIdentity(encode(), r); err == nil {
		t.Fatal("a no-op cannot carry fabricated scope")
	}
}

func (m identityThenPolishRewriter) Rewrite(_ context.Context, s Snapshot, _ int) (RewriteResult, error) {
	if s.Identity != nil {
		if m.failIdentity {
			return RewriteResult{}, ErrInvalid
		}
		return RewriteResult{Identity: &IdentityRevision{Request: *s.Identity, Assignments: []IdentityAssignment{}, NarratorEvidenceIDs: []string{}, Commands: []IdentityCommand{}}}, nil
	}
	return RewriteResult{Polish: &PolishRevision{Targets: s.Polish.Targets, Paragraphs: []PolishParagraph{{TargetIDs: []string{s.Polish.Targets[0].ID}, Text: "今天去了河边。", Style: "body"}}, Questions: []string{}}}, nil
}

func TestIdentityAcknowledgementAdvancesToProseAndFinishes(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "identity-failed-but-prose-continues"}[failed], func(t *testing.T) {
			ws := polishSocket(t, identityThenPolishRewriter{failIdentity: failed}, nil)
			snapshot := polishFixture()
			prose := snapshot.Polish
			snapshot.Polish = nil
			identity := identityFixture("今天去了河边。")
			snapshot.Identity = &identity
			if err := websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot}); err != nil {
				t.Fatal(err)
			}
			var event Event
			if err := receiveVoiceResult(ws, &event); err != nil {
				t.Fatal(err)
			}
			want := "identity"
			if failed {
				want = "identity_error"
			}
			if event.Type != want || event.Identity == nil || event.Identity.Request.Fingerprint != identity.Fingerprint {
				t.Fatalf("identity response: %+v", event)
			}
			snapshot.Identity = nil
			snapshot.Polish = prose
			websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
			if err := receiveVoiceResult(ws, &event); err != nil || event.Type != "polish" {
				t.Fatalf("prose after identity: %+v %v", event, err)
			}
			snapshot.AcknowledgedPolishID = event.Polish.ID
			snapshot.Polish.Targets = nil
			websocket.JSON.Send(ws, Frame{Type: "snapshot", Snapshot: &snapshot})
			websocket.JSON.Send(ws, Frame{Type: "finish"})
			ws.SetDeadline(time.Now().Add(3 * time.Second))
			if err := receiveVoiceResult(ws, &event); err != nil || event.Type != "finished" {
				t.Fatalf("finished after prose acknowledgement: %+v %v", event, err)
			}
		})
	}
}

func TestIdentityPreservesExactNamesAndRejectsUnbackedReferences(t *testing.T) {
	r := identityFixture("我和我老婆宝旅游很开心。", "我跟我老公宝出去玩很合拍。")
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	valid := IdentityAssignment{SpeakerKey: r.Speakers[1].Key, Scope: "voice", SourceIDs: []string{r.Turns[1].ID}, Name: "老婆宝", Kind: "context", EvidenceIDs: []string{r.Turns[0].ID, r.Turns[1].ID}}
	encode := func(a IdentityAssignment) string {
		b, _ := json.Marshal(map[string]any{"assignments": []IdentityAssignment{a}, "narratorSourceID": "", "narratorEvidenceIDs": []string{}, "commands": []IdentityCommand{}})
		return string(b)
	}
	out, err := decodeIdentity(encode(valid), r)
	if err != nil || out.Assignments[0].Name != "老婆宝" {
		t.Fatalf("exact name: %v", err)
	}
	for _, change := range []func(*IdentityAssignment){
		func(a *IdentityAssignment) { a.Name = "小林" },
		func(a *IdentityAssignment) { a.SpeakerKey = "unknown" },
		func(a *IdentityAssignment) { a.PersonID = uuid.NewString() },
		func(a *IdentityAssignment) { a.EvidenceIDs = []string{uuid.NewString()} },
		func(a *IdentityAssignment) { a.SourceIDs = nil },
		func(a *IdentityAssignment) { a.SourceIDs = []string{r.Turns[0].ID} },
		func(a *IdentityAssignment) { a.SourceIDs = []string{r.Turns[1].ID, r.Turns[1].ID} },
		func(a *IdentityAssignment) { a.Scope = "unknown" },
	} {
		a := valid
		change(&a)
		if _, err := decodeIdentity(encode(a), r); err == nil {
			t.Fatal("accepted unbacked identity")
		}
	}
	r.Speakers[1].Explicit = true
	r.Speakers[1].Name = "小林"
	if _, err := decodeIdentity(encode(valid), r); err == nil {
		t.Fatal("inference overrode explicit name")
	}
	valid.Kind = "command"
	if _, err := decodeIdentity(encode(valid), r); err == nil {
		t.Fatal("a command kind without a spoken operation overrode explicit identity")
	}
	r.Turns[1].Text = "把这个声音叫老婆宝。今天去公园。"
	valid.EvidenceIDs = []string{r.Turns[1].ID}
	operation := IdentityCommand{SourceID: r.Turns[1].ID, Text: "把这个声音叫老婆宝。"}
	rename, _ := json.Marshal(map[string]any{"assignments": []IdentityAssignment{valid}, "narratorSourceID": "", "narratorEvidenceIDs": []string{}, "commands": []IdentityCommand{operation}})
	if _, err := decodeIdentity(string(rename), r); err != nil {
		t.Fatal("actual explicit rename should be allowed", err)
	}
	echo, _ := json.Marshal(map[string]any{"assignments": []IdentityAssignment{}, "narratorSourceID": r.Turns[0].ID, "narratorEvidenceIDs": []string{}, "commands": []IdentityCommand{}})
	if result, err := decodeIdentity(string(echo), r); err != nil || result.NarratorSourceID != "" {
		t.Fatal("unchanged echo must be a no-op", err)
	}
	changed, _ := json.Marshal(map[string]any{"assignments": []IdentityAssignment{}, "narratorSourceID": r.Turns[1].ID, "narratorEvidenceIDs": []string{}, "commands": []IdentityCommand{}})
	if _, err := decodeIdentity(string(changed), r); err == nil {
		t.Fatal("changed narrator requires source evidence")
	}
	r.Turns[1].Text = "这篇手记按我的视角写。今天去公园。"
	operation.Text = "这篇手记按我的视角写。"
	changeNarrator := func(commands []IdentityCommand) string {
		b, _ := json.Marshal(map[string]any{"assignments": []IdentityAssignment{}, "narratorSourceID": r.Turns[1].ID, "narratorEvidenceIDs": []string{r.Turns[1].ID}, "commands": commands})
		return string(b)
	}
	if _, err := decodeIdentity(changeNarrator([]IdentityCommand{}), r); err == nil {
		t.Fatal("narrator changed without a spoken operation")
	}
	if _, err := decodeIdentity(changeNarrator([]IdentityCommand{operation}), r); err != nil {
		t.Fatal("actual explicit narrator change should be allowed", err)
	}
}

// Synthetic text is only the first integration layer. App capture acceptance
// separately exercises actual ASR, application, audio, persistence and UI.
func TestIdentityWithConfiguredAI(t *testing.T) {
	path := os.Getenv("JOURNAL_IDENTITY_INTEGRATION_CONFIG")
	if path == "" {
		t.Skip("Explicit configured-provider identity integration only")
	}
	var config struct{ BaseURL, APIKey, Model, OutputDirectory string }
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read private integration configuration")
	}
	if json.Unmarshal(b, &config) != nil || config.APIKey == "" || config.BaseURL == "" || config.OutputDirectory == "" {
		t.Fatal("invalid private integration configuration")
	}
	model := ArkRewriter{BaseURL: config.BaseURL, APIKey: config.APIKey, Model: config.Model}
	cases := []struct {
		name        string
		texts       []string
		expected    map[int]string
		empty       bool
		narrator    int
		sharedVoice bool
	}{
		{"reciprocal-affection", []string{"我跟我老婆宝在杭州旅游，非常非常开心。", "对，我跟我老公宝确实很开心，我们两个人出去玩就很合拍。"}, map[int]string{0: "老公宝", 1: "老婆宝"}, false, -1, false},
		{"self-introduction", []string{"今天在河边散步很舒服。", "我叫小林，我也觉得今天的天气很好。"}, map[int]string{1: "小林"}, false, -1, false},
		{"two-friends", []string{"我昨天跟我老婆去了杭州旅游，很开心。", "我上个月也和我老婆去了苏州，玩得不错。"}, nil, true, -1, false},
		{"quoted-introduction", []string{"我今天读到一段故事，里面的人说：我叫小林，明天去北京。"}, nil, true, -1, false},
		{"third-person-unknown", []string{"我和我老婆宝在杭州旅游，很开心。", "我跟我老公宝确实玩得很合拍。", "我是路过的游客，今天来这里看风景。"}, map[int]string{0: "老公宝", 1: "老婆宝"}, false, -1, false},
		{"explicit-narrator", []string{"老婆宝也来补充今天的事。", "我是老婆宝，这篇手记按我的视角写。今天我在医院值班很累。"}, map[int]string{1: "老婆宝"}, false, 1, false},
		{"merged-voice-narrator", []string{"今天和小林一起在河边散步。", "我是小林，这篇手记按我的视角写。我从杭州坐火车过来。"}, map[int]string{1: "小林"}, false, 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := identityFixture(c.texts...)
			if c.sharedVoice {
				r.Turns[1].Speaker = r.Turns[0].Speaker
				r.Turns[1].PersonID = r.NarratorPersonID
				r.Turns[1].Person = "我"
				r.Speakers = r.Speakers[:1]
			}
			s := Snapshot{Identity: &r, DictationMode: true, Blocks: []Block{{ID: uuid.NewString(), Text: "原话", Style: "body"}}, WritingStyle: "natural"}
			out, err := model.Rewrite(context.Background(), s, 1)
			record := map[string]any{"request": r, "response": out.Identity, "raw": out.OutputText, "model": out.Model, "stage": out.Diagnostics.Stage, "inputTokens": out.InputTokens, "outputTokens": out.OutputTokens}
			if err != nil {
				record["error"] = err.Error()
			}
			encoded, _ := json.MarshalIndent(record, "", "  ")
			if e := os.MkdirAll(config.OutputDirectory, 0700); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(config.OutputDirectory, c.name+".json"), encoded, 0600); e != nil {
				t.Fatal(e)
			}
			if err != nil {
				t.Fatal("configured provider failed; synthetic response preserved", err)
			}
			if out.Identity == nil {
				t.Fatal("missing identity result")
			}
			if c.empty && len(out.Identity.Assignments) != 0 {
				t.Fatal("invented identity", out.Identity.Assignments)
			}
			for index, name := range c.expected {
				found := false
				for _, a := range out.Identity.Assignments {
					if a.SpeakerKey == r.Turns[index].Speaker && a.Name == name && slices.Contains(a.SourceIDs, r.Turns[index].ID) {
						found = true
					}
				}
				if !found {
					t.Errorf("expected exact name %s for voice %d; got %+v", name, index, out.Identity.Assignments)
				}
			}
			if c.narrator < 0 && out.Identity.NarratorSourceID != "" {
				t.Fatal("changed default narrator")
			}
			if c.narrator >= 0 && out.Identity.NarratorSourceID != r.Turns[c.narrator].ID {
				t.Fatal("explicit narrator instruction was not applied")
			}
		})
	}
}
