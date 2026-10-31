package business

// What the Excel (xlsx.go) and OpenDocument (ods.go) readers share: the one
// way a workbook's XML is read, within limits, and its typed cells.

import (
	"bufio"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	formula "github.com/akashc777/OneCamp/business/Formula"
)

// maxXMLDepth is how deep a workbook's XML may nest: a sheet's cells sit six
// or seven elements down, an OpenDocument cell's text a few more.
const maxXMLDepth = 64

var errTooDeep = errors.New("content io: a workbook's XML nests too deep")

// workbookXML reads a workbook part's XML as an import does: no token past
// maxSheetRowBytes (runCap), and no element nested past maxXMLDepth, so a
// part made to fill memory is an error. Elements are skipped by its own
// skip, which keeps its depth true.
type workbookXML struct {
	d     *xml.Decoder
	depth int
}

func newWorkbookXML(r io.Reader) *workbookXML {
	return &workbookXML{d: xml.NewDecoder(bufio.NewReaderSize(&runCap{r: r, sep: '<', what: "a cell"}, 64<<10))}
}

// token is the next token, or io.EOF after the last.
func (w *workbookXML) token() (xml.Token, error) {
	tok, err := w.d.Token()
	if err != nil {
		return nil, err
	}
	switch tok.(type) {
	case xml.StartElement:
		if w.depth++; w.depth > maxXMLDepth {
			return nil, errTooDeep
		}
	case xml.EndElement:
		w.depth--
	}
	return tok, nil
}

// skip reads past the end of the element just started.
func (w *workbookXML) skip() error {
	for depth := 1; depth > 0; {
		tok, err := w.token()
		if err != nil {
			return err
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return nil
}

// text is the text of the element just started, up to its end; what it
// holds inside other elements is left out.
func (w *workbookXML) text() (string, error) {
	var b strings.Builder
	for {
		tok, err := w.token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			b.Write(t)
		case xml.StartElement:
			if err := w.skip(); err != nil {
				return "", err
			}
		case xml.EndElement:
			return b.String(), nil
		}
	}
}

// sheetError is what reading a sheet failed with, said to the person
// importing: past the unpack budget, a cell past maxSheetRowBytes, or else a
// file that may be damaged.
func sheetError(name string, err error) error {
	var run runError
	switch {
	case errors.Is(err, errUnpackBudget):
		return readError{fmt.Sprintf("The sheet %q unpacks to more than an import reads.", name)}
	case errors.As(err, &run):
		return readError{fmt.Sprintf("The sheet %q has a cell that runs past %d MB.", name, maxSheetRowBytes>>20)}
	}
	return readError{fmt.Sprintf("The sheet %q could not be read: the file may be damaged.", name)}
}

// finite reads s as a number other than NaN or an infinity. Pure.
func finite(s string) (float64, bool) {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func numberCell(n float64) Cell {
	return Cell{Kind: CellNumber, Number: n, Text: formula.FormatNumber(n)}
}

// percentCell is a fraction shown as a percent: 0.125 holds 12.5.
func percentCell(fraction float64) Cell {
	p, _ := strconv.ParseFloat(formula.FormatNumber(fraction*100), 64)
	return Cell{Kind: CellNumber, Number: p, Percent: true, Text: formula.FormatNumber(p) + "%"}
}

// moneyCell is an amount shown with a currency's sign ("$", "₹") or code
// ("EUR").
func moneyCell(n float64, sign string) Cell {
	amount := formula.FormatNumber(math.Abs(n))
	text := sign + amount
	if sign != "" && strings.IndexFunc(sign, func(r rune) bool { return !unicode.IsLetter(r) }) < 0 {
		text = amount + " " + sign
	}
	if n < 0 {
		text = "-" + text
	}
	return Cell{Kind: CellNumber, Number: n, Currency: currencyOf(sign), Text: text}
}

// currencyOf is a currency's sign or code as a cell's Currency: "USD" and
// "INR" for the two a table's number shows, "other" for another, "" for
// none. Pure.
func currencyOf(sign string) string {
	switch strings.ToUpper(strings.TrimSpace(sign)) {
	case "":
		return ""
	case "$", "US$", "USD":
		return "USD"
	case "₹", "RS", "RS.", "INR":
		return "INR"
	}
	return "other"
}

// BoolCell is a checkbox's value, written as TRUE or FALSE.
func BoolCell(b bool) Cell {
	text := "FALSE"
	if b {
		text = "TRUE"
	}
	return Cell{Kind: CellBool, Bool: b, Text: text}
}

// DayCell is a day and its time of day, written as 2026-10-23, or 2026-10-23
// 14:30 (14:30:05) when it has a time.
func DayCell(t time.Time) Cell {
	text := t.Format("2006-01-02")
	switch {
	case t.Second() != 0:
		text = t.Format("2006-01-02 15:04:05")
	case t.Hour() != 0 || t.Minute() != 0:
		text = t.Format("2006-01-02 15:04")
	}
	return Cell{Kind: CellDate, Time: t, Text: text}
}

// DurationCell is a length of time in seconds, written as 1:30, or 1:30:15
// when it has seconds.
func DurationCell(secs float64) Cell {
	s := int64(math.Abs(secs))
	text := fmt.Sprintf("%d:%02d", s/3600, s/60%60)
	if s%60 != 0 {
		text += fmt.Sprintf(":%02d", s%60)
	}
	if secs < 0 {
		text = "-" + text
	}
	return Cell{Kind: CellDuration, Number: secs, Text: text}
}

// isoDay reads an ISO 8601 day, with or without a time of day, as written: a
// zone after it is left aside, since the day and the time are what the
// sheet shows. Pure.
func isoDay(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, 'T'); i >= 0 {
		if j := strings.IndexAny(s[i:], "Z+-"); j >= 0 {
			s = s[:i+j]
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
