package documents

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
)

var (
	ErrInvalidSpreadsheet = errors.New("spreadsheet is invalid or encrypted")
	ErrSpreadsheetLimit   = errors.New("spreadsheet exceeds the configured parsing limits")
	ErrSpreadsheetFormat  = errors.New("spreadsheet format is unsupported")
)

// ExcelLimits bounds the amount read, expanded, enumerated and returned. The
// caller should additionally run parsing in its bounded document worker.
type ExcelLimits struct {
	MaxFileBytes   int
	MaxUnzipBytes  int64
	MaxEntryBytes  int64
	MaxZipEntries  int
	MaxSheets      int
	MaxCells       int
	MaxCellBytes   int
	MaxOutputBytes int
}

func DefaultExcelLimits() ExcelLimits {
	return ExcelLimits{MaxFileBytes: 20 << 20, MaxUnzipBytes: 64 << 20, MaxEntryBytes: 16 << 20,
		MaxZipEntries: 2048, MaxSheets: 32, MaxCells: 20000, MaxCellBytes: 8192, MaxOutputBytes: 512 << 10}
}

func normalizeExcelLimits(l ExcelLimits) ExcelLimits {
	d := DefaultExcelLimits()
	if l.MaxFileBytes > 0 && l.MaxFileBytes < d.MaxFileBytes {
		d.MaxFileBytes = l.MaxFileBytes
	}
	if l.MaxUnzipBytes > 0 && l.MaxUnzipBytes < d.MaxUnzipBytes {
		d.MaxUnzipBytes = l.MaxUnzipBytes
	}
	if l.MaxEntryBytes > 0 && l.MaxEntryBytes < d.MaxEntryBytes {
		d.MaxEntryBytes = l.MaxEntryBytes
	}
	if d.MaxEntryBytes > d.MaxUnzipBytes {
		d.MaxEntryBytes = d.MaxUnzipBytes
	}
	if l.MaxZipEntries > 0 && l.MaxZipEntries < d.MaxZipEntries {
		d.MaxZipEntries = l.MaxZipEntries
	}
	if l.MaxSheets > 0 && l.MaxSheets < d.MaxSheets {
		d.MaxSheets = l.MaxSheets
	}
	if l.MaxCells > 0 && l.MaxCells < d.MaxCells {
		d.MaxCells = l.MaxCells
	}
	if l.MaxCellBytes > 0 && l.MaxCellBytes < d.MaxCellBytes {
		d.MaxCellBytes = l.MaxCellBytes
	}
	if l.MaxOutputBytes > 0 && l.MaxOutputBytes < d.MaxOutputBytes {
		d.MaxOutputBytes = l.MaxOutputBytes
	}
	return d
}

type Spreadsheet struct {
	Format      string             `json:"format"`
	Sheets      []SpreadsheetSheet `json:"sheets"`
	TotalSheets int                `json:"total_sheets"`
	ParsedCells int                `json:"parsed_cells"`
	Truncated   bool               `json:"truncated"`
	Warnings    []string           `json:"warnings,omitempty"`
}
type SpreadsheetSheet struct {
	Name       string            `json:"name"`
	Visibility string            `json:"visibility"`
	Dimension  string            `json:"declared_dimension,omitempty"`
	Cells      []SpreadsheetCell `json:"cells"`
	Merges     []string          `json:"merged_ranges,omitempty"`
	Truncated  bool              `json:"truncated"`
}
type SpreadsheetCell struct {
	Address            string `json:"address"`
	Value              string `json:"display_value"`
	RawValue           string `json:"raw_value"`
	Formula            string `json:"formula,omitempty"`
	CachedValuePresent bool   `json:"cached_value_present"`
	Type               string `json:"type,omitempty"`
}

type excelBudget struct {
	cells, bytes, visited int
	limits                ExcelLimits
}

