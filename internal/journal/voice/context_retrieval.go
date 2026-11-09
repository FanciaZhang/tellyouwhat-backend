package voice

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Retrieval offers evidence to the model; it never grants rewrite permission
// or executes an instruction. MatchCount preserves ambiguity outside the window.
type ContextTarget struct {
	BlockID    string `json:"blockID"`
	Ordinal    int    `json:"ordinal"`
	Anchor     string `json:"anchor"`
	MatchCount int    `json:"matchCount"`
}

var contextQuotes = regexp.MustCompile(`[“「『"]([^”」』"]{2,80})[”」』"]`)
var contextTopic = regexp.MustCompile(`(?:讲|关于|提到|讨论|有关|把|将)([\p{Han}A-Za-z0-9]{2,24}?)(?:的那一段|的那段|那一段|那段|的段落|的那部分|的部分|的地方)`)
var contextOrdinal = regexp.MustCompile(`第([0-9一二三四五六七八九十百零两]{1,8})(个段落|段)`)
var chineseOrdinal = regexp.MustCompile(`^(?:[一二两三四五六七八九]百(?:零[一二三四五六七八九]|[一二三四五六七八九]十[一二三四五六七八九]?)?|[一二三四五六七八九]?十[一二三四五六七八九]?|[一二两三四五六七八九])$`)

func spokenOrdinal(text string) int {
	if value, err := strconv.Atoi(text); err == nil {
		return value
	}
	if !chineseOrdinal.MatchString(text) {
		return 0
	}
	digits := map[rune]int{'零': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	total, digit, lastUnit := 0, 0, 1000
	for _, r := range text {
		if value, ok := digits[r]; ok {
			digit = value
			continue
		}
		unit := 10
		if r == '百' {
			unit = 100
		}
		if unit >= lastUnit {
			return 0
		}
		if digit == 0 {
			digit = 1
		}
		total += digit * unit
		digit = 0
		lastUnit = unit
	}
	return total + digit
}

func retrieveContextTargets(s Snapshot) []ContextTarget {
	anchors := []string{}
	ordinals := []int{}
	for _, turn := range s.PendingUtterances {
		for _, pattern := range []*regexp.Regexp{contextQuotes, contextTopic} {
			for _, match := range pattern.FindAllStringSubmatch(turn.Text, 8) {
				anchor := strings.TrimSpace(match[1])
				if pattern == contextTopic {
					for _, prefix := range []string{"关于", "提到", "讨论", "有关", "讲"} {
						if rest, ok := strings.CutPrefix(anchor, prefix); ok && len([]rune(rest)) >= 2 {
							anchor = rest
							break
						}
					}
				}
				if anchor != "" && !slices.Contains(anchors, anchor) && len(anchors) < 8 {
					anchors = append(anchors, anchor)
				}
			}
		}
		for _, match := range contextOrdinal.FindAllStringSubmatch(turn.Text, 8) {
			ordinal := spokenOrdinal(match[1])
			if ordinal > 0 && ordinal <= len(s.Blocks) && !slices.Contains(ordinals, ordinal) && len(ordinals) < 8 {
				ordinals = append(ordinals, ordinal)
			}
		}
	}
	result := []ContextTarget{}
	for _, ordinal := range ordinals {
		result = append(result, ContextTarget{BlockID: s.Blocks[ordinal-1].ID, Ordinal: ordinal, Anchor: "第" + strconv.Itoa(ordinal) + "段", MatchCount: 1})
	}
	for _, anchor := range anchors {
		count := 0
		for _, block := range s.Blocks {
			if strings.Contains(block.Text, anchor) {
				count++
			}
		}
		for index, block := range s.Blocks {
			if strings.Contains(block.Text, anchor) {
				result = append(result, ContextTarget{BlockID: block.ID, Ordinal: index + 1, Anchor: anchor, MatchCount: count})
			}
		}
	}
	// Interleave independent references: a repeated source quote must not crowd
	// the unique destination quote out of a bounded movement context.
	buckets := map[string][]ContextTarget{}
	keys := []string{}
	for _, target := range result {
		if _, exists := buckets[target.Anchor]; !exists {
			keys = append(keys, target.Anchor)
		}
		buckets[target.Anchor] = append(buckets[target.Anchor], target)
	}
	result = nil
	for round := 0; ; round++ {
		added := false
		for _, key := range keys {
			if round < len(buckets[key]) {
				result = append(result, buckets[key][round])
				added = true
			}
		}
		if !added {
			break
		}
	}
	return result
}

// A six-block window reserves slots for fresh speech, explicit references, and
// unresolved corrections. Fair per-block budgets prevent one old long paragraph
// from consuming the entire window and hiding the active paragraph.
func selectContextBlocks(s Snapshot, replaceable, corrections []string, focus map[string][]string) ([]Block, []ContextTarget) {
	targets := retrieveContextTargets(s)
	selected := []string{}
	add := func(id string) {
		if len(selected) < 6 && !slices.Contains(selected, id) {
			selected = append(selected, id)
		}
	}
	if len(replaceable) > 0 {
		add(replaceable[len(replaceable)-1])
	}
	uniqueTargets := []string{}
	for _, target := range targets {
		if !slices.Contains(uniqueTargets, target.BlockID) {
			uniqueTargets = append(uniqueTargets, target.BlockID)
		}
	}
	for i := 0; len(selected) < 6 && (i < len(uniqueTargets) || i < len(corrections) || i < len(replaceable)); i++ {
		if i < len(uniqueTargets) {
			add(uniqueTargets[i])
		}
		if i < len(corrections) {
			add(corrections[i])
		}
		if i < len(replaceable) {
			add(replaceable[len(replaceable)-1-i])
		}
	}
	for i := len(s.Blocks) - 1; i >= 0 && len(selected) < 6; i-- {
		add(s.Blocks[i].ID)
	}
	offered := []ContextTarget{}
	focused := map[string]bool{}
	for _, target := range targets {
		if slices.Contains(selected, target.BlockID) {
			offered = append(offered, target)
			// Literal anchors are used to center the excerpt, never ordinal labels.
			if !focused[target.BlockID] && strings.Contains(s.Blocks[target.Ordinal-1].Text, target.Anchor) {
				focus[target.BlockID] = append([]string{target.Anchor}, focus[target.BlockID]...)
				focused[target.BlockID] = true
			}
		}
	}
	remaining := MaxRewriteContextCharacters
	blocks := []Block{}
	for _, source := range s.Blocks {
		if !slices.Contains(selected, source.ID) {
			continue
		}
		budget := remaining / (len(selected) - len(blocks))
		block := source
		block.Text = boundedContextText(source.Text, focus[source.ID], budget)
		remaining -= len([]rune(block.Text))
		blocks = append(blocks, block)
	}
	return blocks, offered
}
