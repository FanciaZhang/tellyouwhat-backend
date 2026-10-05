package contracts

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Optional editorial advice is independently decoded. A malformed scene must
// never invalidate an otherwise valid journal rewrite or organize response.
type IllustrationSuggestion struct {
	Summary      string   `json:"summary"`
	Subject      string   `json:"subject"`
	Setting      string   `json:"setting"`
	Composition  string   `json:"composition"`
	Style        string   `json:"style"`
	SourceQuotes []string `json:"sourceQuotes"`
	Reason       string   `json:"reason"`
}

func (s *IllustrationSuggestion) UnmarshalJSON(data []byte) error {
	type scene IllustrationSuggestion
	var value scene
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil {
		*s = IllustrationSuggestion{}
		return nil
	}
	*s = IllustrationSuggestion(value)
	return nil
}

func (s *IllustrationSuggestion) Valid() bool {
	if s == nil || len(s.SourceQuotes) == 0 || len(s.SourceQuotes) > 3 {
		return false
	}
	for _, value := range []struct {
		text     string
		maximum  int
		required bool
	}{
		{s.Summary, 120, true}, {s.Subject, 500, true}, {s.Setting, 500, false},
		{s.Composition, 500, false}, {s.Style, 120, false}, {s.Reason, 240, true},
	} {
		if !utf8.ValidString(value.text) || utf8.RuneCountInString(value.text) > value.maximum ||
			value.required && strings.TrimSpace(value.text) == "" {
			return false
		}
	}
	seen := map[string]bool{}
	for _, quote := range s.SourceQuotes {
		if strings.TrimSpace(quote) == "" || utf8.RuneCountInString(quote) > 600 || seen[quote] {
			return false
		}
		seen[quote] = true
	}
	return true
}

// Ground quotes in material already provided for this analysis. Never expand
// the speech context window or send additional content merely to find an image.
func (s *IllustrationSuggestion) Grounded(material []string) bool {
	if !s.Valid() {
		return false
	}
	for _, quote := range s.SourceQuotes {
		found := false
		for _, text := range material {
			if strings.Contains(text, quote) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func IllustrationSuggestionSchema() map[string]any {
	str := func(max int) map[string]any { return map[string]any{"type": "string", "maxLength": max} }
	return map[string]any{
		"type": []string{"object", "null"}, "additionalProperties": false,
		"required": []string{"summary", "subject", "setting", "composition", "style", "sourceQuotes", "reason"},
		"properties": map[string]any{
			"summary": str(120), "subject": str(500), "setting": str(500), "composition": str(500), "style": str(120), "reason": str(240),
			"sourceQuotes": map[string]any{"type": "array", "minItems": 1, "maxItems": 3, "items": str(600)},
		},
	}
}

const IllustrationSuggestionInstructions = `
illustrationSuggestionsEnabled 为 true 时，可在 illustrationSuggestion 提供最多一个有价值的配图场景，否则必须为 null。不适合配图或没有完整具体场景时返回 null；不要按字数或情绪判断，不要为了每篇都有图片而补造办公桌等装饰场景。summary 用一句用户容易理解的话描述将生成什么；subject、setting、composition、style 给出同一个场景的具体视觉信息，reason 简述选择理由。默认横向柔和水彩插画；用户明确要求其他风格则沿用。sourceQuotes 必须逐字引用本次输入的正文或口述，最多三段，支持主要场景事实。未知人物外貌可用背影或远景，未知动物品种不指定。不要根据姓名、声音猜测性别或外貌；不添加原文未讲述的事件、人物关系、诊断或重要物品。summary 和场景字段不包含姓名、精确地址、电话、证件或医疗设备读数；使用必要的通用关系和地点类别。构图留白等审美补充可以完善表达，但不得改变事实。无需绘制文字。建议是独立的辅助信息，禁止把配图建议抄入正文、标签、问题或改写命令。`