func (b *excelBudget) add(cell SpreadsheetCell) bool {
	// Include a conservative allowance for field names/JSON escaping so output
	// does not grow with unusually sparse addresses or control characters.
	cost := 96 + len(cell.Address) + 6*(len(cell.Value)+len(cell.RawValue)+len(cell.Formula))
	if b.cells >= b.limits.MaxCells || cost > b.limits.MaxOutputBytes-b.bytes {
		return false
	}
	b.cells++
	b.bytes += cost
	return true
}
func spreadsheetWarning(out *Spreadsheet, message string) {
	for _, old := range out.Warnings {
		if old == message {
			return
		}
	}
	if len(out.Warnings) < 16 {
		out.Warnings = append(out.Warnings, message)
	}
}
func boundedCellText(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end] + "… [truncated]", true
}

// ParseExcel reads values, formatting and saved formulas. It never calculates
// formulas, follows external links, executes macros or modifies the workbook.
// format is an extension or MIME type, not an arbitrary filesystem path.
func ParseExcel(ctx context.Context, data []byte, format string, limits ExcelLimits) (Spreadsheet, error) {
	l := normalizeExcelLimits(limits)
	out := Spreadsheet{Sheets: []SpreadsheetSheet{}}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if len(data) == 0 {
		return out, ErrInvalidSpreadsheet
	}
	if len(data) > l.MaxFileBytes {
		return out, ErrSpreadsheetLimit
	}
	switch strings.ToLower(strings.TrimPrefix(strings.TrimSpace(format), ".")) {
	case "csv", "text/csv":
		return parseCSV(ctx, data, l, 0)
	case "tsv", "text/tab-separated-values":
		return parseCSV(ctx, data, l, '\t')
	case "xlsx", "xlsm", "xltx", "xltm", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "application/vnd.ms-excel.sheet.macroenabled.12":
		out.Format = "xlsx"
		if strings.EqualFold(format, "xlsm") || strings.EqualFold(format, ".xlsm") {
			out.Format = "xlsm"
		}
	default:
		return out, ErrSpreadsheetFormat
	}
	files, err := preflightWorkbook(ctx, data, l)
	if err != nil {
		return out, err
	}
	sheets, err := readWorkbookSheets(ctx, files, l)
	if err != nil {
		return out, err
	}
	out.TotalSheets = len(sheets)
	if len(sheets) == 0 {
		return out, ErrInvalidSpreadsheet
	}
	temp, err := os.MkdirTemp("", "bps-excel-")
	if err != nil {
		return out, ErrInvalidSpreadsheet
	}
	defer os.RemoveAll(temp)
	book, err := excelize.OpenReader(bytes.NewReader(data), excelize.Options{UnzipSizeLimit: l.MaxUnzipBytes, UnzipXMLSizeLimit: l.MaxEntryBytes, TmpDir: temp})
	if err != nil {
		return out, ErrInvalidSpreadsheet
	}
	defer book.Close()
	if files["xl/vbaProject.bin"] != nil {
		spreadsheetWarning(&out, "Workbook contains macros; macros were not executed.")
	}
	for name := range files {
		if strings.HasPrefix(name, "xl/externalLinks/") {
			spreadsheetWarning(&out, "Workbook contains external links; external data was not fetched.")
		}
		if strings.HasPrefix(name, "xl/drawings/") || strings.HasPrefix(name, "xl/charts/") {
			spreadsheetWarning(&out, "Workbook contains drawings or charts; this extraction includes worksheet cells and structure only.")
		}
	}
	budget := excelBudget{limits: l}
	for index, sheet := range sheets {
		if err := ctx.Err(); err != nil {
			return Spreadsheet{}, err
		}
		if index >= l.MaxSheets {
			out.Truncated = true
			spreadsheetWarning(&out, "Only the first configured number of worksheets was parsed; additional worksheets were omitted.")
			break
		}
		if 96+6*len(sheet.name) > l.MaxOutputBytes-budget.bytes {
			out.Truncated = true
			spreadsheetWarning(&out, "Workbook extracted-output limit reached; additional worksheets were omitted.")
			break
		}
		parsed, stop, err := parseWorksheet(ctx, files[sheet.file], files["xl/sharedStrings.xml"], sheet, book, &budget, &out)
		if err != nil {
			return Spreadsheet{}, err
		}
		out.Sheets = append(out.Sheets, parsed)
		if stop {
			out.Truncated = true
			spreadsheetWarning(&out, "Workbook cell or extracted-output limit reached; additional cells and worksheets were omitted.")
			break
		}
	}
	out.ParsedCells = budget.cells
	if out.ParsedCells == 0 {
		spreadsheetWarning(&out, "No nonempty worksheet cells were extracted.")
	}
	return out, nil
}

