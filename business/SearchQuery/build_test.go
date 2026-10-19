package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
)

// The people, channels and readers the tests' lookups know. Sam's own
// channels are #launch and #design; #secret exists but Sam isn't in it.
const (
	samID     = "11111111-1111-4111-8111-111111111111"
	anaID     = "22222222-2222-4222-8222-222222222222"
	launchID  = "33333333-3333-4333-8333-333333333333"
	designID  = "44444444-4444-4444-8444-444444444444"
	secretID  = "55555555-5555-4555-8555-555555555555"
	visitorID = "66666666-6666-4666-8666-666666666666"
)

var sam = Asker{Subject: accessBusiness.Subject{UserUID: "0x1", UserUUID: samID}, Email: "sam@example.com"}

// fakeWorld stands in for the databases and the Access Resolver, and gives
// the tests a registry of their own: the core fields and the fields an area
// would register (projects' here, as the contract lists them).
func fakeWorld(t *testing.T, extra ...Field) {
	t.Helper()
	saved := Fields
	savedHandle, savedEmail, savedChannel, savedReadable, savedHandleOf, savedNames := userIDByHandle, userEmail, channelIDByName, readable, handleOf, channelNames
	t.Cleanup(func() {
		Fields = saved
		userIDByHandle, userEmail, channelIDByName, readable, handleOf, channelNames = savedHandle, savedEmail, savedChannel, savedReadable, savedHandleOf, savedNames
	})
	Fields = &registry{byName: map[string]*Field{}, scopes: map[string]ScopeFields{}}
	registerCore(Fields)
	Fields.Register(Field{Name: "status", Family: FamilyOption, Sortable: true, Choices: []Choice{{Value: "open"}, {Value: "done", Synonyms: []string{"closed"}}, {Value: "review", Label: "In review"}}})
	Fields.Register(Field{Name: "status_category", Family: FamilyOption, Group: true, Choices: []Choice{{Value: "started"}, {Value: "done"}}})
	Fields.Register(Field{Name: "priority", Family: FamilyOption, Ordered: true, Sortable: true, Choices: []Choice{{Value: "low"}, {Value: "medium"}, {Value: "high"}}})
	Fields.Register(Field{Name: "labels", Aliases: []string{"label"}, Family: FamilyList, Many: true})
	Fields.Register(Field{Name: "due", Family: FamilyDate, Ahead: true, Sortable: true})
	Fields.Register(Field{Name: "estimate", Family: FamilyNumber, Sortable: true})
	Fields.Register(Field{Name: "assignee", Family: FamilyPeople, Resolve: resolvePerson, Label: labelPerson})
	Fields.Register(Field{Name: "key", Family: FamilyText})
	Fields.AddChoices("is", Choice{Value: "blocked"}, Choice{Value: "thread"})
	for _, f := range extra {
		Fields.Register(f)
	}
	handles := map[string]string{"sam": samID, "ana": anaID, "visitor": visitorID}
	userIDByHandle = func(_ context.Context, h string) (string, error) { return handles[strings.ToLower(h)], nil }
	userEmail = func(_ context.Context, id string) (string, bool, bool, error) {
		switch id {
		case samID:
			return "sam@example.com", false, true, nil
		case anaID:
			return "ana@cast.demo.onemana.dev", false, true, nil
		}
		return "", false, false, nil
	}
	channels := map[string]string{"launch": launchID, "design": designID, "secret": secretID}
	channelIDByName = func(_ context.Context, name string) (string, error) { return channels[name], nil }
	mine := map[string]bool{launchID: true, designID: true}
	readable = func(_ context.Context, who accessBusiness.Subject, res []accessBusiness.Resource) ([]accessBusiness.Resource, error) {
		var out []accessBusiness.Resource
		for _, r := range res {
			if who.UserUUID == samID && mine[r.UUID] {
				out = append(out, r)
			}
		}
		return out, nil
	}
	handleOf = func(_ context.Context, id string) (string, error) {
		for h, hid := range handles {
			if hid == id {
				return h, nil
			}
		}
		return "", nil
	}
	channelNames = func(_ context.Context, ids []string) (map[string]string, error) {
		out := map[string]string{}
		for _, id := range ids {
			for name, cid := range channels {
				if cid == id {
					out[id] = name
				}
			}
		}
		return out, nil
	}
}

func search(t *testing.T, text string) (querySpec.Spec, []Issue) {
	t.Helper()
	return Build(context.Background(), sam, text, querySpec.Scope{Kind: querySpec.ScopeSearch}, ModeSearch)
}

