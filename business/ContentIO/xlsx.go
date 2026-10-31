package business

// Excel workbooks (.xlsx, .xlsm) as an import reads them (tables S1, design/
// tables-vs-airtable/PROGRAM.md §15): by hand, on archive/zip and
// encoding/xml within the unpack rule every zip an import reads keeps
// (openPackage), as the Word reader is, so no new dependency. ReadSheets
// opens the workbook once and reads its list of sheets, its number formats
// and its shared strings; each sheet is streamed from the stored file each
// time it is read, every read unpacking within the workbook's one budget, so
// an import's reads of all its sheets unpack no more than any one zip an
// import reads (maxZipUnpacked). A number's format says what it
// is (ECMA-376 part 1, §18.8.30): a day (a serial number of days), a time of
// day or a length of time, a percent, or an amount of money.

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// maxWorkbookPart bounds each small part of a workbook, read whole: its
	// list of sheets, their relationships, its styles.
	maxWorkbookPart = 16 << 20
	// maxSharedText bounds a workbook's shared strings, held while its sheets
	// are read: each one's text, and 16 bytes for keeping it. A workbook with
	// more is better brought as CSV, which is read a row at a time.
	maxSharedText = 64 << 20
	// maxSerial is the serial number of 1 January 10000, the first day past
	// the last a workbook holds.
	maxSerial = 2958466
)

var errNotAWorkbook = readError{"This is not an Excel workbook (.xlsx) that can be read."}

// xlsxBook is what a workbook's sheets are read with.
type xlsxBook struct {
	z       *ZipArchive
	strings []string
	// formats are the number formats of its cell styles (a cell's s).
	formats  []numberFormat
	date1904 bool
}

// readXLSX reads an Excel workbook's worksheets, in its order (a chart sheet
// has no cells).
func readXLSX(in Input) ([]Sheet, error) {
	z, err := openPackage(in.File, in.Size, nil)
	if err != nil {
		return nil, errNotAWorkbook
	}
	main := "xl/workbook.xml"
	for _, r := range xlsxRels(z, "") {
		if r.kind == "officeDocument" {
			main = r.target
		}
	}
	data, err := z.Read(xlsxPart(z, main), maxWorkbookPart)
	if err != nil {
		return nil, workbookError(err)
	}
	var wb struct {
		Pr struct {
			Date1904 string `xml:"date1904,attr"`
		} `xml:"workbookPr"`
		Sheets []struct {
			Name  string `xml:"name,attr"`
			State string `xml:"state,attr"`
			Rel   string `xml:"id,attr"`
		} `xml:"sheets>sheet"`
	}
	if xml.Unmarshal(data, &wb) != nil {
		return nil, errNotAWorkbook
	}
	book := &xlsxBook{z: z, date1904: wb.Pr.Date1904 == "1" || strings.EqualFold(wb.Pr.Date1904, "true")}
	worksheets := map[string]string{} // their parts, by relationship
	var styles, shared string
	for _, r := range xlsxRels(z, main) {
		switch r.kind {
		case "worksheet":
			worksheets[r.id] = r.target
		case "styles":
			styles = r.target
		case "sharedStrings":
			shared = r.target
		}
	}
	if book.formats, err = xlsxFormats(z, styles); err != nil {
		return nil, err
	}
	if book.strings, err = xlsxSharedStrings(z, shared); err != nil {
		return nil, err
	}
	var out []Sheet
	for _, s := range wb.Sheets {
		part, ok := worksheets[s.Rel]
		if !ok {
			continue
		}
		out = append(out, Sheet{Name: s.Name, Hidden: s.State == "hidden" || s.State == "veryHidden", Rows: func(row func([]Cell) error) error {
			return book.readSheet(part, s.Name, row)
		}})
	}
	return out, nil
}

// workbookError is what reading a workbook's part failed with, said to the
// person importing.
func workbookError(err error) error {
	if errors.Is(err, errUnpackBudget) {
		return readError{"The workbook unpacks to more than an import reads."}
	}
	return errNotAWorkbook
}

// xlsxRel is one relationship of a workbook's part: what it is (the last
// segment of its type: "worksheet", "styles") and the part it names.
type xlsxRel struct{ id, kind, target string }