func preflightWorkbook(ctx context.Context, data []byte, l ExcelLimits) (map[string]*zip.File, error) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, ErrInvalidSpreadsheet
	}
	if len(archive.File) > l.MaxZipEntries {
		return nil, ErrSpreadsheetLimit
	}
	files := map[string]*zip.File{}
	var total uint64
	for _, f := range archive.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := f.Name
		if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || path.Clean(name) != strings.TrimSuffix(name, "/") || strings.HasPrefix(name, "../") {
			return nil, ErrInvalidSpreadsheet
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if files[name] != nil {
			return nil, ErrInvalidSpreadsheet
		}
		if f.UncompressedSize64 > uint64(l.MaxEntryBytes) || total > uint64(l.MaxUnzipBytes)-f.UncompressedSize64 {
			return nil, ErrSpreadsheetLimit
		}
		total += f.UncompressedSize64
		files[name] = f
	}
	if files["[Content_Types].xml"] == nil || files["xl/workbook.xml"] == nil || files["xl/_rels/workbook.xml.rels"] == nil {
		return nil, ErrInvalidSpreadsheet
	}
	return files, nil
}

type workbookSheet struct{ name, state, file string }

func readZipXML(ctx context.Context, f *zip.File, l ExcelLimits) ([]byte, error) {
	if f == nil {
		return nil, ErrInvalidSpreadsheet
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, err := f.Open()
	if err != nil {
		return nil, ErrInvalidSpreadsheet
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, l.MaxEntryBytes+1))
	if err != nil {
		return nil, ErrInvalidSpreadsheet
	}
	if int64(len(data)) > l.MaxEntryBytes {
		return nil, ErrSpreadsheetLimit
	}
	return data, nil
}
func readWorkbookSheets(ctx context.Context, files map[string]*zip.File, l ExcelLimits) ([]workbookSheet, error) {
	relRaw, err := readZipXML(ctx, files["xl/_rels/workbook.xml.rels"], l)
	if err != nil {
		return nil, err
	}
	var rels struct {
		Items []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
			Type   string `xml:"Type,attr"`
			Mode   string `xml:"TargetMode,attr"`
		} `xml:"Relationship"`
	}
	if xml.Unmarshal(relRaw, &rels) != nil {
		return nil, ErrInvalidSpreadsheet
	}
	targets := map[string]string{}
	for _, r := range rels.Items {
		if r.Mode == "External" || !strings.HasSuffix(r.Type, "/worksheet") {
			continue
		}
		target := r.Target
		if strings.Contains(target, "\\") || strings.Contains(target, ":") {
			return nil, ErrInvalidSpreadsheet
		}
		if strings.HasPrefix(target, "/") {
			target = strings.TrimPrefix(target, "/")
		} else {
			target = path.Join("xl", target)
		}
		target = path.Clean(target)
		if !strings.HasPrefix(target, "xl/") || files[target] == nil || targets[r.ID] != "" {
			return nil, ErrInvalidSpreadsheet
		}
		targets[r.ID] = target
	}
	raw, err := readZipXML(ctx, files["xl/workbook.xml"], l)
	if err != nil {
		return nil, err
	}
	var workbook struct {
		Sheets []struct {
			Name  string `xml:"name,attr"`
			State string `xml:"state,attr"`
			ID    string `xml:"id,attr"`
		} `xml:"sheets>sheet"`
	}
	if xml.Unmarshal(raw, &workbook) != nil {
		return nil, ErrInvalidSpreadsheet
	}
	result := make([]workbookSheet, 0, min(len(workbook.Sheets), l.MaxSheets+1))
	names := map[string]bool{}
	for _, s := range workbook.Sheets {
		if s.Name == "" || len(s.Name) > 512 || targets[s.ID] == "" || names[s.Name] {
			return nil, ErrInvalidSpreadsheet
		}
		state := s.State
		if state == "" {
			state = "visible"
		}
		if state != "visible" && state != "hidden" && state != "veryHidden" {
			return nil, ErrInvalidSpreadsheet
		}
		names[s.Name] = true
		result = append(result, workbookSheet{s.Name, state, targets[s.ID]})
	}
	return result, nil
}

