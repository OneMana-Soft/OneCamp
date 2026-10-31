package business

// Excel workbooks written (tables, package 2b: a table's view as .xlsx): by
// hand, on archive/zip, as xlsx.go reads them, so no converter or library
// runs on the server. Each sheet's rows stream through the zip as they come,
// and none is held whole. A cell is written as what it holds (Cell): a
// number, a day, a length of time and a checkbox as themselves, under a
// number format that shows them so (a day in the reader's own short date),
// and text as inline text, never as a formula. What Excel can't hold is cut
// to its limits, and the workbook's last sheet, Notes, says what was: a
// sheet holds maxSheetRows rows, its header among them, and maxSheetColumns
// columns; a cell, excelCellChars characters; and a sheet's name,
// excelNameChars characters, none of : \ / ? * [ ], and no other sheet's
// name in any case.

import (
	"archive/zip"
	"bufio"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/akashc777/OneCamp/helpers"
)

const (
	// excelCellChars is the most characters (UTF-16 units) a cell holds.
	excelCellChars = 32767
	// excelNameChars is the most characters a sheet's name has.
	excelNameChars = 31
	// excelListChars is the most characters a dropdown's values written
	// into its rule may take; longer ones are a column of a hidden sheet.
	excelListChars = 255
	// excelColumnWidth is the widest a column is, in characters.
	excelColumnWidth = 255
)

// WorkbookSheet is a sheet to write: its name, its columns, and its rows,
// which Rows hands to row in turn, as a Sheet's are, each row's cells in its
// columns' order.
type WorkbookSheet struct {
	Name    string
	Columns []WorkbookColumn
	Rows    func(row func(cells []Cell) error) error
}

// WorkbookColumn is a written sheet's column: its header; its width in
// characters (0 to fit its header); its numbers' decimal places, Places of
// them when Fixed, else as many as each number needs (up to six); whether
// its lengths of time show their seconds; and the values its dropdown
// offers, in order (a select's options).
type WorkbookColumn struct {
	Name    string
	Width   float64
	Fixed   bool
	Places  int
	Seconds bool
	Choices []string
}

// errSheetFull ends a sheet's rows at the last row Excel holds.
var errSheetFull = errors.New("content io: the sheet holds as many rows as Excel does")

// WriteXLSX writes sheets to w as an Excel workbook, a row at a time, and
// answers what it cut to Excel's limits, which the workbook's Notes sheet
// says too. An error from a sheet's rows, or from w, ends the workbook
// unwritten: w then holds part of a zip.
func WriteXLSX(w io.Writer, sheets []WorkbookSheet) ([]string, error) {
	zw := zip.NewWriter(w)
	// A sheet's XML is the same few tags over and over: the fastest deflate
	// still makes it about a tenth of its size, in a third of the time.
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) { return flate.NewWriter(out, flate.BestSpeed) })
	o := &xlsxOut{zw: zw, at: time.Now().UTC(), taken: map[string]bool{"history": true}, styles: newXLSXStyles()}

	names := make([]string, len(sheets))
	for i, s := range sheets {
		names[i] = o.name(s.Name)
		if names[i] != s.Name {
			o.notes = append(o.notes, fmt.Sprintf("The sheet “%s” is named “%s”: a sheet's name in Excel has at most %d characters, none of : \\ / ? * [ ], and is no other sheet's.", s.Name, names[i], excelNameChars))
		}
	}
	// A dropdown's values too long for its rule are a column of a hidden
	// sheet, Lists, which the rule names.
	rules := make([][]string, len(sheets))
	var lists [][]string
	listsName := ""
	for i, s := range sheets {
		rules[i] = make([]string, min(len(s.Columns), maxSheetColumns))
		for c := range rules[i] {
			choices := s.Columns[c].Choices
			if len(choices) == 0 {
				continue
			}
			if inline, ok := inlineList(choices); ok {
				rules[i][c] = inline
				continue
			}
			if listsName == "" {
				listsName = o.name("Lists")
			}
			lists = append(lists, choices)
			rules[i][c] = fmt.Sprintf("%s!$%s$1:$%s$%d", quotedSheet(listsName), columnName(len(lists)-1), columnName(len(lists)-1), min(len(choices), maxSheetRows))
		}
	}

	for i, s := range sheets {
		cols := s.Columns
		if len(cols) > maxSheetColumns {
			o.notes = append(o.notes, fmt.Sprintf("The sheet “%s” has %s columns, and Excel holds %s: the columns past them weren't written.", names[i], helpers.Thousands(int64(len(cols))), helpers.Thousands(maxSheetColumns)))
			cols = cols[:maxSheetColumns]
		}
		if err := o.sheet(names[i], false, cols, rules[i], s.Rows); err != nil {
			return nil, err
		}
	}
	if lists != nil {
		cols := make([]WorkbookColumn, len(lists))
		if err := o.sheet(listsName, true, cols, nil, columnsAsRows(lists)); err != nil {
			return nil, err
		}
	}
	switch {
	case o.cut == 1:
		o.notes = append(o.notes, fmt.Sprintf("A cell held more than the %s characters a cell holds in Excel, and was cut there.", helpers.Thousands(excelCellChars)))
	case o.cut > 1:
		o.notes = append(o.notes, fmt.Sprintf("%s cells held more than the %s characters a cell holds in Excel, and were cut there.", helpers.Thousands(int64(o.cut)), helpers.Thousands(excelCellChars)))
	}
	notes := o.notes
	if len(notes) > 0 {
		rows := func(row func([]Cell) error) error {
			for _, n := range notes {
				if err := row([]Cell{{Text: n}}); err != nil {
					return err
				}
			}
			return nil
		}
		if err := o.sheet(o.name("Notes"), false, []WorkbookColumn{{Name: "Note", Width: 120}}, nil, rows); err != nil {
			return nil, err
		}
	}
	if err := o.parts(); err != nil {
		return nil, err
	}
	return notes, zw.Close()
}