func list(t *testing.T, text string) (querySpec.Spec, []Issue) {
	t.Helper()
	return Build(context.Background(), sam, text, querySpec.Scope{Kind: querySpec.ScopeMine}, ModeList)
}

// shapeOf is a filter as a short line, without spans and labels, for
// comparing: and[from in(@me) …].
func shapeOf(f *querySpec.Filter) string {
	if f == nil {
		return "∅"
	}
	if f.Op != "" {
		parts := make([]string, len(f.Filters))
		for i := range f.Filters {
			parts[i] = shapeOf(&f.Filters[i])
		}
		return f.Op + "[" + strings.Join(parts, " ") + "]"
	}
	vals := make([]string, len(f.Values))
	for i, v := range f.Values {
		switch {
		case v.ID != "":
			vals[i] = v.Type + ":" + v.ID[:4]
		case v.Number != nil:
			raw, _ := json.Marshal(*v.Number)
			vals[i] = string(raw)
		case v.Relative != "":
			vals[i] = v.Relative
			if v.N != 0 {
				raw, _ := json.Marshal(v.N)
				vals[i] += "/" + string(raw)
			}
		default:
			vals[i] = v.Text
		}
	}
	return f.Field + " " + f.Operator + "(" + strings.Join(vals, ",") + ")"
}

func codes(issues []Issue) string {
	var out []string
	for _, is := range issues {
		out = append(out, is.Code+"/"+is.Level)
	}
	return strings.Join(out, " ")
}

// Words and phrases are text conditions in the tree, so they take part in
// OR, NOT and brackets; a dash or NOT folds into not_contains.
func TestWordsAreConditionsInTheTree(t *testing.T) {
	fakeWorld(t)
	for text, want := range map[string]string{
		"launch":                     "text contains(launch)",
		"launch plan":                "and[text contains(launch) text contains(plan)]",
		`"pricing page" -draft`:      "and[text contains(pricing page) text not_contains(draft)]",
		"plan OR roadmap":            "or[text contains(plan) text contains(roadmap)]",
		"NOT (plan OR roadmap) week": "and[not[or[text contains(plan) text contains(roadmap)]] text contains(week)]",
		"--draft":                    "text contains(draft)",
		"":                           "∅",
	} {
		spec, issues := search(t, text)
		if got := shapeOf(spec.Filter); got != want || len(issues) > 0 {
			t.Errorf("%q: %s %s, want %s", text, got, codes(issues), want)
		}
	}
}

// Slack's modifiers, as chat writes them (contract §3c): every name resolved
// as the asker, dates by the day.
func TestSlackModifiersResolveAsTheAsker(t *testing.T) {
	fakeWorld(t)
	spec, issues := search(t, `from:@sam in:#launch before:friday "pricing page" -draft`)
	want := "and[from in(user:1111) in is(channel:3333) created before(weekday/5) text contains(pricing page) text not_contains(draft)]"
	if got := shapeOf(spec.Filter); got != want || len(issues) > 0 {
		t.Fatalf("%s %s\nwant %s", got, codes(issues), want)
	}
	for text, want := range map[string]string{
		"from:me":                "from in(@me)",
		"from:@ana to:me":        "and[from in(user:2222) text contains(to:me)]",
		"in:@ana":                "in is(dm_with:2222)",
		"after:2026-10-01":       "created after(2026-10-01)",
		"on:yesterday":           "created is(yesterday)",
		"during:2026-10":         "created within(2026-10)",
		"creator:@sam":           "from in(user:1111)",
		"type:issue":             "type is(task)",
		"-type:doc":              "type is_not(doc)",
		"title:onboarding":       "title contains(onboarding)",
		"is:thread":              "is is(thread)",
		"updated:>=past_30_days": "updated on_or_after(past_n_days/30)",
	} {
		spec, _ := search(t, text)
		if got := shapeOf(spec.Filter); got != want {
			t.Errorf("%q: %s, want %s", text, got, want)
		}
	}
}

