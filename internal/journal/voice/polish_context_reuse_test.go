package voice

import (
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"slices"
	"testing"
)

func TestPolishReusesAuthorizedIdenticalContextWithoutDuplicatingItsBody(t *testing.T) {
	for _, text := range []string{"窗外的风很轻，河边也很安静。", ""} {
		p := *comparisonSnapshot([]string{"窗外的风很轻，河边也很安静。"}, false).Polish
		old := Block{ID: uuid.NewString(), Style: "body", Text: "窗外的风很轻，河边也很安静。"}
		p.Context = []Block{old}
		p.ReusableContextIDs = []string{old.ID}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"paragraphs": []PolishParagraph{{Text: text, Style: "body", TargetIDs: []string{p.Targets[0].ID}}}, "questions": []string{}})
		result, err := decodePolish(string(raw), p)
		if err != nil || len(result.Targets) != 2 || len(result.Paragraphs) != 1 {
			t.Fatal("safe repeated source was rejected", err)
		}
		if result.Paragraphs[0].Text != old.Text || !slices.Contains(result.Paragraphs[0].TargetIDs, old.ID) || !slices.Contains(result.Paragraphs[0].TargetIDs, p.Targets[0].ID) {
			t.Fatal("both source owners must share the original paragraph")
		}
		reused := result.Targets[1]
		if reused.Text != old.Text || reused.RetainedText != old.Text || len(reused.Turns) != 0 || reused.CompleteSource {
			t.Fatal("context reuse invented raw evidence")
		}
		p.ReusableContextIDs = nil
		if _, err := decodePolish(string(raw), p); err == nil {
			t.Fatal("read-only context became writable without authorization")
		}
	}
}

func TestPolishCannotUseContextReuseToHideNewFactsOrExceedBounds(t *testing.T) {
	p := *comparisonSnapshot([]string{"窗外的风很轻，河边也很安静。预算280元，还没付款。"}, false).Polish
	old := Block{ID: uuid.NewString(), Style: "body", Text: "窗外的风很轻，河边也很安静。"}
	p.Context = []Block{old}
	p.ReusableContextIDs = []string{old.ID}
	raw, _ := json.Marshal(map[string]any{"paragraphs": []PolishParagraph{{Text: "", Style: "body", TargetIDs: []string{p.Targets[0].ID}}}, "questions": []string{}})
	if _, err := decodePolish(string(raw), p); err == nil {
		t.Fatal("empty output hid an additional fact behind existing prose")
	}
	for _, ids := range [][]string{{uuid.NewString()}, {old.ID, old.ID}} {
		p.ReusableContextIDs = ids
		if p.Validate() == nil {
			t.Fatal("unbounded or unlisted context was authorized")
		}
	}
	p.ReusableContextIDs = []string{old.ID}
	p.Lightweight = true
	if p.Validate() == nil {
		t.Fatal("live cleanup gained authority to merge earlier narrative")
	}
	p.Lightweight = false
	first := p.Targets[0]
	first.Text = old.Text
	first.SourceText = old.Text
	first.Turns[0].Text = old.Text
	p.Targets = nil
	ids := []string{}
	for range maxPolishParagraphs {
		target := first
		target.ID = uuid.NewString()
		p.Targets = append(p.Targets, target)
		ids = append(ids, target.ID)
	}
	raw, _ = json.Marshal(map[string]any{"paragraphs": []PolishParagraph{{Text: old.Text, Style: "body", TargetIDs: ids}}, "questions": []string{}})
	if _, err := decodePolish(string(raw), p); !errors.Is(err, ErrInvalid) {
		t.Fatal("context expansion exceeded the App target limit", err)
	}
}

func TestContextReuseCannotConflateDecimalPunctuationOrEmoji(t *testing.T) {
	for _, pair := range [][2]string{{"预算280元。", "预算2.80元。"}, {"今天很好😀", "今天很好😢"}} {
		p := *comparisonSnapshot([]string{pair[1]}, false).Polish
		old := Block{ID: uuid.NewString(), Style: "body", Text: pair[0]}
		p.Context = []Block{old}
		p.ReusableContextIDs = []string{old.ID}
		raw, _ := json.Marshal(map[string]any{"paragraphs": []PolishParagraph{{Text: pair[1], Style: "body", TargetIDs: []string{p.Targets[0].ID}}}, "questions": []string{}})
		if _, err := decodePolish(string(raw), p); err == nil {
			t.Fatal("different punctuation or symbols changed an earlier fact")
		}
	}
}
