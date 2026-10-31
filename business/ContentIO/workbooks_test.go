package business

// Tables S1: Excel workbooks and OpenDocument spreadsheets read as sheets of
// typed cells. testdata/sheets/libreoffice-deals.{xlsx,ods} are LibreOffice
// 24.2's own files, converted from deals.fods beside them (soffice --headless
// --convert-to xlsx / ods); the workbooks built here take the shapes ECMA-376
// lets other writers use: shared strings with runs and phonetic guides,
// number formats by id, a 1904 workbook, cells without references, the
// strict namespaces.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// shown is a cell as these tests read it: text as it is, a typed value as
// its kind and value, after "=" when a formula worked it out.
func shown(c Cell) string {
	var s string
	switch c.Kind {
	case CellText:
		s = c.Text
	case CellNumber:
		s = "n:" + strconv.FormatFloat(c.Number, 'f', -1, 64)
		if c.Percent {
			s += "%"
		}
		if c.Currency != "" {
			s += " " + c.Currency
		}
	case CellBool:
		s = fmt.Sprintf("b:%t", c.Bool)
	case CellDate:
		s = "d:" + c.Time.Format("2006-01-02 15:04:05")
	case CellDuration:
		s = "dur:" + strconv.FormatFloat(c.Number, 'f', -1, 64)
	case CellError:
		s = "e:" + c.Text
	}
	if c.Formula {
		s = "=" + s
	}
	return s
}

// shownRows reads a sheet's rows as shown.
func shownRows(t *testing.T, s Sheet) [][]string {
	t.Helper()
	var out [][]string
	if err := s.Rows(func(cells []Cell) error {
		row := make([]string, len(cells))
		for i, c := range cells {
			row[i] = shown(c)
		}
		out = append(out, row)
		return nil
	}); err != nil {
		t.Fatalf("reading %s: %v", s.Name, err)
	}
	return out
}

// fixture is a file of testdata/sheets as an import is given it.
func fixture(t *testing.T, name string) Input {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sheets", name))
	if err != nil {
		t.Fatal(err)
	}
	return sheetOf(name, data)
}

// sheetsOf is what ReadSheets says of in: its sheets' names, hidden ones
// marked, and each one's rows as shown.
func sheetsOf(t *testing.T, in Input) ([]string, map[string][][]string) {
	t.Helper()
	sheets, err := ReadSheets(in)
	if err != nil {
		t.Fatalf("%s: %v", in.Filename, err)
	}
	var names []string
	rows := map[string][][]string{}
	for _, s := range sheets {
		name := s.Name
		if s.Hidden {
			name += " (hidden)"
		}
		names = append(names, name)
		rows[s.Name] = shownRows(t, s)
	}
	return names, rows
}

// A workbook LibreOffice wrote, as an Excel file and as an OpenDocument
// file, reads as the same sheets of typed cells: amounts with their
// currencies, percents as they show, days with their times, checkboxes,
// lengths of time and times of day, a formula's value and its error, rich
// and two-line text, a merged cell's value in its first cell, and blank rows
// left out.
func TestAWorkbookIsReadAsItsSheets(t *testing.T) {
	deals := [][]string{
		{"Name", "Region", "Amount", "Fee", "Share", "Closed", "Signed", "Won", "Hours", "Starts", "Total", "Notes", "Ratio", "Code", "Size"},
		{"Ada", "North", "n:1200.5 USD", "n:50 INR", "n:12.5%", "d:2026-10-23 00:00:00", "d:2026-10-23 14:30:00", "b:true", "dur:5400", "dur:34200", "=n:2401 USD", "Big deal", "=e:#DIV/0!", "00123", "n:3"},
		{"Bo", "", "n:30 USD", "n:100000 INR", "n:7%", "d:2026-01-05 00:00:00", "d:2026-01-05 09:05:00", "b:false", "dur:94500", "dur:61200", "=n:60 USD", "two\nlines", "n:0.5", "00456", "n:3"},
		{"Cy", "South", "n:4 USD", "n:1 INR", "n:100%", "d:2026-02-01 00:00:00", "d:2026-02-01 00:00:00", "b:true", "dur:2700", "dur:28800", "=n:8 USD", "", "n:0.25", "00789", "n:3"},
	}
	for _, file := range []string{"libreoffice-deals.xlsx", "libreoffice-deals.ods"} {
		t.Run(file, func(t *testing.T) {
			in := fixture(t, file)
			names, rows := sheetsOf(t, in)
			if want := "[Deals Lists (hidden) Empty]"; fmt.Sprint(names) != want {
				t.Errorf("the sheets are %v, want %s", names, want)
			}
			for i, want := range deals {
				if i >= len(rows["Deals"]) || fmt.Sprintf("%q", rows["Deals"][i]) != fmt.Sprintf("%q", want) {
					t.Errorf("Deals' row %d reads %q,\nwant %q", i+1, at(rows["Deals"], i), want)
				}
			}
			if len(rows["Deals"]) != len(deals) {
				t.Errorf("Deals has %d rows with values, want %d", len(rows["Deals"]), len(deals))
			}
			if got := fmt.Sprintf("%q", rows["Lists"]); got != `[["Status"] ["Open"] ["Closed"]]` {
				t.Errorf("Lists reads %s", got)
			}
			if len(rows["Empty"]) != 0 {
				t.Errorf("an empty sheet reads %q", rows["Empty"])
			}
			_, again := sheetsOf(t, in)
			if fmt.Sprint(again) != fmt.Sprint(rows) {
				t.Error("read again, the sheets read otherwise")
			}
		})
	}
}

