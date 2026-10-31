package csv

import (
	"math"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	taskField "github.com/akashc777/OneCamp/business/TaskField"
	"github.com/akashc777/OneCamp/helpers"
)

// property is a task's own property a column fills.
type property string

const (
	propTitle       property = "title"
	propKey         property = "key"
	propID          property = "id" // another id rows' parents may name (Asana's Task ID, Jira's Issue id)
	propParent      property = "parent"
	propStatus      property = "status"
	propCompleted   property = "completed"
	propPriority    property = "priority"
	propAssignee    property = "assignee"
	propAssignees   property = "assignees" // several, comma-separated (ClickUp)
	propStart       property = "start"
	propDue         property = "due"
	propEstimate    property = "estimate" // hours
	propTags        property = "tags"
	propDescription property = "description"
	propCreated     property = "created"
	propCreatedBy   property = "created_by"
	propUpdated     property = "updated"
	propType        property = "type"
	propLink        property = "link"
	// propEmail is the PlanColumn property of a column of addresses beside a
	// column of people ("Assignee Email").
	propEmail property = "email"
)

// aliases are the headers each property is read from: OneCamp's export's
// first, then the rivals' (Asana, Jira, Linear, ClickUp, Notion, Trello).
var aliases = func() map[string]property {
	out := map[string]property{}
	for p, names := range map[property][]string{
		propTitle:       {"title", "name", "task", "task name", "summary", "issue", "card name", "item", "item name"},
		propKey:         {"key", "issue key", "task key", "identifier"},
		propID:          {"id", "task id", "issue id", "item id", "card id"},
		propParent:      {"parent", "parent task", "parent id", "parent issue", "parent key", "parent item"},
		propStatus:      {"status", "state", "section/column", "section", "column", "list", "stage"},
		propCompleted:   {"completed at", "completed on", "completed", "date completed", "done at", "resolved"},
		propPriority:    {"priority"},
		propAssignee:    {"assignee", "assigned to", "owner"},
		propAssignees:   {"assignees"},
		propStart:       {"start date", "start", "start on", "starts on"},
		propDue:         {"due date", "due", "due on", "deadline", "due by"},
		propEstimate:    {"estimate (hours)", "estimated hours", "estimate hours"},
		propTags:        {"tags", "labels", "label", "tag"},
		propDescription: {"description", "notes", "task content", "details"},
		propCreated:     {"created", "created at", "created on", "date created", "creation date"},
		propCreatedBy:   {"created by", "creator", "reporter"},
		propUpdated:     {"updated", "updated at", "last modified", "modified", "date updated"},
		propType:        {"type", "issue type", "task type", "work type"},
		propLink:        {"link"},
	} {
		for _, n := range names {
			out[n] = p
		}
	}
	return out
}()

// fieldName and currency are what a field column's header says: our export
// names a money field "Budget (USD)", and a field named like one of the
// list's own columns "Status (field)".
func fieldName(header string) (name, currency string) {
	open := strings.LastIndexByte(header, '(')
	if open <= 0 || !strings.HasSuffix(header, ")") {
		return header, ""
	}
	inner, base := header[open+1:len(header)-1], strings.TrimSpace(header[:open])
	if strings.EqualFold(inner, "field") {
		return base, ""
	}
	if code := importProvider.CurrencyOf(inner); code != "" && len(inner) == 3 {
		return base, code
	}
	return header, ""
}

// guessKind is the field kind a column's values suggest: a money header's
// money; numbers; days; Yes (and No) for a checkbox; links; a few values that
// repeat for a select, or for a multi-select when they're lists ("a, b");
// else text. Pure.
func guessKind(values []string, currency string) string {
	switch {
	case currency != "":
		return importProvider.FieldMoney
	case len(values) == 0:
		return importProvider.FieldText
	case every(values, isNumber):
		return importProvider.FieldNumber
	case every(values, isDay):
		return importProvider.FieldDate
	case checkboxes(values):
		return importProvider.FieldCheckbox
	case every(values, isLink):
		return importProvider.FieldURL
	}
	if parts := listParts(values); parts != nil && repeats(parts) {
		return importProvider.FieldMultiSelect
	}
	if repeats(values) {
		return importProvider.FieldSelect
	}
	return importProvider.FieldText
}

// kinds are the kinds the admin can give a field column.
var kinds = map[string]bool{
	importProvider.FieldText: true, importProvider.FieldNumber: true, importProvider.FieldMoney: true,
	importProvider.FieldDate: true, importProvider.FieldSelect: true, importProvider.FieldMultiSelect: true,
	importProvider.FieldPerson: true, importProvider.FieldCheckbox: true, importProvider.FieldURL: true,
	importProvider.KindSkip: true,
}

func every(values []string, ok func(string) bool) bool {
	for _, v := range values {
		if !ok(v) {
			return false
		}
	}
	return true
}

func isNumber(s string) bool {
	_, ok := number(s)
	return ok
}

func isDay(s string) bool {
	_, ok := importProvider.ParseDay(s)
	return ok
}

