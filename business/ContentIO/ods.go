package business

// OpenDocument spreadsheets (.ods) as an import reads them (tables S1,
// design/tables-vs-airtable/PROGRAM.md §15): by hand, on archive/zip and
// encoding/xml within the unpack rule (openPackage), as the Excel reader is.
// Every sheet is in one part, content.xml: ReadSheets opens the spreadsheet
// once and lists them, and each is streamed from the stored file each time
// it is read, every read unpacking within the spreadsheet's one budget; a
// read goes on from where the last one ended (odsCursor), so reading the
// sheets in turn unpacks content.xml once. A cell says what it holds
// (office:value-type), and its text is as the file shows it. Repeated cells
// and rows are counted, not spelled out: LibreOffice writes a sheet's unused
// rows as one row repeated a million times.

import (
	"encoding/xml"
	"errors"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
)

const (
	odsOffice = "urn:oasis:names:tc:opendocument:xmlns:office:1.0"
	odsTable  = "urn:oasis:names:tc:opendocument:xmlns:table:1.0"
	odsText   = "urn:oasis:names:tc:opendocument:xmlns:text:1.0"
	odsStyle  = "urn:oasis:names:tc:opendocument:xmlns:style:1.0"
)

var errNotASpreadsheet = readError{"This is not an OpenDocument spreadsheet (.ods) that can be read."}

// odsSheet is a sheet as the spreadsheet lists it.
type odsSheet struct {
	name   string
	hidden bool
}

// readODS reads an OpenDocument spreadsheet's sheets, in its order.
func readODS(in Input) ([]Sheet, error) {
	z, err := openPackage(in.File, in.Size, nil)
	if err != nil {
		return nil, errNotASpreadsheet
	}
	r, _, err := z.Assets().Open("content.xml")
	if err != nil {
		if errors.Is(err, errUnpackBudget) {
			return nil, readError{"The spreadsheet unpacks to more than an import reads."}
		}
		return nil, errNotASpreadsheet
	}
	defer r.Close()
	listed, err := odsSheets(newWorkbookXML(r))
	if err != nil {
		return nil, errNotASpreadsheet
	}
	c := &odsCursor{z: z, sheets: len(listed)}
	out := make([]Sheet, len(listed))
	for i, s := range listed {
		out[i] = Sheet{Name: s.name, Hidden: s.hidden, Rows: func(row func([]Cell) error) error {
			return c.read(i, s.name, row)
		}}
	}
	return out, nil
}

// odsSheets lists a spreadsheet's sheets from its content: their names, and
// whether a sheet's style hides it (table:display="false").
func odsSheets(w *workbookXML) ([]odsSheet, error) {
	hiding := map[string]bool{} // table styles that hide their sheet
	style := ""                 // the table style being read
	var out []odsSheet
	for {
		tok, err := w.token()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch {
		case se.Name.Space == odsStyle && se.Name.Local == "style":
			style = ""
			if attr(se, "family") == "table" {
				style = attr(se, "name")
			}
		case se.Name.Space == odsStyle && se.Name.Local == "table-properties":
			if style != "" && attr(se, "display") == "false" {
				hiding[style] = true
			}
		case se.Name.Space == odsTable && se.Name.Local == "table":
			out = append(out, odsSheet{name: attr(se, "name"), hidden: hiding[attr(se, "style-name")]})
			if err := w.skip(); err != nil {
				return nil, err
			}
		}
	}
}

// odsCursor reads a spreadsheet's sheets out of content.xml, one read at a
// time, going on from the end of the last sheet read: an import reads the
// sheets in turn, and reading each from the part's start would unpack the
// part once a sheet. A read of an earlier sheet, or after a read that
// stopped partway, starts from the part's start again.
type odsCursor struct {
	z      *ZipArchive
	sheets int
	r      io.ReadCloser
	w      *workbookXML
	// next is the sheet whose table the part comes to next.
	next int
}

