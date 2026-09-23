package voice

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Flat wire data is independent of Swift's enum encoding. The app supplies the
// archive identity and revalidates all references before offering a preview.
type TableSource struct {
	SourceID string     `json:"sourceID"`
	Anchor   TextAnchor `json:"anchor"`
}
type TableColumn struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}
type TableCell struct {
	ColumnID    string        `json:"columnID"`
	Kind        string        `json:"kind"`
	Text        string        `json:"text"`
	Number      string        `json:"number"`
	Unit        string        `json:"unit"`
	Approximate bool          `json:"approximate"`
	NeedsReview bool          `json:"needsReview"`
	Sources     []TableSource `json:"sources"`
}
type TableRow struct {
	ID    string      `json:"id"`
	Cells []TableCell `json:"cells"`
}
type TableCreation struct {
	ID          string        `json:"id"`
	BlockID     string        `json:"blockID"`
	TableID     string        `json:"tableID"`
	AfterID     *string       `json:"afterID"`
	SourceID    string        `json:"sourceID"`
	Instruction string        `json:"instruction"`
	Title       string        `json:"title"`
	Columns     []TableColumn `json:"columns"`
	Rows        []TableRow    `json:"rows"`
}

var tableDecimal = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)

func tableAnchorRange(text string, anchor TextAnchor) (int, int, bool) {
	if strings.TrimSpace(anchor.Quote) == "" || utf8.RuneCountInString(anchor.Quote+anchor.Prefix+anchor.Suffix) > 6000 {
		return 0, 0, false
	}
	count, start, end := 0, 0, 0
	for cursor := 0; cursor < len(text); {
		relative := strings.Index(text[cursor:], anchor.Quote)
		if relative < 0 {
			break
		}
		a := cursor + relative
		b := a + len(anchor.Quote)
		if strings.HasSuffix(text[:a], anchor.Prefix) && strings.HasPrefix(text[b:], anchor.Suffix) {
			count++
			start = a
			end = b
		}
		_, width := utf8.DecodeRuneInString(text[a:])
		cursor = a + width
	}
	return start, end, count == 1
}

func tableSourceIsContent(r Revision, source TableSource, text, blockID string) bool {
	start, end, ok := tableAnchorRange(text, source.Anchor)
	if !ok {
		return false
	}
	for _, partition := range r.SourcePartitions {
		if partition.SourceID != source.SourceID {
			continue
		}
		offset := 0
		for _, segment := range partition.Segments {
			next := offset + len(segment.Text)
			if segment.Role == "content" && slices.Contains(segment.BlockIDs, blockID) && start >= offset && end <= next {
				return true
			}
			offset = next
		}
	}
	return false
}

func validateTables(r Revision, s Snapshot) error {
	if len(r.TableCreations) > 4 {
		return ErrInvalid
	}
	if len(r.TableCreations) == 0 {
		return nil
	}
	// Structural moves are handled in another revision; ordinary independent
	// prose/formatting may continue alongside a table proposal.
	if len(r.MoveCommands)+len(r.MoveResolutions)+len(r.ParagraphCommands)+len(r.ParagraphResolutions) > 0 {
		return ErrInvalid
	}
	used, blocks, consumed, sources := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]string{}
	for _, b := range s.Blocks {
		used[b.ID] = true
		blocks[b.ID] = true
	}
	for _, ids := range s.BlockComponents {
		for _, id := range ids {
			used[id] = true
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
	for _, id := range r.ConsumedSourceIDs {
		consumed[id] = true
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
	total := 0
	for _, c := range r.TableCreations {
		if !claim(c.ID) || !claim(c.BlockID) || !claim(c.TableID) || (c.AfterID != nil && !blocks[*c.AfterID]) ||
			!consumed[c.SourceID] || strings.TrimSpace(c.Instruction) == "" || utf8.RuneCountInString(c.Instruction) > 500 || strings.Count(sources[c.SourceID], c.Instruction) != 1 ||
			strings.TrimSpace(c.Title) == "" || utf8.RuneCountInString(c.Title) > 300 || len(c.Columns) == 0 || len(c.Columns) > 16 || len(c.Rows) == 0 || len(c.Rows) > 64 || len(c.Columns)*len(c.Rows) > 512 {
			return ErrInvalid
		}
		instructionLinked := false
		for _, p := range r.SourcePartitions {
			if p.SourceID == c.SourceID {
				for _, part := range p.Segments {
					if part.Role == "instruction" && part.Text == c.Instruction && slices.Contains(part.BlockIDs, c.BlockID) {
						instructionLinked = true
					}
				}
			}
		}
		if !instructionLinked {
			return ErrInvalid
		}
		cols := map[string]bool{}
		for _, col := range c.Columns {
			if !claim(col.ID) || strings.TrimSpace(col.Title) == "" || utf8.RuneCountInString(col.Title) > 160 {
				return ErrInvalid
			}
			cols[col.ID] = true
		}
		for _, row := range c.Rows {
			if !claim(row.ID) || len(row.Cells) != len(c.Columns) {
				return ErrInvalid
			}
			seen := map[string]bool{}
			for _, cell := range row.Cells {
				if !cols[cell.ColumnID] || seen[cell.ColumnID] || len(cell.Sources) > 16 {
					return ErrInvalid
				}
				seen[cell.ColumnID] = true
				if !validateTableCellValue(cell) || (cell.Kind == "text" && strings.TrimSpace(cell.Text) == "") {
					return ErrInvalid
				}
				total += utf8.RuneCountInString(cell.Text + cell.Number + cell.Unit)
				if total > 20000 || cell.Kind != "pending" && len(cell.Sources) == 0 {
					return ErrInvalid
				}
				evidence := map[TableSource]bool{}
				for _, source := range cell.Sources {
					if !consumed[source.SourceID] || evidence[source] || !tableSourceIsContent(r, source, sources[source.SourceID], c.BlockID) {
						return ErrInvalid
					}
					evidence[source] = true
				}
			}
		}
	}
	return nil
}