// A name that finds nothing the asker may open reads the same whether it
// exists or not: the same words, the same note, nothing more (G-PRIV).
func TestANameNobodyMayOpenGivesNothingAway(t *testing.T) {
	fakeWorld(t)
	hidden, hiddenIssues := search(t, "in:#secret launch")
	missing, missingIssues := search(t, "in:#nowhere launch")
	if shapeOf(hidden.Filter) != "and[text contains(in:#secret) text contains(launch)]" {
		t.Errorf("a channel Sam can't read: %s", shapeOf(hidden.Filter))
	}
	if shapeOf(missing.Filter) != "and[text contains(in:#nowhere) text contains(launch)]" {
		t.Errorf("a channel that isn't there: %s", shapeOf(missing.Filter))
	}
	if len(hiddenIssues) != 1 || len(missingIssues) != 1 {
		t.Fatalf("issues: %s / %s", codes(hiddenIssues), codes(missingIssues))
	}
	h, m := hiddenIssues[0], missingIssues[0]
	if h.Code != IssueUnknownValue || h.Level != LevelNote || h.Code != m.Code || h.Level != m.Level ||
		strings.ReplaceAll(h.Message, "#secret", "#X") != strings.ReplaceAll(m.Message, "#nowhere", "#X") {
		t.Errorf("hidden and missing differ:\n  %+v\n  %+v", h, m)
	}
	if !strings.Contains(h.Message, "searched as words") || !Runs(hiddenIssues) {
		t.Errorf("a search still runs, as words: %+v", h)
	}
	// In a list it is an error, the same for both.
	_, li := list(t, "in:#secret")
	_, lm := list(t, "in:#nowhere")
	if Runs(li) || Runs(lm) || li[0].Code != lm[0].Code {
		t.Errorf("a list refuses both alike: %s / %s", codes(li), codes(lm))
	}
}

// A field nobody registered is its words in a search and an error in a list,
// with the nearest names.
func TestAnUnknownFieldIsWordsOrAnError(t *testing.T) {
	fakeWorld(t)
	spec, issues := search(t, "stauts:open https://example.com/a")
	if got := shapeOf(spec.Filter); got != "and[text contains(stauts:open) text contains(https://example.com/a)]" {
		t.Errorf("as words: %s", got)
	}
	if codes(issues) != "unknown_field/note unknown_field/note" || !Runs(issues) {
		t.Errorf("notes: %s", codes(issues))
	}
	_, issues = list(t, "stauts = open")
	if codes(issues) != "unknown_field/error" || Runs(issues) {
		t.Fatalf("a list: %s", codes(issues))
	}
	if len(issues[0].Suggestions) == 0 || issues[0].Suggestions[0] != "status" {
		t.Errorf("suggestions: %v", issues[0].Suggestions)
	}
	if issues[0].Span != (querySpec.Span{0, 13}) {
		t.Errorf("span: %v", issues[0].Span)
	}
	// "this is empty" names no field: three words, not a clause.
	spec, _ = search(t, "this is empty")
	if got := shapeOf(spec.Filter); got != "and[text contains(this) text contains(is) text contains(empty)]" {
		t.Errorf("words that look like a clause: %s", got)
	}
}

// Repeated modifiers mean any of them on a one-valued field and all of them
// on a many-valued one (contract §4.3); compare form keeps strict logic.
func TestRepeatedModifiersJoinAsSlackDoes(t *testing.T) {
	fakeWorld(t)
	for text, want := range map[string]string{
		"in:#launch in:#design":           "in is(channel:3333,channel:4444)",
		"from:@sam from:@ana plan":        "and[from in(user:1111,user:2222) text contains(plan)]",
		"status:open status:review":       "status in(open,review)",
		"labels:bug labels:urgent":        "labels all_of(bug,urgent)",
		"-in:#launch -in:#design":         "in is_not(channel:3333,channel:4444)",
		"-from:@sam -from:@ana":           "from not_in(user:1111,user:2222)",
		"status = open AND status = done": "and[status is(open) status is(done)]",
	} {
		spec, issues := search(t, text)
		if got := shapeOf(spec.Filter); got != want || len(issues) > 0 {
			t.Errorf("%q: %s %s, want %s", text, got, codes(issues), want)
		}
	}
}

