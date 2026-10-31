package business

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// The natural sort key, held to the fixture the browser's (the web's
// lib/tables/sortKey.ts) is held to: testdata/sortkey/natural.json, byte for
// byte the web repository's.
type sortKeyFixture struct {
	Pad   int `json:"pad"`
	Runes int `json:"runes"`
	Keys  []struct {
		In  string `json:"in"`
		Key string `json:"key"`
	} `json:"keys"`
	Order []string `json:"order"`
}

func readSortKeyFixture(t *testing.T) sortKeyFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "sortkey", "natural.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx sortKeyFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

func TestNaturalKeyIsTheFixtures(t *testing.T) {
	fx := readSortKeyFixture(t)
	if digitsPad != fx.Pad || keyRunes != fx.Runes {
		t.Fatalf("pad %d and runes %d, the fixture's %d and %d", digitsPad, keyRunes, fx.Pad, fx.Runes)
	}
	for _, c := range fx.Keys {
		if got := naturalKey(c.In); got != c.Key {
			t.Errorf("%q: %q, want %q", c.In, got, c.Key)
		}
	}
	// Byte order, as Go and Postgres's C collation compare them.
	type keyed struct{ in, key string }
	ks := make([]keyed, len(fx.Keys))
	for i, c := range fx.Keys {
		ks[i] = keyed{c.In, naturalKey(c.In)}
	}
	sort.SliceStable(ks, func(i, j int) bool { return ks[i].key < ks[j].key })
	got := make([]string, len(ks))
	for i, k := range ks {
		got[i] = k.in
	}
	if strings.Join(got, "\x00") != strings.Join(fx.Order, "\x00") {
		t.Errorf("order:\n%q\nwant\n%q", got, fx.Order)
	}
}

// fixtureRows is a row for each of the fixture's texts in field f, in the
// keys' order, as the table holds them.
func fixtureRows(fx sortKeyFixture, f *model.Field) []*evalRow {
	rows := make([]*evalRow, len(fx.Keys))
	for i, c := range fx.Keys {
		rows[i] = &evalRow{id: uuid.New(), positionKey: fmt.Sprintf("d%04d", i), values: map[string]interface{}{f.Id.String(): c.In}}
	}
	return rows
}

func textsOf(rows []*evalRow, f *model.Field) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i], _ = r.values[f.Id.String()].(string)
	}
	return out
}

// A text column sorts by the natural key, the browser's order for a table it
// holds whole, its blank cells last either way: what the index's sort column
// holds (cellIndex.go) and the SQL reads.
func TestTextSortsByTheNaturalKey(t *testing.T) {
	fx := readSortKeyFixture(t)
	cols := columns(deals)
	who := asker{now: now, zone: time.UTC}
	rows := fixtureRows(fx, fName)
	if err := sortRows(rows, bySorts([]query.Sort{{Field: "field:" + fName.Id.String()}}), cols, nil, who); err != nil {
		t.Fatal(err)
	}
	var valued, blank []string
	for _, in := range fx.Order {
		if strings.TrimSpace(in) == "" {
			blank = append(blank, in)
		} else {
			valued = append(valued, in)
		}
	}
	// Blank cells keep the table's order: the keys'.
	blank = blank[:0]
	for _, c := range fx.Keys {
		if strings.TrimSpace(c.In) == "" {
			blank = append(blank, c.In)
		}
	}
	want := append(append([]string{}, valued...), blank...)
	if got := textsOf(rows, fName); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("by name:\n%q\nwant\n%q", got, want)
	}
	// Going down: the keys turned round, blanks still last, and ties (same
	// key) still in the table's order.
	if err := sortRows(rows, bySorts([]query.Sort{{Field: "field:" + fName.Id.String(), Desc: true}}), cols, nil, who); err != nil {
		t.Fatal(err)
	}
	got := textsOf(rows, fName)
	for i := 1; i < len(valued); i++ {
		a, b := naturalKey(got[i-1]), naturalKey(got[i])
		if a < b {
			t.Errorf("going down, %q before %q", got[i-1], got[i])
		}
	}
	if strings.Join(got[len(valued):], "\x00") != strings.Join(blank, "\x00") {
		t.Errorf("going down, the blanks: %q", got[len(valued):])
	}
}

// A choice sorts by its options' order, and a label no option has after them
// all, by its natural key.
func TestChoiceLabelsNoOptionHasSortByTheNaturalKey(t *testing.T) {
	cols := columns(deals)
	who := asker{now: now, zone: time.UTC}
	labels := []string{"Row 10", "Won", "row 2", "New", "Éclair", "eclair 1", "Talking"}
	var rows []*evalRow
	for i, l := range labels {
		rows = append(rows, &evalRow{id: uuid.New(), positionKey: fmt.Sprintf("d%04d", i), values: map[string]interface{}{fStage.Id.String(): l}})
	}
	if err := sortRows(rows, bySorts([]query.Sort{{Field: "field:" + fStage.Id.String()}}), cols, nil, who); err != nil {
		t.Fatal(err)
	}
	want := "New,Talking,Won,Éclair,eclair 1,row 2,Row 10"
	if got := strings.Join(textsOf(rows, fStage), ","); got != want {
		t.Errorf("by stage: %s, want %s", got, want)
	}
}
