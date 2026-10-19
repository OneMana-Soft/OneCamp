package business

import (
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"testing"
)

// The golden fixtures both parsers are held to (design/QUERY_STACK_CONTRACT.md
// §4.8): testdata/searchquery/parse.json, byte-identical in the web repo, whose
// lib/search/query.test.ts runs every case through the TypeScript twin. Each
// case is a text, the tree it parses to (spans, issues and tokens included)
// and its canonical form. -update rewrites the file from this parser; review
// the diff before keeping it.
var update = flag.Bool("update", false, "rewrite testdata/searchquery/parse.json from the Go parser")

const fixturePath = "../../testdata/searchquery/parse.json"

type parseCase struct {
	Name      string `json:"name"`
	Text      string `json:"text"`
	Syntax    Syntax `json:"syntax"`
	Canonical string `json:"canonical"`
}

type parseFixtures struct {
	Note  string      `json:"note"`
	Cases []parseCase `json:"cases"`
}

// The texts the fixtures hold: every rule of §4.1–§4.2, each area's examples
// (chat §3c, projects §3, tables §3b) and what a person mistypes.
var fixtureTexts = []struct{ name, text string }{
	{"empty", ""},
	{"spaces", "   "},
	{"one word", "launch"},
	{"words side by side", "launch plan review"},
	{"a phrase", `"pricing page"`},
	{"an unclosed phrase", `"pricing page`},
	{"an escaped quote", `"say \"hi\" \\ bye"`},
	{"a negated word", "-draft"},
	{"a negated phrase", `-"old plan"`},
	{"a lone dash", "a - b"},
	{"slack modifiers", `from:@sam in:#launch before:friday "pricing page" -draft`},
	{"a modifier with a quoted value", `from:"Sam Rivera"`},
	{"a modifier with a comparison", "due:<friday estimate:>=3 status:!=done"},
	{"an empty modifier", "from:"},
	{"emoji modifiers", "has::tada: hasmy::eyes:"},
	{"a negated modifier", "-in:#random"},
	{"a quoted field modifier", `"Stage":done`},
	{"a quoted field modifier with a quoted value", `"Stage":"In review"`},
	{"a url is a modifier nobody registered", "https://example.com/a?b=c"},
	{"a time stays one word", "standup 10:30"},
	{"compare form", "status = done"},
	{"compare form, touching", "status!=done due<=today"},
	{"compare with a quoted value", `status = "In review"`},
	{"compare with a quoted field", `"Story points" >= 3`},
	{"compare with a relative value", "created > -7d"},
	{"contains", "name ~ launch"},
	{"doesn't contain", "name !~ draft"},
	{"in a list", "labels in (bug, urgent)"},
	{"not in a list", `labels NOT IN (bug, "won't fix")`},
	{"exactly a list", "labels = (bug, urgent)"},
	{"is empty", "due is empty"},
	{"is not empty", "due IS NOT EMPTY"},
	{"is null", "assignee is null"},
	{"jql", "project = Q4 AND status != Done AND (assignee = @me OR labels in (bug, urgent)) ORDER BY due ASC, priority DESC"},
	{"lower-case and between clauses", "status = done and assignee = @me"},
	{"lower-case or between clauses", "status:open or status:review"},
	{"lower-case and between words", "terms and conditions"},
	{"lower-case not before a clause", "not status:done"},
	{"lower-case not among words", "do not disturb"},
	{"capital OR between words", "plan OR roadmap"},
	{"capital NOT", "NOT draft"},
	{"NOT over brackets", "NOT (a OR b)"},
	{"a dash over brackets", "-(a OR b) c"},
	{"OR at the start is a word", "OR plan"},
	{"OR at the end is a word", "plan OR"},
	{"AND at the start is a word", "AND plan"},
	{"brackets fold", "(a b) c"},
	{"or within and", "(a OR b) (c OR d)"},
	{"three deep", "a AND (b OR (c AND (d OR e)))"},
	{"four deep", "((((a))))"},
	{"empty brackets", "() launch"},
	{"an unclosed bracket", "(a OR b"},
	{"a stray bracket", "a) b"},
	{"an unclosed list", "labels in (bug, urgent"},
	{"brackets in a list", "labels in ((bug))"},
	{"a compare with no value", "status ="},
	{"order by alone", "ORDER BY due"},
	{"order by, lower case", "status:open order by due desc, created"},
	{"too many sorts", "order by a, b, c, d, e, f"},
	{"words after order by", "x ORDER BY due extra words"},
	{"order by with no field is words", "order by"},
	{"in is a word in a sentence", "bugs in login"},
	{"a wildcard", "key = Q4-*"},
	{"a range", "created:2026-10-01..today"},
	{"a type and a title", "type:doc in:\"Product handbook\" updated:>=past_30_days title:onboarding"},
	{"a word that looks like a comparison", "<3 a=b 123=456"},
	{"commas", "a, b"},
	{"non-latin text", "réunion 会议 🎉"},
	{"a dm and a group", "in:@ana in:\"Launch crew\" to:me with:@sam is:thread"},
	{"a custom field by name", `"Budget" > 1200.50 AND "Due date" < next_week`},
	{"a status group", `"Stage group" = done`},
	{"double negation", "--draft"},
	{"a negated compare clause", "NOT status = done"},
	{"a negated list", "-labels in (bug)"},
	{"chat search in a conversation", `in:#launch "release notes" from:@sam after:2026-10-01`},
	{"an exact key", "Q4-12"},
	{"a status category", "status_category = done AND key = Q4-*"},
	{"tables' relative dates", `"Due date" = next_7_days AND created:-2w..today`},
	{"keywords in mixed case", "labels Not In (a) ORDER by due Desc"},
	{"a kelvin sign is not a k", "status:open \u212aND status:done"},
	{"spaces of every kind", "a\u00a0b\u3000c\u2003d\u0085e"},
	{"astral characters keep their places", "𝔘 from:@sam 🎉"},
}

