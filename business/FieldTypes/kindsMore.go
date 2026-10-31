package business

// The kinds tables B1 adds (D-TB-3): a status, people, a long text, a phone
// number, a duration and a rating, which rows store;
// and an id, when a row was made and last changed and by whom, which each
// read works out from the row (FromRow). Each is one Register line in
// registry.go, with its rules here. A currency and a percent are a number's
// format (its config's "format"), as Notion's are, not kinds of their own.

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	formula "github.com/akashc777/OneCamp/business/Formula"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// ------------------------------------------------------------------ status

// The groups a status's options sit in, which boards and automations read:
// work not started, under way, and finished.
const (
	StatusToDo  = "todo"
	StatusDoing = "doing"
	StatusDone  = "done"
)

// StatusGroup is the group a status option sits in: to do when it names
// none, or one that isn't a group. Pure.
func StatusGroup(o map[string]any) string {
	switch g, _ := o["group"].(string); g {
	case StatusDoing, StatusDone:
		return g
	}
	return StatusToDo
}

// statusConfig gives a status's options their groups, and a status saved
// with no options at all the three a new one starts with. A status whose
// options were all taken out keeps none.
func statusConfig(cfg map[string]any) (map[string]any, error) {
	if _, given := cfg["options"]; !given {
		cfg["options"] = []any{
			map[string]any{"label": "Not started", "group": StatusToDo},
			map[string]any{"label": "In progress", "group": StatusDoing},
			map[string]any{"label": "Done", "group": StatusDone},
		}
		return cfg, nil
	}
	opts := Options(cfg)
	list := make([]any, 0, len(opts))
	for _, o := range opts {
		o["group"] = StatusGroup(o)
		list = append(list, o)
	}
	cfg["options"] = list
	return cfg, nil
}

// ------------------------------------------------------------------ people

// OnePerson is whether a people field takes one person only. Pure.
func OnePerson(f *model.Field) bool {
	cfg := configOf(f)
	one, _ := cfg["one"].(bool)
	return one
}

// coercePeople is a people cell: members of the workspace by their ids, each
// once, stored as a link to people is ([{id, type: "user"}]), which every read
// names. A person's id given bare, or as {id} or {id, type: "user"}, is one.
func coercePeople(f *model.Field, v any, _ Input) (any, error) {
	refs, bad := ParseItems(v, ItemUser)
	if bad != "" {
		if r := []rune(bad); len(r) > maxQuoted {
			bad = string(r[:maxQuoted]) + "…"
		}
		return nil, fmt.Errorf("%q takes people in this workspace, by their ids: %s isn't one", f.Name, bad)
	}
	if len(refs) > 1 && OnePerson(f) {
		return nil, fmt.Errorf("%q takes one person", f.Name)
	}
	return refs, nil
}

// toPeople keeps the people a relation to people links to, and clears
// anything else: free text names nobody until it is matched to members
// (D-TB-4).
func toPeople(v any, _ formula.NumberSigns) (any, bool, int) {
	refs, bad := ParseItems(v, ItemUser)
	if bad != "" || len(refs) == 0 {
		return nil, false, 1
	}
	return refs, true, 0
}

func peopleConfig(cfg map[string]any) (map[string]any, error) {
	if one, given := cfg["one"]; given {
		if _, ok := one.(bool); !ok {
			return nil, fmt.Errorf("say whether the field takes one person, true or false")
		}
	}
	return cfg, nil
}

// ------------------------------------------------------------------ phone

// phoneShape is what a phone number may be written with: digits, spaces, a
// leading +, brackets, dots, dashes and slashes, and an extension ("x", "ext").
var phoneShape = regexp.MustCompile(`^\+?[0-9 ().\-/]*(?:\s*(?:x|ext\.?|extension)\s*[0-9]+)?$`)

// maxPhone is the longest phone number a cell keeps, in characters.
const maxPhone = 40

// IsPhone is whether s is a phone number as people write one: at least three
// digits, and nothing a number isn't written with. Pure.
func IsPhone(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len([]rune(s)) > maxPhone || !phoneShape.MatchString(strings.ToLower(s)) {
		return false
	}
	digits := 0
	for _, r := range s {
		if unicode.IsDigit(r) {
			digits++
		}
	}
	return digits >= 3
}

// PhoneKey is what tells a phone number apart: its digits, after a + (a
// country's code) written before them, bracketed or not, or the 00 that dials
// out as a + does, and its extension's digits after an x, so a number written
// with other spaces, brackets, dots or dashes, or its extension written as
// "ext.", is the same number. Pure.
func PhoneKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	plus, digit, ext := false, false, false
	for _, r := range s {
		switch {
		case r == '+' && !digit:
			plus = true
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			digit = true
		case !ext && r >= 'a' && r <= 'z': // phoneShape lets letters only begin an extension
			ext = true
			b.WriteByte('x')
		}
	}
	key := b.String()
	if !plus && strings.HasPrefix(key, "00") {
		key, plus = key[2:], true
	}
	if plus {
		return "+" + key
	}
	return key
}

