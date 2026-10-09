package business

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/business/DataTable/formula"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// rollUpValues adds up a linked field's values (n linked rows) that way, as
// rollUp does a row's.
func rollUpValues(how string, values []formula.Value, n int) formula.Value {
	if how == "count" {
		return formula.Number(float64(n))
	}
	acc := newRollupAcc(how)
	for _, v := range values {
		if acc.add(v) {
			break
		}
	}
	return acc.result()
}

func TestRollUpValues(t *testing.T) {
	day := func(s string) formula.Value {
		v, _ := formula.ParseDate(s)
		return v
	}
	nums := []formula.Value{formula.Number(1200), formula.Blank(), formula.Number(300), formula.Error("Divided by zero"), formula.Number(180)}
	texts := []formula.Value{formula.Text("Print"), formula.Text("Video"), formula.Blank(), formula.Text("Print")}
	ticks := []formula.Value{formula.Bool(true), formula.Bool(false), formula.Bool(true)}
	dates := []formula.Value{day("2026-10-12"), day("2026-10-05"), formula.Blank(), day("2026-10-20")}
	cases := []struct {
		how    string
		values []formula.Value
		n      int
		want   interface{}
	}{
		{"count", nums, 5, float64(5)},
		{"count_values", nums, 5, float64(3)},
		{"sum", nums, 5, float64(1680)},
		{"average", nums, 5, float64(560)},
		{"min", nums, 5, float64(180)},
		{"max", nums, 5, float64(1200)},
		{"sum", nil, 0, float64(0)},
		{"average", nil, 0, nil},
		{"list", texts, 4, "Print, Video, Print"},
		{"unique", texts, 4, "Print, Video"},
		{"unique_count", texts, 4, float64(2)},
		{"checked", ticks, 3, float64(2)},
		{"earliest", dates, 4, "2026-10-05"},
		{"latest", dates, 4, "2026-10-20"},
		{"latest", nil, 0, nil},
	}
	for _, c := range cases {
		if got := rollUpValues(c.how, c.values, c.n).JSON(); got != c.want {
			t.Errorf("%s = %#v, want %#v", c.how, got, c.want)
		}
	}
	long := make([]formula.Value, 200)
	for i := range long {
		long[i] = formula.Text(strings.Repeat("x", 60))
	}
	if got := rollUpValues("list", long, len(long)); got.Kind != formula.KindError {
		t.Errorf("a list past 10,000 characters: %.80v", got)
	}
}

func TestLinkCells(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	// Refs as a read gives them, and ids, each once; a read's count of the
	// links past those shown is passed over.
	got, bad := linkItems([]interface{}{map[string]interface{}{"id": a.String(), "label": "Acme"}, b.String(), map[string]interface{}{"id": a.String()},
		map[string]interface{}{"id": "", "type": "more", "label": "5 more"}})
	if bad != "" || len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("links: %v, %q", got, bad)
	}
	if got, bad := linkItems(" " + a.String() + " "); bad != "" || len(got) != 1 || got[0] != a {
		t.Errorf("one id, not in a list: %v, %q", got, bad)
	}
	if got, bad := linkItems(nil); bad != "" || got != nil {
		t.Errorf("no cell: %v, %q", got, bad)
	}
	// Anything else is named, not dropped.
	for raw, want := range map[string]interface{}{
		`"Acme"`:            []interface{}{a.String(), "Acme"},
		`7`:                 []interface{}{7.0},
		`{"label":"Acme"}`:  []interface{}{map[string]interface{}{"label": "Acme"}},
		`"not an id"`:       "not an id",
		`{"id":"Acme Inc"}`: map[string]interface{}{"id": "Acme Inc"},
	} {
		if got, bad := linkItems(want); bad != raw || got != nil {
			t.Errorf("%s: %v, %q", raw, got, bad)
		}
	}
	// Which side of a link a cell is on.
	forward := tableLink{field: &model.Field{Id: uuid.New()}}
	inverse := tableLink{field: &model.Field{Id: uuid.New()}, inverseOf: forward.field.Id.String()}
	if l := forward.link(a, b); l.From != a || l.To != b {
		t.Errorf("forward: %+v", l)
	}
	if l := inverse.link(a, b); l.From != b || l.To != a {
		t.Errorf("the other side: %+v", l)
	}
	if id, ok := inverse.linkField(); !ok || id != forward.field.Id {
		t.Errorf("the other side reads %v", id)
	}
}