// xlsxOut is a workbook being written.
type xlsxOut struct {
	zw     *zip.Writer
	at     time.Time
	styles *xlsxStyles
	// sheets are the sheets written, in order; taken, their names folded.
	sheets []xlsxSheet
	taken  map[string]bool
	notes  []string
	// cut counts the cells cut to excelCellChars.
	cut int
}

type xlsxSheet struct {
	name   string
	hidden bool
}

// part starts a part of the zip.
func (o *xlsxOut) part(name string) (io.Writer, error) {
	return o.zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: o.at})
}

// name is name as Excel names a sheet, and no other taken already in any
// case: each of : \ / ? * [ ] a space, no ' at either end, what XML can't
// hold left out, at most excelNameChars characters, and "Sheet" for none. A
// name taken is numbered ("Deals (2)"). Excel keeps History for itself.
func (o *xlsxOut) name(name string) string {
	clean, _ := excelText(strings.Map(func(r rune) rune {
		if strings.ContainsRune(`:\/?*[]`, r) {
			return ' '
		}
		return r
	}, name), excelNameChars)
	clean = strings.Trim(strings.TrimSpace(clean), "'")
	if clean == "" {
		clean = "Sheet"
	}
	got := clean
	for n := 2; o.taken[strings.ToLower(got)]; n++ {
		suffix := fmt.Sprintf(" (%d)", n)
		base, _ := excelText(clean, excelNameChars-len(suffix))
		got = strings.TrimSpace(base) + suffix
	}
	o.taken[strings.ToLower(got)] = true
	return got
}