// Symbols become the one list's operator names by the field's family
// (contract §4.5), and a negation folds into its opposite where there is one.
func TestSymbolsBecomeOperatorNamesByFamily(t *testing.T) {
	fakeWorld(t)
	cases := map[string]string{
		"due < friday":                "due before(weekday/5)",
		"due:<=+7d":                   "due on_or_before(n_days_from_now/7)",
		"due > -2w":                   "due after(n_days_ago/14)",
		"due = this_week":             "due is(this_week)",
		"due = next_7_days":           "due within(next_n_days/7)",
		"due != past_week":            "not[due within(past_week)]",
		"due:2026-10-01..today":       "due between(2026-10-01,today)",
		"due is empty":                "due empty()",
		"due IS NOT EMPTY":            "due not_empty()",
		"estimate >= 3":               "estimate gte(3)",
		"estimate:1..5":               "estimate between(1,5)",
		"estimate in (1, 2)":          "or[estimate is(1) estimate is(2)]",
		"priority > low":              "priority gt(low)",
		"status != done":              "status is_not(done)",
		"status in (open, closed)":    "status in(open,done)",
		"status not in (open)":        "status not_in(open)",
		"status_category = done":      "status_category status_group_is(done)",
		"status_category != done":     "not[status_category status_group_is(done)]",
		"labels in (bug, urgent)":     "labels in(bug,urgent)",
		"labels = (bug, urgent)":      "labels is_exactly(bug,urgent)",
		"labels != bug":               "labels not_in(bug)",
		"assignee in (@me, @my_team)": "assignee in(@me,@my_team)",
		"-assignee = @sam":            "assignee not_in(user:1111)",
		"key = Q4-*":                  "key starts_with(Q4-)",
		"key = *-draft":               "key ends_with(-draft)",
		"key:*plan*":                  "key contains(plan)",
		"key = Q4-12":                 "key is(Q4-12)",
		"key ~ launch":                "key contains(launch)",
		"key !~ draft":                "key not_contains(draft)",
		"title in (a, b)":             "or[title is(a) title is(b)]",
		"-due < friday":               "due on_or_after(weekday/5)",
		"NOT estimate > 3":            "estimate lte(3)",
	}
	for text, want := range cases {
		spec, issues := list(t, text)
		if got := shapeOf(spec.Filter); got != want || !Runs(issues) {
			t.Errorf("%q: %s %s, want %s", text, got, codes(issues), want)
		}
	}
}

// What a field can't take is an error in a list and words in a search.
func TestWhatAFieldCantTakeIsSaidWhereItIs(t *testing.T) {
	fakeWorld(t)
	for text, code := range map[string]string{
		"status > done":   IssueBadOperator, // its options aren't ordered
		"due < soon":      IssueBadValue,
		"estimate = lots": IssueBadValue,
		"status:nothing":  IssueUnknownValue,
		"has:link":        IssueUnknownValue, // nobody has registered has:link yet
		"type:everything": IssueUnknownValue,
		"in < #launch":    IssueBadOperator,
		"from:@nobody":    IssueUnknownValue,
		"before:<friday":  IssueBadOperator,
		"type is empty":   IssueBadOperator,
		"title = (a, b)":  IssueBadOperator,
	} {
		_, issues := list(t, text)
		if len(issues) != 1 || issues[0].Code != code || issues[0].Level != LevelError || issues[0].Message == "" {
			t.Errorf("%q in a list: %s, want %s/error", text, codes(issues), code)
		}
		spec, issues := search(t, text)
		if len(issues) != 1 || issues[0].Code != code || issues[0].Level != LevelNote || !strings.Contains(issues[0].Message, ", so it was searched as words.") {
			t.Errorf("%q in a search: %s %+v", text, codes(issues), issues)
		}
		if spec.Filter == nil {
			t.Errorf("%q in a search is searched as words, not dropped", text)
		}
	}
}

// Past a limit nothing runs, in a search too, and the issue says where.
func TestPastALimitNothingRuns(t *testing.T) {
	fakeWorld(t)
	many := strings.Repeat("w ", querySpec.MaxConditions+1)
	if _, issues := search(t, many); Runs(issues) || codes(issues) != "too_many_conditions/error" {
		t.Errorf("51 words: %s", codes(issues))
	}
	if _, issues := search(t, "a AND (b OR (c AND (d OR e)))"); Runs(issues) || !strings.Contains(codes(issues), "too_deep/error") {
		t.Errorf("groups four deep: %s", codes(issues))
	}
	if _, issues := search(t, "((((a))))"); Runs(issues) {
		t.Errorf("brackets four deep: %s", codes(issues))
	}
	if _, issues := search(t, strings.Repeat("a", MaxText+1)); Runs(issues) || codes(issues) != "too_long/error" {
		t.Errorf("too long: %s", codes(issues))
	}
	// A typo is a note in a search: it still runs.
	if _, issues := search(t, `"unclosed`); !Runs(issues) || codes(issues) != "unclosed_quote/note" {
		t.Errorf("an unclosed quote: %s", codes(issues))
	}
}