type rawSpreadsheetCell struct {
	Reference string                   `xml:"r,attr"`
	Type      string                   `xml:"t,attr"`
	Value     *string                  `xml:"v"`
	Inline    *spreadsheetInlineString `xml:"is"`
	Formula   *struct {
		Text   string `xml:",chardata"`
		Type   string `xml:"t,attr"`
		Shared string `xml:"si,attr"`
		Ref    string `xml:"ref,attr"`
	} `xml:"f"`
}

type spreadsheetInlineString struct {
	Text string `xml:"t"`
	Runs []struct {
		Text string `xml:"t"`
	} `xml:"r"`
}

func inlineSpreadsheetText(value *spreadsheetInlineString) string {
	if value == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(value.Text)
	for _, r := range value.Runs {
		b.WriteString(r.Text)
	}
	return b.String()
}

func parseWorksheet(ctx context.Context, file, sharedFile *zip.File, sheet workbookSheet, book *excelize.File, budget *excelBudget, out *Spreadsheet) (SpreadsheetSheet, bool, error) {
	result := SpreadsheetSheet{Name: sheet.name, Visibility: sheet.state, Cells: []SpreadsheetCell{}}
	budget.bytes += 96 + 6*len(sheet.name)
	raw, err := readZipXML(ctx, file, budget.limits)
	if err != nil {
		return result, false, err
	}
	sparse, err := sparseWorksheet(ctx, raw)
	if err != nil {
		return result, false, err
	}
	var shared map[int]string
	if sparse {
		spreadsheetWarning(out, "Sparse worksheet has a large row/column span; saved raw values are shown without display formatting.")
		shared, err = readNeededSharedStrings(ctx, sharedFile, raw, budget.limits)
		if err != nil {
			return result, false, err
		}
	}
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	depth := 0
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return result, false, err
		}
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, false, ErrInvalidSpreadsheet
		}
		switch t := tok.(type) {
		case xml.Directive:
			return result, false, ErrInvalidSpreadsheet
		case xml.StartElement:
			depth++
			if depth > 64 {
				return result, false, ErrSpreadsheetLimit
			}
			switch t.Name.Local {
			case "dimension":
				for _, a := range t.Attr {
					if a.Name.Local == "ref" && len(a.Value) <= 32 {
						result.Dimension = a.Value
					}
				}
			case "mergeCell":
				for _, a := range t.Attr {
					if a.Name.Local == "ref" {
						if len(result.Merges) >= 1024 || 32+len(a.Value) > budget.limits.MaxOutputBytes-budget.bytes {
							result.Truncated = true
							out.Truncated = true
							spreadsheetWarning(out, "Merged-range metadata was truncated.")
							break
						}
						if validCellRange(a.Value) {
							result.Merges = append(result.Merges, a.Value)
							budget.bytes += 32 + len(a.Value)
						}
					}
				}
			case "c":
				budget.visited++
				if budget.visited > max(1000, budget.limits.MaxCells*4) {
					result.Truncated = true
					spreadsheetWarning(out, "Worksheet enumeration limit reached; remaining cells were omitted.")
					return result, true, nil
				}
				var cell rawSpreadsheetCell
				if decoder.DecodeElement(&cell, &t) != nil {
					return result, false, ErrInvalidSpreadsheet
				}
				depth--
				col, row, err := excelize.CellNameToCoordinates(cell.Reference)
				if err != nil || col < 1 || col > 16384 || row < 1 || row > 1048576 || seen[cell.Reference] {
					return result, false, ErrInvalidSpreadsheet
				}
				seen[cell.Reference] = true
				if cell.Value == nil && cell.Formula == nil && cell.Inline == nil {
					continue
				}
				value, rawValue := "", ""
				formula := ""
				if sparse {
					if cell.Value != nil {
						rawValue = *cell.Value
					}
					value = rawValue
					switch cell.Type {
					case "s":
						index, parseErr := strconv.Atoi(rawValue)
						if parseErr != nil {
							return result, false, ErrInvalidSpreadsheet
						}
						var exists bool
						value, exists = shared[index]
						if !exists {
							return result, false, ErrInvalidSpreadsheet
						}
						rawValue = value
					case "inlineStr":
						value = inlineSpreadsheetText(cell.Inline)
						rawValue = value
					case "b":
						if rawValue == "1" {
							value = "TRUE"
						} else if rawValue == "0" {
							value = "FALSE"
						}
					}
					if cell.Formula != nil {
						formula = cell.Formula.Text
						if formula == "" && cell.Formula.Type == "shared" {
							formula = "[shared formula index " + cell.Formula.Shared + "; not expanded]"
							spreadsheetWarning(out, "Shared formulas on sparse worksheets were not expanded; saved cached values are retained.")
						}
					}
				} else {
					value, err = book.GetCellValue(sheet.name, cell.Reference)
					if err != nil {
						return result, false, ErrInvalidSpreadsheet
					}
					rawValue, err = book.GetCellValue(sheet.name, cell.Reference, excelize.Options{RawCellValue: true})
					if err != nil {
						return result, false, ErrInvalidSpreadsheet
					}
				}
				if cell.Formula != nil && !sparse {
					formula, err = book.GetCellFormula(sheet.name, cell.Reference)
					if err != nil {
						return result, false, ErrInvalidSpreadsheet
					}
				}
				if value == "" && rawValue == "" && formula == "" {
					continue
				}
				var clipped bool
				for _, v := range []*string{&value, &rawValue, &formula} {
					text, cut := boundedCellText(*v, budget.limits.MaxCellBytes)
					*v = text
					clipped = clipped || cut
				}
				if clipped {
					result.Truncated = true
					out.Truncated = true
					spreadsheetWarning(out, "One or more cell values or formulas were truncated.")
				}
				cached := cell.Value != nil && (*cell.Value != "" || cell.Type == "str")
				entry := SpreadsheetCell{Address: cell.Reference, Value: value, RawValue: rawValue, Formula: formula, CachedValuePresent: cached, Type: cell.Type}
				if cell.Formula != nil && !cached {
					spreadsheetWarning(out, "Some formulas have no saved cached value; formulas were not recalculated.")
				}
				if !budget.add(entry) {
					result.Truncated = true
					return result, true, nil
				}
				result.Cells = append(result.Cells, entry)
			}
		case xml.EndElement:
			depth--
		}
	}
	return result, false, nil
}
func validCellRange(value string) bool {
	parts := strings.Split(value, ":")
	if len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		c, r, e := excelize.CellNameToCoordinates(p)
		if e != nil || c < 1 || c > 16384 || r < 1 || r > 1048576 {
			return false
		}
	}
	return true
}