// sheet writes a worksheet: a header of its columns' names, frozen in
// place, and its rows below, each column the width asked or its header's,
// and each with a dropdown rule (a list, or a range of Lists) a dropdown.
// A hidden sheet (Lists) has no header.
func (o *xlsxOut) sheet(name string, hidden bool, cols []WorkbookColumn, rules []string, rows func(func([]Cell) error) error) error {
	o.sheets = append(o.sheets, xlsxSheet{name: name, hidden: hidden})
	part, err := o.part(fmt.Sprintf("xl/worksheets/sheet%d.xml", len(o.sheets)))
	if err != nil {
		return err
	}
	b := bufio.NewWriterSize(part, 64<<10)
	b.WriteString(xmlHead + `<worksheet xmlns="` + nsMain + `" xmlns:r="` + nsRelations + `">`)
	r := 0
	if !hidden {
		b.WriteString(`<sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/><selection pane="bottomLeft"/></sheetView></sheetViews>`)
		b.WriteString(`<cols>`)
		for i, c := range cols {
			width := min(c.Width, excelColumnWidth)
			if width <= 0 {
				width = min(max(float64(utf8.RuneCountInString(c.Name))+2, 10), 60)
			}
			fmt.Fprintf(b, `<col min="%d" max="%d" width="%s" customWidth="1"/>`, i+1, i+1, strconv.FormatFloat(width, 'f', 2, 64))
		}
		b.WriteString(`</cols><sheetData>`)
		r = 1
		b.WriteString(`<row r="1">`)
		for i, c := range cols {
			o.text(b, columnName(i)+"1", c.Name, xlsxHeaderStyle)
		}
		b.WriteString(`</row>`)
	} else {
		b.WriteString(`<sheetData>`)
	}
	err = rows(func(cells []Cell) error {
		if r == maxSheetRows {
			o.notes = append(o.notes, fmt.Sprintf("The sheet “%s” has more rows than the %s Excel holds, its header among them: the rows past them weren't written.", name, helpers.Thousands(maxSheetRows)))
			return errSheetFull
		}
		r++
		row := strconv.Itoa(r)
		b.WriteString(`<row r="` + row + `">`)
		for i, c := range cells[:min(len(cells), len(cols))] {
			o.cell(b, columnName(i)+row, c, cols[i])
		}
		b.WriteString(`</row>`)
		return nil
	})
	if err != nil && !errors.Is(err, errSheetFull) {
		return err
	}
	b.WriteString(`</sheetData>`)
	if n := countRules(rules); n > 0 {
		fmt.Fprintf(b, `<dataValidations count="%d">`, n)
		for i, rule := range rules {
			if rule == "" {
				continue
			}
			col := columnName(i)
			fmt.Fprintf(b, `<dataValidation type="list" allowBlank="1" showErrorMessage="1" sqref="%s2:%s%d"><formula1>`, col, col, maxSheetRows)
			xmlEscape(b, rule, false)
			b.WriteString(`</formula1></dataValidation>`)
		}
		b.WriteString(`</dataValidations>`)
	}
	b.WriteString(`</worksheet>`)
	return b.Flush()
}

// cell writes c, in column col, at ref: an empty one not at all.
func (o *xlsxOut) cell(b *bufio.Writer, ref string, c Cell, col WorkbookColumn) {
	switch c.Kind {
	case CellNumber:
		if math.IsNaN(c.Number) || math.IsInf(c.Number, 0) {
			o.value(b, ref, "e", "#NUM!", 0)
			return
		}
		places := -1
		if col.Fixed {
			places = min(max(col.Places, 0), 6)
		}
		switch {
		case c.Percent:
			if places < 0 {
				places = placesOf(c.Number)
			}
			o.value(b, ref, "", excelNumber(c.Number/100), o.styles.percent(places))
		case c.Currency == "USD" || c.Currency == "INR":
			if places < 0 {
				places = placesOf(c.Number)
			}
			o.value(b, ref, "", excelNumber(c.Number), o.styles.money(c.Currency, places))
		default:
			o.value(b, ref, "", excelNumber(c.Number), o.styles.number(places))
		}
	case CellBool:
		v := "0"
		if c.Bool {
			v = "1"
		}
		o.value(b, ref, "b", v, 0)
	case CellDate:
		serial, ok := excelSerial(c.Time)
		if !ok {
			// Before 1900 or past 9999 a day isn't a serial Excel shows: its
			// text is.
			o.text(b, ref, c.Text, 0)
			return
		}
		style := o.styles.builtin(14) // the reader's own short date
		if serial != math.Trunc(serial) {
			style = o.styles.builtin(22)
		}
		o.value(b, ref, "", excelNumber(serial), style)
	case CellDuration:
		if !(c.Number >= 0 && c.Number < maxSerial*86400) {
			// A negative length of time is one Excel can't show.
			o.text(b, ref, c.Text, 0)
			return
		}
		// The workbook's own codes: a reader may take builtin 46 for a time
		// of day (LibreOffice shows 25 hours as "01:00:00 AM").
		code := "[h]:mm"
		if col.Seconds {
			code = "[h]:mm:ss"
		}
		o.value(b, ref, "", excelNumber(c.Number/86400), o.styles.code(code))
	case CellError:
		o.value(b, ref, "e", excelError(c.Text), 0)
	default:
		if c.Text != "" {
			o.text(b, ref, c.Text, 0)
		}
	}
}