// linkRead is a read of one row's links through a relation, as readLinks
// and loadLinked leave it: n links counted, the first of them fetched, and
// those loaded named.
type linkRead struct {
	c   *computed
	l   tableLink
	row uuid.UUID
	pl  *pageLinks
	t   *target
	of  []uuid.UUID
}

func newLinkRead(n, fetched, loaded int) *linkRead {
	table := &model.DataTable{Id: uuid.New(), Name: "Vendors"}
	l := tableLink{field: &model.Field{Id: uuid.New(), Name: "Vendor"}, table: table.Id}
	r := &linkRead{l: l, row: uuid.New(), t: newTarget()}
	r.t.table, r.t.visible = table, true
	for i := 0; i < fetched; i++ {
		id := uuid.New()
		r.of = append(r.of, id)
		if i < loaded {
			r.t.rows[id] = &linkedRow{label: fmt.Sprint("V", i), values: []formula.Value{formula.Number(10)}}
		} else {
			r.t.skipped[id] = true
		}
	}
	r.pl = &pageLinks{count: map[uuid.UUID]int{r.row: n}, uncounted: map[uuid.UUID]bool{}, of: map[uuid.UUID][]uuid.UUID{r.row: r.of}}
	r.c = &computed{budget: formula.NewBudget(), targets: map[uuid.UUID]*target{table.Id: r.t}, problems: map[string]string{},
		linked: map[string]*pageLinks{l.field.Id.String(): r.pl}}
	return r
}

// cell is the row's cell as readers get it: each ref's label, and what
// formulas read.
func (r *linkRead) cell() ([]string, formula.Value) {
	refs, v := r.c.linkCell(r.l, r.row)
	labels := make([]string, len(refs))
	for i, ref := range refs {
		labels[i] = ref.Label
		if ref.Type == "more" {
			labels[i] = "[" + ref.Label + "]"
		}
	}
	return labels, v
}

func (r *linkRead) rollUp(how string) formula.Value {
	cost := &model.Field{Id: uuid.New(), Name: "Cost"}
	r.t.reads, r.t.readAt = []*model.Field{cost}, map[string]int{cost.Id.String(): 0}
	return r.c.rollUp(&rollupField{field: &model.Field{Id: uuid.New()}, cfg: rollupConfig{Aggregate: how}, link: r.l, of: cost}, r.row)
}