func parseCSV(ctx context.Context, data []byte, l ExcelLimits, delimiter rune) (Spreadsheet, error) {
	out := Spreadsheet{Format: "csv", TotalSheets: 1, Sheets: []SpreadsheetSheet{{Name: "CSV", Visibility: "visible", Cells: []SpreadsheetCell{}}}}
	if delimiter == '\t' {
		out.Format = "tsv"
		out.Sheets[0].Name = "TSV"
	}
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		data = data[3:]
	}
	if bytes.HasPrefix(data, []byte{0xff, 0xfe}) || bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		decoded, err := unicode.UTF16(unicode.LittleEndian, unicode.ExpectBOM).NewDecoder().Bytes(data)
		if err != nil || len(data)%2 != 0 || bytes.Contains(decoded, []byte("\uFFFD")) {
			return out, ErrInvalidSpreadsheet
		}
		data = decoded
	} else if !utf8.Valid(data) {
		decoded, err := simplifiedchinese.GB18030.NewDecoder().Bytes(data)
		if err != nil || !utf8.Valid(decoded) || bytes.Contains(decoded, []byte("\uFFFD")) {
			return out, ErrInvalidSpreadsheet
		}
		data = decoded
		spreadsheetWarning(&out, "CSV was decoded as GB18030 because it was not UTF-8.")
	}
	// Prefer the separator with a stable multi-field first record. The reader
	// validates every subsequent record rather than splitting quoted strings.
	// An explicit TSV format already fixes its separator, even when cells have
	// commas or semicolons that would otherwise win this heuristic.
	type candidate struct {
		separator rune
		fields    int
	}
	candidates := []candidate{}
	if delimiter == 0 {
		delimiter = ','
		for _, sep := range []rune{',', '\t', ';'} {
			r := csv.NewReader(bytes.NewReader(data))
			r.Comma = sep
			r.FieldsPerRecord = -1
			if row, err := r.Read(); err == nil && len(row) > 1 {
				candidates = append(candidates, candidate{sep, len(row)})
			}
		}
		sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].fields > candidates[j].fields })
		if len(candidates) > 0 {
			delimiter = candidates[0].separator
		}
	}
	reader := csv.NewReader(bytes.NewReader(data))
	reader.Comma = delimiter
	reader.FieldsPerRecord = -1
	budget := excelBudget{limits: l}
	for row := 1; ; row++ {
		if err := ctx.Err(); err != nil {
			return Spreadsheet{}, err
		}
		values, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Spreadsheet{}, ErrInvalidSpreadsheet
		}
		if row > 1048576 || len(values) > 16384 {
			return Spreadsheet{}, ErrSpreadsheetLimit
		}
		for col, value := range values {
			if value == "" {
				continue
			}
			text, cut := boundedCellText(value, l.MaxCellBytes)
			if cut {
				out.Truncated = true
				out.Sheets[0].Truncated = true
				spreadsheetWarning(&out, "One or more CSV fields were truncated.")
			}
			address, _ := excelize.CoordinatesToCellName(col+1, row)
			cell := SpreadsheetCell{Address: address, Value: text, RawValue: text, Type: "text", CachedValuePresent: true}
			if !budget.add(cell) {
				out.Truncated = true
				out.Sheets[0].Truncated = true
				spreadsheetWarning(&out, "CSV cell or extracted-output limit reached; additional data was omitted.")
				out.ParsedCells = budget.cells
				return out, nil
			}
			out.Sheets[0].Cells = append(out.Sheets[0].Cells, cell)
		}
	}
	out.ParsedCells = budget.cells
	if out.ParsedCells == 0 {
		spreadsheetWarning(&out, "The CSV contains no nonempty cells.")
	}
	return out, nil
}