// at is rows' i-th row, or none.
func at(rows [][]string, i int) []string {
	if i < len(rows) {
		return rows[i]
	}
	return nil
}

const (
	pkgRels  = "http://schemas.openxmlformats.org/package/2006/relationships"
	officeNS = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
)

// rels is a part's relationships, each "id type target".
func rels(base string, each ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="%s">`, pkgRels)
	for _, r := range each {
		f := strings.Fields(r)
		fmt.Fprintf(&b, `<Relationship Id="%s" Type="%s/%s" Target="%s"/>`, f[0], base, f[1], f[2])
	}
	return b.String() + `</Relationships>`
}

// excelParts is a workbook in the shape Excel writes: a 1904 workbook whose
// first sheet, Plan, is named by an absolute target; a chart sheet; a very
// hidden sheet of lookups; number formats by built-in id and by code; shared
// strings with runs, a phonetic guide and an escaped carriage return.
func excelParts(plan string) map[string][]byte {
	const ns = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="` + officeNS + `"`
	return map[string][]byte{
		"[Content_Types].xml": []byte(`<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`),
		"_rels/.rels":         []byte(rels(officeNS, "rId1 officeDocument xl/workbook.xml")),
		"xl/workbook.xml": []byte(`<?xml version="1.0"?><workbook ` + ns + `><workbookPr date1904="1"/><sheets>` +
			`<sheet name="Plan" sheetId="1" r:id="rId1"/><sheet name="Chart1" sheetId="2" r:id="rId2"/><sheet name="Lookups" sheetId="3" state="veryHidden" r:id="rId3"/></sheets></workbook>`),
		"xl/_rels/workbook.xml.rels": []byte(rels(officeNS, "rId1 worksheet /xl/worksheets/sheet1.xml", "rId2 chartsheet chartsheets/sheet1.xml",
			"rId3 worksheet worksheets/sheet2.xml", "rId4 styles styles.xml", "rId5 sharedStrings sharedStrings.xml")),
		"xl/styles.xml": []byte(`<?xml version="1.0"?><styleSheet ` + ns + `><numFmts count="4">` +
			`<numFmt numFmtId="164" formatCode="[$-409]d\-mmm\-yy;@"/><numFmt numFmtId="165" formatCode="&quot;₹&quot;\ #,##0.00"/>` +
			`<numFmt numFmtId="166" formatCode="#,##0.00\ [$€-1]"/><numFmt numFmtId="167" formatCode="0.00E+00"/></numFmts>` +
			`<cellXfs count="10"><xf numFmtId="0"/><xf numFmtId="14"/><xf numFmtId="22"/><xf numFmtId="10"/><xf numFmtId="46"/>` +
			`<xf numFmtId="164"/><xf numFmtId="165"/><xf numFmtId="166"/><xf numFmtId="167"/><xf numFmtId="20"/></cellXfs></styleSheet>`),
		"xl/sharedStrings.xml": []byte(`<?xml version="1.0"?><sst ` + ns + ` count="15" uniqueCount="15">` +
			`<si><t>Item</t></si><si><t>When</t></si><si><t>Due</t></si><si><t>Share</t></si><si><t>Time</t></si><si><t>Rupees</t></si>` +
			`<si><t>Euros</t></si><si><t>Big</t></si><si><t>Note</t></si><si><t>Done</t></si><si><t>Check</t></si><si><t>Starts</t></si>` +
			`<si><r><rPr><b/></rPr><t>Big</t></r><r><t xml:space="preserve"> deal</t></r></si>` +
			`<si><t>漢字</t><rPh sb="0" eb="2"><t>かんじ</t></rPh><phoneticPr fontId="1"/></si>` +
			"<si><t>line one_x000D_\nline two, _x005F_x000D_ kept</t></si></sst>"),
		"xl/worksheets/sheet1.xml": []byte(`<?xml version="1.0"?><worksheet ` + ns + `><dimension ref="A1:L6"/><sheetData>` + plan + `</sheetData>` +
			`<mergeCells count="1"><mergeCell ref="A1:B1"/></mergeCells></worksheet>`),
		"xl/worksheets/sheet2.xml": []byte(`<?xml version="1.0"?><worksheet ` + ns + `><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>Open</t></is></c></row></sheetData></worksheet>`),
	}
}

