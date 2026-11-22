package documents

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
)

func workbookFixture(t *testing.T) []byte {
	t.Helper()
	f := excelize.NewFile()
	defer f.Close()
	for cell, value := range map[string]any{"A1": "Name", "B1": "Quantity", "A2": 2, "B2": 3, "D2": .25, "E2": 45292, "A4": "merged label", "F2": true} {
		if err := f.SetCellValue("Sheet1", cell, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.SetCellFormula("Sheet1", "C2", "SUM(A2:B2)"); err != nil {
		t.Fatal(err)
	}
	percent, err := f.NewStyle(&excelize.Style{NumFmt: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.SetCellStyle("Sheet1", "D2", "D2", percent); err != nil {
		t.Fatal(err)
	}
	date, err := f.NewStyle(&excelize.Style{NumFmt: 14})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.SetCellStyle("Sheet1", "E2", "E2", date); err != nil {
		t.Fatal(err)
	}
	if err = f.MergeCell("Sheet1", "A4", "B4"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.NewSheet("Hidden Data"); err != nil {
		t.Fatal(err)
	}
	if err = f.SetCellValue("Hidden Data", "A1", "secret-looking synthetic fixture"); err != nil {
		t.Fatal(err)
	}
	if err = f.SetSheetVisible("Hidden Data", false); err != nil {
		t.Fatal(err)
	}
	buffer, err := f.WriteToBuffer()
	if err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
func patchWorkbook(t *testing.T, raw []byte, patch func(map[string][]byte)) []byte {
	t.Helper()
	r, e := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if e != nil {
		t.Fatal(e)
	}
	files := map[string][]byte{}
	for _, f := range r.File {
		reader, e := f.Open()
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(reader)
		reader.Close()
		if e != nil {
			t.Fatal(e)
		}
		files[f.Name] = b
	}
	patch(files)
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for name, b := range files {
		writer, e := w.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = writer.Write(b); e != nil {
			t.Fatal(e)
		}
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	return out.Bytes()
}
func replaceSheetData(t *testing.T, raw []byte, data string) []byte {
	return patchWorkbook(t, raw, func(files map[string][]byte) {
		s := string(files["xl/worksheets/sheet1.xml"])
		start := strings.Index(s, "<sheetData>")
		end := strings.Index(s, "</sheetData>")
		if start < 0 || end < 0 {
			t.Fatal("fixture missing sheetData")
		}
		files["xl/worksheets/sheet1.xml"] = []byte(s[:start] + "<sheetData>" + data + "</sheetData>" + s[end+len("</sheetData>"):])
	})
}
func findSpreadsheetCell(t *testing.T, s Spreadsheet, name, address string) SpreadsheetCell {
	t.Helper()
	for _, sheet := range s.Sheets {
		if sheet.Name == name {
			for _, c := range sheet.Cells {
				if c.Address == address {
					return c
				}
			}
		}
	}
	t.Fatalf("cell %s!%s absent", name, address)
	return SpreadsheetCell{}
}
func TestExcelReadsSavedValuesFormulaFormattingAndStructure(t *testing.T) {
	raw := patchWorkbook(t, workbookFixture(t), func(files map[string][]byte) {
		s := string(files["xl/worksheets/sheet1.xml"])
		needle := "<f>SUM(A2:B2)</f>"
		if !strings.Contains(s, needle) {
			t.Fatal("missing formula")
		}
		s = strings.Replace(s, needle, needle+"<v>5</v>", 1)
		files["xl/worksheets/sheet1.xml"] = []byte(s)
	})
	before := append([]byte(nil), raw...)
	got, e := ParseExcel(context.Background(), raw, "xlsx", ExcelLimits{})
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(raw, before) || got.TotalSheets != 2 || len(got.Sheets) != 2 || got.Truncated {
		t.Fatal("workbook changed or sheets lost", got)
	}
	formula := findSpreadsheetCell(t, got, "Sheet1", "C2")
	if formula.Formula != "SUM(A2:B2)" || formula.RawValue != "5" || !formula.CachedValuePresent {
		t.Fatal("formula/cached value lost", formula)
	}
	percent := findSpreadsheetCell(t, got, "Sheet1", "D2")
	if percent.RawValue != "0.25" || percent.Value != "25.00%" {
		t.Fatal("number formatting lost", percent)
	}
	date := findSpreadsheetCell(t, got, "Sheet1", "E2")
	if date.RawValue != "45292" || date.Value == date.RawValue {
		t.Fatal("saved date not formatted", date)
	}
	if got.Sheets[1].Visibility != "hidden" || len(got.Sheets[0].Merges) != 1 || got.Sheets[0].Merges[0] != "A4:B4" {
		t.Fatal("hidden or merge structure lost", got.Sheets)
	}
	if text := SpreadsheetText(got); !strings.Contains(text, "C2:") || !strings.Contains(text, "SUM(A2:B2)") || !strings.Contains(text, "hidden") {
		t.Fatal("model text lost structure")
	}
}
func TestExcelFormulaNeverRecalculatedOrMacrosExecuted(t *testing.T) {
	raw := patchWorkbook(t, workbookFixture(t), func(files map[string][]byte) {
		files["xl/vbaProject.bin"] = []byte("SYNTHETIC_NOT_EXECUTABLE_MACRO")
		files["xl/externalLinks/externalLink1.xml"] = []byte(`<externalLink/>`)
	})
	got, e := ParseExcel(context.Background(), raw, "xlsm", ExcelLimits{})
	if e != nil {
		t.Fatal(e)
	}
	c := findSpreadsheetCell(t, got, "Sheet1", "C2")
	if c.CachedValuePresent || c.RawValue != "" || c.Formula != "SUM(A2:B2)" {
		t.Fatal("missing formula cache was recalculated or invented", c)
	}
	warnings := strings.Join(got.Warnings, " ")
	for _, expected := range []string{"macros were not executed", "external data was not fetched", "not recalculated"} {
		if !strings.Contains(warnings, expected) {
			t.Fatal("missing explicit limitation", warnings)
		}
	}
}
func TestExcelSparseDistantCellsUseBoundedRawFallback(t *testing.T) {
	raw := replaceSheetData(t, workbookFixture(t), `<row r="1048576"><c r="XFD1048576" t="inlineStr"><is><t>far cell</t></is></c><c r="A1048576"><f>2+3</f><v>5</v></c></row>`)
	got, e := ParseExcel(context.Background(), raw, "xlsx", ExcelLimits{})
	if e != nil {
		t.Fatal(e)
	}
	if c := findSpreadsheetCell(t, got, "Sheet1", "XFD1048576"); c.Value != "far cell" {
		t.Fatal(c)
	}
	if c := findSpreadsheetCell(t, got, "Sheet1", "A1048576"); c.Formula != "2+3" || c.RawValue != "5" {
		t.Fatal(c)
	}
	if !strings.Contains(strings.Join(got.Warnings, " "), "without display formatting") {
		t.Fatal("sparse fallback not disclosed")
	}
}
func TestExcelLimitsAreVisibleAndZIPLimitsFailClosed(t *testing.T) {
	raw := workbookFixture(t)
	for _, limit := range []ExcelLimits{{MaxSheets: 1}, {MaxCells: 2}, {MaxOutputBytes: 600}, {MaxCellBytes: 3}} {
		got, e := ParseExcel(context.Background(), raw, "xlsx", limit)
		if e != nil {
			t.Fatal(e)
		}
		if !got.Truncated || len(got.Warnings) == 0 || !strings.Contains(SpreadsheetText(got), "partial") {
			t.Fatal("silent workbook truncation", got)
		}
	}
	for _, limit := range []ExcelLimits{{MaxFileBytes: 10}, {MaxZipEntries: 2}, {MaxEntryBytes: 50}, {MaxUnzipBytes: 100}} {
		if _, e := ParseExcel(context.Background(), raw, "xlsx", limit); !errors.Is(e, ErrSpreadsheetLimit) {
			t.Fatal("ZIP bound ignored", e)
		}
	}
	bad := patchWorkbook(t, raw, func(m map[string][]byte) { m["../escape.xml"] = []byte(`<a/>`) })
	if _, e := ParseExcel(context.Background(), bad, "xlsx", ExcelLimits{}); !errors.Is(e, ErrInvalidSpreadsheet) {
		t.Fatal("unsafe package path accepted")
	}
	deep := replaceSheetData(t, raw, strings.Repeat("<a>", 70)+strings.Repeat("</a>", 70))
	if _, e := ParseExcel(context.Background(), deep, "xlsx", ExcelLimits{}); !errors.Is(e, ErrSpreadsheetLimit) {
		t.Fatal("deep XML accepted", e)
	}
	duplicate := replaceSheetData(t, raw, `<row r="1"><c r="A1"><v>1</v></c><c r="A1"><v>2</v></c></row>`)
	if _, e := ParseExcel(context.Background(), duplicate, "xlsx", ExcelLimits{}); !errors.Is(e, ErrInvalidSpreadsheet) {
		t.Fatal("duplicate cell identity accepted")
	}
}
func TestCSVQuotedUnicodeEncodedAndFormulaText(t *testing.T) {
	src := "name,value\n\"item, quoted\",\"line1\nline2\"\nChinese,中文\nformula,=2+3\n"
	utf16, e := unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewEncoder().Bytes([]byte(src))
	if e != nil {
		t.Fatal(e)
	}
	gb, e := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(src))
	if e != nil {
		t.Fatal(e)
	}
	for _, data := range [][]byte{[]byte(src), append([]byte{0xef, 0xbb, 0xbf}, []byte(src)...), utf16, gb} {
		got, e := ParseExcel(context.Background(), data, "csv", ExcelLimits{})
		if e != nil {
			t.Fatal(e)
		}
		if c := findSpreadsheetCell(t, got, "CSV", "A2"); c.Value != "item, quoted" {
			t.Fatal(c)
		}
		if c := findSpreadsheetCell(t, got, "CSV", "B2"); c.Value != "line1\nline2" {
			t.Fatal(c)
		}
		if c := findSpreadsheetCell(t, got, "CSV", "B3"); c.Value != "中文" {
			t.Fatal(c)
		}
		if c := findSpreadsheetCell(t, got, "CSV", "B4"); c.Value != "=2+3" || c.Formula != "" {
			t.Fatal("CSV formula interpreted", c)
		}
	}
	for _, data := range []string{"A\tB\n1\t2\n", "A;B\n1;2\n"} {
		got, e := ParseExcel(context.Background(), []byte(data), "csv", ExcelLimits{})
		if e != nil || findSpreadsheetCell(t, got, "CSV", "B2").Value != "2" {
			t.Fatal("delimiter failed", e)
		}
	}
}
func TestExcelCancellationCorruptionAndOutputBounds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := ParseExcel(ctx, workbookFixture(t), "xlsx", ExcelLimits{}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	for _, data := range [][]byte{nil, []byte("not zip"), []byte("PK\x03\x04broken")} {
		if _, e := ParseExcel(context.Background(), data, "xlsx", ExcelLimits{}); e == nil {
			t.Fatal("invalid workbook accepted")
		}
	}
	if _, e := ParseExcel(context.Background(), []byte("anything"), "xls", ExcelLimits{}); !errors.Is(e, ErrSpreadsheetFormat) {
		t.Fatal("legacy XLS silently treated as XML")
	}
	if _, e := ParseExcel(context.Background(), []byte("a,b\n\"unterminated"), "csv", ExcelLimits{}); !errors.Is(e, ErrInvalidSpreadsheet) {
		t.Fatal("malformed CSV accepted")
	}
	source := []byte("A,B\n" + strings.Repeat("中", 10000) + ",2\n")
	got, e := ParseExcel(context.Background(), source, "csv", ExcelLimits{MaxCellBytes: 32, MaxOutputBytes: 1000})
	if e != nil {
		t.Fatal(e)
	}
	encoded, _ := json.Marshal(got)
	if !got.Truncated || len(encoded) > 1600 {
		t.Fatal("output not bounded", len(encoded))
	}
	// The XML scanner never accepts DTD/entity expansion directives.
	raw := replaceSheetData(t, workbookFixture(t), `<!DOCTYPE a [<!ENTITY x "boom">]><row r="1"><c r="A1"><v>1</v></c></row>`)
	if _, e := ParseExcel(context.Background(), raw, "xlsx", ExcelLimits{}); e == nil {
		t.Fatal("XML directive accepted")
	}
}

func TestTSVHonorsExplicitSeparatorWithCommaRichCells(t *testing.T) {
	source := []byte("Name, full; display\tAmount, USD; saved\nChicago, IL; USA\t3,200; gross\n")
	for _, format := range []string{"tsv", ".tsv", "text/tab-separated-values"} {
		t.Run(format, func(t *testing.T) {
			got, err := ParseExcel(context.Background(), source, format, ExcelLimits{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Format != "tsv" || got.ParsedCells != 4 {
				t.Fatalf("explicit TSV was parsed as %s with %d cells", got.Format, got.ParsedCells)
			}
			if cell := findSpreadsheetCell(t, got, "TSV", "A2"); cell.Value != "Chicago, IL; USA" {
				t.Fatal("first TSV cell split at punctuation", cell)
			}
			if cell := findSpreadsheetCell(t, got, "TSV", "B2"); cell.Value != "3,200; gross" {
				t.Fatal("second TSV cell split at punctuation", cell)
			}
		})
	}
}

func TestExcelSparseSharedStringsAreResolved(t *testing.T) {
	raw := workbookFixture(t)
	var sheetName string
	archive, _ := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	for _, f := range archive.File {
		if f.Name == "xl/sharedStrings.xml" {
			r, _ := f.Open()
			b, _ := io.ReadAll(r)
			r.Close()
			var d struct {
				Items []spreadsheetInlineString `xml:"si"`
			}
			if xml.Unmarshal(b, &d) == nil && len(d.Items) > 0 {
				sheetName = inlineSpreadsheetText(&d.Items[0])
			}
		}
	}
	if sheetName == "" {
		t.Fatal("fixture shared strings missing")
	}
	raw = replaceSheetData(t, raw, `<row r="1048576"><c r="A1048576" t="s"><v>0</v></c></row>`)
	got, e := ParseExcel(context.Background(), raw, "xlsx", ExcelLimits{})
	if e != nil {
		t.Fatal(e)
	}
	if c := findSpreadsheetCell(t, got, "Sheet1", "A1048576"); c.Value != sheetName {
		t.Fatal("shared text lost", c)
	}
}
