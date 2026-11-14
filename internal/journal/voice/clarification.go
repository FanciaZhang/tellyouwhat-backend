package voice

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// A choice authorizes only exact, source-scoped text edits, never an instruction
// to execute arbitrary operations or rewrite the rest of the document.
type ClarificationEdit struct {
	Anchor      TextAnchor `json:"anchor"`
	Replacement string     `json:"replacement"`
}

type ClarificationChoice struct {
	ID    string              `json:"id,omitempty"`
	Label string              `json:"label"`
	Fact  string              `json:"fact"`
	Edits []ClarificationEdit `json:"edits"`
}

type Clarification struct {
	ID        string                `json:"id,omitempty"`
	Question  string                `json:"question"`
	SourceIDs []string              `json:"sourceIDs"`
	Choices   []ClarificationChoice `json:"choices"`
}

const clarificationInstructions = `
需要用户判断且会改变正文含义的歧义，用 questions 返回可直接作答的结构化问题。已由上下文唯一确定的误识别直接纠正，不提问；不为补全背景、类别、性别等与当前表达无关的信息提问，也不把作者自己的“可能、记不清”当成必须消除的错误。
同一事实出现互斥版本且没有明确改口依据，或关键指代/归属无法唯一确定时，不能默选最后一句、抹去冲突或以含糊代词假装处理完成；必须给出可选修订。已表达的不确定感受仍忠实保留，不强行变成定论。
关键指代有多个上下文候选且一次选择就能消歧时，不能只改成“其中一个”等泛指来回避确认；最近提及或名称相近本身不构成唯一依据。
每题只决定一个事实，question 简短具体，不把几个独立疑点捆在一起。sourceIDs 精确引用相关 turns 的 id。给出 2 至 4 个有实际区别的 choices，label 直接写答案，fact 写用户选择后确认的事实，须含明确对象和归属、可独立理解，不用孤立的“是/不是/这个”代替上下文；不要声称某个选项已获确认。候选必须来自现有内容或其合理的互斥解释，不编造人物、关系、数字或经历，不问原话没有涉及的事实。
每个选项的 edits 列出选择后所有必要的局部修改，修改名字、指代、归属时同步修正受影响的相关句子。anchor.quote/prefix/suffix 必须逐字来自本次 paragraphs 的最终正文，并在 sourceIDs 关联的正文中唯一定位；replacement 是完整替换文字，非修改说明。多处修改分别给出，不用全局替换，不删除段落或改变格式，不改不相关的表达。保持当前正文的选项用 edits=[]，不要列出相同文字的替换；不同选项应有不同的事实与效果。未作答时不能把互斥结论中的某一个先写成定论；保留原话冲突或自然的不确定表述，让各选项修改这个相同基线。不能给出安全且可执行的选项时保留原意，不返回只有“请核对”的说明。客户端另提供“暂不确定，保留原文”，不要将其重复作为模型选项。
confirmedDetails 是用户已经明确确认的事实，优先于旧识别、旧稿和本次推测。整理必须遵守，不改回旧错误，也不就同一事实重复提问；它不是新口述，不要将确认操作或这些说明抄进正文。`

func clarificationSchema(sourceIDs []string, lightweight bool) map[string]any {
	text := func(limit int) map[string]any {
		return map[string]any{"type": "string", "minLength": 1, "maxLength": limit}
	}
	anchor := map[string]any{"type": "object", "additionalProperties": false,
		"required": []string{"quote", "prefix", "suffix"}, "properties": map[string]any{
			"quote": text(1000), "prefix": map[string]any{"type": "string", "maxLength": 120}, "suffix": map[string]any{"type": "string", "maxLength": 120}}}
	edit := map[string]any{"type": "object", "additionalProperties": false,
		"required": []string{"anchor", "replacement"}, "properties": map[string]any{"anchor": anchor, "replacement": text(1500)}}
	choice := map[string]any{"type": "object", "additionalProperties": false,
		"required": []string{"label", "fact", "edits"}, "properties": map[string]any{
			"label": text(120), "fact": text(300), "edits": map[string]any{"type": "array", "maxItems": 16, "items": edit}}}
	item := map[string]any{"type": "object", "additionalProperties": false,
		"required": []string{"question", "sourceIDs", "choices"}, "properties": map[string]any{
			"question": text(200), "sourceIDs": map[string]any{"type": "array", "minItems": 1, "maxItems": 32,
				"items": map[string]any{"type": "string", "enum": sourceIDs}},
			"choices": map[string]any{"type": "array", "minItems": 2, "maxItems": 4, "items": choice}}}
	if len(sourceIDs) == 0 {
		item["properties"].(map[string]any)["sourceIDs"].(map[string]any)["items"] = map[string]any{"type": "string"}
	}
	limit := 8
	if lightweight || len(sourceIDs) == 0 {
		limit = 0
	}
	return map[string]any{"type": "array", "maxItems": limit, "items": item}
}