// ORDER BY takes up to five sortable fields.
func TestOrderByTakesSortableFields(t *testing.T) {
	fakeWorld(t)
	spec, issues := list(t, "status:open ORDER BY due ASC, priority DESC")
	if len(issues) > 0 || len(spec.Sorts) != 2 || spec.Sorts[0] != (querySpec.Sort{Field: "due"}) || spec.Sorts[1] != (querySpec.Sort{Field: "priority", Desc: true}) {
		t.Fatalf("%+v %s", spec.Sorts, codes(issues))
	}
	if _, issues := list(t, "ORDER BY has"); codes(issues) != "unsortable/error" {
		t.Errorf("a field that can't sort: %s", codes(issues))
	}
	if _, issues := search(t, "ORDER BY stauts"); codes(issues) != "unknown_field/note" {
		t.Errorf("a sort nobody knows, in a search: %s", codes(issues))
	}
}

type fakeScope struct {
	fields []Field
	err    error
}

func (s fakeScope) Fields(context.Context, Asker, querySpec.Scope) ([]Field, error) {
	return s.fields, s.err
}

// A project's or a table's own fields are reached by name: quoted names look
// there first, bare names after the built-ins; two of one name is an error.
func TestCustomFieldsAreReachedByName(t *testing.T) {
	fakeWorld(t)
	Fields.RegisterScope(querySpec.ScopeTable, fakeScope{fields: []Field{
		{ID: "field:budget", Name: "Budget", Family: FamilyNumber, Sortable: true},
		{ID: "field:paid", Name: "Paid", Family: FamilyCheckbox},
		{ID: "field:due2", Name: "due", Family: FamilyDate},
		{ID: "field:stage", Name: "Stage group", Family: FamilyOption, Group: true, Choices: []Choice{{Value: "done"}}},
		{ID: "field:twin1", Name: "Twin", Family: FamilyText},
		{ID: "field:twin2", Name: "twin", Family: FamilyText},
	}})
	table := querySpec.Scope{Kind: querySpec.ScopeTable, ID: "t"}
	build := func(text string) (querySpec.Spec, []Issue) {
		return Build(context.Background(), sam, text, table, ModeList)
	}
	for text, want := range map[string]string{
		`"Budget" > 1200.50`:   "field:budget gt(1200.5)",
		`"Budget":₹12`:         "field:budget is(12)",
		`"Paid" = yes`:         "field:paid checked()",
		`"Paid" != yes`:        "field:paid unchecked()",
		`"due" < today`:        "field:due2 before(today)",
		`due < today`:          "due before(today)",
		`"Stage group" = done`: "field:stage status_group_is(done)",
	} {
		spec, issues := build(text)
		if got := shapeOf(spec.Filter); got != want || len(issues) > 0 {
			t.Errorf("%q: %s %s, want %s", text, got, codes(issues), want)
		}
	}
	if _, issues := build(`"Twin" ~ x`); codes(issues) != "ambiguous_field/error" {
		t.Errorf("two fields of one name: %s", codes(issues))
	}
	if spec, issues := build(`ORDER BY "Budget" DESC`); len(issues) > 0 || spec.Sorts[0].Field != "field:budget" {
		t.Errorf("a sort by a custom field: %+v %s", spec.Sorts, codes(issues))
	}
	// When the scope's fields can't be read, a name that isn't built in says
	// so, rather than that the field doesn't exist.
	Fields.scopes[querySpec.ScopeProject] = fakeScope{err: errors.New("down")}
	_, issues := Build(context.Background(), sam, `"Budget" > 3`, querySpec.Scope{Kind: querySpec.ScopeProject, ID: "p"}, ModeList)
	if codes(issues) != "unavailable/error" {
		t.Errorf("fields that can't be read: %s", codes(issues))
	}
}

// The demo's shared visitor finds only the demo's own people: a real member's
// handle names nobody for them.
func TestTheDemoVisitorResolvesOnlyTheDemosPeople(t *testing.T) {
	fakeWorld(t)
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.onemana.dev")
	visitor := Asker{Subject: accessBusiness.Subject{UserUUID: visitorID}, Email: "visitor@demo.onemana.dev"}
	spec, issues := Build(context.Background(), visitor, "from:@ana", querySpec.Scope{Kind: querySpec.ScopeSearch}, ModeSearch)
	if shapeOf(spec.Filter) != "from in(user:2222)" || len(issues) > 0 {
		t.Errorf("a cast member: %s %s", shapeOf(spec.Filter), codes(issues))
	}
	spec, issues = Build(context.Background(), visitor, "from:@sam", querySpec.Scope{Kind: querySpec.ScopeSearch}, ModeSearch)
	if shapeOf(spec.Filter) != "text contains(from:@sam)" || codes(issues) != "unknown_value/note" {
		t.Errorf("a real member, for the visitor: %s %s", shapeOf(spec.Filter), codes(issues))
	}
}