func coercePhone(f *model.Field, v any, _ Input) (any, error) {
	switch x := v.(type) {
	case string:
		if IsPhone(x) {
			return strings.TrimSpace(x), nil
		}
	case float64:
		if s := strconv.FormatFloat(x, 'f', -1, 64); x >= 0 && x == math.Trunc(x) && IsPhone(s) {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%q takes a phone number: %s isn't one", f.Name, quoted(v))
}

func toPhone(v any, signs formula.NumberSigns) (any, bool, int) {
	out, _, cleared := toText(v, signs)
	if s, ok := out.(string); ok && cleared == 0 && IsPhone(s) {
		_, kept := v.(string)
		return strings.TrimSpace(s), kept, 0
	}
	return nil, false, 1
}

// ---------------------------------------------------------------- duration

// durationClock is a duration as a clock writes it: h:mm or h:mm:ss.
var durationClock = regexp.MustCompile(`^(\d+):([0-5]?\d)(?::([0-5]?\d(?:\.\d+)?))?$`)

// durationUnits is a duration in units: 1h 30m, 90m, 45s, 1.5h, 2d.
var durationUnits = regexp.MustCompile(`(?i)^(?:(\d+(?:\.\d+)?)\s*d(?:ays?)?)?\s*(?:(\d+(?:\.\d+)?)\s*h(?:ours?|rs?)?)?\s*(?:(\d+(?:\.\d+)?)\s*m(?:in(?:ute)?s?)?)?\s*(?:(\d+(?:\.\d+)?)\s*s(?:ec(?:ond)?s?)?)?$`)

// durationHoursMinutes is hours with their minutes unmarked: 1h30.
var durationHoursMinutes = regexp.MustCompile(`(?i)^(\d+)\s*h(?:ours?|rs?)?\s*(\d+)$`)

// durationMinutes is a number alone, which a person writing a duration means
// as minutes: 90 (STEP5 spec E, as Airtable reads h:mm). A writer's JSON
// number is seconds.
var durationMinutes = regexp.MustCompile(`^\d+(?:\.\d+)?$`)

// ParseDuration reads a duration as people write one, in seconds: a clock
// (1:30 is an hour and a half, 1:30:15 adds seconds), units (1h 30m, 90m,
// 45s, 1.5h, 2d), hours with their minutes (1h30), or a number alone, which
// is minutes (90). ok is false for anything else. Pure.
func ParseDuration(s string) (seconds float64, ok bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false
	}
	if m := durationHoursMinutes.FindStringSubmatch(t); m != nil {
		h, _ := strconv.ParseFloat(m[1], 64)
		mins, _ := strconv.ParseFloat(m[2], 64)
		return h*3600 + mins*60, true
	}
	if durationMinutes.MatchString(t) {
		n, _ := strconv.ParseFloat(t, 64)
		return n * 60, true
	}
	if m := durationClock.FindStringSubmatch(t); m != nil {
		h, _ := strconv.ParseFloat(m[1], 64)
		mins, _ := strconv.ParseFloat(m[2], 64)
		secs := 0.0
		if m[3] != "" {
			secs, _ = strconv.ParseFloat(m[3], 64)
		}
		return h*3600 + mins*60 + secs, true
	}
	if m := durationUnits.FindStringSubmatch(t); m != nil && strings.TrimSpace(strings.Join(m[1:], "")) != "" {
		total := 0.0
		for i, per := range []float64{86400, 3600, 60, 1} {
			if m[i+1] != "" {
				n, _ := strconv.ParseFloat(m[i+1], 64)
				total += n * per
			}
		}
		return total, true
	}
	return 0, false
}

func coerceDuration(f *model.Field, v any, in Input) (any, error) {
	switch x := v.(type) {
	case float64:
		if x >= 0 {
			return x, nil
		}
	case string:
		if n, ok := ParseDuration(x); ok {
			return n, nil
		}
		// Minutes written in the writer's signs: 1,5 in German.
		if n, ok := formula.ParseNumber(x, in.signs()); ok && n >= 0 {
			return n * 60, nil
		}
	}
	return nil, fmt.Errorf("%q takes a duration, such as 90, 1:30 or 1h 30m: %s isn't one", f.Name, quoted(v))
}

func toDuration(v any, signs formula.NumberSigns) (any, bool, int) {
	if n, ok := v.(float64); ok && n >= 0 {
		return n, true, 0
	}
	if s, ok := v.(string); ok {
		if n, ok := ParseDuration(s); ok {
			return n, false, 0
		}
	}
	return toNumber(v, signs)
}

func durationConfig(cfg map[string]any) (map[string]any, error) {
	if f, given := cfg["format"]; given {
		if s, _ := f.(string); s != "h:mm" && s != "h:mm:ss" {
			return nil, fmt.Errorf("show a duration as h:mm or h:mm:ss")
		}
	}
	return cfg, nil
}

// ------------------------------------------------------------------ rating

// RatingMax is the most a rating field rates: its config's, 1 to 10, or 5.
// Pure.
func RatingMax(f *model.Field) int {
	if m, ok := configOf(f)["max"].(float64); ok && m >= 1 && m <= 10 && m == math.Trunc(m) {
		return int(m)
	}
	return 5
}

func coerceRating(f *model.Field, v any, in Input) (any, error) {
	max := RatingMax(f)
	n, err := coerceNumber(f, v, in)
	if err == nil {
		r := n.(float64)
		if r == math.Trunc(r) && r >= 1 && r <= float64(max) {
			return r, nil
		}
	}
	return nil, fmt.Errorf("%q is rated from 1 to %d: %s isn't a rating", f.Name, max, quoted(v))
}

func toRating(v any, signs formula.NumberSigns) (any, bool, int) {
	out, _, cleared := toNumber(v, signs)
	if n, ok := out.(float64); ok && cleared == 0 {
		r := math.Round(n)
		if r >= 1 && r <= 10 {
			return r, r == n, 0
		}
	}
	return nil, false, 1
}

func ratingConfig(cfg map[string]any) (map[string]any, error) {
	if m, given := cfg["max"]; given {
		n, ok := m.(float64)
		if !ok || n < 1 || n > 10 || n != math.Trunc(n) {
			return nil, fmt.Errorf("a rating rates up to a whole number from 1 to 10")
		}
	}
	return cfg, nil
}

// ------------------------------------------------- worked out from the row

// idPrefix is what an id's prefix may be: letters, digits, _ and -, up to 12.
var idPrefix = regexp.MustCompile(`^[A-Za-z0-9_-]{0,12}$`)

// IDPrefix is an autonumber field's prefix ("" for none). Pure.
func IDPrefix(f *model.Field) string {
	p, _ := configOf(f)["prefix"].(string)
	if !idPrefix.MatchString(p) {
		return ""
	}
	return p
}

func autonumberConfig(cfg map[string]any) (map[string]any, error) {
	if p, given := cfg["prefix"]; given {
		s, _ := p.(string)
		s = strings.TrimSpace(s)
		if !idPrefix.MatchString(s) {
			return nil, fmt.Errorf("an id's prefix is up to 12 letters, digits, _ or -")
		}
		cfg["prefix"] = s
	}
	return cfg, nil
}

// rowNumber is a row's number in its table, which an id shows after its
// prefix; nothing for a row with none yet.
func rowNumber(_ *model.Field, r *model.Row) any {
	if r == nil || r.RowNo <= 0 {
		return nil
	}
	return float64(r.RowNo)
}

// stamp is a time as a row's stamps show it: RFC 3339, in UTC.
func stamp(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func rowCreatedTime(_ *model.Field, r *model.Row) any {
	if r == nil {
		return nil
	}
	return stamp(r.CreatedAt)
}

func rowModifiedTime(_ *model.Field, r *model.Row) any {
	if r == nil {
		return nil
	}
	return stamp(r.UpdatedAt)
}

// personRef is a person as a link to people, which a read names.
func personRef(id *string) any {
	if id == nil || *id == "" {
		return nil
	}
	return []ItemRef{{ID: *id, Type: ItemUser}}
}

func rowCreatedBy(_ *model.Field, r *model.Row) any {
	if r == nil || r.CreatedBy == nil {
		return nil
	}
	id := r.CreatedBy.String()
	return personRef(&id)
}

// rowModifiedBy is who last changed the row: whoever made it, until someone
// changes it.
func rowModifiedBy(f *model.Field, r *model.Row) any {
	if r == nil {
		return nil
	}
	if r.UpdatedBy == nil {
		return rowCreatedBy(f, r)
	}
	id := r.UpdatedBy.String()
	return personRef(&id)
}

// --------------------------------------------------------------- helpers

// configOf is a field's config as a map ({} for none or broken). Pure.
func configOf(f *model.Field) map[string]any {
	cfg := map[string]any{}
	if f != nil && f.Config != "" {
		_ = json.Unmarshal([]byte(f.Config), &cfg)
	}
	return cfg
}
