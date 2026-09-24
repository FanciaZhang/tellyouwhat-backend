package voice

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func tableContextFixture() Snapshot {
	s, r := tableFixture()
	c := r.TableCreations[0]
	for row := range c.Rows {
		for cell := range c.Rows[row].Cells {
			c.Rows[row].Cells[cell].Sources = nil
		}
	}
	s.BlockComponents = map[string][]string{s.Blocks[0].ID: {c.TableID}}
	s.TableContext = []TableContext{{BlockID: s.Blocks[0].ID, TableID: c.TableID, Title: c.Title, Columns: c.Columns, Rows: c.Rows}}
	return s
}

func TestTableContextPreservesExactDocumentValuesInModelInput(t *testing.T) {
	s := tableContextFixture()
	c := &s.TableContext[0]
	c.Rows[0].Cells[1].Approximate = true
	c.Rows[0].Cells[1].NeedsReview = true
	c.Rows = append(c.Rows, TableRow{ID: uuid.NewString(), Cells: []TableCell{
		{ColumnID: c.Columns[0].ID, Kind: "text", Text: ""},
		{ColumnID: c.Columns[1].ID, Kind: "pending"},
	}})
	// Displayed order must survive without sorting IDs or normalizing decimals.
	c.Columns[0], c.Columns[1] = c.Columns[1], c.Columns[0]
	data, err := rewriteModelInput(s, 1)
	if err != nil {
		t.Fatal(err)
	}
	var output rewriteModelDocument
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.TableContext, output.TableContext) {
		t.Fatal("table state changed in model projection")
	}
	if !strings.Contains(string(data), `"number":"39.90"`) {
		t.Fatal("lost decimal precision")
	}
	// An empty existing table is editable document state, not missing context.
	s.TableContext[0].Rows = []TableRow{}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestTableContextRejectsInvalidIdentityShapeValuesAndSourcePayload(t *testing.T) {
	for name, change := range map[string]func(*Snapshot){
		"unknown owner":        func(s *Snapshot) { s.TableContext[0].BlockID = uuid.NewString() },
		"wrong component":      func(s *Snapshot) { s.TableContext[0].TableID = uuid.NewString() },
		"duplicate table":      func(s *Snapshot) { s.TableContext = append(s.TableContext, s.TableContext[0]) },
		"block collision":      func(s *Snapshot) { s.TableContext[0].Columns[0].ID = s.Blocks[0].ID },
		"component collision":  func(s *Snapshot) { s.TableContext[0].Rows[0].ID = s.TableContext[0].TableID },
		"row column collision": func(s *Snapshot) { s.TableContext[0].Rows[0].ID = s.TableContext[0].Columns[0].ID },
		"duplicate column":     func(s *Snapshot) { s.TableContext[0].Columns[1].ID = s.TableContext[0].Columns[0].ID },
		"missing cell":         func(s *Snapshot) { s.TableContext[0].Rows[0].Cells = s.TableContext[0].Rows[0].Cells[:1] },
		"duplicate cell":       func(s *Snapshot) { s.TableContext[0].Rows[0].Cells[1].ColumnID = s.TableContext[0].Columns[0].ID },
		"invalid decimal":      func(s *Snapshot) { s.TableContext[0].Rows[0].Cells[1].Number = "39.90元" },
		"excess precision":     func(s *Snapshot) { s.TableContext[0].Rows[0].Cells[1].Number = strings.Repeat("1", 29) },
		"pending with value":   func(s *Snapshot) { s.TableContext[0].Rows[0].Cells[1].Kind = "pending" },
		"historical evidence": func(s *Snapshot) {
			s.TableContext[0].Rows[0].Cells[0].Sources = []TableSource{{SourceID: uuid.NewString()}}
		},
		"oversized value": func(s *Snapshot) { s.TableContext[0].Rows[0].Cells[0].Text = strings.Repeat("字", 6001) },
		"blank column":    func(s *Snapshot) { s.TableContext[0].Columns[0].Title = " " },
		"too many tables": func(s *Snapshot) { s.TableContext = make([]TableContext, 5) },
	} {
		t.Run(name, func(t *testing.T) {
			s := tableContextFixture()
			change(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("invalid table accepted")
			}
			if _, err := rewriteModelInput(s, 1); err == nil {
				t.Fatal("invalid context reached model")
			}
		})
	}
}

func TestTableContextEnforcesAggregateContentBudget(t *testing.T) {
	s := tableContextFixture()
	c := &s.TableContext[0]
	c.Rows = nil
	for i := 0; i < 4; i++ {
		c.Rows = append(c.Rows, TableRow{ID: uuid.NewString(), Cells: []TableCell{
			{ColumnID: c.Columns[0].ID, Kind: "text", Text: strings.Repeat("字", 5000)},
			{ColumnID: c.Columns[1].ID, Kind: "pending"},
		}})
	}
	if err := s.Validate(); err == nil {
		t.Fatal("aggregate table content exceeded budget")
	}
	c.Rows = c.Rows[:3]
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}