// A built spec written back reads back as the same spec, and is written for
// whoever reads it: a channel they can't open is (hidden).
func TestFormatWritesASpecBackForItsReader(t *testing.T) {
	fakeWorld(t)
	for _, text := range []string{
		`from:@sam in:#launch before:friday "pricing page" -draft`,
		"in:#launch in:#design status:open status:review",
		"labels:bug labels:urgent -from:@ana",
		"due < friday AND estimate >= 3 ORDER BY due, priority DESC",
		"due:2026-10-01..today key = Q4-* title:onboarding",
		"labels = (bug, urgent) status in (open, done) assignee in (@me, @my_team)",
		"NOT (plan OR roadmap) due IS EMPTY updated:>=past_30_days",
		"type:issue -type:doc is:thread due = next_7_days",
	} {
		first, issues := list(t, text)
		if !Runs(issues) {
			t.Fatalf("%q: %s", text, codes(issues))
		}
		written := Format(context.Background(), sam, first)
		again, issues := list(t, written)
		if !Runs(issues) || shapeOf(again.Filter) != shapeOf(first.Filter) || len(again.Sorts) != len(first.Sorts) {
			t.Errorf("%q\n  wrote %q\n  %s\n  %s %s", text, written, shapeOf(first.Filter), shapeOf(again.Filter), codes(issues))
		}
	}
	spec, _ := search(t, "in:#launch from:@sam")
	ana := Asker{Subject: accessBusiness.Subject{UserUUID: anaID}, Email: "ana@example.com"}
	if got := Format(context.Background(), ana, spec); got != `in:"(hidden)" from:@sam` {
		t.Errorf("for someone who can't open #launch: %q", got)
	}
}

// Suggestions: field names while a word is typed, a field's values after its
// colon.
func TestSuggestionsAtTheCaret(t *testing.T) {
	fakeWorld(t)
	ctx := context.Background()
	scope := querySpec.Scope{Kind: querySpec.ScopeSearch}
	labels := func(text string, caret int) []string {
		var out []string
		for _, s := range Suggest(ctx, sam, text, caret, scope) {
			out = append(out, s.Insert)
		}
		return out
	}
	if got := labels("launch fr", 9); len(got) != 1 || got[0] != "from:" {
		t.Errorf("a field being typed: %v", got)
	}
	if got := labels("from:", 5); strings.Join(got, ",") != "me,@my_team" {
		t.Errorf("after from: %v", got)
	}
	if got := labels("type:d", 6); strings.Join(got, ",") != "doc" {
		t.Errorf("type:d: %v", got)
	}
	if got := labels("status:", 7); strings.Join(got, ",") != "open,done,review" {
		t.Errorf("status values: %v", got)
	}
	if got := labels("created:", 8); len(got) == 0 || got[0] != "today" {
		t.Errorf("dates: %v", got)
	}
	if got := labels(`"pricing`, 5); got != nil {
		t.Errorf("inside a phrase: %v", got)
	}
}

// One name means one thing: a name or an alias registered twice, or a value
// added twice to a shared field, fails where it happens.
func TestANameIsRegisteredOnce(t *testing.T) {
	fakeWorld(t)
	for name, register := range map[string]func(){
		"a taken name":       func() { Fields.Register(Field{Name: "status", Family: FamilyText}) },
		"a taken alias":      func() { Fields.Register(Field{Name: "owner", Aliases: []string{"creator"}, Family: FamilyPeople}) },
		"a name with spaces": func() { Fields.Register(Field{Name: "due date", Family: FamilyDate}) },
		"no family":          func() { Fields.Register(Field{Name: "nothing"}) },
		"a value twice":      func() { Fields.AddChoices("is", Choice{Value: "thread"}) },
		"a synonym twice":    func() { Fields.AddChoices("type", Choice{Value: "ticketing", Synonyms: []string{"tickets"}}) },
		"two scope fields":   func() { Fields.RegisterScope("x", fakeScope{}); Fields.RegisterScope("x", fakeScope{}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			register()
		}()
	}
}
