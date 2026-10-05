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
