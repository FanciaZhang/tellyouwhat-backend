package voice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
			r.NarratorSpeaker = key
			r.Speakers[0].PersonID = uuid.NewString()
			r.Speakers[0].Name = "我"
		}
	}
	return r
}

type identityThenPolishRewriter struct{ failIdentity bool }

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
	valid := IdentityAssignment{SpeakerKey: r.Speakers[1].Key, Name: "老婆宝", Kind: "context", EvidenceIDs: []string{r.Turns[0].ID, r.Turns[1].ID}}
	encode := func(a IdentityAssignment) string {
		b, _ := json.Marshal(map[string]any{"assignments": []IdentityAssignment{a}, "narratorSpeaker": "", "narratorEvidenceIDs": []string{}, "commands": []IdentityCommand{}})
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
	rename, _ := json.Marshal(map[string]any{"assignments": []IdentityAssignment{valid}, "narratorSpeaker": "", "narratorEvidenceIDs": []string{}, "commands": []IdentityCommand{operation}})
	if _, err := decodeIdentity(string(rename), r); err != nil {
		t.Fatal("actual explicit rename should be allowed", err)
	}
	echo, _ := json.Marshal(map[string]any{"assignments": []IdentityAssignment{}, "narratorSpeaker": r.NarratorSpeaker, "narratorEvidenceIDs": []string{}, "commands": []IdentityCommand{}})
	if result, err := decodeIdentity(string(echo), r); err != nil || result.NarratorSpeaker != "" {
		t.Fatal("unchanged echo must be a no-op", err)
	}
	changed, _ := json.Marshal(map[string]any{"assignments": []IdentityAssignment{}, "narratorSpeaker": r.Speakers[1].Key, "narratorEvidenceIDs": []string{}, "commands": []IdentityCommand{}})
	if _, err := decodeIdentity(string(changed), r); err == nil {
		t.Fatal("changed narrator requires source evidence")
	}
	r.Turns[1].Text = "这篇手记按我的视角写。今天去公园。"
	operation.Text = "这篇手记按我的视角写。"
	changeNarrator := func(commands []IdentityCommand) string {
		b, _ := json.Marshal(map[string]any{"assignments": []IdentityAssignment{}, "narratorSpeaker": r.Speakers[1].Key, "narratorEvidenceIDs": []string{r.Turns[1].ID}, "commands": commands})
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
		name     string
		texts    []string
		expected map[int]string
		empty    bool
		narrator int
	}{
		{"reciprocal-affection", []string{"我跟我老婆宝在杭州旅游，非常非常开心。", "对，我跟我老公宝确实很开心，我们两个人出去玩就很合拍。"}, map[int]string{0: "老公宝", 1: "老婆宝"}, false, -1},
		{"self-introduction", []string{"今天在河边散步很舒服。", "我叫小林，我也觉得今天的天气很好。"}, map[int]string{1: "小林"}, false, -1},
		{"two-friends", []string{"我昨天跟我老婆去了杭州旅游，很开心。", "我上个月也和我老婆去了苏州，玩得不错。"}, nil, true, -1},
		{"quoted-introduction", []string{"我今天读到一段故事，里面的人说：我叫小林，明天去北京。"}, nil, true, -1},
		{"third-person-unknown", []string{"我和我老婆宝在杭州旅游，很开心。", "我跟我老公宝确实玩得很合拍。", "我是路过的游客，今天来这里看风景。"}, map[int]string{0: "老公宝", 1: "老婆宝"}, false, -1},
		{"explicit-narrator", []string{"老婆宝也来补充今天的事。", "我是老婆宝，这篇手记按我的视角写。今天我在医院值班很累。"}, map[int]string{1: "老婆宝"}, false, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := identityFixture(c.texts...)
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
					if a.SpeakerKey == r.Speakers[index].Key && a.Name == name {
						found = true
					}
				}
				if !found {
					t.Errorf("expected exact name %s for voice %d; got %+v", name, index, out.Identity.Assignments)
				}
			}
			if c.narrator < 0 && out.Identity.NarratorSpeaker != "" {
				t.Fatal("changed default narrator")
			}
			if c.narrator >= 0 && out.Identity.NarratorSpeaker != r.Speakers[c.narrator].Key {
				t.Fatal("explicit narrator instruction was not applied")
			}
		})
	}
}
