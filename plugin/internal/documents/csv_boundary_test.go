package documents

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func FuzzCSVParsingStaysBoundedAndDoesNotMutateInput(f *testing.F) {
	f.Add([]byte("Name,Value\n\"quoted, name\",=2+3\n"), false)
	f.Add([]byte("Name, full\tAmount, USD\nA, B\t1,200\n"), true)
	f.Add([]byte{0xff, 0xfe, 'A', 0, '\t', 0, 'B', 0, '\n', 0, '1', 0, '\t', 0, '2', 0}, true)
	f.Fuzz(func(t *testing.T, data []byte, tsv bool) {
		if len(data) == 0 || len(data) > 16<<10 {
			return
		}
		before := bytes.Clone(data)
		format := "csv"
		if tsv {
			format = "tsv"
		}
		result, err := ParseExcel(context.Background(), data, format, ExcelLimits{MaxCells: 32, MaxCellBytes: 64, MaxOutputBytes: 4096})
		if !bytes.Equal(before, data) {
			t.Fatal("spreadsheet parsing mutated caller-owned bytes")
		}
		if err != nil {
			return
		}
		if len(result.Sheets) != 1 || result.ParsedCells > 32 || result.ParsedCells != len(result.Sheets[0].Cells) {
			t.Fatal("CSV parsing escaped the cell budget")
		}
		seen := map[string]bool{}
		for _, cell := range result.Sheets[0].Cells {
			if seen[cell.Address] || !validCellRange(cell.Address) || len(cell.Value) > 128 || cell.Formula != "" {
				t.Fatal("CSV parsing changed cell identity, size or treated data as a formula")
			}
			seen[cell.Address] = true
		}
		encoded, _ := json.Marshal(result)
		if len(encoded) > 8192 || len(SpreadsheetText(result)) > 8192 {
			t.Fatal("CSV extracted output escaped its bounded budget")
		}
	})
}