func TestTheParserMatchesItsFixtures(t *testing.T) {
	var fx parseFixtures
	if *update {
		fx.Note = "Golden cases for the one query grammar (design/QUERY_STACK_CONTRACT.md §4). Byte-identical in the backend and web repos; generated by the Go parser with go test ./business/SearchQuery -run TestTheParserMatchesItsFixtures -update, then reviewed."
		for _, c := range fixtureTexts {
			syn := Parse(c.text)
			fx.Cases = append(fx.Cases, parseCase{Name: c.name, Text: c.text, Syntax: syn, Canonical: syn.String()})
		}
		raw, err := json.MarshalIndent(fx, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixturePath, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("the fixtures: %v", err)
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("the fixtures: %v", err)
	}
	if len(fx.Cases) != len(fixtureTexts) {
		t.Errorf("the fixtures hold %d cases, the test %d: run with -update and review", len(fx.Cases), len(fixtureTexts))
	}
	for _, c := range fx.Cases {
		got := Parse(c.Text)
		if !reflect.DeepEqual(normalised(got), normalised(c.Syntax)) {
			g, _ := json.Marshal(got)
			w, _ := json.Marshal(c.Syntax)
			t.Errorf("%s: %q parsed to\n  %s\nwant\n  %s", c.Name, c.Text, g, w)
		}
		if s := got.String(); s != c.Canonical {
			t.Errorf("%s: %q writes back as %q, want %q", c.Name, c.Text, s, c.Canonical)
		}
	}
}

// Writing a tree back and reading it again gives the same tree: chips that
// write tokens, and saved views written by Format, mean what they meant.
func TestTheCanonicalTextParsesToTheSameTree(t *testing.T) {
	for _, c := range fixtureTexts {
		first := Parse(c.text)
		again := Parse(first.String())
		if !reflect.DeepEqual(shape(first.Root), shape(again.Root)) || !reflect.DeepEqual(sortShape(first.Sorts), sortShape(again.Sorts)) {
			a, _ := json.Marshal(shape(first.Root))
			b, _ := json.Marshal(shape(again.Root))
			t.Errorf("%s: %q → %q changed the tree:\n  %s\n  %s", c.name, c.text, first.String(), a, b)
		}
	}
}

// normalised is s as JSON would read it back, so nil and empty lists compare
// alike.
func normalised(s Syntax) Syntax {
	raw, _ := json.Marshal(s)
	var out Syntax
	_ = json.Unmarshal(raw, &out)
	return out
}

// shape is a tree without its places.
func shape(n *Node) *Node {
	if n == nil {
		return nil
	}
	c := *n
	c.Span = [2]int{}
	c.Children = nil
	for _, ch := range n.Children {
		c.Children = append(c.Children, shape(ch))
	}
	if len(c.Values) == 0 {
		c.Values = nil
	}
	return &c
}

func sortShape(keys []SortKey) []SortKey {
	var out []SortKey
	for _, k := range keys {
		k.Span = [2]int{}
		out = append(out, k)
	}
	return out
}
