package voice

import (
	"regexp"
	"slices"
	"strings"
)

// Repair an explicit enumeration only after the model has classified it as a
// list. Never infer lists from ordinary prose, dates, priorities or quotations.
var spokenListMarker = regexp.MustCompile(`(?:^|[。！？；：\n])[ \t]*(第([一二三四五六七八九十]|[1-9][0-9]?)点?[，、：:,])`)

func expandSpokenList(p PolishParagraph) []PolishParagraph {
	unchanged := []PolishParagraph{p}
	if p.Style != "orderedListItem" || strings.ContainsAny(p.Text, "“”‘’\"「」『』") {
		return unchanged
	}
	matches := spokenListMarker.FindAllStringSubmatchIndex(p.Text, -1)
	if len(matches) < 2 {
		return unchanged
	}
	previous := 0
	for i, match := range matches {
		number := p.Text[match[4]:match[5]]
		value := spokenOrdinal(number)
		if i > 0 && value != previous+1 {
			return unchanged
		}
		previous = value
	}
	result := []PolishParagraph{}
	appendPart := func(text, style string) bool {
		text = strings.TrimSpace(text)
		if text == "" {
			return false
		}
		result = append(result, PolishParagraph{Text: text, Style: style, TargetIDs: slices.Clone(p.TargetIDs)})
		return true
	}
	if prefix := strings.TrimSpace(p.Text[:matches[0][2]]); prefix != "" {
		// An introduction must be explicitly separated from the enumeration.
		if !strings.HasSuffix(prefix, "：") && !strings.HasSuffix(prefix, "。") {
			return unchanged
		}
		appendPart(prefix, "body")
	}
	for i, match := range matches {
		end := len(p.Text)
		if i+1 < len(matches) {
			end = matches[i+1][2]
		}
		if !appendPart(p.Text[match[3]:end], "orderedListItem") {
			return unchanged
		}
	}
	return result
}

// Native list numbering owns the ordinal. Remove a spoken prefix even when the
// provider already returned separate items. Keep semantic ordinals (第一天、
// 第一名、第三代), quoted speech and ambiguous multi-point paragraphs intact.
var spokenListLead = regexp.MustCompile(`^第(?:[一二三四五六七八九十]{1,3}|[1-9][0-9]?)点?[，、：:,][ \t]*`)
var spokenPointSubject = regexp.MustCompile(`^第(?:[一二三四五六七八九十]{1,3}|[1-9][0-9]?)点(和|与|跟)`)
var spokenPointCopula = regexp.MustCompile(`^第(?:[一二三四五六七八九十]{1,3}|[1-9][0-9]?)点(?:就是|是)[，、：:,]?[ \t]*`)

func normalizeSpokenList(p PolishParagraph) []PolishParagraph {
	parts := expandSpokenList(p)
	for i := range parts {
		part := &parts[i]
		if part.Style != "orderedListItem" || len(spokenListMarker.FindAllStringIndex(part.Text, -1)) > 1 {
			continue
		}
		original := strings.TrimSpace(part.Text)
		cleaned := spokenListLead.ReplaceAllString(original, "")
		if cleaned == original {
			// An ordinal can be the grammatical subject, not a detachable label.
			// “第三点与天气有关” becomes “这与天气有关”, never “与天气有关”.
			cleaned = spokenPointSubject.ReplaceAllString(original, "这$1")
			if cleaned == original {
				cleaned = spokenPointCopula.ReplaceAllString(original, "")
			}
		}
		if strings.Trim(cleaned, " \t。！？；，、：:,.!?;") != "" {
			part.Text = cleaned
		}
	}
	return parts
}