func clarificationParagraphs(q Clarification, revision *PolishRevision) []string {
	texts := []string{}
	for _, p := range revision.Paragraphs {
		if slices.ContainsFunc(revision.Targets, func(t PolishTarget) bool {
			return slices.Contains(p.TargetIDs, t.ID) && slices.ContainsFunc(q.SourceIDs, func(id string) bool { return slices.Contains(t.SourceIDs, id) })
		}) {
			texts = append(texts, p.Text)
		}
	}
	return texts
}

func clarificationRanges(anchor TextAnchor, text string) [][2]int {
	result := [][2]int{}
	if anchor.Quote == "" {
		return result
	}
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], anchor.Quote)
		if index < 0 {
			break
		}
		start := offset + index
		end := start + len(anchor.Quote)
		if strings.HasSuffix(text[:start], anchor.Prefix) && strings.HasPrefix(text[end:], anchor.Suffix) {
			result = append(result, [2]int{start, end})
		}
		_, size := utf8.DecodeRuneInString(text[start:])
		offset = start + size
	}
	return result
}

func validateClarifications(revision *PolishRevision) error {
	if len(revision.Clarifications) > 8 || revision.Lightweight && len(revision.Clarifications) > 0 {
		return polishInvalid("clarification_limit")
	}
	knownSources := map[string]bool{}
	for _, target := range revision.Targets {
		for _, id := range target.SourceIDs {
			knownSources[id] = true
		}
	}
	bounded := func(text string, limit int) bool {
		return strings.TrimSpace(text) != "" && utf8.RuneCountInString(text) <= limit
	}
	for _, q := range revision.Clarifications {
		if !bounded(q.Question, 200) || len(q.SourceIDs) == 0 || len(q.SourceIDs) > 32 || len(q.Choices) < 2 || len(q.Choices) > 4 {
			return polishInvalid("clarification_question")
		}
		seen := map[string]bool{}
		for _, id := range q.SourceIDs {
			if !knownSources[id] || seen[id] {
				return polishInvalid("clarification_source")
			}
			seen[id] = true
		}
		texts := clarificationParagraphs(q, revision)
		labels, facts := map[string]bool{}, map[string]bool{}
		for c, choice := range q.Choices {
			if !bounded(choice.Label, 120) || !bounded(choice.Fact, 300) || labels[choice.Label] || facts[choice.Fact] || choice.Edits == nil || len(choice.Edits) > 16 {
				return polishInvalid("clarification_choice")
			}
			for _, previous := range q.Choices[:c] {
				if slices.Equal(previous.Edits, choice.Edits) {
					return polishInvalid("clarification_duplicate_effect")
				}
			}
			labels[choice.Label], facts[choice.Fact] = true, true
			ranges := map[int][][2]int{}
			for _, edit := range choice.Edits {
				if !bounded(edit.Anchor.Quote, 1000) || utf8.RuneCountInString(edit.Anchor.Prefix) > 120 || utf8.RuneCountInString(edit.Anchor.Suffix) > 120 ||
					!bounded(edit.Replacement, 1500) || strings.ContainsAny(edit.Replacement, "\r\n") || edit.Anchor.Quote == edit.Replacement {
					return polishInvalid("clarification_edit")
				}
				matches := 0
				for p, text := range texts {
					for _, span := range clarificationRanges(edit.Anchor, text) {
						matches++
						for _, previous := range ranges[p] {
							if span[0] < previous[1] && previous[0] < span[1] {
								return polishInvalid("clarification_overlap")
							}
						}
						ranges[p] = append(ranges[p], span)
					}
				}
				if matches != 1 {
					return polishInvalid("clarification_anchor")
				}
			}
		}
	}
	return nil
}

func identifyClarifications(items []Clarification) {
	for i := range items {
		items[i].ID = uuid.NewString()
		for j := range items[i].Choices {
			items[i].Choices[j].ID = uuid.NewString()
		}
	}
}