const excelPlan = `<row r="1" spans="1:12">` +
	`<c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>2</v></c><c r="D1" t="s"><v>3</v></c>` +
	`<c r="E1" t="s"><v>4</v></c><c r="F1" t="s"><v>5</v></c><c r="G1" t="s"><v>6</v></c><c r="H1" t="s"><v>7</v></c>` +
	`<c r="I1" t="s"><v>8</v></c><c r="J1" t="s"><v>9</v></c><c r="K1" t="s"><v>10</v></c><c r="L1" t="s"><v>11</v></c></row>` +
	`<row r="3"><c r="A3" t="s"><v>12</v></c><c r="B3" s="1"><v>44856</v></c><c r="C3" s="2"><v>44856.5</v></c>` +
	`<c r="D3" s="3"><v>0.125</v></c><c r="E3" s="4"><v>1.09375</v></c><c r="F3" s="6"><v>1200.5</v></c>` +
	`<c r="G3" s="7"><v>-3.5</v></c><c r="H3" s="8"><v>1.5E+20</v></c><c r="J3" t="b"><v>1</v></c>` +
	`<c r="K3" t="e"><f>1/0</f><v>#DIV/0!</v></c><c r="L3" s="9"><v>0.395833333333333</v></c></row>` +
	`<row r="4"><c r="A4" t="inlineStr"><is><t>inline</t></is></c><c r="B4" s="5"><v>44856</v></c>` +
	`<c r="C4" t="str"><f>A4&amp;"!"</f><v>inline!</v></c><c r="I4" t="s"><v>13</v></c><c r="J4" t="s"><v>14</v></c></row>` +
	`<row r="5" spans="1:12"/>` +
	`<row><c t="inlineStr"><is><t>no ref</t></is></c><c><v>7</v></c></row>`

