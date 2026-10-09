package voice

import (
	"strings"

	"github.com/google/uuid"
)

// Local names belong only to freshly created objects. Never repair an existing
// document reference or source identity: those must still pass exact validation.
// Repeated names map to the same identity so duplicate claims remain invalid.
func normalizeGeneratedStructures(r Revision, s Snapshot) Revision {
	for i := range r.ParagraphCommands {
		c := &r.ParagraphCommands[i]
		if c.Kind != "reflow" || len(c.BlockIDs) != 1 {
			continue
		}
		for _, b := range s.Blocks {
			if b.ID == c.BlockIDs[0] {
				c.Fragments = coalesceReflowSeparators(restoreReflowSeparators(b.Text, c.Fragments))
			}
		}
	}
	names := map[string]string{}
	claim := func(id string) string {
		if id == "" || validID(id) {
			return id
		}
		if fresh, ok := names[id]; ok {
			return fresh
		}
		fresh := uuid.NewString()
		names[id] = fresh
		return fresh
	}
	for i := range r.TableCreations {
		c := &r.TableCreations[i]
		c.ID, c.BlockID, c.TableID = claim(c.ID), claim(c.BlockID), claim(c.TableID)
		for j := range c.Columns {
			c.Columns[j].ID = claim(c.Columns[j].ID)
		}
		for j := range c.Rows {
			c.Rows[j].ID = claim(c.Rows[j].ID)
		}
	}
	for i := range r.TimelineCreations {
		c := &r.TimelineCreations[i]
		c.ID, c.BlockID, c.TimelineID = claim(c.ID), claim(c.BlockID), claim(c.TimelineID)
		for j := range c.Events {
			c.Events[j].ID = claim(c.Events[j].ID)
		}
	}
	ref := func(id string) string {
		if fresh, ok := names[id]; ok {
			return fresh
		}
		return id
	}
	for i := range r.TableCreations {
		for j := range r.TableCreations[i].Rows {
			for k := range r.TableCreations[i].Rows[j].Cells {
				cell := &r.TableCreations[i].Rows[j].Cells[k]
				cell.ColumnID = ref(cell.ColumnID)
			}
		}
	}
	for i := range r.TimelineCreations {
		for j := range r.TimelineCreations[i].Events {
			e := &r.TimelineCreations[i].Events[j]
			if e.AfterEventID != nil {
				value := ref(*e.AfterEventID)
				e.AfterEventID = &value
			}
			// Minute precision already contains the time of day. Drop only a redundant,
			// consistent period; conflicting model evidence remains invalid.
			if e.Precision == "minute" && e.Minute != nil && consistentMinutePeriod(*e.Minute, e.Period) {
				e.Period = ""
			}
		}
	}
	for i := range r.SourcePartitions {
		for j := range r.SourcePartitions[i].Segments {
			for k, id := range r.SourcePartitions[i].Segments[j].BlockIDs {
				r.SourcePartitions[i].Segments[j].BlockIDs[k] = ref(id)
			}
		}
	}
	return r
}

func consistentMinutePeriod(minute int, period string) bool {
	switch period {
	case "earlyMorning":
		return minute >= 0 && minute < 360
	case "morning":
		return minute >= 360 && minute < 720
	case "noon":
		return minute >= 660 && minute < 840
	case "afternoon":
		return minute >= 720 && minute < 1080
	case "evening":
		return minute >= 1080 && minute < 1260
	case "night":
		return minute >= 1140 && minute < 1440 || minute >= 0 && minute < 360
	}
	return false
}

// Models often omit list delimiters when returning split text. Recover only
// punctuation at exact sequential boundaries; never recover omitted words,
// reorder fragments, or alter the user's original prose.
func restoreReflowSeparators(text string, parts []ParagraphFragment) []ParagraphFragment {
	if len(parts) == 0 {
		return parts
	}
	restored := append([]ParagraphFragment(nil), parts...)
	rest := text
	for i, p := range parts {
		if p.Text == "" {
			return parts
		}
		if !strings.HasPrefix(rest, p.Text) {
			trimmed := strings.TrimLeft(rest, "、，,；; \t")
			if !strings.HasPrefix(trimmed, p.Text) {
				return parts
			}
			gap := rest[:len(rest)-len(trimmed)]
			if i == 0 {
				restored[i].Text = gap + p.Text
			} else {
				restored[i-1].Text += gap
			}
			rest = trimmed
		}
		rest = strings.TrimPrefix(rest, p.Text)
	}
	if strings.Trim(rest, "、，,；;。.!！?？ \t") != "" {
		return parts
	}
	restored[len(restored)-1].Text += rest
	return restored
}

// Separator-only fragments are not paragraphs. Retain them in the adjacent
// exact source fragment, where list rendering can remove the delimiter.
func coalesceReflowSeparators(parts []ParagraphFragment) []ParagraphFragment {
	result := []ParagraphFragment{}
	leading := ""
	for _, p := range parts {
		if strings.Trim(p.Text, "、，,；;。.!！?？ \t") == "" {
			if len(result) > 0 {
				result[len(result)-1].Text += p.Text
			} else {
				leading += p.Text
			}
			continue
		}
		p.Text = leading + p.Text
		leading = ""
		result = append(result, p)
	}
	if leading != "" {
		return parts
	}
	return result
}
