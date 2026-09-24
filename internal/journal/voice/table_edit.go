package voice

import (
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"
)

type TableEdit struct {
	ID          string       `json:"id"`
	BlockID     string       `json:"blockID"`
	TableID     string       `json:"tableID"`
	SourceID    string       `json:"sourceID"`
	Instruction string       `json:"instruction"`
	Patches     []TablePatch `json:"patches"`
}

func validateTableEdits(r Revision, s Snapshot) error {
	if len(r.TableEdits) == 0 {
		return nil
	}
	if len(r.TableEdits) > 4 || len(r.TableCreations)+len(r.MoveCommands)+len(r.MoveResolutions)+len(r.ParagraphCommands)+len(r.ParagraphResolutions) > 0 {
		return ErrInvalid
	}
	used := map[string]bool{}
	tables := map[string]TableContext{}
	sources := map[string]string{}
	for _, b := range s.Blocks {
		used[b.ID] = true
	}
	for _, ids := range s.BlockComponents {
		for _, id := range ids {
			used[id] = true
		}
	}
	for _, t := range s.TableContext {
		tables[t.TableID] = t
		for _, c := range t.Columns {
			used[c.ID] = true
		}
		for _, row := range t.Rows {
			used[row.ID] = true
		}
		for _, c := range t.Calculations {
			used[c.ID] = true
		}
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
	for _, c := range s.FormatContext {
		used[c.ReceiptID] = true
	}
	for _, c := range s.MoveContext {
		used[c.ReceiptID] = true
	}
	for _, c := range s.ParagraphContext {
		used[c.ReceiptID] = true
	}
	for _, u := range s.PendingUtterances {
		sources[u.ID] = u.Text
	}
	claim := func(id string) bool {
		if !validID(id) || used[id] {
			return false
		}
		used[id] = true
		return true
	}
	seen := map[string]bool{}
	total := 0
	for _, edit := range r.TableEdits {
		before, ok := tables[edit.TableID]
		if !ok || before.BlockID != edit.BlockID || seen[edit.TableID] || !claim(edit.ID) ||
			!slices.Contains(r.ConsumedSourceIDs, edit.SourceID) || strings.TrimSpace(edit.Instruction) == "" ||
			utf8.RuneCountInString(edit.Instruction) > 500 || strings.Count(sources[edit.SourceID], edit.Instruction) != 1 {
			return ErrInvalid
		}
		seen[edit.TableID] = true
		for _, c := range r.BlockEdits {
			if c.ID == edit.BlockID {
				return ErrInvalid
			}
		}
		linked := false
		for _, p := range r.SourcePartitions {
			if p.SourceID == edit.SourceID {
				for _, segment := range p.Segments {
					if segment.Role == "instruction" && segment.Text == edit.Instruction && slices.Contains(segment.BlockIDs, edit.BlockID) {
						linked = true
					}
				}
			}
		}
		if !linked {
			return ErrInvalid
		}
		for _, patch := range edit.Patches {
			if patch.Calculation != nil && !claim(patch.Calculation.ID) {
				return ErrInvalid
			}
			cells := []TableCell{}
			if patch.Cell != nil {
				cells = append(cells, *patch.Cell)
			}
			if patch.Row != nil {
				if !claim(patch.Row.ID) {
					return ErrInvalid
				}
				cells = append(cells, patch.Row.Cells...)
			}
			if patch.Column != nil {
				if !claim(patch.Column.ID) {
					return ErrInvalid
				}
			}
			for _, cell := range cells {
				total += utf8.RuneCountInString(cell.Text + cell.Number + cell.Unit)
				if total > 20000 || len(cell.Sources) > 16 || (cell.Kind != "pending" && len(cell.Sources) == 0) {
					return ErrInvalid
				}
				evidence := map[TableSource]bool{}
				for _, source := range cell.Sources {
					if !slices.Contains(r.ConsumedSourceIDs, source.SourceID) || evidence[source] {
						return ErrInvalid
					}
					start, end, valid := tableAnchorRange(sources[source.SourceID], source.Anchor)
					// An explicit correction often embeds the new value within
					// its instruction. It must not be duplicated as body content.
					insideInstruction := source.SourceID == edit.SourceID && start >= strings.Index(sources[source.SourceID], edit.Instruction) && end <= strings.Index(sources[source.SourceID], edit.Instruction)+len(edit.Instruction)
					if !valid || (!insideInstruction && !tableSourceIsContent(r, source, sources[source.SourceID], edit.BlockID)) {
						return ErrInvalid
					}
					evidence[source] = true
				}
			}
		}
		after, err := applyTablePatches(before, edit.Patches)
		if err != nil {
			return err
		}
		// Evidence alone is not a document change.
		before.Rows = slices.Clone(before.Rows)
		for i := range before.Rows {
			before.Rows[i].Cells = slices.Clone(before.Rows[i].Cells)
			for j := range before.Rows[i].Cells {
				before.Rows[i].Cells[j].Sources = nil
			}
		}
		for i := range after.Rows {
			for j := range after.Rows[i].Cells {
				after.Rows[i].Cells[j].Sources = nil
			}
		}
		if reflect.DeepEqual(before, after) {
			return ErrInvalid
		}
	}
	return nil
}