// xlsxRels are part's relationships ("" for the package's own), each target
// named as the zip names it.
func xlsxRels(z *ZipArchive, part string) []xlsxRel {
	dir, name := path.Split(part)
	data, err := z.Read(xlsxPart(z, dir+"_rels/"+name+".rels"), maxWorkbookPart)
	if err != nil {
		return nil
	}
	var doc struct {
		Rels []struct {
			ID     string `xml:"Id,attr"`
			Type   string `xml:"Type,attr"`
			Target string `xml:"Target,attr"`
			Mode   string `xml:"TargetMode,attr"`
		} `xml:"Relationship"`
	}
	if xml.Unmarshal(data, &doc) != nil {
		return nil
	}
	out := make([]xlsxRel, 0, len(doc.Rels))
	for _, r := range doc.Rels {
		if strings.EqualFold(r.Mode, "External") {
			continue
		}
		target := strings.ReplaceAll(r.Target, "\\", "/")
		if strings.HasPrefix(target, "/") {
			target = target[1:]
		} else {
			target = path.Join(dir, target)
		}
		out = append(out, xlsxRel{id: r.ID, kind: path.Base(r.Type), target: path.Clean(target)})
	}
	return out
}

// xlsxPart is the name the zip stores a part under: a package's part names
// ignore case.
func xlsxPart(z *ZipArchive, name string) string {
	if _, ok := z.files[name]; ok {
		return name
	}
	for n := range z.files {
		if strings.EqualFold(n, name) {
			return n
		}
	}
	return name
}

// xlsxFormats are the number formats of a workbook's cell styles, in their
// order; none without styles.
func xlsxFormats(z *ZipArchive, part string) ([]numberFormat, error) {
	if part == "" {
		return nil, nil
	}
	data, err := z.Read(xlsxPart(z, part), maxWorkbookPart)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, workbookError(err)
	}
	var doc struct {
		NumFmts []struct {
			ID   int    `xml:"numFmtId,attr"`
			Code string `xml:"formatCode,attr"`
		} `xml:"numFmts>numFmt"`
		Xfs []struct {
			NumFmt int `xml:"numFmtId,attr"`
		} `xml:"cellXfs>xf"`
	}
	if xml.Unmarshal(data, &doc) != nil {
		return nil, errNotAWorkbook
	}
	codes := make(map[int]string, len(doc.NumFmts))
	for _, f := range doc.NumFmts {
		codes[f.ID] = f.Code
	}
	out := make([]numberFormat, len(doc.Xfs))
	for i, x := range doc.Xfs {
		if code, ok := codes[x.NumFmt]; ok {
			out[i] = numberFormatOf(code)
		} else {
			out[i] = builtinFormat(x.NumFmt)
		}
	}
	return out, nil
}

// xlsxSharedStrings are a workbook's shared strings, which its cells name by
// their place in the list; none without them.
func xlsxSharedStrings(z *ZipArchive, part string) ([]string, error) {
	if part == "" {
		return nil, nil
	}
	r, _, err := z.Assets().Open(xlsxPart(z, part))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, workbookError(err)
	}
	defer r.Close()
	w := newWorkbookXML(r)
	var out []string
	held := 0
	for {
		tok, err := w.token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, workbookError(err)
		}
		if se, ok := tok.(xml.StartElement); !ok || se.Name.Local != "si" {
			continue
		}
		s, err := richText(w)
		if err != nil {
			return nil, workbookError(err)
		}
		if held += len(s) + 16; held > maxSharedText {
			return nil, readError{fmt.Sprintf("The workbook holds more text than an import reads (%d MB): save it as CSV and import that.", maxSharedText>>20)}
		}
		out = append(out, s)
	}
}

// richText is the text of the element just started (a shared string's <si>,
// a cell's <is>): its <t>, or its runs' <t> in turn, without the phonetic
// guides (<rPh>) Japanese text carries, its escapes undone (xstring).
func richText(w *workbookXML) (string, error) {
	var b strings.Builder
	inText := false
	for depth := 1; depth > 0; {
		tok, err := w.token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "rPh" {
				if err := w.skip(); err != nil {
					return "", err
				}
				continue
			}
			depth++
			inText = t.Name.Local == "t"
		case xml.EndElement:
			depth--
			inText = false
		case xml.CharData:
			if !inText {
				continue
			}
			if b.Len()+len(t) > maxSheetRowBytes {
				return "", runError{"a cell"}
			}
			b.Write(t)
		}
	}
	return xstring(b.String()), nil
}

