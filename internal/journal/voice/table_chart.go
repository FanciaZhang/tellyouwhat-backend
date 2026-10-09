package voice

import (
	"math/big"
	"slices"
	"strings"
	"unicode/utf8"
)

// TableChart contains references only. Values and evidence belong to the table.
type TableChart struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Kind             string   `json:"kind"`
	CategoryColumnID string   `json:"categoryColumnID"`
	ValueColumnIDs   []string `json:"valueColumnIDs"`
}

func (c TableChart) validShape() bool {
	if !validID(c.ID) || strings.TrimSpace(c.Title) == "" || utf8.RuneCountInString(c.Title) > 300 ||
		!slices.Contains([]string{"bar", "line", "proportion"}, c.Kind) || !validID(c.CategoryColumnID) ||
		len(c.ValueColumnIDs) < 1 || len(c.ValueColumnIDs) > 8 || (c.Kind == "proportion" && len(c.ValueColumnIDs) != 1) {
		return false
	}
	seen := map[string]bool{c.CategoryColumnID: true}
	for _, id := range c.ValueColumnIDs {
		if !validID(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

// Existing dangling references remain serializable; only new/revised charts
// require resolvable data. Gaps are excluded, never replaced with zero.
func (c TableChart) canResolve(t TableContext) bool {
	if !c.validShape() {
		return false
	}
	columns := map[string]bool{}
	for _, column := range t.Columns {
		columns[column.ID] = true
	}
	if !columns[c.CategoryColumnID] {
		return false
	}
	for _, id := range c.ValueColumnIDs {
		if !columns[id] {
			return false
		}
	}
	unit, hasUnit, positive := "", false, false
	for _, row := range t.Rows {
		category := slices.IndexFunc(row.Cells, func(cell TableCell) bool { return cell.ColumnID == c.CategoryColumnID })
		if category < 0 {
			return false
		}
		cat := row.Cells[category]
		categoryGap := cat.NeedsReview || cat.Kind == "pending" || (cat.Kind == "text" && strings.TrimSpace(cat.Text) == "")
		for _, id := range c.ValueColumnIDs {
			index := slices.IndexFunc(row.Cells, func(cell TableCell) bool { return cell.ColumnID == id })
			if index < 0 {
				return false
			}
			cell := row.Cells[index]
			if categoryGap || cell.NeedsReview || cell.Kind != "number" {
				if c.Kind == "proportion" {
					return false
				}
				continue
			}
			if !validateTableCellValue(cell) || (hasUnit && unit != cell.Unit) {
				return false
			}
			unit, hasUnit = cell.Unit, true
			value, ok := new(big.Rat).SetString(cell.Number)
			if !ok {
				return false
			}
			if c.Kind == "proportion" && value.Sign() < 0 {
				return false
			}
			positive = positive || value.Sign() > 0
		}
	}
	return c.Kind != "proportion" || positive
}
