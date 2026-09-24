package voice

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// Expressions carry addresses, never a model-generated result. Existing
// expressions may have deleted operands; the client displays that invalid state.
type TableCalculation struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Kind     string   `json:"kind"`
	ColumnID string   `json:"columnID"`
	RowIDs   []string `json:"rowIDs"`
}

func (c TableCalculation) validShape() bool {
	if !validID(c.ID) || !validID(c.ColumnID) || strings.TrimSpace(c.Title) == "" || utf8.RuneCountInString(c.Title) > 160 {
		return false
	}
	switch c.Kind {
	case "sum":
		return len(c.RowIDs) == 0
	case "difference":
		return len(c.RowIDs) == 2 && validID(c.RowIDs[0]) && validID(c.RowIDs[1]) && c.RowIDs[0] != c.RowIDs[1]
	default:
		return false
	}
}

// A newly requested expression must have compatible, confirmed operands.
// Pending/review rows are excluded from sums, not treated as zero.
func (c TableCalculation) canEvaluate(table TableContext) bool {
	if !c.validShape() || !slices.ContainsFunc(table.Columns, func(column TableColumn) bool { return column.ID == c.ColumnID }) {
		return false
	}
	included, unit := 0, ""
	for _, row := range table.Rows {
		if c.Kind == "difference" && !slices.Contains(c.RowIDs, row.ID) {
			continue
		}
		index := slices.IndexFunc(row.Cells, func(cell TableCell) bool { return cell.ColumnID == c.ColumnID })
		if index < 0 {
			return false
		}
		cell := row.Cells[index]
		if cell.NeedsReview || cell.Kind == "pending" {
			if c.Kind == "difference" {
				return false
			}
			continue
		}
		if cell.Kind != "number" || !validateTableCellValue(cell) || (included > 0 && cell.Unit != unit) {
			return false
		}
		included++
		unit = cell.Unit
	}
	return (c.Kind == "sum" && included > 0) || (c.Kind == "difference" && included == 2)
}