func isLink(s string) bool {
	return !strings.ContainsAny(s, " \t\n") && (strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://"))
}

// checkboxes is a column of Yes (or true) and No (or false), with a Yes.
func checkboxes(values []string) bool {
	yes := false
	for _, v := range values {
		b, ok := checkbox(v)
		if !ok {
			return false
		}
		yes = yes || b
	}
	return yes
}

func checkbox(s string) (on, ok bool) {
	switch strings.ToLower(s) {
	case "yes", "true":
		return true, true
	case "no", "false":
		return false, true
	}
	return false, false
}

// listParts is each value split at its commas, when some value is a list and
// every part could be an option's label; nil when they aren't lists.
func listParts(values []string) []string {
	var parts []string
	list := false
	for _, v := range values {
		p := splitList(v)
		list = list || len(p) > 1
		parts = append(parts, p...)
	}
	if !list {
		return nil
	}
	return parts
}

// repeats is a column of option labels: short, few, and some said more than
// once.
func repeats(values []string) bool {
	distinct := map[string]bool{}
	for _, v := range values {
		if utf8.RuneCountInString(v) > taskField.MaxOptionLabel {
			return false
		}
		distinct[strings.ToLower(v)] = true
	}
	return len(distinct) <= taskField.MaxOptions && len(distinct) < len(values)
}

// splitList is a cell's list: "a, b", or ClickUp's "[a, b]". Pure.
func splitList(s string) []string {
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "["), "]")
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// number reads a number as spreadsheets write one: "1250.5", "-3", "1,250.50".
func number(s string) (float64, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	return n, true
}

// money reads an amount, its currency's sign or code beside it or not:
// "1250.50", "$1,250.50", "1,250.50 EUR".
func money(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	for _, cut := range []func(string) (string, bool){leadingCurrency, trailingCurrency} {
		if rest, ok := cut(s); ok {
			s = rest
		}
	}
	return number(s)
}

func leadingCurrency(s string) (string, bool) {
	i := strings.IndexFunc(s, func(r rune) bool { return r == '-' || (r >= '0' && r <= '9') })
	if i <= 0 || importProvider.CurrencyOf(s[:i]) == "" {
		return s, false
	}
	return s[i:], true
}

func trailingCurrency(s string) (string, bool) {
	i := strings.LastIndexAny(s, "0123456789")
	if i < 0 || i == len(s)-1 || importProvider.CurrencyOf(s[i+1:]) == "" {
		return s, false
	}
	return s[:i+1], true
}

// dayLayouts are the days the rivals' files write, past ParseDay's own.
var dayLayouts = []string{"2006/01/02", "Jan 2, 2006", "January 2, 2006", "2 Jan 2006", "2 January 2006", "Monday, January 2, 2006"}

// timeLayouts are the times they write; one with no zone is in the import's.
var timeLayouts = []string{"2006-01-02 15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05", "02/Jan/06 3:04 PM", "January 2, 2006 3:04 PM", "Jan 2, 2006 3:04 PM"}

// when reads a date: a day with no time (ParseDay and the layouts above), or
// a time, in loc when it says no zone. ok is false for anything else, which
// is never guessed at: 03/04/2026 is March in one country and April in the
// next. Pure.
func when(s string, loc *time.Location) (day importProvider.Day, at *time.Time, ok bool) {
	s = strings.TrimSpace(s)
	if d, ok := importProvider.ParseDay(s); ok {
		return d, nil, true
	}
	for _, l := range dayLayouts {
		if t, err := time.Parse(l, s); err == nil {
			y, m, d := t.Date()
			return importProvider.Day{Year: y, Month: m, Day: d}, nil, true
		}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return importProvider.Day{}, &t, true
	}
	if loc == nil {
		loc = time.UTC
	}
	for _, l := range timeLayouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return importProvider.Day{}, &t, true
		}
	}
	return importProvider.Day{}, nil, false
}

// instant is a date as a time: a day is its midnight in loc (noon UTC with
// none, as importProvider.Day.At says).
func instant(s string, loc *time.Location) (time.Time, bool) {
	d, at, ok := when(s, loc)
	switch {
	case !ok:
		return time.Time{}, false
	case at != nil:
		return *at, true
	}
	return d.At(loc), true
}

// address is s as an email address, when it is one.
func address(s string) (string, bool) {
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || !helpers.AddressIsASCII(s) {
		return "", false
	}
	return helpers.NormalizeEmail(s), true
}

// value is a cell as a field of kind f.Type holds it (SourceFieldValue);
// person gives a name's or address's source id. A cell the kind can't read
// stays its text, which the import keeps in the task's description.
func value(f importProvider.SourceField, cell string, person func(name, email string) string) importProvider.SourceFieldValue {
	v := importProvider.SourceFieldValue{Field: f, Value: cell, Text: cell}
	switch f.Type {
	case importProvider.FieldNumber:
		if n, ok := number(cell); ok {
			v.Value = n
		}
	case importProvider.FieldMoney:
		if n, ok := money(cell); ok {
			v.Value = n
		}
	case importProvider.FieldDate:
		if d, at, ok := when(cell, nil); ok && at == nil {
			v.Value = d.String()
		}
	case importProvider.FieldCheckbox:
		if on, ok := checkbox(cell); ok {
			v.Value = on
		}
	case importProvider.FieldMultiSelect:
		v.Value = splitList(cell)
	case importProvider.FieldPerson:
		if email, ok := address(cell); ok {
			v.Value = person("", email)
		} else {
			v.Value = person(cell, "")
		}
	}
	return v
}