// SpreadsheetText is a compact, coordinate-preserving representation for model
// input. Values are quoted so embedded newlines remain visibly inside a cell.
func SpreadsheetText(book Spreadsheet) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Spreadsheet (%s), %d worksheet(s). Values are saved values; formulas are not recalculated.\n", book.Format, book.TotalSheets)
	for _, sheet := range book.Sheets {
		fmt.Fprintf(&b, "\nWorksheet %q [%s]\n", sheet.Name, sheet.Visibility)
		if len(sheet.Merges) > 0 {
			fmt.Fprintf(&b, "Merged ranges: %s\n", strings.Join(sheet.Merges, ", "))
		}
		for _, cell := range sheet.Cells {
			fmt.Fprintf(&b, "%s: %s", cell.Address, strconv.Quote(cell.Value))
			if cell.RawValue != cell.Value {
				fmt.Fprintf(&b, " (raw=%s)", strconv.Quote(cell.RawValue))
			}
			if cell.Formula != "" {
				fmt.Fprintf(&b, " formula=%s; cached=%t", strconv.Quote(cell.Formula), cell.CachedValuePresent)
			}
			b.WriteByte('\n')
		}
		if sheet.Truncated {
			b.WriteString("[Worksheet content was truncated by parsing limits.]\n")
		}
	}
	for _, warning := range book.Warnings {
		fmt.Fprintf(&b, "Warning: %s\n", warning)
	}
	if book.Truncated {
		b.WriteString("[Document is partial; do not assume omitted data is empty.]\n")
	}
	return b.String()
}