// read streams the index-th sheet, handing row its rows.
func (c *odsCursor) read(index int, name string, row func([]Cell) error) error {
	if c.w == nil || index < c.next {
		c.close()
		r, _, err := c.z.Assets().Open("content.xml")
		if err != nil {
			return sheetError(name, err)
		}
		c.r, c.w, c.next = r, newWorkbookXML(r), 0
	}
	for {
		tok, err := c.w.token()
		if err != nil {
			c.close()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return sheetError(name, err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Space != odsTable || se.Name.Local != "table" {
			continue
		}
		c.next++
		if c.next-1 == index {
			err := odsRows(c.w, name, row)
			if err != nil || c.next == c.sheets {
				c.close()
			}
			return err
		}
		if err := c.w.skip(); err != nil {
			c.close()
			return sheetError(name, err)
		}
	}
}

// close ends the read of content.xml: the next read starts it again.
func (c *odsCursor) close() {
	if c.r != nil {
		c.r.Close()
	}
	c.r, c.w = nil, nil
}

// odsRows reads the rows of the sheet just started up to its end, handing
// row each one with a value as many times as it repeats (never past
// maxSheetRows + 1, past which ReadSheets refuses the sheet).
func odsRows(w *workbookXML, name string, row func([]Cell) error) error {
	for {
		tok, err := w.token()
		if err != nil {
			return sheetError(name, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space != odsTable || t.Name.Local != "table-row" {
				continue
			}
			repeat := repeatOf(t, "number-rows-repeated")
			cells, err := odsRow(w)
			if err != nil {
				return sheetError(name, err)
			}
			if len(cells) == 0 {
				continue
			}
			for ; repeat > 0; repeat-- {
				if err := row(cells); err != nil {
					return err
				}
			}
		case xml.EndElement:
			if t.Name.Space == odsTable && t.Name.Local == "table" {
				return nil
			}
		}
	}
}

// repeatOf is how many times a row or a cell repeats (table:number-rows-
// repeated, table:number-columns-repeated): 1 unless it says more, and
// never past maxSheetRows + 1, beyond which no count matters.
func repeatOf(se xml.StartElement, local string) int {
	n, err := strconv.Atoi(attr(se, local))
	if err != nil || n < 1 {
		return 1
	}
	return min(n, maxSheetRows+1)
}

// odsRow reads the cells of the row just started up to its end: empty cells
// are counted, and spelled out only before a cell with a value, so a row
// ends at its last value; none for an empty row.
func odsRow(w *workbookXML) ([]Cell, error) {
	var cells []Cell
	empties := 0
	for {
		tok, err := w.token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space != odsTable || (t.Name.Local != "table-cell" && t.Name.Local != "covered-table-cell") {
				if err := w.skip(); err != nil {
					return nil, err
				}
				continue
			}
			repeat := repeatOf(t, "number-columns-repeated")
			c, err := odsCell(w, t)
			if err != nil {
				return nil, err
			}
			if c.empty() {
				empties = min(empties+repeat, maxSheetColumns)
				continue
			}
			for ; empties > 0 && len(cells) < maxSheetColumns; empties-- {
				cells = append(cells, Cell{})
			}
			empties = 0
			for ; repeat > 0 && len(cells) < maxSheetColumns; repeat-- {
				cells = append(cells, c)
			}
		case xml.EndElement:
			return cells, nil
		}
	}
}

// odsCell reads the cell just started, se, up to its end: what its value
// type says it holds, with its text as the file shows it. A value that
// doesn't read as its type says keeps the cell's text.
func odsCell(w *workbookXML, se xml.StartElement) (Cell, error) {
	text, err := odsCellText(w)
	if err != nil {
		return Cell{}, err
	}
	var typ, value, date, clock, truth, currency, str string
	computed, failed := false, false
	for _, a := range se.Attr {
		switch {
		case a.Name.Space == odsOffice:
			switch a.Name.Local {
			case "value-type":
				typ = a.Value
			case "value":
				value = a.Value
			case "date-value":
				date = a.Value
			case "time-value":
				clock = a.Value
			case "boolean-value":
				truth = a.Value
			case "currency":
				currency = a.Value
			case "string-value":
				str = a.Value
			}
		case a.Name.Local == "formula":
			computed = a.Value != ""
		case a.Name.Local == "value-type" && a.Value == "error":
			// LibreOffice says a formula's error in a value type of its own
			// namespace (calcext), beside a string's.
			failed = true
		}
	}
	if failed {
		return Cell{Kind: CellError, Text: text, Formula: computed}, nil
	}
	var c Cell
	ok := true
	switch typ {
	case "float":
		var n float64
		n, ok = finite(value)
		c = numberCell(n)
	case "percentage":
		var n float64
		n, ok = finite(value)
		c = percentCell(n)
	case "currency":
		var n float64
		n, ok = finite(value)
		c = moneyCell(n, currency)
	case "date":
		t, read := isoDay(date)
		c, ok = DayCell(t), read
	case "time":
		secs, read := isoDuration(clock)
		c, ok = DurationCell(secs), read
	case "boolean":
		c = BoolCell(truth == "true" || truth == "1")
	default:
		if text == "" {
			text = str
		}
		ok = false
	}
	if !ok {
		return Cell{Text: text, Formula: computed && text != ""}, nil
	}
	if text != "" {
		c.Text = text
	}
	c.Formula = computed
	return c, nil
}

// odsCellText is the text of the cell just started, up to its end: its
// paragraphs a line each, with their spaces (text:s), tabs and line breaks,
// and the text of what they hold (spans, links); never its comment
// (office:annotation), its drawings, or a table inside it.
func odsCellText(w *workbookXML) (string, error) {
	var b strings.Builder
	paragraphs, open := 0, 0
	for depth := 1; depth > 0; {
		tok, err := w.token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space != odsText {
				if err := w.skip(); err != nil {
					return "", err
				}
				continue
			}
			depth++
			switch t.Name.Local {
			case "p", "h":
				if open == 0 && paragraphs > 0 {
					b.WriteByte('\n')
				}
				if open == 0 {
					paragraphs++
				}
				open++
			case "s":
				n, err := strconv.Atoi(attr(t, "c"))
				if err != nil || n < 1 {
					n = 1
				}
				if n > maxSheetRowBytes-b.Len() {
					return "", runError{"a cell"}
				}
				b.WriteString(strings.Repeat(" ", n))
			case "tab":
				b.WriteByte('\t')
			case "line-break":
				b.WriteByte('\n')
			}
		case xml.EndElement:
			depth--
			if t.Name.Space == odsText && (t.Name.Local == "p" || t.Name.Local == "h") {
				open--
			}
		case xml.CharData:
			if open > 0 {
				b.Write(t)
			}
		}
		if b.Len() > maxSheetRowBytes {
			return "", runError{"a cell"}
		}
	}
	return b.String(), nil
}

var isoDurationText = regexp.MustCompile(`^(-)?P(?:(\d{1,9}(?:[.,]\d{1,9})?)D)?(?:T(?:(\d{1,9}(?:[.,]\d{1,9})?)H)?(?:(\d{1,9}(?:[.,]\d{1,9})?)M)?(?:(\d{1,9}(?:[.,]\d{1,9})?)S)?)?$`)

// isoDuration reads an ISO 8601 length of time (PT26H15M00S) in seconds, to
// the second. Pure.
func isoDuration(s string) (float64, bool) {
	m := isoDurationText.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil || m[2]+m[3]+m[4]+m[5] == "" {
		return 0, false
	}
	secs := 0.0
	for i, per := range []float64{86400, 3600, 60, 1} {
		if m[i+2] != "" {
			n, _ := strconv.ParseFloat(strings.Replace(m[i+2], ",", ".", 1), 64)
			secs += n * per
		}
	}
	if m[1] == "-" {
		secs = -secs
	}
	return math.Round(secs), true
}
