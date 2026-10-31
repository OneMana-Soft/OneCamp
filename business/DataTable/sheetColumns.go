package business

// A spreadsheet's columns as fields (tables S1, design/tables-vs-airtable/
// PROGRAM.md §15). An import reads a sheet's cells once and gives each column
// the narrowest kind all its cells fit, so the preview shows, and the import
// stores, what someone setting the table up by hand would choose. A
// workbook's cell that says what it holds (a number, a checkbox, a day, a
// duration) is taken as it is; text reads as typed text does: a number as
// formula.ParseNumber reads it, with the import's number signs, and a day in
// the one order all its column's cells are written in (a typed date cell
// takes only ISO). A date field holds the day, stored as ISO.

import (
	"cmp"
	"fmt"
	"math"
	"net/mail"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	formula "github.com/akashc777/OneCamp/business/Formula"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// SheetOptions are how a sheet's text reads: the signs its numbers are
// written with, whether a date like 03/04/2026 is the 3rd of April
// (DayFirst) or March the 4th when its column doesn't say, and the zone of
// the person importing, which a moment written with its own zone
// (2026-10-23T22:30:00Z) falls on a day in.
type SheetOptions struct {
	Signs    formula.NumberSigns
	DayFirst bool
	Zone     *time.Location
}

// SheetColumns is a sheet's columns as fields, named by its header (its first
// row with a value) and guessed from the rows under it.
type SheetColumns struct {
	// Fields are the columns' fields in order, keyed "c1", "c2", ...
	Fields []ImportField
	// Rows is how many rows with a value the header stands over.
	Rows int
	// Notes are what the fields keep of their cells less than whole: a
	// formula's last value without the formula, a day without its time. By
	// field key; the importer names the table.
	Notes []ImportNote
	// reads are how each column's cells are stored: nil for a column left
	// out (choose).
	reads   []cellRead
	guesses []*columnGuess
	opts    SheetOptions
	// sample is the first sampleRows rows under the header, as the file
	// shows each cell, for a preview.
	sample [][]string
	// headers are the columns' header cells as written, trimmed: a name
	// another column has too is numbered in Fields, not here.
	headers []string
}

// sampleRows is how many of a sheet's rows its preview shows.
const sampleRows = 50

// cellRead is a cell holding a value as its field stores it; false for one
// the field can't store.
type cellRead func(c contentIOBusiness.Cell) (any, bool)

const (
	// A column is a select when its values are few (maxGuessedOptions at
	// most, each maxOptionRunes long at most) and repeat: it has at least
	// twice as many cells as values. A multi-select's are its cells' comma
	// lists' parts.
	maxGuessedOptions = 25
	maxOptionRunes    = 60
	// longTextRunes is how long a cell runs before its column is long text.
	longTextRunes = 255
	// maxColumnNameRunes is how long a column's name from its header is kept.
	maxColumnNameRunes = 100
	// maxSheetFields is how many of a sheet's columns, from its first, a
	// table takes: more than any table a team works in, short of one too
	// wide to read.
	maxSheetFields = 500
)

// GuessColumns reads sheet once: its header names its columns, and every row
// under it says their kinds. Columns past the first maxSheetFields are left
// out, and said.
func GuessColumns(sheet contentIOBusiness.Sheet, opts SheetOptions) (*SheetColumns, error) {
	if opts.Zone == nil {
		opts.Zone = time.UTC
	}
	var header []string
	var cols []*columnGuess
	var sample [][]string
	rows := 0
	over, err := eachRow(sheet, func(head bool, cells []contentIOBusiness.Cell) error {
		if head {
			for _, c := range cells {
				header = append(header, c.Text)
			}
			return nil
		}
		rows++
		var shown []string
		for i, c := range cells {
			for len(cols) <= i {
				cols = append(cols, newColumnGuess())
			}
			cols[i].see(c, opts)
			if len(sample) < sampleRows {
				shown = append(shown, c.Text)
			}
		}
		if len(sample) < sampleRows {
			sample = append(sample, shown)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for len(cols) < len(header) {
		cols = append(cols, newColumnGuess())
	}
	out := &SheetColumns{Rows: rows, guesses: cols, opts: opts, sample: sample}
	names := map[string]bool{}
	for i, c := range cols {
		name := ""
		if i < len(header) {
			name = strings.TrimSpace(header[i])
		}
		kind := c.kind()
		f := ImportField{Key: "c" + strconv.Itoa(i+1), Name: columnName(name, i, names), Type: kind, Config: c.configFor(kind)}
		out.Fields = append(out.Fields, f)
		out.headers = append(out.headers, name)
		out.reads = append(out.reads, readerFor(kind, opts, c.dayOrder(opts)))
		out.Notes = append(out.Notes, c.notes(f)...)
	}
	switch {
	case over == 1:
		out.Notes = append(out.Notes, ImportNote{Code: "COLUMNS_LEFT_OUT", Msg: fmt.Sprintf("A table takes a sheet's first %d columns, so the one after them isn't brought.", maxSheetFields)})
	case over > 1:
		out.Notes = append(out.Notes, ImportNote{Code: "COLUMNS_LEFT_OUT", Msg: fmt.Sprintf("A table takes a sheet's first %d columns, so the %d after them aren't brought.", maxSheetFields, over)})
	}
	return out, nil
}

// sheetKinds are the kinds a person may make a sheet's column.
var sheetKinds = map[string]bool{
	model.FieldText: true, model.FieldLongText: true, model.FieldNumber: true, model.FieldDate: true, model.FieldCheckbox: true,
	model.FieldSelect: true, model.FieldMultiSelect: true, model.FieldEmail: true, model.FieldURL: true, model.FieldPhone: true,
	model.FieldDuration: true,
}

// matchKinds are the kinds of field whose value can name the row a sheet's
// row changes: a value written out whole, as the row's value is stored.
var matchKinds = map[string]bool{
	model.FieldText: true, model.FieldNumber: true, model.FieldDate: true, model.FieldEmail: true,
	model.FieldURL: true, model.FieldPhone: true, model.FieldDuration: true,
}

// leave leaves column i out, with its notes, and says why unless why is "".
func (s *SheetColumns) leave(i int, why string) {
	key := s.Fields[i].Key
	s.reads[i] = nil
	s.Notes = slices.DeleteFunc(s.Notes, func(n ImportNote) bool { return n.Field == key })
	if why != "" {
		s.Notes = append(s.Notes, ImportNote{Field: key, Code: "COLUMN_LEFT_OUT", Msg: why})
	}
}

// into makes the columns go into fields, those of a table the person has:
// each into the field chosen for it (fieldChoice.Field, by id), else the
// field of its header's name, ignoring case, its cells read as that field's kind reads
// them (choose). A column chosen out doesn't come; nor, said, does one with
// no such field, one whose field an earlier column fills, or one into a field
// a sheet's cells can't fill (sheetKinds). A column takes its field's name,
// kind and settings (none guessed for it are kept), so a choice of its own
// of a name or kind can't be followed.
func (s *SheetColumns) into(fields []*model.Field, chosen []fieldChoice) error {
	byId, byName := map[string]*model.Field{}, map[string]*model.Field{}
	for _, f := range fields {
		byId[f.Id.String()] = f
		if name := strings.ToLower(strings.TrimSpace(f.Name)); byName[name] == nil {
			byName[name] = f
		}
	}
	choices := map[string]fieldChoice{}
	for _, ch := range chosen {
		switch {
		case !slices.ContainsFunc(s.Fields, func(f ImportField) bool { return f.Key == ch.Key }):
			return contentIOBusiness.ReadError(fmt.Sprintf("The choices name a column the sheet doesn't have (%q).", ch.Key))
		case ch.Name != "" || ch.Type != "":
			return contentIOBusiness.ReadError("A column coming into a table takes its field's name and kind.")
		case ch.Field != "" && byId[ch.Field] == nil:
			return contentIOBusiness.ReadError("The choices name a field the table doesn't have.")
		}
		choices[ch.Key] = ch
	}
	filled := map[uuid.UUID]bool{}
	for i, col := range s.Fields {
		ch := choices[col.Key]
		f := byId[ch.Field]
		if f == nil {
			f = byName[strings.ToLower(s.headers[i])]
		}
		switch {
		case ch.Leave:
			s.leave(i, "")
		case f == nil:
			s.leave(i, fmt.Sprintf("The table has no field named %q, so the column isn't brought.", cmp.Or(s.headers[i], col.Name)))
		case !sheetKinds[f.Type]:
			s.leave(i, fmt.Sprintf("%q is a %s field, which a sheet's cells can't fill, so the column isn't brought.", f.Name, f.Type))
		case filled[f.Id]:
			s.leave(i, fmt.Sprintf("An earlier column fills %q, so this one isn't brought.", f.Name))
		default:
			filled[f.Id] = true
			if err := s.choose(fieldChoice{Key: col.Key, Type: f.Type}); err != nil {
				return err
			}
			s.Fields[i].Name, s.Fields[i].Into, s.Fields[i].Config = f.Name, f.Id, nil
		}
	}
	return nil
}

// choose makes a column what the person chose of it: another name, another
// kind (its cells then read as that kind reads them, and its notes are that
// kind's), or left out.
func (s *SheetColumns) choose(ch fieldChoice) error {
	i := slices.IndexFunc(s.Fields, func(f ImportField) bool { return f.Key == ch.Key })
	if i < 0 {
		return contentIOBusiness.ReadError(fmt.Sprintf("The choices name a column the sheet doesn't have (%q).", ch.Key))
	}
	if ch.Leave {
		s.leave(i, "")
		return nil
	}
	f := &s.Fields[i]
	if name := strings.TrimSpace(ch.Name); name != "" {
		taken := map[string]bool{}
		for j, other := range s.Fields {
			if j != i && s.reads[j] != nil {
				taken[strings.ToLower(other.Name)] = true
			}
		}
		f.Name = columnName(name, i, taken)
	}
	if ch.Type == "" || ch.Type == f.Type {
		return nil
	}
	if !sheetKinds[ch.Type] {
		return contentIOBusiness.ReadError(fmt.Sprintf("A sheet's column can't be made a %q field.", ch.Type))
	}
	g := s.guesses[i]
	order := g.dayOrder(s.opts)
	f.Type, f.Config = ch.Type, g.configFor(ch.Type)
	s.reads[i] = readerFor(ch.Type, s.opts, order)
	s.Notes = slices.DeleteFunc(s.Notes, func(n ImportNote) bool { return n.Field == ch.Key })
	s.Notes = append(s.Notes, g.notes(*f)...)
	if n, what := g.unread(ch.Type, order); n == 1 {
		s.Notes = append(s.Notes, ImportNote{Field: f.Key, Code: "CELL_LEFT_OUT", Msg: fmt.Sprintf("A cell isn't %s, so it's left empty.", what[0])})
	} else if n > 1 {
		s.Notes = append(s.Notes, ImportNote{Field: f.Key, Code: "CELL_LEFT_OUT", Msg: fmt.Sprintf("%d of its cells aren't %s, so they're left empty.", n, what[1])})
	}
	return nil
}

// unread is how many of the column's cells with a value a field of kind
// can't hold, and so leaves empty, with what such a cell isn't (one, many):
// cells that don't read as its numbers, days in order, ticks or lengths of
// time. Other kinds take every cell, or refuse each one as the import
// writes it, which says so (an email, a link, a phone number).
func (c *columnGuess) unread(kind string, order int) (int, [2]string) {
	switch kind {
	case model.FieldNumber:
		return c.filled - c.numbers, [2]string{"a number", "numbers"}
	case model.FieldDate:
		return c.filled - c.dates[order], [2]string{"a date", "dates"}
	case model.FieldCheckbox:
		return c.filled - c.checks, [2]string{"yes or no", "yes or no"}
	case model.FieldDuration:
		return c.filled - c.durations - c.writtenDurations, [2]string{"a length of time", "lengths of time"}
	}
	return 0, [2]string{}
}

// kept are the columns' fields, but those left out.
func (s *SheetColumns) kept() []ImportField {
	var out []ImportField
	for i, f := range s.Fields {
		if s.reads[i] != nil {
			out = append(out, f)
		}
	}
	return out
}

// Row is a row of the sheet under its header as its fields store it, by
// field key; a cell without a value is left out.
func (s *SheetColumns) Row(cells []contentIOBusiness.Cell) map[string]any {
	out := map[string]any{}
	for i, c := range cells {
		if i >= len(s.reads) {
			break
		}
		if s.reads[i] == nil || !valued(c) {
			continue
		}
		if v, ok := s.reads[i](c); ok {
			out[s.Fields[i].Key] = v
		}
	}
	return out
}

// eachRow hands fn each row of sheet with a value in the columns a table
// takes (its first maxSheetFields), the first of them its header: a blank
// row above it, or a row of empty cells, names nothing. It says how many
// columns past those the widest row holds values out to.
func eachRow(sheet contentIOBusiness.Sheet, fn func(header bool, cells []contentIOBusiness.Cell) error) (over int, err error) {
	header := true
	err = sheet.Rows(func(cells []contentIOBusiness.Cell) error {
		if len(cells) > maxSheetFields {
			wide := len(cells)
			for wide > maxSheetFields && !valued(cells[wide-1]) {
				wide--
			}
			over = max(over, wide-maxSheetFields)
			cells = cells[:maxSheetFields]
		}
		if !slices.ContainsFunc(cells, valued) {
			return nil
		}
		head := header
		header = false
		return fn(head, cells)
	})
	return over, err
}

// valued says whether a cell holds a value: text other than spaces, or a
// workbook's typed value; a formula's error holds none. Pure.
func valued(c contentIOBusiness.Cell) bool {
	switch c.Kind {
	case contentIOBusiness.CellError:
		return false
	case contentIOBusiness.CellText:
		return strings.TrimSpace(c.Text) != ""
	}
	return true
}

// columnName is a column's name: its header's, cut to maxColumnNameRunes, or
// "Column N" for none, numbered on from 2 when one taken (seen) has it.
func columnName(name string, i int, seen map[string]bool) string {
	name = strings.TrimSpace(helpers.TruncateRunes(name, maxColumnNameRunes))
	if name == "" {
		name = fmt.Sprintf("Column %d", i+1)
	}
	return uniqueName(name, seen)
}

// The orders a numeric date is written in. A day-first and a month-first
// date read alike when neither part is past 12; an ISO date or a month
// written in words reads in either.
const (
	dayFirst = iota
	monthFirst
	dateOrders
)

// columnGuess is what a column's cells have shown so far.
type columnGuess struct {
	filled int
	// Cells each kind reads.
	numbers, percents, checks, emails, urls, phones, durations int
	dates                                                      [dateOrders]int
	// writtenDurations are the other cells whose text a length of time reads
	// (1:30, 90m, a bare 90 as minutes), as its reader does when a person
	// chooses one: the guess makes a column one only from a workbook's typed
	// durations, since a bare number reads as one too. One with seconds sets
	// seconds, as a typed one does.
	writtenDurations                          int
	leadingZero, long, tooManyValues, seconds bool
	// formulas are the cells whose value a formula worked out; failed the
	// formulas that couldn't be, the first of whose errors is failure; timed
	// the days written with a time.
	formulas, failed, timed int
	failure                 string
	// currency counts a number's currency: "USD", "INR", "" for none, or
	// "other" for a sign a column doesn't show.
	currency map[string]int
	values   []string
	counts   map[string]int
	// parts are a cell's comma list's parts, for a multi-select.
	parts      []string
	partCounts map[string]int
	lists      int
	partsSeen  int
	// choices are the values the column's dropdown offers (the first
	// dropdown's, an empty cell's too); listed, chipped and linked count the
	// cells with a value that have a dropdown, people chips and a link, and
	// linkTexts the links that show other text than where they go.
	choices                            []string
	listed, chipped, linked, linkTexts int
}

func newColumnGuess() *columnGuess {
	return &columnGuess{currency: map[string]int{}, counts: map[string]int{}, partCounts: map[string]int{}}
}

// see takes a cell into the guess.
func (c *columnGuess) see(cell contentIOBusiness.Cell, opts SheetOptions) {
	if cell.Kind == contentIOBusiness.CellError {
		if c.failed++; c.failure == "" {
			c.failure = cell.Text
		}
		return
	}
	if c.choices == nil && len(cell.Choices) > 0 {
		c.choices = slices.Clone(cell.Choices)
	}
	if !valued(cell) {
		return
	}
	if cell.Formula {
		c.formulas++
	}
	c.filled++
	s := strings.TrimSpace(cell.Text)
	if len(cell.Choices) > 0 {
		c.listed++
	}
	if len(cell.People) > 0 {
		c.chipped++
	}
	if cell.Link != "" {
		if c.linked++; s != cell.Link {
			c.linkTexts++
		}
	}
	if cell.Kind != contentIOBusiness.CellDuration {
		if n, ok := fieldTypes.ParseDuration(s); ok {
			c.writtenDurations++
			if math.Mod(n, 60) != 0 {
				c.seconds = true
			}
		}
	}
	switch cell.Kind {
	case contentIOBusiness.CellNumber:
		c.numbers++
		c.currency[cell.Currency]++
		if cell.Percent {
			c.percents++
		}
	case contentIOBusiness.CellBool:
		c.checks++
	case contentIOBusiness.CellDate:
		c.dates[dayFirst]++
		c.dates[monthFirst]++
		if h, m, sec := cell.Time.Clock(); h+m+sec > 0 {
			c.timed++
		}
	case contentIOBusiness.CellDuration:
		c.durations++
		if math.Mod(cell.Number, 60) != 0 {
			c.seconds = true
		}
	default:
		c.seeText(s, opts)
	}
	if strings.ContainsAny(s, "\r\n") || utf8.RuneCountInString(s) > longTextRunes {
		c.long = true
	}
	c.count(s)
}

// seeText takes a text cell's text into the guess: each kind it reads as.
func (c *columnGuess) seeText(s string, opts SheetOptions) {
	if _, cur, pct, ok := readNumber(s, opts.Signs); ok {
		c.numbers++
		c.currency[cur]++
		if pct {
			c.percents++
		}
	}
	if len(s) > 1 && s[0] == '0' && s[1] >= '0' && s[1] <= '9' {
		c.leadingZero = true
	}
	if _, ok := checkboxText(s); ok {
		c.checks++
	}
	timed := false
	for o := 0; o < dateOrders; o++ {
		if _, t, ok := readDate(s, o, opts.Zone); ok {
			c.dates[o]++
			timed = timed || t
		}
	}
	if timed {
		c.timed++
	}
	if isEmail(s) {
		c.emails++
	}
	if isURL(s) {
		c.urls++
	}
	if isPhone(s) {
		c.phones++
	}
}

// count takes s into the column's values, and its comma list's parts.
func (c *columnGuess) count(s string) {
	if !c.tooManyValues && c.counts[s] == 0 && (len(c.values) == maxGuessedOptions || utf8.RuneCountInString(s) > maxOptionRunes) {
		c.tooManyValues, c.values, c.counts = true, nil, nil
	}
	if !c.tooManyValues {
		if c.counts[s] == 0 {
			c.values = append(c.values, s)
		}
		c.counts[s]++
	}
	parts := listParts(s)
	if len(parts) > 1 {
		c.lists++
	}
	for _, p := range parts {
		if c.partCounts == nil {
			break
		}
		if c.partCounts[p] == 0 {
			if len(c.parts) == 2*maxGuessedOptions || utf8.RuneCountInString(p) > maxOptionRunes {
				c.partCounts, c.parts = nil, nil
				break
			}
			c.parts = append(c.parts, p)
		}
		c.partCounts[p]++
		c.partsSeen++
	}
}

// kind is the narrowest kind all the column's cells fit, or the one the
// sheet says they are: a select of dropdowns (nothing picked in them yet
// too), people of chips, a url of links.
func (c *columnGuess) kind() string {
	all := func(n int) bool { return c.filled > 0 && n == c.filled }
	switch {
	case c.filled == 0 && c.choices != nil, all(c.listed):
		return model.FieldSelect
	case all(c.chipped):
		return model.FieldPeople
	case all(c.linked):
		return model.FieldURL
	case c.filled == 0:
		return model.FieldText
	case all(c.checks):
		return model.FieldCheckbox
	case all(c.numbers) && !c.leadingZero:
		return model.FieldNumber
	case all(c.durations):
		return model.FieldDuration
	case all(c.dates[dayFirst]) || all(c.dates[monthFirst]):
		return model.FieldDate
	case all(c.emails):
		return model.FieldEmail
	case all(c.urls):
		return model.FieldURL
	case all(c.phones):
		return model.FieldPhone
	case c.lists > 0 && c.partCounts != nil && c.partsSeen >= 2*len(c.parts) && !c.long:
		return model.FieldMultiSelect
	case !c.tooManyValues && c.filled >= 2*len(c.values) && !c.long:
		return model.FieldSelect
	case c.long:
		return model.FieldLongText
	}
	return model.FieldText
}

// configFor is the config a field of kind made of the column's cells has:
// a number's format, a duration's, a select's options from its dropdown's
// values and then the values it has seen (none past the dropdown's when too
// many: an import adds them as rows come).
func (c *columnGuess) configFor(kind string) map[string]any {
	switch kind {
	case model.FieldNumber:
		return c.numberFormat()
	case model.FieldDuration:
		if c.seconds {
			return map[string]any{"format": "h:mm:ss"}
		}
		return map[string]any{"format": "h:mm"}
	case model.FieldSelect:
		if c.choices != nil {
			labels := slices.Clone(c.choices)
			for _, v := range c.values {
				if !slices.Contains(labels, v) {
					labels = append(labels, v)
				}
			}
			return optionsConfig(labels)
		}
		if !c.tooManyValues && len(c.values) > 0 {
			return optionsConfig(c.values)
		}
	case model.FieldMultiSelect:
		if c.partCounts != nil && len(c.parts) > 0 {
			return optionsConfig(c.parts)
		}
	}
	return nil
}

// dayOrder is the order the column's dates are written in: the one all its
// cells read in, or the person's when both or neither do.
func (c *columnGuess) dayOrder(opts SheetOptions) int {
	days, months := c.dates[dayFirst] == c.filled, c.dates[monthFirst] == c.filled
	switch {
	case days && !months:
		return dayFirst
	case months && !days:
		return monthFirst
	case opts.DayFirst:
		return dayFirst
	}
	return monthFirst
}

// readerFor is how a cell is stored as a field of kind: a workbook's typed
// value when it is of that kind, else its text read as the kind reads text
// (a number with opts' signs, a day in order).
func readerFor(kind string, opts SheetOptions, order int) cellRead {
	switch kind {
	case model.FieldCheckbox:
		return func(cell contentIOBusiness.Cell) (any, bool) {
			if cell.Kind == contentIOBusiness.CellBool {
				return true, cell.Bool // an unticked box holds no value
			}
			ticked, ok := checkboxText(strings.TrimSpace(cell.Text))
			return true, ticked && ok
		}
	case model.FieldNumber:
		return func(cell contentIOBusiness.Cell) (any, bool) {
			if cell.Kind == contentIOBusiness.CellNumber {
				return cell.Number, true
			}
			n, _, _, ok := readNumber(strings.TrimSpace(cell.Text), opts.Signs)
			return n, ok
		}
	case model.FieldDuration:
		return func(cell contentIOBusiness.Cell) (any, bool) {
			if cell.Kind == contentIOBusiness.CellDuration {
				return cell.Number, true
			}
			return fieldTypes.ParseDuration(cell.Text)
		}
	case model.FieldDate:
		return func(cell contentIOBusiness.Cell) (any, bool) {
			if cell.Kind == contentIOBusiness.CellDate {
				return cell.Time.Format("2006-01-02"), true
			}
			day, _, ok := readDate(strings.TrimSpace(cell.Text), order, opts.Zone)
			return day, ok
		}
	case model.FieldMultiSelect:
		return func(cell contentIOBusiness.Cell) (any, bool) {
			parts := listParts(cell.Text)
			out := make([]any, len(parts))
			for i, p := range parts {
				out[i] = p
			}
			return out, len(out) > 0
		}
	case model.FieldPeople:
		return func(cell contentIOBusiness.Cell) (any, bool) {
			out := make([]any, len(cell.People))
			for i, email := range cell.People {
				out[i] = PersonRef{Email: email}
			}
			return out, len(out) > 0
		}
	case model.FieldURL:
		return func(cell contentIOBusiness.Cell) (any, bool) {
			if cell.Link != "" {
				return cell.Link, true
			}
			s := strings.TrimSpace(cell.Text)
			return s, s != ""
		}
	}
	return func(cell contentIOBusiness.Cell) (any, bool) {
		s := strings.TrimSpace(cell.Text)
		return s, s != ""
	}
}

// notes are what f keeps of the column's cells less than whole: the values
// its formulas last worked out, without the formulas; nothing of a formula
// that couldn't be worked out; days without the times they were written
// with; and links without the text they showed.
func (c *columnGuess) notes(f ImportField) []ImportNote {
	var out []ImportNote
	if c.formulas > 0 {
		out = append(out, ImportNote{Field: f.Key, Code: "FORMULA_AS_VALUES", Msg: "Its cells are the values their formulas last worked out; the formulas aren't brought."})
	}
	switch {
	case c.failed == 1:
		out = append(out, ImportNote{Field: f.Key, Code: "CELL_LEFT_OUT", Msg: fmt.Sprintf("A cell held a formula's error (%s), so it's left empty.", c.failure)})
	case c.failed > 1:
		out = append(out, ImportNote{Field: f.Key, Code: "CELL_LEFT_OUT", Msg: fmt.Sprintf("%d cells held a formula's error (such as %s), so they're left empty.", c.failed, c.failure)})
	}
	if c.timed > 0 && f.Type == model.FieldDate {
		out = append(out, ImportNote{Field: f.Key, Code: "TIMES_LEFT_OUT", Msg: "A date field holds the day, so the times of day its cells were written with aren't kept."})
	}
	if c.linkTexts > 0 && f.Type == model.FieldURL {
		out = append(out, ImportNote{Field: f.Key, Code: "LINK_TEXT_LEFT_OUT", Msg: "A url field holds where a link goes, so the text its links showed isn't kept."})
	}
	return out
}

// numberFormat is a number column's format: a percent when every cell is
// one, a currency the grid shows (rupees or dollars) when every cell has it,
// else none.
func (c *columnGuess) numberFormat() map[string]any {
	switch {
	case c.percents == c.filled:
		return map[string]any{"format": map[string]any{"percent": true}}
	case c.currency["USD"] == c.filled:
		return map[string]any{"format": map[string]any{"currency": "USD"}}
	case c.currency["INR"] == c.filled:
		return map[string]any{"format": map[string]any{"currency": "INR"}}
	}
	return nil
}

// optionsConfig is a select's config of labels, in the order first seen.
func optionsConfig(labels []string) map[string]any {
	opts := make([]any, len(labels))
	for i, l := range labels {
		opts[i] = map[string]any{"id": fieldTypes.NewOptionID(), "label": l}
	}
	return map[string]any{"options": opts}
}

// listParts is a cell's comma list, each part trimmed, empty ones left out.
// Pure.
func listParts(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// readNumber reads s as a number written with signs: in full ("1,200.50"),
// with a currency sign ("$12", "12 €"), or as a percent ("12.5%", which is
// 12.5, as a percent column holds it). cur is "USD" for a dollar sign, "INR"
// for a rupee sign, "" for none, "other" for any other. Pure.
func readNumber(s string, signs formula.NumberSigns) (n float64, cur string, pct bool, ok bool) {
	if t, found := strings.CutSuffix(s, "%"); found {
		s, pct = strings.TrimSpace(t), true
	}
	if n, ok = formula.ParseNumber(s, signs); !ok {
		return 0, "", false, false
	}
	switch {
	case strings.Contains(s, "$"):
		cur = "USD"
	case strings.Contains(s, "₹"):
		cur = "INR"
	case strings.IndexFunc(s, isCurrencySign) >= 0:
		cur = "other"
	}
	return n, cur, pct, true
}

func isCurrencySign(r rune) bool {
	return r > 0x7f && strings.ContainsRune("€£¥₩₽₺₫₦₱₪₴₸฿", r)
}

// checkboxText reads a checkbox written as words or marks, as a checkbox
// field takes it (fieldTypes.Truth), but for 1 and 0, which a column of
// counts holds. Pure.
func checkboxText(s string) (ticked, ok bool) {
	if s == "1" || s == "0" {
		return false, false
	}
	return fieldTypes.Truth(s)
}

func isEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && a.Name == ""
}

func isURL(s string) bool {
	if strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	if strings.HasPrefix(strings.ToLower(s), "www.") {
		s = "https://" + s
	}
	u, ok := helpers.WebLink(s)
	return ok && strings.Contains(u.Host, ".")
}

// isPhone is a phone number as a phone field takes it (fieldTypes.IsPhone),
// of 7 to 15 digits with a leading + or a space, bracket, dot or dash
// between them, and no slash, which a date has: a bare string of digits is
// a number, and three digits a code. Pure.
func isPhone(s string) bool {
	if !fieldTypes.IsPhone(s) || strings.Contains(s, "/") {
		return false
	}
	digits := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	return digits >= 7 && digits <= 15 && (s[0] == '+' || strings.ContainsAny(s, " ().-"))
}

var (
	numericDate = regexp.MustCompile(`^(\d{1,4})[/.-](\d{1,2})[/.-](\d{1,4})$`)
	timeOfDay   = regexp.MustCompile(`^(.*?)[ T](\d{1,2}):(\d{2})(?::(\d{2})(?:\.\d{1,9})?)?\s*([AaPp][Mm])?$`)
	// wordDates are dates with the month in words, which read alike in any
	// order.
	wordDates = []string{"2 Jan 2006", "2-Jan-2006", "Jan 2, 2006", "Jan 2 2006", "2 January 2006", "January 2, 2006", "January 2 2006"}
)

// readDate reads s as a day written in order, or with the month in words,
// or in ISO, with or without a time of day: the day it was written on, as a
// date cell stores it ("2026-10-23"), and whether s gave a time, which a
// date field doesn't keep. A moment with its own zone (RFC 3339) is the day
// it falls on in zone. Pure.
func readDate(s string, order int, zone *time.Location) (day string, timed bool, ok bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.In(zone).Format("2006-01-02"), true, true
	}
	m := timeOfDay.FindStringSubmatch(s)
	text := s
	if m != nil {
		text = m[1]
	}
	d, ok := readDay(strings.TrimSpace(text), order)
	if !ok {
		return "", false, false
	}
	if m != nil {
		h, _ := strconv.Atoi(m[2])
		mins, _ := strconv.Atoi(m[3])
		sec, _ := strconv.Atoi(m[4])
		if m[5] != "" && (h < 1 || h > 12) {
			return "", false, false
		}
		if h > 23 || mins > 59 || sec > 59 {
			return "", false, false
		}
	}
	return d.Format("2006-01-02"), m != nil, true
}

// readDay reads a date without a time: ISO, a month in words, or numeric in
// order with a four-digit year. Pure.
func readDay(s string, order int) (time.Time, bool) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true
	}
	for _, layout := range wordDates {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	m := numericDate.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])
	var y, mo, d int
	switch {
	case len(m[1]) == 4:
		y, mo, d = a, b, c
	case len(m[3]) == 4 && order == dayFirst:
		y, mo, d = c, b, a
	case len(m[3]) == 4:
		y, mo, d = c, a, b
	default:
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if t.Year() != y || int(t.Month()) != mo || t.Day() != d {
		return time.Time{}, false // 31/02 isn't a day
	}
	return t, true
}