// A workbook in the shape Excel writes reads as Excel shows it: a 1904
// workbook's days, number formats by id and by code, text from its shared
// strings' runs without their phonetic guides, a formula's text, gaps
// between cells filled, cells without references in turn, and a chart
// sheet, which holds no cells, left out.
func TestAnExcelWorkbookIsReadAsExcelWritesIt(t *testing.T) {
	in := sheetOf("Plan.xlsx", zipOf(t, excelParts(excelPlan)))
	names, rows := sheetsOf(t, in)
	if want := "[Plan Lookups (hidden)]"; fmt.Sprint(names) != want {
		t.Errorf("the sheets are %v, want %s", names, want)
	}
	want := [][]string{
		{"Item", "When", "Due", "Share", "Time", "Rupees", "Euros", "Big", "Note", "Done", "Check", "Starts"},
		{"Big deal", "d:2026-10-23 00:00:00", "d:2026-10-23 12:00:00", "n:12.5%", "dur:94500", "n:1200.5 INR", "n:-3.5 other", "n:150000000000000000000", "", "b:true", "=e:#DIV/0!", "dur:34200"},
		{"inline", "d:2026-10-23 00:00:00", "=inline!", "", "", "", "", "", "漢字", "line one\r\nline two, _x000D_ kept"},
		{"no ref", "n:7"},
	}
	if fmt.Sprintf("%q", rows["Plan"]) != fmt.Sprintf("%q", want) {
		t.Errorf("Plan reads\n%q,\nwant\n%q", rows["Plan"], want)
	}
	if got := fmt.Sprintf("%q", rows["Lookups"]); got != `[["Open"]]` {
		t.Errorf("Lookups reads %s", got)
	}

	// Each typed cell's text is its value written out, as a column of text
	// shows it.
	sheets, err := ReadSheets(in)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	if err := sheets[0].Rows(func(cells []Cell) error {
		if cells[0].Text == "Big deal" {
			for _, c := range cells[1:] {
				texts = append(texts, c.Text)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := `["2026-10-23" "2026-10-23 12:00" "12.5%" "26:15" "₹1200.5" "-€3.5" "150000000000000000000" "" "TRUE" "#DIV/0!" "9:30"]`; fmt.Sprintf("%q", texts) != want {
		t.Errorf("the typed cells' text is %q, want %s", texts, want)
	}
}

// A workbook in the strict namespaces, its elements prefixed, its part names
// in another case than its relationships give, and its cells without
// references, reads as any other.
func TestAStrictWorkbookIsRead(t *testing.T) {
	const strict = "http://purl.oclc.org/ooxml/officeDocument/relationships"
	in := sheetOf("Strict.xlsx", zipOf(t, map[string][]byte{
		"_rels/.rels": []byte(rels(strict, "rId1 officeDocument xl/workbook.xml")),
		"XL/Workbook.xml": []byte(`<x:workbook xmlns:x="http://purl.oclc.org/ooxml/spreadsheetml/main" xmlns:r="` + strict + `">` +
			`<x:sheets><x:sheet name="Strict" sheetId="1" r:id="rId1"/></x:sheets></x:workbook>`),
		"XL/_rels/Workbook.xml.rels": []byte(rels(strict, "rId1 worksheet worksheets/sheet1.xml")),
		"xl/worksheets/sheet1.xml": []byte(`<x:worksheet xmlns:x="http://purl.oclc.org/ooxml/spreadsheetml/main"><x:sheetData>` +
			`<x:row><x:c t="inlineStr"><x:is><x:t>a</x:t></x:is></x:c><x:c><x:v>2</x:v></x:c></x:row></x:sheetData></x:worksheet>`),
	}))
	names, rows := sheetsOf(t, in)
	if fmt.Sprint(names) != "[Strict]" || fmt.Sprintf("%q", rows["Strict"]) != `[["a" "n:2"]]` {
		t.Errorf("a strict workbook reads as %v %q", names, rows["Strict"])
	}
}

// A number format's code says what its numbers are: days, days with their
// times, times of day, lengths of time, percents, amounts of money (with the
// sign shown), or numbers. Quoted text that names no currency, scientific
// and fraction formats, text formats and colours leave a number a number.
func TestANumberFormatSaysWhatItsCellsHold(t *testing.T) {
	kinds := map[formatKind]string{formatNumber: "number", formatPercent: "percent", formatCurrency: "currency", formatDay: "day",
		formatDayTime: "day and time", formatClock: "time of day", formatElapsed: "length of time"}
	for code, want := range map[string]string{
		"General":                 "number",
		"0.00":                    "number",
		"#,##0.00;[Red]-#,##0.00": "number",
		"0%":                      "percent",
		"0.0%":                    "percent",
		`[$$-409]#,##0.00`:        "currency $",
		`[$₹-439]#,##0`:           "currency ₹",
		`"₹"\ #,##0.00`:           "currency ₹",
		`_("$"* #,##0.00_);_("$"* \(#,##0.00\);_("$"* "-"??_);_(@_)`: "currency $",
		`#,##0.00\ [$€-1]`:      "currency €",
		`"USD "#,##0`:           "currency USD",
		`0" km"`:                "number",
		`"Days: "0`:             "number",
		`yyyy\-mm\-dd`:          "day",
		`[$-409]d\-mmm\-yy;@`:   "day",
		`mmm-yy`:                "day",
		`dd/mm/yyyy\ hh:mm`:     "day and time",
		`m/d/yyyy h:mm AM/PM`:   "day and time",
		"hh:mm":                 "time of day",
		"h:mm AM/PM":            "time of day",
		"mm:ss":                 "time of day",
		"[h]:mm":                "length of time",
		"[mm]:ss":               "length of time",
		"0.00E+00":              "number",
		"# ?/?":                 "number",
		"@":                     "number",
		`"TRUE";"TRUE";"FALSE"`: "number",
		"[Red][<=100]0;[Blue]0": "number",
	} {
		f := numberFormatOf(code)
		got := kinds[f.kind]
		if f.sign != "" {
			got += " " + f.sign
		}
		if got != want {
			t.Errorf("%s is %s, want %s", code, got, want)
		}
	}
	for id, want := range map[int]string{0: "number", 5: "number", 9: "percent", 10: "percent", 14: "day", 22: "day and time",
		18: "time of day", 30: "day", 33: "time of day", 46: "length of time", 47: "time of day", 49: "number"} {
		if got := kinds[builtinFormat(id).kind]; got != want {
			t.Errorf("the built-in format %d is %s, want %s", id, got, want)
		}
	}
}

// A workbook's serial number is the day Excel shows: days since 1899, or
// 1904, with 29 February 1900, which never was, read as the 28th.
func TestAnExcelSerialIsTheDayItNames(t *testing.T) {
	for _, c := range []struct {
		serial   float64
		date1904 bool
		want     string
	}{
		{1, false, "1900-01-01 00:00:00"},
		{59, false, "1900-02-28 00:00:00"},
		{60, false, "1900-02-28 00:00:00"},
		{61, false, "1900-03-01 00:00:00"},
		{46318.6041666667, false, "2026-10-23 14:30:00"},
		{46318.99999999, false, "2026-10-24 00:00:00"},
		{0, true, "1904-01-01 00:00:00"},
		{44856, true, "2026-10-23 00:00:00"},
		{-1, false, "none"},
		{2958466, false, "none"},
		{math.NaN(), false, "none"},
	} {
		got := "none"
		if day, ok := excelDay(c.serial, c.date1904); ok {
			got = day.Format("2006-01-02 15:04:05")
		}
		if got != c.want {
			t.Errorf("serial %v (1904: %v) is %s, want %s", c.serial, c.date1904, got, c.want)
		}
	}
}

// A workbook is read within the import's limits: a part that says it
// unpacks to more than an import reads is refused unread, shared strings
// past what an import holds are said (save it as CSV), and XML nested
// deeper than a sheet's or a cell past a row's limit is an error, never read
// into memory whole. A sheet's columns past maxSheetColumns are left out.
func TestAWorkbookIsReadWithinItsLimits(t *testing.T) {
	parts := func(plan string) map[string]string {
		out := map[string]string{}
		for name, data := range excelParts(plan) {
			out[name] = string(data)
		}
		return out
	}
	readErr := func(in Input) error {
		t.Helper()
		sheets, err := ReadSheets(in)
		if err != nil {
			return err
		}
		return sheets[0].Rows(func([]Cell) error { return nil })
	}

	// Every read of one import draws on one budget: a sheet that says it
	// unpacks to more than the import has left after its other sheet is
	// refused unread, though it alone would fit.
	budget := parts(`<row r="1"><c r="A1" t="inlineStr"><is><t>` + strings.Repeat("y", 3<<20) + `</t></is></c></row>`)
	delete(budget, "xl/worksheets/sheet2.xml")
	sheets, err := ReadSheets(sheetOf("Budget.xlsx", zipWith(t, budget, "xl/worksheets/sheet2.xml", maxZipUnpacked-(1<<20), 1024)))
	if err != nil || len(sheets) != 2 {
		t.Fatalf("%d sheets, %v", len(sheets), err)
	}
	if err := sheets[0].Rows(func([]Cell) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := sheets[1].Rows(func([]Cell) error { return nil }); !said(err, "unpacks to more than an import reads") {
		t.Errorf("a sheet that says it unpacks past what the import has left: %v", err)
	}

	sheet := parts(excelPlan)
	delete(sheet, "xl/worksheets/sheet1.xml")
	if err := readErr(sheetOf("Bomb.xlsx", zipWith(t, sheet, "xl/worksheets/sheet1.xml", maxZipUnpacked+1, 1024))); !said(err, "unpacks to more than an import reads") {
		t.Errorf("a sheet that says it unpacks past the budget: %v", err)
	}
	shared := parts(excelPlan)
	delete(shared, "xl/sharedStrings.xml")
	if _, err := ReadSheets(sheetOf("Bomb.xlsx", zipWith(t, shared, "xl/sharedStrings.xml", math.MaxUint64, 1024))); !said(err, "unpacks to more than an import reads") {
		t.Errorf("shared strings that say they unpack past the budget: %v", err)
	}

	var many bytes.Buffer
	many.WriteString(`<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	run := strings.Repeat("x", 1<<20)
	for i := 0; i <= maxSharedText>>20; i++ {
		many.WriteString("<si><t>" + run + "</t></si>")
	}
	many.WriteString("</sst>")
	big := excelParts(excelPlan)
	big["xl/sharedStrings.xml"] = many.Bytes()
	if _, err := ReadSheets(sheetOf("Text.xlsx", zipOf(t, big))); !said(err, "save it as CSV") {
		t.Errorf("shared strings past what an import holds: %v", err)
	}

	deep := `<row r="1"><c r="A1" t="inlineStr"><is>` + strings.Repeat("<r>", 100) + strings.Repeat("</r>", 100) + `</is></c></row>`
	if err := readErr(sheetOf("Deep.xlsx", zipOf(t, excelParts(deep)))); !said(err, "may be damaged") {
		t.Errorf("a sheet nested 100 deep: %v", err)
	}
	long := `<row r="1"><c r="A1" t="inlineStr"><is><t>` + strings.Repeat("y", maxSheetRowBytes+1) + `</t></is></c></row>`
	if err := readErr(sheetOf("Long.xlsx", zipOf(t, excelParts(long)))); !said(err, "runs past 4 MB") {
		t.Errorf("a cell past 4 MB: %v", err)
	}

	wide := `<row r="1"><c r="A1" t="inlineStr"><is><t>a</t></is></c><c r="SG1" t="inlineStr"><is><t>far</t></is></c><c r="XFD1" t="inlineStr"><is><t>last</t></is></c></row>`
	sheets, err = ReadSheets(sheetOf("Wide.xlsx", zipOf(t, excelParts(wide))))
	if err != nil {
		t.Fatal(err)
	}
	if rows := shownRows(t, sheets[0]); len(rows) != 1 || len(rows[0]) != maxSheetColumns || rows[0][500] != "far" || rows[0][maxSheetColumns-1] != "last" {
		t.Errorf("a row out to the last column, XFD, reads %d cells", len(at(rows, 0)))
	}
}

// A sheet whose every row holds one value far to the right is read out to
// that value, a row of 16,384 cells each, so a sheet of more cells than an
// import reads is refused, whatever its rows: 1,250 such rows hold 20.5
// million, from a few kilobytes of XML.
func TestASheetOfMoreCellsThanAnImportReadsIsRefused(t *testing.T) {
	const rows = 1250
	var far strings.Builder
	for r := 1; r <= rows; r++ {
		fmt.Fprintf(&far, `<row r="%d"><c r="XFD%d"><v>1</v></c></row>`, r, r)
	}
	for _, in := range []Input{
		sheetOf("Far.xlsx", zipOf(t, excelParts(far.String()))),
		odsOf(t, fmt.Sprintf(`<table:table table:name="Far"><table:table-row table:number-rows-repeated="%d">`+
			`<table:table-cell table:number-columns-repeated="16383"/><table:table-cell office:value-type="float" office:value="1"><text:p>1</text:p></table:table-cell>`+
			`</table:table-row></table:table>`, rows)),
	} {
		sheets, err := ReadSheets(in)
		if err != nil {
			t.Fatalf("%s: %v", in.Filename, err)
		}
		read := 0
		if err := sheets[0].Rows(func([]Cell) error { read++; return nil }); !said(err, "more cells than an import reads") {
			t.Errorf("%s: %d rows of 16,384 cells read, then %v", in.Filename, read, err)
		}
	}
}

// countedFile is a stored file that counts the bytes read out of it.
type countedFile struct {
	io.ReaderAt
	read int64
}

func (f *countedFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.ReaderAt.ReadAt(p, off)
	f.read += int64(n)
	return n, err
}

// An import reads each sheet of a workbook of many sheets twice, in turn: to
// guess its columns, then to bring its rows. The stored file is read a few
// times over for it, never once a sheet, as an OpenDocument file (all its
// sheets in one part) would be if each sheet were read from the part's
// start, or any workbook if each read opened it again.
func TestAWorkbookOfManySheetsIsReadAFewTimesOver(t *testing.T) {
	const sheets, rows = 300, 20
	cell := func(s, r int) string { return fmt.Sprintf("sheet %d row %d %x", s, r, (s*7919+r*104729)%65521) }
	const ns = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="` + officeNS + `"`
	xlsx := map[string][]byte{"_rels/.rels": []byte(rels(officeNS, "rId1 officeDocument xl/workbook.xml"))}
	var listed, related []string
	var tables strings.Builder
	for s := range sheets {
		listed = append(listed, fmt.Sprintf(`<sheet name="S%d" sheetId="%d" r:id="rId%d"/>`, s, s+1, s+1))
		related = append(related, fmt.Sprintf("rId%d worksheet worksheets/sheet%d.xml", s+1, s+1))
		var data strings.Builder
		fmt.Fprintf(&tables, `<table:table table:name="S%d">`, s)
		for r := range rows {
			fmt.Fprintf(&data, `<row><c t="inlineStr"><is><t>%s</t></is></c><c><v>%d</v></c></row>`, cell(s, r), r)
			fmt.Fprintf(&tables, `<table:table-row><table:table-cell office:value-type="string"><text:p>%s</text:p></table:table-cell>`+
				`<table:table-cell office:value-type="float" office:value="%d"><text:p>%d</text:p></table:table-cell></table:table-row>`, cell(s, r), r, r)
		}
		tables.WriteString(`</table:table>`)
		xlsx[fmt.Sprintf("xl/worksheets/sheet%d.xml", s+1)] = []byte(`<worksheet ` + ns + `><sheetData>` + data.String() + `</sheetData></worksheet>`)
	}
	xlsx["xl/workbook.xml"] = []byte(`<workbook ` + ns + `><sheets>` + strings.Join(listed, "") + `</sheets></workbook>`)
	xlsx["xl/_rels/workbook.xml.rels"] = []byte(rels(officeNS, related...))
	for _, in := range []Input{sheetOf("Many.xlsx", zipOf(t, xlsx)), odsOf(t, tables.String())} {
		file := &countedFile{ReaderAt: in.File}
		in.File = file
		got, err := ReadSheets(in)
		if err != nil || len(got) != sheets {
			t.Fatalf("%s: %d sheets, %v", in.Filename, len(got), err)
		}
		for range 2 {
			for s, sheet := range got {
				var first string
				read := 0
				if err := sheet.Rows(func(cells []Cell) error {
					if read++; read == 1 {
						first = cells[0].Text
					}
					return nil
				}); err != nil || read != rows || first != cell(s, 0) {
					t.Fatalf("%s: %s read %d rows, the first %q, then %v", in.Filename, sheet.Name, read, first, err)
				}
			}
		}
		if file.read > 4*in.Size {
			t.Errorf("%s of %d bytes: reading its %d sheets twice read %d bytes of it", in.Filename, in.Size, sheets, file.read)
		}
	}
}

// odsOf is an OpenDocument spreadsheet of the tables given, in content.xml.
func odsOf(t *testing.T, tables string) Input {
	t.Helper()
	return sheetOf("Book.ods", zipOf(t, map[string][]byte{
		"mimetype": []byte("application/vnd.oasis.opendocument.spreadsheet"),
		"content.xml": []byte(`<?xml version="1.0" encoding="UTF-8"?><office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" ` +
			`xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0">` +
			`<office:body><office:spreadsheet>` + tables + `</office:spreadsheet></office:body></office:document-content>`),
	}))
}

// An OpenDocument sheet's repeated cells and rows are counted, not spelled
// out: a value repeated across columns is spelled out as far as the last
// column a spreadsheet has, empty cells and rows repeated to the sheet's end
// (as LibreOffice writes them) are nothing, and a sheet that says it has
// more rows than a spreadsheet holds is an error. A cell's text is its paragraphs with their spaces, tabs
// and line breaks, never its comment.
func TestAnOpenDocumentSheetsRepeatsAreReadWithinLimits(t *testing.T) {
	in := odsOf(t, `<table:table table:name="Wide">`+
		`<table:table-row><table:table-cell office:value-type="string" table:number-columns-repeated="1000"><text:p>x</text:p></table:table-cell><table:table-cell table:number-columns-repeated="16384"/></table:table-row>`+
		`<table:table-row><table:table-cell office:value-type="string" table:number-columns-repeated="20000"><text:p>x</text:p></table:table-cell></table:table-row>`+
		`<table:table-row table:number-rows-repeated="3"><table:table-cell table:number-columns-repeated="2"/><table:table-cell office:value-type="float" office:value="7"><text:p>7</text:p></table:table-cell></table:table-row>`+
		`<table:table-row table:number-rows-repeated="1048000"><table:table-cell table:number-columns-repeated="1024"/></table:table-row>`+
		`</table:table><table:table table:name="Text"><table:table-row><table:table-cell office:value-type="string">`+
		`<office:annotation><text:p>a comment</text:p></office:annotation><text:p>a<text:s text:c="3"/>b<text:tab/>c<text:line-break/>d</text:p><text:p><text:span>e</text:span></text:p>`+
		`</table:table-cell></table:table-row></table:table>`+
		`<table:table table:name="Long"><table:table-row table:number-rows-repeated="2000000"><table:table-cell office:value-type="string"><text:p>y</text:p></table:table-cell></table:table-row></table:table>`+
		`<table:table table:name="Spaces"><table:table-row><table:table-cell office:value-type="string"><text:p><text:s text:c="100000000"/></text:p></table:table-cell></table:table-row></table:table>`)
	sheets, err := ReadSheets(in)
	if err != nil || len(sheets) != 4 {
		t.Fatalf("%d sheets, %v", len(sheets), err)
	}
	wide := shownRows(t, sheets[0])
	if len(wide) != 5 || len(wide[0]) != 1000 || len(wide[1]) != maxSheetColumns || fmt.Sprintf("%q", wide[2:]) != `[["" "" "n:7"] ["" "" "n:7"] ["" "" "n:7"]]` {
		t.Errorf("Wide reads %d rows, the first of %d cells, the next of %d, then %q", len(wide), len(at(wide, 0)), len(at(wide, 1)), wide[min(len(wide), 2):])
	}
	if got := fmt.Sprintf("%q", shownRows(t, sheets[1])); got != `[["a   b\tc\nd\ne"]]` {
		t.Errorf("a cell's text reads %s", got)
	}
	var r readError
	if err := sheets[2].Rows(func([]Cell) error { return nil }); !errors.As(err, &r) || !strings.Contains(r.msg, "more rows than a spreadsheet can") {
		t.Errorf("a sheet of 2,000,000 rows: %v", err)
	}
	if err := sheets[3].Rows(func([]Cell) error { return nil }); !errors.As(err, &r) || !strings.Contains(r.msg, "runs past 4 MB") {
		t.Errorf("a cell of 100,000,000 spaces: %v", err)
	}
}

// A file named as a workbook that isn't one is said to be what it isn't; the
// files ReadSheets reads are told by their extension.
func TestAFileThatIsntAWorkbookIsSaidSo(t *testing.T) {
	for _, c := range []struct {
		in   Input
		says string
	}{
		{sheetOf("Report.xlsx", []byte("Name,Amount\nAda,3\n")), "not an Excel workbook"},
		{sheetOf("Report.xlsx", zipOf(t, map[string][]byte{"readme.txt": []byte("hi")})), "not an Excel workbook"},
		{sheetOf("Report.ods", []byte("Name,Amount\n")), "not an OpenDocument spreadsheet"},
	} {
		var r readError
		if _, err := ReadSheets(c.in); !errors.As(err, &r) || !strings.Contains(r.msg, c.says) {
			t.Errorf("%s holding %d bytes: %v, want it said to be %s", c.in.Filename, c.in.Size, err, c.says)
		}
	}
	for name, want := range map[string]bool{"a.csv": true, "a.TSV": true, "a.xlsx": true, "a.XLSM": true, "a.ods": true, "a.xls": false, "a.numbers": false, "a.docx": false, "csv": false} {
		if IsSheetFile(name) != want {
			t.Errorf("IsSheetFile(%q) = %v", name, !want)
		}
	}
}
