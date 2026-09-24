package voice

import (
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// TableContext is an exact, bounded projection of an existing table. Values
// may be manually entered; this is current document state, not ASR evidence.
// Row and column ordering is significant for spoken ordinal references.
type TableContext struct {
	BlockID      string             `json:"blockID"`
	TableID      string             `json:"tableID"`
	Title        string             `json:"title"`
	Columns      []TableColumn      `json:"columns"`
	Rows         []TableRow         `json:"rows"`
	Calculations []TableCalculation `json:"calculations"`
}

func validateTableCellValue(cell TableCell) bool {
	switch cell.Kind {
	case "date":
		date, err := time.Parse("2006-01-02", cell.Text)
		// Match the native Gregorian date picker: the 1582 calendar reform gap
		// is not silently normalized to a different day.
		gap := cell.Text >= "1582-10-05" && cell.Text <= "1582-10-14"
		return err == nil && !gap && date.Year() >= 1 && date.Year() <= 9999 && date.Format("2006-01-02") == cell.Text && cell.Number == "" && cell.Unit == "" && !cell.Approximate
	case "text":
		return utf8.RuneCountInString(cell.Text) <= 6000 && cell.Number == "" && cell.Unit == "" && !cell.Approximate
	case "number":
		digits := 0
		for _, ch := range cell.Number {
			if ch >= '0' && ch <= '9' {
				digits++
			}
		}
		return cell.Text == "" && len(cell.Number) <= 40 && digits <= 28 && tableDecimal.MatchString(cell.Number) && utf8.RuneCountInString(cell.Unit) <= 32
	case "pending":
		return cell.Text == "" && cell.Number == "" && cell.Unit == "" && !cell.Approximate
	default:
		return false
	}
}

func validateTableContext(s Snapshot) error {
	if len(s.TableContext) > 4 {
		return ErrInvalid
	}
	used, blocks, tables := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, block := range s.Blocks {
		used[block.ID] = true
		blocks[block.ID] = true
	}
	for _, ids := range s.BlockComponents {
		for _, id := range ids {
			used[id] = true
		}
	}
	claim := func(id string) bool {
		if !validID(id) || used[id] {
			return false
		}
		used[id] = true
		return true
	}
	total := 0
	for _, table := range s.TableContext {
		if !blocks[table.BlockID] || !validID(table.TableID) || blocks[table.TableID] || tables[table.TableID] ||
			!slices.Contains(s.BlockComponents[table.BlockID], table.TableID) || utf8.RuneCountInString(table.Title) > 300 ||
			len(table.Columns) == 0 || len(table.Columns) > 16 || len(table.Rows) > 64 || len(table.Columns)*len(table.Rows) > 512 {
			return ErrInvalid
		}
		tables[table.TableID] = true
		total += utf8.RuneCountInString(table.Title)
		columns := map[string]bool{}
		for _, column := range table.Columns {
			if !claim(column.ID) || strings.TrimSpace(column.Title) == "" || utf8.RuneCountInString(column.Title) > 160 {
				return ErrInvalid
			}
			columns[column.ID] = true
			total += utf8.RuneCountInString(column.Title)
		}
		for _, row := range table.Rows {
			if !claim(row.ID) || len(row.Cells) != len(table.Columns) {
				return ErrInvalid
			}
			seen := map[string]bool{}
			for _, cell := range row.Cells {
				// Historical source quotes are not needed to locate an existing
				// cell, and must not inflate every incremental model request.
				if !columns[cell.ColumnID] || seen[cell.ColumnID] || !validateTableCellValue(cell) || len(cell.Sources) != 0 {
					return ErrInvalid
				}
				seen[cell.ColumnID] = true
				total += utf8.RuneCountInString(cell.Text + cell.Number + cell.Unit)
			}
		}
		if len(table.Calculations) > 64 {
			return ErrInvalid
		}
		for _, calculation := range table.Calculations {
			if !claim(calculation.ID) || !calculation.validShape() {
				return ErrInvalid
			}
			total += utf8.RuneCountInString(calculation.Title)
		}
		if total > 20000 {
			return ErrInvalid
		}
	}
	return nil
}
