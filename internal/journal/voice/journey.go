package voice

import (
	"slices"
	"strings"
	"unicode/utf8"
)

type JourneyVisit struct {
	ID         string     `json:"id"`
	Expression string     `json:"expression"`
	SourceID   string     `json:"sourceID"`
	Anchor     TextAnchor `json:"anchor"`
}

type JourneyCreation struct {
	ID          string         `json:"id"`
	BlockID     string         `json:"blockID"`
	MapID       string         `json:"mapID"`
	AfterID     *string        `json:"afterID"`
	SourceID    string         `json:"sourceID"`
	Instruction string         `json:"instruction"`
	Title       string         `json:"title"`
	Visits      []JourneyVisit `json:"visits"`
}

func validateJourneySourceContext(s Snapshot) error {
	if len(s.JourneySourceContext) > 16 {
		return ErrInvalid
	}
	size, seen := 0, map[string]bool{}
	for _, source := range s.JourneySourceContext {
		key := strings.ToLower(source.SourceID) + "/" + source.Anchor.Quote
		known := slices.ContainsFunc(s.KnownSourceIDs, func(id string) bool { return strings.EqualFold(id, source.SourceID) })
		if !validID(source.SourceID) || !known || seen[key] || strings.TrimSpace(source.Anchor.Quote) == "" || source.Anchor.Prefix != "" || source.Anchor.Suffix != "" {
			return ErrInvalid
		}
		for _, pending := range s.PendingUtterances {
			if strings.EqualFold(pending.ID, source.SourceID) {
				return ErrInvalid
			}
		}
		seen[key] = true
		size += utf8.RuneCountInString(source.Anchor.Quote)
		if size > 6000 {
			return ErrInvalid
		}
	}
	return nil
}

func validateJourneyCreations(r Revision, s Snapshot) error {
	if len(r.JourneyCreations) == 0 {
		return nil
	}
	if len(r.JourneyCreations) > 4 || len(r.MoveCommands)+len(r.MoveResolutions)+len(r.ParagraphCommands)+len(r.ParagraphResolutions) > 0 {
		return ErrInvalid
	}
	if err := validateJourneySourceContext(s); err != nil {
		return err
	}
	used, blocks := map[string]bool{}, map[string]bool{}
	for _, b := range s.Blocks {
		used[b.ID] = true
		blocks[b.ID] = true
	}
	for _, ids := range s.BlockComponents {
		for _, id := range ids {
			used[id] = true
		}
	}
	for _, t := range s.TimelineContext {
		used[t.TimelineID] = true
		for _, e := range t.Events {
			used[e.ID] = true
		}
	}
	for _, t := range s.TableContext {
		used[t.TableID] = true
		for _, x := range t.Rows {
			used[x.ID] = true
		}
		for _, x := range t.Columns {
			used[x.ID] = true
		}
		for _, x := range t.Calculations {
			used[x.ID] = true
		}
	}
	for _, c := range s.FormatContext {
		used[c.ReceiptID] = true
	}
	for _, c := range s.MoveContext {
		used[c.ReceiptID] = true
	}
	for _, c := range s.ParagraphContext {
		used[c.ReceiptID] = true
	}
	for _, c := range s.TableReceiptContext {
		used[c.ReceiptID] = true
	}
	for _, c := range r.BlockEdits {
		used[c.ID] = true
	}
	for _, c := range r.FormatCommands {
		used[c.ID] = true
	}
	for _, c := range r.FormatResolutions {
		used[c.ID] = true
	}
	for _, c := range r.TableResolutions {
		used[c.ID] = true
	}
	for _, c := range r.TimelineCreations {
		used[c.ID] = true
		used[c.BlockID] = true
		used[c.TimelineID] = true
		for _, e := range c.Events {
			used[e.ID] = true
		}
	}
	for _, c := range r.TimelineEdits {
		used[c.ID] = true
		for _, e := range c.Insertions {
			used[e.ID] = true
		}
	}
	for _, c := range r.TableCreations {
		used[c.ID] = true
		used[c.BlockID] = true
		used[c.TableID] = true
		for _, x := range c.Rows {
			used[x.ID] = true
		}
		for _, x := range c.Columns {
			used[x.ID] = true
		}
	}
	for _, c := range r.TableEdits {
		used[c.ID] = true
		for _, p := range c.Patches {
			if p.Row != nil {
				used[p.Row.ID] = true
			}
			if p.Column != nil {
				used[p.Column.ID] = true
			}
			if p.Calculation != nil {
				used[p.Calculation.ID] = true
			}
		}
	}
	claim := func(id string) bool {
		if !validID(id) || used[id] {
			return false
		}
		used[id] = true
		return true
	}
	linked := func(source TableSource, block, role string) bool {
		if role == "content" && !slices.Contains(r.ConsumedSourceIDs, source.SourceID) {
			for _, old := range s.JourneySourceContext {
				if old.SourceID == source.SourceID {
					if _, _, ok := tableAnchorRange(old.Anchor.Quote, source.Anchor); ok {
						return true
					}
				}
			}
			return false
		}
		text, matches := "", 0
		for _, u := range s.PendingUtterances {
			if u.ID == source.SourceID {
				text = u.Text
				matches++
			}
		}
		if matches != 1 || !slices.Contains(r.ConsumedSourceIDs, source.SourceID) {
			return false
		}
		start, end, ok := tableAnchorRange(text, source.Anchor)
		if !ok {
			return false
		}
		count, found := 0, false
		for _, p := range r.SourcePartitions {
			if p.SourceID != source.SourceID {
				continue
			}
			count++
			offset, joined := 0, ""
			for _, seg := range p.Segments {
				next := offset + len(seg.Text)
				if seg.Role == role && slices.Equal(seg.BlockIDs, []string{block}) && start >= offset && end <= next && (role != "instruction" || start == offset && end == next) {
					found = true
				}
				offset = next
				joined += seg.Text
			}
			if joined != text {
				return false
			}
		}
		return count == 1 && found
	}
	total := 0
	for _, c := range r.JourneyCreations {
		if !claim(c.ID) || !claim(c.BlockID) || !claim(c.MapID) || c.AfterID != nil && !blocks[*c.AfterID] || strings.TrimSpace(c.Title) == "" || utf8.RuneCountInString(c.Title) > 300 || utf8.RuneCountInString(c.Instruction) > 500 || len(c.Visits) == 0 || len(c.Visits) > 64 || !linked(TableSource{SourceID: c.SourceID, Anchor: TextAnchor{Quote: c.Instruction}}, c.BlockID, "instruction") {
			return ErrInvalid
		}
		for _, v := range c.Visits {
			if !claim(v.ID) || strings.TrimSpace(v.Expression) == "" || utf8.RuneCountInString(v.Expression) > 500 || !strings.Contains(v.Anchor.Quote, v.Expression) || utf8.RuneCountInString(v.Anchor.Quote+v.Anchor.Prefix+v.Anchor.Suffix) > 6000 || !linked(TableSource{SourceID: v.SourceID, Anchor: v.Anchor}, c.BlockID, "content") {
				return ErrInvalid
			}
			total += utf8.RuneCountInString(v.Expression + v.Anchor.Quote)
			if total > 20000 {
				return ErrInvalid
			}
		}
	}
	return nil
}