// value writes a cell of type typ ("" for a number) holding v, in style.
func (o *xlsxOut) value(b *bufio.Writer, ref, typ, v string, style int) {
	b.WriteString(`<c r="` + ref + `"`)
	if style != 0 {
		b.WriteString(` s="` + strconv.Itoa(style) + `"`)
	}
	if typ != "" {
		b.WriteString(` t="` + typ + `"`)
	}
	b.WriteString(`><v>` + v + `</v></c>`)
}

// text writes s as an inline text cell, cut to excelCellChars.
func (o *xlsxOut) text(b *bufio.Writer, ref, s string, style int) {
	s, cut := excelText(s, excelCellChars)
	if cut {
		o.cut++
	}
	b.WriteString(`<c r="` + ref + `"`)
	if style != 0 {
		b.WriteString(` s="` + strconv.Itoa(style) + `"`)
	}
	b.WriteString(` t="inlineStr"><is><t`)
	if strings.TrimSpace(s) != s {
		b.WriteString(` xml:space="preserve"`)
	}
	b.WriteString(`>`)
	xmlEscape(b, xstringEscape(s), false)
	b.WriteString(`</t></is></c>`)
}

// parts writes the workbook's own parts, once its sheets are: its styles,
// its list of sheets, their relationships and the package's content types.
func (o *xlsxOut) parts() error {
	var b strings.Builder
	b.WriteString(xmlHead + `<workbook xmlns="` + nsMain + `" xmlns:r="` + nsRelations + `"><bookViews><workbookView/></bookViews><sheets>`)
	for i, s := range o.sheets {
		b.WriteString(`<sheet name="`)
		xmlEscape(&b, s.name, true)
		fmt.Fprintf(&b, `" sheetId="%d"`, i+1)
		if s.hidden {
			b.WriteString(` state="hidden"`)
		}
		fmt.Fprintf(&b, ` r:id="rId%d"/>`, i+1)
	}
	b.WriteString(`</sheets></workbook>`)
	workbook := b.String()

	b.Reset()
	b.WriteString(xmlHead + `<Relationships xmlns="` + nsPackageRelations + `">`)
	for i := range o.sheets {
		fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="%s/worksheet" Target="worksheets/sheet%d.xml"/>`, i+1, nsRelations, i+1)
	}
	fmt.Fprintf(&b, `<Relationship Id="rId%d" Type="%s/styles" Target="styles.xml"/></Relationships>`, len(o.sheets)+1, nsRelations)
	workbookRels := b.String()

	b.Reset()
	b.WriteString(xmlHead + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
		`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`)
	for i := range o.sheets {
		fmt.Fprintf(&b, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, i+1)
	}
	b.WriteString(`</Types>`)
	types := b.String()

	for _, p := range []struct{ name, body string }{
		{"xl/styles.xml", o.styles.xml()},
		{"xl/workbook.xml", workbook},
		{"xl/_rels/workbook.xml.rels", workbookRels},
		{"[Content_Types].xml", types},
		{"_rels/.rels", xmlHead + `<Relationships xmlns="` + nsPackageRelations + `"><Relationship Id="rId1" Type="` + nsRelations + `/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
	} {
		w, err := o.part(p.name)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, p.body); err != nil {
			return err
		}
	}
	return nil
}

const (
	xmlHead            = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"
	nsMain             = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	nsRelations        = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	nsPackageRelations = "http://schemas.openxmlformats.org/package/2006/relationships"
)

// xlsxHeaderStyle is the header's bold, style 1 of every workbook written.
const xlsxHeaderStyle = 1

// xlsxStyles are a workbook's cell styles as its cells take them: 0 is
// Excel's General, 1 the header's bold, and each after a number format,
// Excel's own by its id or one of the workbook's own (164 and up).
type xlsxStyles struct {
	codes []string
	ids   map[string]int
	// formats are the number format of each style from 2, and byFormat the
	// style of each.
	formats  []int
	byFormat map[int]int
}

func newXLSXStyles() *xlsxStyles {
	return &xlsxStyles{ids: map[string]int{}, byFormat: map[int]int{}}
}