// A cell says what a read left out of it, and so do the rollups and
// formulas reading it.
func TestLinkCellsSayWhatTheyLeaveOut(t *testing.T) {
	// All its links, shown.
	r := newLinkRead(3, 3, 3)
	if labels, v := r.cell(); strings.Join(labels, ",") != "V0,V1,V2" || v.Text() != "V0, V1, V2" {
		t.Errorf("three links: %v, %v", labels, v)
	}
	if got := r.rollUp("count").JSON(); got != float64(3) {
		t.Errorf("count: %v", got)
	}
	if got := r.rollUp("sum").JSON(); got != float64(30) {
		t.Errorf("sum: %v", got)
	}
	// More than it shows: counted, and formulas don't get a part of them.
	r = newLinkRead(250, maxShownLinks, maxShownLinks)
	labels, v := r.cell()
	if len(labels) != maxShownLinks+1 || labels[maxShownLinks] != "[150 more]" || v != errTooManyToRead {
		t.Errorf("250 links: %d refs, last %q, formulas read %v", len(labels), labels[len(labels)-1], v)
	}
	if got := r.rollUp("count").JSON(); got != float64(250) {
		t.Errorf("count of 250: %v", got)
	}
	if got := r.rollUp("sum"); got != errTooManyLinked {
		t.Errorf("sum over links not fetched: %v", got)
	}
	// All of them fetched, for a rollup, but past what one read loads.
	r = newLinkRead(150, 150, 120)
	if got := r.rollUp("sum"); got != errTooManyLinked {
		t.Errorf("sum over rows not loaded: %v", got)
	}
	labels, _ = r.cell()
	if labels[0] != "V0" || labels[maxShownLinks-1] != "V99" || labels[maxShownLinks] != "[50 more]" {
		t.Errorf("150 links, 120 loaded: %v ... %v", labels[0], labels[maxShownLinks:])
	}
	r = newLinkRead(3, 3, 1)
	if labels, v := r.cell(); strings.Join(labels, ",") != "V0,…,…" || v != errTooManyToRead {
		t.Errorf("rows not loaded: %v, %v", labels, v)
	}
	// Past the links one read fetches: only how many.
	r = newLinkRead(12, 0, 0)
	if labels, _ := r.cell(); strings.Join(labels, ",") != "[12 links]" {
		t.Errorf("links not fetched: %v", labels)
	}
	if labels, _ := newLinkRead(1, 0, 0).cell(); strings.Join(labels, ",") != "[1 link]" {
		t.Errorf("a link not fetched: %v", labels)
	}
	// Past the links one read counts.
	r = newLinkRead(maxLinks+1, maxShownLinks, maxShownLinks)
	r.pl.uncounted[r.row] = true
	if labels, _ := r.cell(); labels[maxShownLinks] != "[901+ more]" {
		t.Errorf("counted as far as the read went: %v", labels[maxShownLinks])
	}
	if got := r.rollUp("count"); got != errLinksUncounted {
		t.Errorf("count of links not all counted: %v", got)
	}
	r = newLinkRead(0, 0, 0)
	r.pl.uncounted[r.row] = true
	if labels, _ := r.cell(); strings.Join(labels, ",") != "[Links not counted here]" {
		t.Errorf("links not counted at all: %v", labels)
	}
	// More than a cell counts.
	r = newLinkRead(maxCounted+1, maxShownLinks, maxShownLinks)
	if labels, _ := r.cell(); labels[maxShownLinks] != "[99,901+ more]" {
		t.Errorf("more than %d: %v", maxCounted, labels[maxShownLinks])
	}
	if got := r.rollUp("count"); got != errTooManyCounted || got.Str != "More than 100,000 linked rows" {
		t.Errorf("count past %d: %v", maxCounted, got)
	}
	// Links that couldn't be read.
	r = newLinkRead(3, 3, 3)
	r.c.linked[r.l.field.Id.String()] = &pageLinks{failed: true}
	if labels, v := r.cell(); strings.Join(labels, ",") != "[Links couldn't be read]" || v != errLinksFailed {
		t.Errorf("links that couldn't be read: %v, %v", labels, v)
	}
	if got := r.rollUp("count"); got != errLinksFailed {
		t.Errorf("count of links that couldn't be read: %v", got)
	}
	// Each of these is no answer, rather than a wrong one.
	for _, v := range []formula.Value{errLinksFailed, errTooManyLinked, errLinksUncounted, errTooManyCounted, errTooManyToRead} {
		if !v.IsUnfinished() || !formula.Unfinished(v.JSON()) {
			t.Errorf("%q reads as an answer", v.Str)
		}
	}
	if formula.Error("Divided by zero").IsUnfinished() {
		t.Error("a wrong answer reads as no answer")
	}
	// A table the reader can't open: its rows unnamed, and no names to
	// load.
	r = newLinkRead(2, 2, 0)
	r.t.visible = false
	if labels, v := r.cell(); strings.Join(labels, ",") != privateRow+","+privateRow || v.Text() != privateRow+", "+privateRow {
		t.Errorf("a private table's rows: %v, %v", labels, v)
	}
	// A deleted table's: no cell.
	r.t.table = nil
	if labels, v := r.cell(); len(labels) != 0 || v.Text() != "" {
		t.Errorf("a deleted table's rows: %v, %v", labels, v)
	}
}