// xstring is a workbook's escaped text (ECMA-376's ST_Xstring) as it reads:
// _xHHHH_ is the character HHHH names, which XML can't hold (a carriage
// return, a control character), and _x005F_ an underscore. Pure.
func xstring(s string) string {
	if !strings.Contains(s, "_x") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if i+7 <= len(s) && s[i] == '_' && s[i+1] == 'x' && s[i+6] == '_' {
			if n, err := strconv.ParseUint(s[i+2:i+6], 16, 16); err == nil {
				b.WriteRune(rune(n))
				i += 7
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// readSheet streams a worksheet's part out of the stored file, handing row
// each row with a value: its cells in their columns, a gap between them
// filled with empty ones.
func (b *xlsxBook) readSheet(part, name string, row func([]Cell) error) error {
	r, _, err := b.z.Assets().Open(xlsxPart(b.z, part))
	if err != nil {
		return sheetError(name, err)
	}
	defer r.Close()
	w := newWorkbookXML(r)
	inData := false
	var cells []Cell
	next := 0 // the column a cell without a reference takes
	for {
		tok, err := w.token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return sheetError(name, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch {
			case t.Name.Local == "sheetData":
				inData = true
			case !inData:
			case t.Name.Local == "row":
				cells, next = nil, 0
			case t.Name.Local == "c":
				col, c, err := b.cell(w, t, next)
				if err != nil {
					return sheetError(name, err)
				}
				next = col + 1
				if col < maxSheetColumns && !c.empty() {
					for len(cells) <= col {
						cells = append(cells, Cell{})
					}
					cells[col] = c
				}
			default:
				if err := w.skip(); err != nil {
					return sheetError(name, err)
				}
			}
		case xml.EndElement:
			switch {
			case !inData:
			case t.Name.Local == "row" && len(cells) > 0:
				if err := row(cells); err != nil {
					return err
				}
			case t.Name.Local == "sheetData":
				return nil
			}
		}
	}
}

// cell reads the cell just started, se, which sits in column next (from 0)
// unless its reference names another.
func (b *xlsxBook) cell(w *workbookXML, se xml.StartElement, next int) (int, Cell, error) {
	col := next
	var typ, style string
	for _, a := range se.Attr {
		switch a.Name.Local {
		case "r":
			if c, ok := columnOf(a.Value); ok {
				col = c
			}
		case "t":
			typ = a.Value
		case "s":
			style = a.Value
		}
	}
	var v, inline string
	computed := false
	for {
		tok, err := w.token()
		if err != nil {
			return 0, Cell{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "v":
				v, err = w.text()
			case "is":
				inline, err = richText(w)
			case "f":
				computed = true
				err = w.skip()
			default:
				err = w.skip()
			}
			if err != nil {
				return 0, Cell{}, err
			}
		case xml.EndElement:
			c, err := b.value(typ, style, v, inline)
			c.Formula = computed && !c.empty()
			return col, c, err
		}
	}
}

// value is a cell of type typ (its t: a shared string, inline text, a
// formula's text, a checkbox, an error, an ISO day, or else a number in
// style's format) holding v, or inline text.
func (b *xlsxBook) value(typ, style, v, inline string) (Cell, error) {
	if typ == "inlineStr" {
		return Cell{Text: inline}, nil
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return Cell{}, nil
	}
	switch typ {
	case "s":
		i, err := strconv.Atoi(v)
		if err != nil || i < 0 || i >= len(b.strings) {
			return Cell{}, errors.New("a cell names a shared string the workbook doesn't have")
		}
		return Cell{Text: b.strings[i]}, nil
	case "str":
		return Cell{Text: xstring(v)}, nil
	case "b":
		return BoolCell(v == "1" || strings.EqualFold(v, "true")), nil
	case "e":
		return Cell{Kind: CellError, Text: v}, nil
	case "d":
		if t, ok := isoDay(v); ok {
			return DayCell(t), nil
		}
		return Cell{Text: v}, nil
	}
	n, ok := finite(v)
	if !ok {
		return Cell{Text: v}, nil
	}
	var f numberFormat
	if i, err := strconv.Atoi(style); err == nil && i >= 0 && i < len(b.formats) {
		f = b.formats[i]
	}
	return f.cell(n, b.date1904), nil
}

// columnOf is the column a cell reference names, from 0 ("B12" is 1); false
// past XFD, the last.
func columnOf(ref string) (int, bool) {
	col, i := 0, 0
	for ; i < len(ref) && i < 4; i++ {
		c := ref[i] | 0x20
		if c < 'a' || c > 'z' {
			break
		}
		col = col*26 + int(c-'a') + 1
	}
	if i == 0 || col > 16384 {
		return 0, false
	}
	return col - 1, true
}

// numberFormat is what a workbook's number format says its numbers are.
type numberFormat struct {
	kind formatKind
	// sign is a currency's sign as it shows ("$", "₹", "€"), or its code.
	sign string
}

type formatKind uint8

const (
	formatNumber formatKind = iota
	formatPercent
	formatCurrency
	// formatDay is a day; formatDayTime a day and its time of day.
	formatDay
	formatDayTime
	// formatClock is a time of day; formatElapsed a length of time ([h]:mm).
	formatClock
	formatElapsed
)

// cell is n, shown in format f, as a cell. Pure.
func (f numberFormat) cell(n float64, date1904 bool) Cell {
	switch f.kind {
	case formatDay, formatDayTime:
		if t, ok := excelDay(n, date1904); ok {
			if f.kind == formatDay {
				t = t.Truncate(24 * time.Hour)
			}
			return DayCell(t)
		}
	case formatClock:
		if n >= 0 && n < maxSerial {
			return DurationCell(math.Mod(math.Round(n*86400), 86400))
		}
	case formatElapsed:
		if n >= 0 && n < maxSerial {
			return DurationCell(math.Round(n * 86400))
		}
	case formatPercent:
		return percentCell(n)
	case formatCurrency:
		return moneyCell(n, f.sign)
	}
	return numberCell(n)
}

// NumberCell is n as a cell shown in code, a spreadsheet's number format
// (Excel's, which a Google Sheet's pattern is too): a day, a day and its
// time, a time of day, a length of time, a percent, an amount of money or a
// number, as numberFormatOf reads code. Its days count from 30 December 1899
// as a Google Sheet's do, with no 29 February 1900. Pure.
func NumberCell(n float64, code string) Cell {
	f := numberFormatOf(code)
	if (f.kind == formatDay || f.kind == formatDayTime) && n >= 1 && n < 60 {
		// excelDay counts Excel's first 59 days from 31 December 1899.
		n--
	}
	return f.cell(n, false)
}

// excelDay is the day and time a workbook's serial number names: days since
// 30 December 1899, or since 1 January 1904 in a 1904 workbook, the fraction
// its time of day, to the second. Excel counts 29 February 1900, a day that
// never was, as Lotus 1-2-3 did: its serial, 60, is taken as the 28th, and
// those before it are a day later than the count from 1899 says. False
// before the first day or past 31 December 9999. Pure.
func excelDay(serial float64, date1904 bool) (time.Time, bool) {
	if !(serial >= 0 && serial < maxSerial) {
		return time.Time{}, false
	}
	secs := int64(math.Round(serial * 86400))
	days, rest := secs/86400, secs%86400
	var from time.Time
	switch {
	case date1904:
		from = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	case days < 60:
		from = time.Date(1899, 12, 31, 0, 0, 0, 0, time.UTC)
	case days == 60:
		from, days = time.Date(1900, 2, 28, 0, 0, 0, 0, time.UTC), 0
	default:
		from = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	}
	return from.AddDate(0, 0, int(days)).Add(time.Duration(rest) * time.Second), true
}

// builtinFormat is what a number format a workbook names by its id alone
// says (ECMA-376 part 1, §18.8.30): 9 and 10 are percents; 14 to 22 and 45
// to 47, and the East Asian 27 to 36 and 50 to 58, are days and times. The
// currencies among them (5 to 8, 42, 44) show the sign of the reader's own
// locale, which the file doesn't say, so they read as plain numbers. Pure.
func builtinFormat(id int) numberFormat {
	switch {
	case id == 9 || id == 10:
		return numberFormat{kind: formatPercent}
	case id >= 14 && id <= 17, id >= 27 && id <= 31, id == 36, id >= 50 && id <= 54, id == 57, id == 58:
		return numberFormat{kind: formatDay}
	case id == 22:
		return numberFormat{kind: formatDayTime}
	case id >= 18 && id <= 21, id >= 32 && id <= 35, id == 45, id == 47, id == 55, id == 56:
		return numberFormat{kind: formatClock}
	case id == 46:
		return numberFormat{kind: formatElapsed}
	}
	return numberFormat{}
}

// numberFormatOf is what a number format's code says its numbers are. Its
// first section (the one for numbers above zero) is read outside its quotes,
// escapes and brackets: a day, month or year part makes it a day, an hour,
// second or AM/PM a time of day, both a day and its time; [h], [m] or [s] a
// length of time; % a percent; and a currency's sign (quoted, escaped, bare,
// or in [$€-407]) an amount of money. A month (m) next to an hour or a
// second is minutes. Pure.
func numberFormatOf(code string) numberFormat {
	section := firstSection(code)
	if strings.EqualFold(strings.TrimSpace(section), "general") {
		return numberFormat{}
	}
	var day, month, clock, elapsed, percent bool
	sign := ""
	for i := 0; i < len(section); {
		r, size := utf8.DecodeRuneInString(section[i:])
		i += size
		switch {
		case r == '"':
			end := strings.IndexByte(section[i:], '"')
			if end < 0 {
				end = len(section) - i
			}
			sign = firstSign(sign, section[i:i+end])
			i = min(i+end+1, len(section))
		case r == '\\' || r == '_' || r == '*':
			next, n := utf8.DecodeRuneInString(section[i:])
			if r == '\\' && n > 0 {
				sign = firstSign(sign, string(next))
			}
			i += n
		case r == '[':
			end := strings.IndexByte(section[i:], ']')
			if end < 0 {
				end = len(section) - i
			}
			inner := section[i : i+end]
			i = min(i+end+1, len(section))
			if s, ok := strings.CutPrefix(inner, "$"); ok {
				s, _, _ = strings.Cut(s, "-")
				sign = firstSign(sign, s)
			} else if inner != "" && strings.Trim(strings.ToLower(inner), "hms") == "" {
				elapsed = true
			}
		case r == '%':
			percent = true
		case strings.ContainsRune("yYdD", r):
			day = true
		case strings.ContainsRune("hHsS", r):
			clock = true
		case r == 'm' || r == 'M':
			month = true
		case r == 'A' || r == 'a':
			for _, half := range []string{"AM/PM", "A/P"} {
				if strings.HasPrefix(strings.ToUpper(section[i-size:]), half) {
					clock = true
					i += len(half) - size
					break
				}
			}
		case unicode.Is(unicode.Sc, r):
			sign = firstSign(sign, string(r))
		}
	}
	isDay := day || (month && !clock)
	switch {
	case elapsed:
		return numberFormat{kind: formatElapsed}
	case isDay && clock:
		return numberFormat{kind: formatDayTime}
	case isDay:
		return numberFormat{kind: formatDay}
	case clock:
		return numberFormat{kind: formatClock}
	case percent:
		return numberFormat{kind: formatPercent}
	case sign != "":
		return numberFormat{kind: formatCurrency, sign: sign}
	}
	return numberFormat{}
}

// firstSection is a number format's code up to its first ; outside quotes,
// escapes and brackets: the format of numbers above zero. Pure.
func firstSection(code string) string {
	quoted := false
	for i := 0; i < len(code); i++ {
		switch c := code[i]; {
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '\\':
			i++
		case c == '[':
			if j := strings.IndexByte(code[i:], ']'); j >= 0 {
				i += j
			}
		case c == ';':
			return code[:i]
		}
	}
	return code
}

// firstSign is sign, or the currency sign literal text in a number format
// shows when sign is none yet: a currency symbol in it ("₹", "€"), or the
// whole of it when it names dollars or rupees ("USD", "Rs."), and not text
// such as " km". Pure.
func firstSign(sign, literal string) string {
	if sign != "" {
		return sign
	}
	if i := strings.IndexFunc(literal, func(r rune) bool { return unicode.Is(unicode.Sc, r) }); i >= 0 {
		r, _ := utf8.DecodeRuneInString(literal[i:])
		return string(r)
	}
	if code := strings.TrimSpace(literal); currencyOf(code) == "USD" || currencyOf(code) == "INR" {
		return code
	}
	return ""
}
