package business

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// The steps a transform runs over a view's rows (G2, gsheets-grid-01 items 3
// and 5): find and replace across a view, and the clean-up steps, trim
// spaces, remove duplicate rows and split a text column. Each is pure over
// one row's cells; the walk over the view and the writes, one change set a
// chunk joined by one operation so one Undo takes a step back, are the
// transform's (design: logs/tables-109/g2/DESIGN.md).

// The steps, as a request names them.
const (
	StepFind    = "find"
	StepReplace = "replace"
	StepTrim    = "trim"
	StepDedupe  = "dedupe"
	StepSplit   = "split"
)

// maxFindText is the longest text, or pattern, a find takes, in bytes.
const maxFindText = 500

// finder is a compiled find. Plain text is a quoted pattern, so one matcher
// serves both, and RE2 never backtracks, whatever the pattern.
type finder struct {
	re      *regexp.Regexp
	literal bool
}

// compileFind compiles what a find looks for: in any case unless matchCase,
// anywhere in a cell unless wholeCell, as a regular expression only when
// isRegex. Spaces alone are a find (two spaces, to replace with one). A find
// of nothing, a pattern that isn't one, or one that matches an empty cell is
// refused in words.
func compileFind(text string, matchCase, wholeCell, isRegex bool) (*finder, error) {
	if text == "" {
		return nil, errors.New("type what to find")
	}
	if len(text) > maxFindText {
		return nil, fmt.Errorf("a find takes at most %d characters", maxFindText)
	}
	pattern := text
	if !isRegex {
		pattern = regexp.QuoteMeta(text)
	}
	if wholeCell {
		pattern = `^(?:` + pattern + `)$`
	}
	if !matchCase {
		pattern = `(?i)` + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		why := err.Error()
		var se *syntax.Error
		if errors.As(err, &se) {
			why = se.Code.String()
		}
		return nil, fmt.Errorf("%q isn't a regular expression this can read: %s", text, why)
	}
	if re.MatchString("") {
		return nil, fmt.Errorf("%q matches an empty cell: make it match at least one character", text)
	}
	return &finder{re: re, literal: !isRegex}, nil
}

// matches is whether s holds what f looks for.
func (f *finder) matches(s string) bool { return f.re.MatchString(s) }

// replace is s with every match of f replaced by with: as written for plain
// text, a pattern's $1 and ${name} filled in for a pattern.
func (f *finder) replace(s, with string) string {
	if f.literal {
		return f.re.ReplaceAllLiteralString(s, with)
	}
	return f.re.ReplaceAllString(s, with)
}

// findText is a cell as a person reads it, for a find: text as it is,
// numbers written plainly (an id with its prefix), lists and links by their
// names. A tick, a formula's error, and a link the reader can't open never
// match.
func findText(f *model.Field, v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, x != ""
	case float64:
		if s, ok := autonumberText(f, x); ok {
			return s, true
		}
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case []any:
		names, _ := listNames(x)
		return strings.Join(names, ", "), len(names) > 0
	case map[string]any:
		if _, failed := x["error"]; failed {
			return "", false
		}
		names, _ := listNames([]any{x})
		return strings.Join(names, ", "), len(names) > 0
	}
	return "", false
}

// trimText is s with its spaces tidied, as a spreadsheet's trim: none at
// either end, and a run of them inside (tabs, no-break and other Unicode
// spaces) one space. A long text (multiline) keeps its lines, each tidied,
// without blank lines at either end; a one-line field's line breaks become
// spaces.
func trimText(s string, multiline bool) string {
	if !multiline {
		return strings.Join(strings.Fields(s), " ")
	}
	lines := strings.Split(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s), "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// emptyCell is a cell with nothing in it: none, "", or an empty list.
func emptyCell(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	}
	return false
}

// dedupeKey is what a row's compared cells hold, as one key: each cell as
// stored (canonical JSON), an empty one as none, in the fields' order. Rows
// with the same key are duplicates.
func dedupeKey(values map[string]any, fields []*model.Field) [32]byte {
	h := sha256.New()
	for _, f := range fields {
		v := values[f.Id.String()]
		if emptyCell(v) {
			v = nil
		}
		b, _ := json.Marshal(v)
		h.Write(b)
		h.Write([]byte{0x1f}) // never in JSON's output, so values can't run into each other
	}
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

// splitText is s cut at sep, at most n parts (n < 0: all), the last holding
// the rest. A separator of spaces takes a run of spaces as one.
func splitText(s, sep string, n int) []string {
	if sep != "" && strings.TrimSpace(sep) == "" && !strings.ContainsAny(sep, "\r\n") {
		s, sep = strings.Join(strings.Fields(s), " "), " "
	}
	return strings.SplitN(s, sep, n)
}

// splitInto is s split at sep into n targets' values: each part trimmed, the
// last target taking the rest, and a part that's empty, or a target past the
// parts, nil (cleared).
func splitInto(s, sep string, n int) []any {
	parts := splitText(s, sep, n)
	out := make([]any, n)
	for i := range out {
		if i < len(parts) {
			if p := strings.TrimSpace(parts[i]); p != "" {
				out[i] = p
			}
		}
	}
	return out
}

// countParts is how many parts s splits into at sep: a dry run proposes as
// many targets.
func countParts(s, sep string) int {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	return len(splitText(s, sep, -1))
}

// transform is one step made ready to run over rows: the fields it covers (a
// split's source alone), what a find or a replace looks for, a replace's
// text, and a split's separator and targets.
type transform struct {
	step   string
	fields []*model.Field
	find   *finder
	with   string
	sep    string
	into   []*model.Field
}

// cells is what t does to one row's cells: each cell it changes, with its
// new value (nil clears one), and the fields where it found its text (find
// and replace) or changed a cell (trim). A dedupe changes no cell here
// (dedupeKey).
func (t *transform) cells(values map[string]any) (map[string]any, []*model.Field) {
	var patch map[string]any
	set := func(f *model.Field, v any) {
		if patch == nil {
			patch = map[string]any{}
		}
		patch[f.Id.String()] = v
	}
	var found []*model.Field
	switch t.step {
	case StepFind:
		for _, f := range t.fields {
			if s, ok := findText(f, values[f.Id.String()]); ok && t.find.matches(s) {
				found = append(found, f)
			}
		}
	case StepReplace:
		for _, f := range t.fields {
			s, ok := values[f.Id.String()].(string)
			if !ok || !t.find.matches(s) {
				continue
			}
			found = append(found, f)
			if n := t.find.replace(s, t.with); n != s {
				set(f, textOrNone(n))
			}
		}
	case StepTrim:
		for _, f := range t.fields {
			s, ok := values[f.Id.String()].(string)
			if !ok {
				continue
			}
			if n := trimText(s, f.Type == model.FieldLongText); n != s {
				found = append(found, f)
				set(f, textOrNone(n))
			}
		}
	case StepSplit:
		s, _ := values[t.fields[0].Id.String()].(string)
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		for i, v := range splitInto(s, t.sep, len(t.into)) {
			f := t.into[i]
			if !holds(values[f.Id.String()], v) {
				set(f, v)
			}
		}
	}
	return patch, found
}

// textOrNone is s as a cell: "" clears it.
func textOrNone(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// holds is whether a cell holds v already (v a text or nil): an empty cell
// holds nil.
func holds(cell, v any) bool {
	if emptyCell(cell) {
		return v == nil
	}
	s, ok := cell.(string)
	w, isText := v.(string)
	return ok && isText && s == w
}
