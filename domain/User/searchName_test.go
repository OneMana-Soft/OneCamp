package domain

import (
	"regexp"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestSearchNameHoldsEveryNameLowerCased(t *testing.T) {
	for _, c := range []struct{ name, full, title, dept, want string }{
		{"alice", "Alice Smith", "Product Designer", "Marketing", "alice alice smith product designer marketing"},
		{"bob", "", "  ", "Ops", "bob ops"},
		{"", "", "", "", ""},
		{"İpek", "ÖZGE", "", "", "İpek özge"}, // İ lowers to i, which /i never pairs with İ: kept
	} {
		if got := SearchName(c.name, c.full, c.title, c.dept); got != c.want {
			t.Errorf("SearchName(%q, %q, %q, %q) = %q, want %q", c.name, c.full, c.title, c.dept, got, c.want)
		}
	}
}

// What the searches' filter matches in any one name, their root matches in
// the search name: the root holds everyone the filter can keep.
func TestEveryNameTheFilterMatchesTheSearchNameMatches(t *testing.T) {
	names := []string{"İstanbul", "Kelvin K", "ſtraße", "ΣΊΣΥΦΟΣ", "Ǆemal", "ǅ", "Ω ohm", "ﬃ ligature", "José Núñez"}
	for _, n := range names {
		sn := SearchName(n, "", "", "")
		runes := []rune(n)
		for i := range runes {
			for j := i + 1; j <= len(runes) && j <= i+4; j++ {
				term := regexp.QuoteMeta(string(runes[i:j]))
				re := regexp.MustCompile(`(?i).*` + term + `.*`)
				if re.MatchString(n) && !re.MatchString(sn) {
					t.Errorf("%q matches %q but not its search name %q", term, n, sn)
				}
			}
		}
	}
}

// A write that leaves a name out leaves it as stored, so the search name is
// made from the names the node will hold.
func TestSearchNameOfAWriteKeepsTheStoredNamesItLeavesOut(t *testing.T) {
	stored := &dgraphStruct.DgraphUser{UserName: "alice", UserFullName: "Alice Smith", Department: "Marketing"}
	update := &dgraphStruct.DgraphUser{Title: "Designer", UserFullName: "Alice Jones"}
	if got := searchNameOf(update, stored); got != "alice alice jones designer marketing" {
		t.Errorf("got %q", got)
	}
	if got := searchNameOf(&dgraphStruct.DgraphUser{UserName: "new"}, &dgraphStruct.DgraphUser{}); got != "new" {
		t.Errorf("a new person: %q", got)
	}
}