// builtin is the style of Excel's own number format id.
func (s *xlsxStyles) builtin(id int) int {
	if style, ok := s.byFormat[id]; ok {
		return style
	}
	s.formats = append(s.formats, id)
	s.byFormat[id] = len(s.formats) + 1
	return len(s.formats) + 1
}

// code is the style of a number format the workbook writes itself.
func (s *xlsxStyles) code(code string) int {
	id, ok := s.ids[code]
	if !ok {
		id = 164 + len(s.codes)
		s.codes = append(s.codes, code)
		s.ids[code] = id
	}
	return s.builtin(id)
}

// number is a number's style: General, or places decimal places after
// grouped thousands.
func (s *xlsxStyles) number(places int) int {
	if places < 0 {
		return 0
	}
	return s.code("#,##0" + decimals(places))
}

func (s *xlsxStyles) percent(places int) int {
	switch places {
	case 0:
		return s.builtin(9)
	case 2:
		return s.builtin(10)
	}
	return s.code("0" + decimals(places) + "%")
}

// money is an amount's style, with its currency's sign: the dollar or the
// rupee, the two a table's number shows.
func (s *xlsxStyles) money(currency string, places int) int {
	sign := "$"
	if currency == "INR" {
		sign = "₹"
	}
	return s.code(`"` + sign + `"#,##0` + decimals(places))
}

func (s *xlsxStyles) xml() string {
	var b strings.Builder
	b.WriteString(xmlHead + `<styleSheet xmlns="` + nsMain + `">`)
	if len(s.codes) > 0 {
		fmt.Fprintf(&b, `<numFmts count="%d">`, len(s.codes))
		for i, code := range s.codes {
			fmt.Fprintf(&b, `<numFmt numFmtId="%d" formatCode="`, 164+i)
			xmlEscape(&b, code, true)
			b.WriteString(`"/>`)
		}
		b.WriteString(`</numFmts>`)
	}
	b.WriteString(`<fonts count="2"><font><sz val="11"/><name val="Calibri"/><family val="2"/></font><font><b/><sz val="11"/><name val="Calibri"/><family val="2"/></font></fonts>` +
		`<fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills>` +
		`<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>` +
		`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>`)
	fmt.Fprintf(&b, `<cellXfs count="%d"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/>`, len(s.formats)+2)
	for _, id := range s.formats {
		fmt.Fprintf(&b, `<xf numFmtId="%d" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>`, id)
	}
	b.WriteString(`</cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`)
	return b.String()
}

// decimals is a number format's decimal places: "" for none, ".00" for two.
// Pure.
func decimals(places int) string {
	if places <= 0 {
		return ""
	}
	return "." + strings.Repeat("0", places)
}

// placesOf is the decimal places n is written with, up to six. Pure.
func placesOf(n float64) int {
	s := strconv.FormatFloat(n, 'f', -1, 64)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return min(len(s)-i-1, 6)
	}
	return 0
}

// excelNumber is n as a cell's value: in full, or in exponent form past
// 1e21 and below 1e-9, as Excel reads either. Pure.
func excelNumber(n float64) string {
	if a := math.Abs(n); a >= 1e21 || (a != 0 && a < 1e-9) {
		return strconv.FormatFloat(n, 'E', -1, 64)
	}
	return strconv.FormatFloat(n, 'f', -1, 64)
}

// excelSerial is the serial number Excel gives t's day and time of day, as
// its clock reads in no zone: excelDay's, the other way. Days count from
// 30 December 1899, and before 1 March 1900 from 31 December 1899, as Excel
// counts a 29 February 1900 that never was. False before 1 January 1900 and
// from 1 January 10000. Pure.
func excelSerial(t time.Time) (float64, bool) {
	t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
	if t.Year() < 1900 || t.Year() > 9999 {
		return 0, false
	}
	from := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	if t.Before(time.Date(1900, 3, 1, 0, 0, 0, 0, time.UTC)) {
		from = time.Date(1899, 12, 31, 0, 0, 0, 0, time.UTC)
	}
	return float64(t.Unix()-from.Unix()) / 86400, true
}

// excelError is a formula's error as Excel writes one; an error it has no
// name for is #VALUE!. Pure.
func excelError(text string) string {
	switch text {
	case "#NULL!", "#DIV/0!", "#VALUE!", "#REF!", "#NAME?", "#NUM!", "#N/A":
		return text
	}
	return "#VALUE!"
}

