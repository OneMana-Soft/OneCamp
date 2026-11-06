package business

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

func lbl(name string, rank int, rules ...string) *Label {
	if rules == nil {
		rules = []string{}
	}
	return &Label{ID: uuid.New(), Name: name, Rank: rank, Rules: rules}
}

// An editor can't strip a label's rules to do what it forbids: anything with
// a lower rank, or without one of the rules, lowers; taking it off always does.
func TestLowersIsALowerRankOrARuleGone(t *testing.T) {
	confidential := lbl("Confidential", 2, RuleNoExport, RuleNoGuestLinks)
	internal := lbl("Internal", 1, RuleNoGuestLinks)
	sameRankFewer := lbl("Shareable", 2, RuleNoExport)
	higherMore := lbl("Restricted", 3, RuleNoExport, RuleNoGuestLinks)
	cases := []struct {
		name     string
		from, to *Label
		want     bool
	}{
		{"nothing to a label", nil, internal, false},
		{"nothing to nothing", nil, nil, false},
		{"a label taken off", internal, nil, true},
		{"to a lower rank", confidential, internal, true},
		{"same rank, a rule gone", confidential, sameRankFewer, true},
		{"up to a higher rank with every rule", confidential, higherMore, false},
		{"up from Internal", internal, confidential, false},
	}
	for _, c := range cases {
		if got := Lowers(c.from, c.to); got != c.want {
			t.Errorf("%s: Lowers = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAnInputIsCheckedBeforeItIsSaved(t *testing.T) {
	s := func(v string) *string { return &v }
	n := func(v int) *int { return &v }
	r := func(v ...string) *[]string { return &v }
	bads := map[string]Input{
		"an empty name":     {Name: s("   ")},
		"a long name":       {Name: s(strings.Repeat("x", maxName+1))},
		"an unknown colour": {Colour: s("chartreuse")},
		"a rank below 0":    {Rank: n(-1)},
		"a rank above 100":  {Rank: n(101)},
		"an unknown rule":   {Rules: r(RuleNoExport, "no_printing")},
	}
	for name, in := range bads {
		l := lbl("Was", 0)
		if err := in.apply(l); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: %v, want ErrBadRequest", name, err)
		}
	}
	l := lbl("Was", 0)
	if err := (Input{Name: s("  Top   secret "), Colour: s("red"), Rank: n(3), Rules: r(RuleNoGuestLinks, RuleNoExport, RuleNoGuestLinks)}).apply(l); err != nil {
		t.Fatal(err)
	}
	if l.Name != "Top secret" || l.Colour != "red" || l.Rank != 3 || strings.Join(l.Rules, ",") != "no_export,no_guest_links" {
		t.Fatalf("applied: %+v", l)
	}
	if got := ruleWords(l.Rules); got != "no exports, no guest links" {
		t.Fatalf("ruleWords = %q", got)
	}
	if got := ruleWords(nil); got != "no rules" {
		t.Fatalf("ruleWords(nil) = %q", got)
	}
}

func TestAnItemsRowSaysWhatHappenedToItsLabel(t *testing.T) {
	k := Kind{Name: "board", Noun: "board", Category: "boards"}
	conf, intl := lbl("Confidential", 2), lbl("Internal", 1)
	cases := []struct {
		before, after *Label
		action, words string
	}{
		{nil, conf, "board.labelled", "Labelled a private board “Confidential”"},
		{conf, intl, "board.relabelled", "Changed the label on a private board from “Confidential” to “Internal”"},
		{intl, nil, "board.unlabelled", "Took the label “Internal” off a private board"},
	}
	for _, c := range cases {
		action, words := itemRow(k, "a private board", c.before, c.after)
		if action != c.action || words != c.words {
			t.Errorf("itemRow = %q %q, want %q %q", action, words, c.action, c.words)
		}
	}
}

// A kind or a rule hook missing a part fails the build's tests where it is
// registered, never at the first request.
func TestARegistrationMissingAPartPanics(t *testing.T) {
	mustPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s didn't panic", name)
			}
		}()
		f()
	}
	stand := func(context.Context, Who, string) (Standing, error) { return Standing{}, nil }
	mustPanic("a kind with no reader", func() { RegisterKind(Kind{Name: "thing", Noun: "thing", Category: "docs"}) })
	mustPanic("a kind with no category", func() { RegisterKind(Kind{Name: "thing", Noun: "thing", Stand: stand}) })
	RegisterKind(Kind{Name: "thing-twice", Noun: "thing", Category: "docs", Stand: stand})
	mustPanic("a kind registered twice", func() {
		RegisterKind(Kind{Name: "thing-twice", Noun: "thing", Category: "docs", Stand: stand})
	})
	hook := func(context.Context, string, []uuid.UUID) (Applied, error) { return Applied{}, nil }
	mustPanic("a hook for an unknown rule", func() { OnApplied("no_printing", hook) })
	mustPanic("a nil hook", func() { OnApplied(RuleNoExport, nil) })
}

// Every rule the admin card lists has its words, and the refusal answers with
// the one stable code, 403, in words that say what to do.
func TestEveryRuleHasItsWordsAndARefusalItsCode(t *testing.T) {
	for _, r := range Rules() {
		if r.Key == "" || r.Name == "" || r.Refuses == "" || r.Attempt == "" {
			t.Errorf("rule %+v is missing its words", r)
		}
	}
	err := error(&helpers.Refusal{Code: CodeForbids, Msg: "This board is labelled Confidential: it can't be exported. Its owner or a workspace admin can change the label."})
	rec := httptest.NewRecorder()
	if !helpers.WriteRefusal(rec, err) || rec.Code != 403 || !strings.Contains(rec.Body.String(), `"label_forbids"`) {
		t.Fatalf("the refusal: %d %s", rec.Code, rec.Body)
	}
}