func TestRollupFits(t *testing.T) {
	for _, c := range []struct {
		how  string
		kind formula.Kind
		ok   bool
	}{
		{"sum", formula.KindNumber, true}, {"sum", formula.KindText, false},
		{"earliest", formula.KindDate, true}, {"earliest", formula.KindNumber, false},
		{"checked", formula.KindBool, true}, {"checked", formula.KindText, false},
		{"list", formula.KindDate, true}, {"count", formula.KindBool, true}, {"median", formula.KindNumber, false},
	} {
		if rollupFits(c.how, c.kind) != c.ok {
			t.Errorf("%s of %v: %v", c.how, c.kind.Name(), !c.ok)
		}
	}
}

func TestLinkNames(t *testing.T) {
	fields := []*model.Field{{Name: "Budget"}, {Name: "budget 2"}}
	if got := inverseName("Budget", fields); got != "Budget 3" {
		t.Errorf("named %q", got)
	}
	if got := inverseName("Vendors", fields); got != "Vendors" {
		t.Errorf("named %q", got)
	}
	if clipLabel("  ") != "Untitled" || clipLabel(strings.Repeat("é", maxLabel+5)) != strings.Repeat("é", maxLabel)+"…" {
		t.Error("labels aren't clipped")
	}
}

// A rollup can't add up a formula that reads links or rollups, through other
// formulas too: a read of the linked table doesn't work those out.
func TestRollupProblems(t *testing.T) {
	field := func(name, typ, cfg string) *model.Field {
		return &model.Field{Id: uuid.New(), Name: name, Type: typ, Config: cfg}
	}
	vendor := field("Vendor", model.FieldRelation, fmt.Sprintf(`{"relation_target":"table","table_id":%q}`, uuid.New()))
	cost := field("Cost", model.FieldNumber, "{}")
	spend := field("Spend", model.FieldRollup, `{"aggregate":"count"}`)
	direct := field("Direct", model.FieldFormula, fmt.Sprintf(`{"formula":"{#%s} & \"\""}`, vendor.Id))
	through := field("Through", model.FieldFormula, fmt.Sprintf(`{"formula":"LEN({#%s})"}`, direct.Id))
	plain := field("Double", model.FieldFormula, fmt.Sprintf(`{"formula":"{#%s} * 2"}`, cost.Id))
	fields := []*model.Field{vendor, cost, spend, direct, through, plain}
	for f, want := range map[*model.Field]string{
		vendor:  "A rollup can't add up links to another table",
		spend:   "A rollup can't add up another rollup",
		direct:  "A rollup can't add up a formula that reads links or rollups",
		through: "A rollup can't add up a formula that reads links or rollups",
		plain:   "",
		cost:    "",
	} {
		if got := rollupProblem("sum", f, fields); got != want {
			t.Errorf("%s: %q, want %q", f.Name, got, want)
		}
	}
	if got := rollupProblem("earliest", cost, fields); got != "That way of adding up doesn't fit this field" {
		t.Errorf("the earliest of numbers: %q", got)
	}
}

// Notices that a table's links changed come at most once each tellEvery: a
// burst is sent as one now and one at the end of the wait.
func TestTellTablesSendsABurstAsOne(t *testing.T) {
	was, wasEvery := publishLinks, tellEvery
	defer func() { publishLinks, tellEvery = was, wasEvery }()
	var mu sync.Mutex
	sent := map[uuid.UUID]int{}
	publishLinks = func(id uuid.UUID) {
		mu.Lock()
		sent[id]++
		mu.Unlock()
	}
	count := func(id uuid.UUID) int {
		mu.Lock()
		defer mu.Unlock()
		return sent[id]
	}
	tellEvery = 100 * time.Millisecond
	a, b := uuid.New(), uuid.New()
	tellTables(a, a, b)
	for i := 0; i < 5; i++ {
		tellTables(a)
	}
	if count(a) != 1 || count(b) != 1 {
		t.Fatalf("at once: %d to a, %d to b", count(a), count(b))
	}
	for deadline := time.Now().Add(5 * time.Second); count(a) < 2 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(3 * tellEvery)
	if count(a) != 2 || count(b) != 1 {
		t.Errorf("after the wait: %d to a, %d to b", count(a), count(b))
	}
}
