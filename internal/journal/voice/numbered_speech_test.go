package voice

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPolishExpandsCollapsedSpokenEnumeration(t *testing.T) {
	p := *polishFixture().Polish
	text := "所以出行前的准备其实分三个点：第一，我要提前买车票。第二，我要收拾行李。第三，天气可能会变冷，但我还没有带外套，得先拿一件。"
	p.Targets[0].Text = text
	p.Targets[0].SourceText = text
	p.Targets[0].RetainedText = "此前已经确认的事实。"
	raw, _ := json.Marshal(map[string]any{"paragraphs": []PolishParagraph{{Text: text, Style: "orderedListItem", TargetIDs: []string{p.Targets[0].ID}}}, "questions": []string{}})
	r, err := decodePolish(string(raw), p)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"所以出行前的准备其实分三个点：", "我要提前买车票。", "我要收拾行李。", "天气可能会变冷，但我还没有带外套，得先拿一件。"}
	if len(r.Paragraphs) != len(expected) {
		t.Fatalf("collapsed enumeration: %#v", r.Paragraphs)
	}
	for i, part := range r.Paragraphs {
		style := "orderedListItem"
		if i == 0 {
			style = "body"
		}
		if part.Text != expected[i] || part.Style != style || !reflect.DeepEqual(part.TargetIDs, []string{p.Targets[0].ID}) {
			t.Fatalf("lost text, structure or source: %#v", part)
		}
	}
	if !reflect.DeepEqual(r.Targets, p.Targets) {
		t.Fatal("source baseline changed")
	}
}

func TestSpokenListRepairIsConservativeAndIdempotent(t *testing.T) {
	for _, text := range []string{
		"第一天我回去上班。第二天再请假。", "这是第一名，也是第二名的目标。",
		"第一，我去上班。第三，我要请假。", "他说“第一，准备。第二，出发。”",
		"第一，我去上班。第二，", "首先准备材料，其次确认时间。",
	} {
		p := PolishParagraph{Text: text, Style: "orderedListItem", TargetIDs: []string{"source"}}
		if got := expandSpokenList(p); !reflect.DeepEqual(got, []PolishParagraph{p}) {
			t.Fatalf("ambiguous content changed: %q => %#v", text, got)
		}
	}
	body := PolishParagraph{Text: "第一，准备材料。第二，确认时间。", Style: "body"}
	if got := expandSpokenList(body); !reflect.DeepEqual(got, []PolishParagraph{body}) {
		t.Fatal("ordinary prose reclassified")
	}
	for _, text := range []string{"第一点，准备材料。第二点，确认时间。第三点，联系同事。", "第2点，确认时间。第3点，联系同事。"} {
		parts := expandSpokenList(PolishParagraph{Text: text, Style: "orderedListItem", TargetIDs: []string{"a", "b"}})
		if len(parts) < 2 {
			t.Fatal("enumeration not expanded")
		}
		for _, part := range parts {
			if got := expandSpokenList(part); !reflect.DeepEqual(got, []PolishParagraph{part}) {
				t.Fatal("repair not idempotent")
			}
		}
	}
}