// excelText is s as Excel holds it: without what XML can't hold (control
// characters but tab, line feed and carriage return; U+FFFE and U+FFFF;
// bytes that aren't UTF-8), cut at limit characters (UTF-16 units, as Excel
// counts them). cut says it was. Pure.
func excelText(s string, limit int) (out string, cut bool) {
	var b strings.Builder
	units := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if (r == utf8.RuneError && size == 1) || r == 0xFFFE || r == 0xFFFF || (r < 0x20 && r != '\t' && r != '\n' && r != '\r') {
			continue
		}
		n := 1
		if r > 0xFFFF {
			n = 2
		}
		if units+n > limit {
			return b.String(), true
		}
		units += n
		b.WriteRune(r)
	}
	return b.String(), false
}

// xstringEscape is s as a workbook's text holds it (ECMA-376's ST_Xstring;
// xstring reads it back): an underscore that would start an escape,
// _xHHHH_, is itself one, _x005F_. Pure.
func xstringEscape(s string) string {
	if !strings.Contains(s, "_x") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if i+7 <= len(s) && s[i] == '_' && s[i+1] == 'x' && s[i+6] == '_' && isHex(s[i+2:i+6]) {
			b.WriteString("_x005F")
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHex(s string) bool {
	_, err := strconv.ParseUint(s, 16, 16)
	return err == nil
}

// xmlEscape writes s as XML text, or as an attribute's value: & < > (and
// " in an attribute) as entities, and a carriage return as &#xD;, which XML
// would otherwise read as a line feed.
func xmlEscape(w io.StringWriter, s string, attr bool) {
	last := 0
	for i := 0; i < len(s); i++ {
		var esc string
		switch s[i] {
		case '&':
			esc = "&amp;"
		case '<':
			esc = "&lt;"
		case '>':
			esc = "&gt;"
		case '\r':
			esc = "&#xD;"
		case '"':
			if !attr {
				continue
			}
			esc = "&quot;"
		case '\n', '\t':
			if !attr {
				continue
			}
			esc = fmt.Sprintf("&#x%X;", s[i])
		default:
			continue
		}
		w.WriteString(s[last:i])
		w.WriteString(esc)
		last = i + 1
	}
	w.WriteString(s[last:])
}

// columnName is a column's letters, from 0: 0 is A, 26 is AA. Pure.
func columnName(col int) string {
	var b [4]byte
	i := len(b)
	for col++; col > 0; col = (col - 1) / 26 {
		i--
		b[i] = byte('A' + (col-1)%26)
	}
	return string(b[i:])
}

// quotedSheet is a sheet's name as a formula names it: quoted, a quote in
// it doubled. Pure.
func quotedSheet(name string) string {
	return "'" + strings.ReplaceAll(name, "'", "''") + "'"
}

// inlineList is a dropdown's values as its rule writes them, "a,b,c", when
// they fit there: none of them empty, holding a comma or a double quote, or
// holding what XML can't (excelText would change it: the hidden sheet's cells
// write it as the column's cells do), and excelListChars characters at most.
// Pure.
func inlineList(choices []string) (string, bool) {
	for _, c := range choices {
		if clean, _ := excelText(c, excelCellChars); c == "" || clean != c || strings.ContainsAny(c, `,"`) {
			return "", false
		}
	}
	list := strings.Join(choices, ",")
	if _, cut := excelText(list, excelListChars); cut || strings.ContainsAny(list, "\r\n") {
		return "", false
	}
	return `"` + list + `"`, true
}

func countRules(rules []string) int {
	n := 0
	for _, r := range rules {
		if r != "" {
			n++
		}
	}
	return n
}

// columnsAsRows hands lists, each a column, to row a row at a time, up to
// maxSheetRows.
func columnsAsRows(lists [][]string) func(func([]Cell) error) error {
	return func(row func([]Cell) error) error {
		longest := 0
		for _, l := range lists {
			longest = max(longest, len(l))
		}
		for r := range min(longest, maxSheetRows) {
			cells := make([]Cell, len(lists))
			for c, l := range lists {
				if r < len(l) {
					cells[c] = Cell{Text: l[r]}
				}
			}
			if err := row(cells); err != nil {
				return err
			}
		}
		return nil
	}
}