// Excelize's formatting reader materializes the row span. A sparse distant
// cell must not cause a million-row allocation just to format one value.
func sparseWorksheet(ctx context.Context, raw []byte) (bool, error) {
	d := xml.NewDecoder(bytes.NewReader(raw))
	sparse := false
	depth := 0
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		tok, err := d.Token()
		if err == io.EOF {
			return sparse, nil
		}
		if err != nil {
			return false, ErrInvalidSpreadsheet
		}
		switch t := tok.(type) {
		case xml.Directive:
			return false, ErrInvalidSpreadsheet
		case xml.StartElement:
			depth++
			if depth > 64 {
				return false, ErrSpreadsheetLimit
			}
			if t.Name.Local == "row" {
				for _, a := range t.Attr {
					if a.Name.Local == "r" {
						row, e := strconv.Atoi(a.Value)
						if e != nil || row < 1 || row > 1048576 {
							return false, ErrInvalidSpreadsheet
						}
						sparse = sparse || row > 100000
					}
				}
			}
			if t.Name.Local == "c" {
				for _, a := range t.Attr {
					if a.Name.Local == "r" {
						c, r, e := excelize.CellNameToCoordinates(a.Value)
						if e != nil || c < 1 || c > 16384 || r < 1 || r > 1048576 {
							return false, ErrInvalidSpreadsheet
						}
						sparse = sparse || r > 100000 || c > 2048
					}
				}
			}
		case xml.EndElement:
			depth--
		}
	}
}

func readNeededSharedStrings(ctx context.Context, file *zip.File, sheet []byte, l ExcelLimits) (map[int]string, error) {
	needed := map[int]bool{}
	d := xml.NewDecoder(bytes.NewReader(sheet))
	visited := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tok, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, ErrInvalidSpreadsheet
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "c" {
			continue
		}
		visited++
		if visited > max(1000, l.MaxCells*4) {
			break
		}
		var c rawSpreadsheetCell
		if d.DecodeElement(&c, &start) != nil {
			return nil, ErrInvalidSpreadsheet
		}
		if c.Type == "s" && c.Value != nil {
			n, e := strconv.Atoi(*c.Value)
			if e != nil || n < 0 || n > 1000000 {
				return nil, ErrInvalidSpreadsheet
			}
			needed[n] = true
		}
	}
	result := map[int]string{}
	if len(needed) == 0 {
		return result, nil
	}
	raw, e := readZipXML(ctx, file, l)
	if e != nil {
		return nil, e
	}
	d = xml.NewDecoder(bytes.NewReader(raw))
	index := 0
	for len(result) < len(needed) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tok, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, ErrInvalidSpreadsheet
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "si" {
			continue
		}
		if needed[index] {
			var s spreadsheetInlineString
			if d.DecodeElement(&s, &start) != nil {
				return nil, ErrInvalidSpreadsheet
			}
			result[index] = inlineSpreadsheetText(&s)
		} else if d.Skip() != nil {
			return nil, ErrInvalidSpreadsheet
		}
		index++
	}
	if len(result) != len(needed) {
		return nil, ErrInvalidSpreadsheet
	}
	return result, nil
}
