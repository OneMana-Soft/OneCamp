package whoKnowsBusiness

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	domain "github.com/akashc777/OneCamp/domain/WhoKnows"
	"github.com/akashc777/OneCamp/helpers"
)

// A small world the seams answer from: items the asker may or may not open,
// who did what on each, and who asked to be left out.
type world struct {
	open      map[string]string // item uuid → title, for items the asker may open
	people    map[string]string // person uuid → name
	did       []domain.Contribution
	left      map[string]bool
	seeds     []domain.Seed
	counted   *domain.Items // what count was asked for
	readKinds [][]entityLinkBusiness.Ref
	who       []accessBusiness.Subject
}

func id(n int) string { return uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprint(n))).String() }

func (w *world) install(t *testing.T) {
	t.Helper()
	oldSeeds, oldRead, oldCount, oldLeft := keywordSeeds, readMany, count, optedOut
	t.Cleanup(func() { keywordSeeds, readMany, count, optedOut = oldSeeds, oldRead, oldCount, oldLeft })
	keywordSeeds = func(_ context.Context, _ Asker, _ string, n int) ([]domain.Seed, error) {
		if len(w.seeds) > n {
			return w.seeds[:n], nil
		}
		return w.seeds, nil
	}
	readMany = func(_ context.Context, who accessBusiness.Subject, refs []entityLinkBusiness.Ref) ([]entityLinkBusiness.Entity, error) {
		w.readKinds = append(w.readKinds, refs)
		w.who = append(w.who, who)
		out := make([]entityLinkBusiness.Entity, len(refs))
		for i, r := range refs {
			out[i] = entityLinkBusiness.Entity{Kind: r.Kind, ID: r.ID, Access: entityLinkBusiness.AccessNone}
			if r.Kind == "person" {
				if n, ok := w.people[r.ID]; ok {
					out[i] = entityLinkBusiness.Entity{Kind: r.Kind, ID: r.ID, Access: entityLinkBusiness.AccessOK, Title: n, Data: map[string]any{"avatar_key": "a/" + n}}
				}
			} else if title, ok := w.open[r.ID]; ok {
				out[i] = entityLinkBusiness.Entity{Kind: r.Kind, ID: r.ID, Access: entityLinkBusiness.AccessOK, Title: title}
			}
		}
		return out, nil
	}
	// count answers as the graph would for the items it is given: only those.
	count = func(_ context.Context, items domain.Items) (*domain.Counted, error) {
		cp := items
		w.counted = &cp
		asked := map[string]bool{}
		for _, l := range [][]string{items.Docs, items.Tasks, items.Messages} {
			for _, i := range l {
				asked[i] = true
			}
		}
		by := map[string]*domain.Person{}
		out := &domain.Counted{}
		for _, c := range w.did {
			if !asked[c.Item] {
				continue
			}
			p := by[c.Person]
			if p == nil {
				p = &domain.Person{UUID: c.Person}
				by[c.Person] = p
			}
			switch c.Role {
			case domain.RoleWrote:
				p.Wrote++
			case domain.RoleAssigned:
				p.Assigned++
			case domain.RoleCommented:
				p.Commented++
			}
			out.Contributions = append(out.Contributions, c)
		}
		for _, p := range by {
			out.People = append(out.People, *p)
		}
		sort.Slice(out.People, func(i, j int) bool { return out.People[i].UUID < out.People[j].UUID })
		return out, nil
	}
	optedOut = func(_ context.Context, ids []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, i := range ids {
			if w.left[i] {
				out[i] = true
			}
		}
		return out, nil
	}
}

var asker = Asker{Subject: accessBusiness.Subject{UserUID: "0x1", UserUUID: id(1000)}, Email: "asker@example.test"}

func names(a *Answer) []string {
	var out []string
	for _, p := range a.People {
		out = append(out, p.Name)
	}
	return out
}

// The privacy rule: a doc the asker can't open is neither counted nor
// evidence, and a person whose only work on the words is in it isn't named.
// A comment on it is dropped with it.
func TestWhoKnowsCountsOnlyWhatTheAskerCanRead(t *testing.T) {
	plan, secret := id(1), id(2)
	ana, ben, cy := id(11), id(12), id(13)
	w := &world{
		open:   map[string]string{plan: "Launch plan"},
		people: map[string]string{ana: "Ana", ben: "Ben", cy: "Cy"},
		seeds:  []domain.Seed{{Kind: "doc", ID: secret}, {Kind: "doc", ID: plan}},
		did: []domain.Contribution{
			{Kind: "doc", Item: plan, Person: ana, Role: domain.RoleWrote},
			{Kind: "doc", Item: secret, Person: ben, Role: domain.RoleWrote},
			{Kind: "doc", Item: secret, Person: ana, Role: domain.RoleCommented},
			{Kind: "doc", Item: secret, Person: cy, Role: domain.RoleCommented},
		},
	}
	w.install(t)
	got, err := Ask(context.Background(), asker, "launch", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names(got), ",") != "Ana" {
		t.Fatalf("named %v, want only Ana (Ben wrote and Cy commented only on a doc the asker can't open)", names(got))
	}
	if w.counted == nil || strings.Join(w.counted.Docs, ",") != plan {
		t.Errorf("the count was asked about %+v, want only the doc the asker can open", w.counted)
	}
	ana0 := got.People[0]
	if ana0.Wrote != 1 || ana0.Commented != 0 || ana0.Score != 3 {
		t.Errorf("Ana counts %+v: her comment on the hidden doc counted", ana0)
	}
	for _, e := range ana0.Evidence {
		if e.ID == secret {
			t.Errorf("the hidden doc is evidence: %+v", e)
		}
	}
	if got.Considered != 1 {
		t.Errorf("considered %d, want 1 (the hidden doc isn't counted, nor said to be left out)", got.Considered)
	}
}

// Evidence is only ever a decided item, even if the count names another.
func TestWhoKnowsEvidenceIsOnlyDecidedItems(t *testing.T) {
	plan, ghost := id(1), id(3)
	ana := id(11)
	readable := map[domain.Seed]decided{{Kind: "doc", ID: plan}: {Title: "Plan"}}
	ev := evidenceOf([]domain.Contribution{
		{Kind: "doc", Item: ghost, Person: ana, Role: domain.RoleWrote},
		{Kind: "doc", Item: plan, Person: ana, Role: domain.RoleCommented},
		{Kind: "doc", Item: plan, Person: ana, Role: domain.RoleWrote},
	}, readable)
	if len(ev[ana]) != 1 || ev[ana][0].ID != plan || ev[ana][0].Role != domain.RoleWrote || ev[ana][0].Title != "Plan" {
		t.Errorf("evidence %+v, want the plan once, as written", ev[ana])
	}
}

// Someone who asked to be left out is never named, however much they did;
// the asker's own choice is said back to them.
func TestWhoKnowsLeavesOutWhoAskedToBe(t *testing.T) {
	plan := id(1)
	ana, ben := id(11), id(12)
	w := &world{
		open:   map[string]string{plan: "Plan"},
		people: map[string]string{ana: "Ana", ben: "Ben"},
		seeds:  []domain.Seed{{Kind: "doc", ID: plan}},
		did: []domain.Contribution{
			{Kind: "doc", Item: plan, Person: ben, Role: domain.RoleWrote},
			{Kind: "doc", Item: plan, Person: ana, Role: domain.RoleCommented},
		},
		left: map[string]bool{ben: true, asker.Subject.UserUUID: true},
	}
	w.install(t)
	got, err := Ask(context.Background(), asker, "plan", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names(got), ",") != "Ana" {
		t.Errorf("named %v, want Ana alone (Ben left himself out)", names(got))
	}
	if !got.YouHidden {
		t.Error("the asker left themselves out and isn't told so")
	}
	w.left = nil
	got, _ = Ask(context.Background(), asker, "plan", 0)
	if strings.Join(names(got), ",") != "Ben,Ana" || got.YouHidden {
		t.Errorf("back in: named %v (hidden %v), want Ben then Ana", names(got), got.YouHidden)
	}
}

// Caps: at most MaxSeeds items are decided and counted, at most the limit of
// people named (MaxLimit at most), EvidencePerPerson items each.
func TestWhoKnowsHoldsItsCaps(t *testing.T) {
	ana := id(11)
	w := &world{open: map[string]string{}, people: map[string]string{ana: "Ana"}}
	for i := 0; i < 300; i++ {
		item := id(100 + i)
		w.open[item] = fmt.Sprint("Doc ", i)
		w.seeds = append(w.seeds, domain.Seed{Kind: "doc", ID: item})
		w.did = append(w.did, domain.Contribution{Kind: "doc", Item: item, Person: ana, Role: domain.RoleWrote})
	}
	for i := 0; i < 40; i++ {
		p := id(5000 + i)
		w.people[p] = fmt.Sprint("P", i)
		w.did = append(w.did, domain.Contribution{Kind: "doc", Item: w.seeds[i].ID, Person: p, Role: domain.RoleCommented})
	}
	w.install(t)
	// Another source after the keyword search can't push past the cap.
	more := func(context.Context, Asker, string, int) ([]domain.Seed, error) {
		return []domain.Seed{{Kind: "doc", ID: id(99999)}}, nil
	}
	w.seeds = append(w.seeds, w.seeds...) // the keyword search may repeat itself
	got, err := Ask(context.Background(), asker, "doc", 0, more)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(w.readKinds[0]); n != MaxSeeds {
		t.Errorf("decided %d items, want %d", n, MaxSeeds)
	}
	if n := len(w.counted.Docs); n != MaxSeeds {
		t.Errorf("counted %d items, want %d", n, MaxSeeds)
	}
	if len(got.People) != DefaultLimit {
		t.Errorf("named %d people, want the default %d", len(got.People), DefaultLimit)
	}
	if got.People[0].Name != "Ana" || got.People[0].Wrote != MaxSeeds || len(got.People[0].Evidence) != EvidencePerPerson {
		t.Errorf("first %+v, want Ana with %d written and %d evidence", got.People[0].Name, MaxSeeds, EvidencePerPerson)
	}
	if got, _ := Ask(context.Background(), asker, "doc", MaxLimit); len(got.People) != MaxLimit {
		t.Errorf("asked for %d, named %d", MaxLimit, len(got.People))
	}
	for _, bad := range []int{-1, MaxLimit + 1} {
		if _, err := Ask(context.Background(), asker, "doc", bad); err != ErrBadRequest {
			t.Errorf("limit %d: %v, want ErrBadRequest", bad, err)
		}
	}
	for _, q := range []string{"", " a ", strings.Repeat("x", MaxQuery+1)} {
		if _, err := Ask(context.Background(), asker, q, 0); err != ErrBadRequest {
			t.Errorf("query %q: %v, want ErrBadRequest", q[:min(len(q), 10)], err)
		}
	}
}

// Nobody asking is nobody answered.
func TestWhoKnowsAnswersNoOneForNoOne(t *testing.T) {
	(&world{}).install(t)
	if _, err := Ask(context.Background(), Asker{}, "plan", 0); err != ErrNoOne {
		t.Errorf("an empty asker: %v, want ErrNoOne", err)
	}
	if _, err := Rank(context.Background(), Asker{}, "plan", nil, 0); err != ErrNoOne {
		t.Errorf("an empty asker ranking: %v, want ErrNoOne", err)
	}
}

// Seeds that aren't uuids of a known kind never reach a read.
func TestWhoKnowsTakesUUIDsOnly(t *testing.T) {
	w := &world{open: map[string]string{}, seeds: []domain.Seed{{Kind: "doc", ID: "0x1"}, {Kind: "board", ID: id(1)}, {Kind: "doc", ID: "<script>"}}}
	w.install(t)
	if _, err := Ask(context.Background(), asker, "plan", 0); err != nil {
		t.Fatal(err)
	}
	if len(w.readKinds) != 0 && len(w.readKinds[0]) != 0 {
		t.Errorf("decided %v; none of them is an item uuid", w.readKinds[0])
	}
}

// Who knows reads as the subject it's given and never consults its Admin
// flag: a token's subject (SubjectFor, no admin) and an admin's get the same
// answer. A source guard holds the package to never reading .Admin.
func TestWhoKnowsNeverReadsTheAdminFlag(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	read := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		read++
		ast.Inspect(file, func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok && (s.Sel.Name == "Admin" || s.Sel.Name == "IsAdmin" || s.Sel.Name == "ActingAdmin") {
				t.Errorf("%s reads %s: Who knows decides as a member, whoever asks", fset.Position(s.Pos()), s.Sel.Name)
			}
			return true
		})
	}
	if read == 0 {
		t.Fatal("read no source; this guard has gone stale")
	}
	plan, ana := id(1), id(11)
	w := &world{open: map[string]string{plan: "Plan"}, people: map[string]string{ana: "Ana"}, seeds: []domain.Seed{{Kind: "doc", ID: plan}},
		did: []domain.Contribution{{Kind: "doc", Item: plan, Person: ana, Role: domain.RoleWrote}}}
	w.install(t)
	admin := asker
	admin.Subject.Admin = true
	a, _ := Ask(context.Background(), admin, "plan", 0)
	m, _ := Ask(context.Background(), asker, "plan", 0)
	if fmt.Sprint(names(a)) != fmt.Sprint(names(m)) {
		t.Errorf("an admin hears %v, a member %v", names(a), names(m))
	}
}

// A seed source that fails is skipped; the keyword seeds still answer.
func TestWhoKnowsSurvivesAFailingSource(t *testing.T) {
	plan, ana := id(1), id(11)
	w := &world{open: map[string]string{plan: "Plan"}, people: map[string]string{ana: "Ana"}, seeds: []domain.Seed{{Kind: "doc", ID: plan}},
		did: []domain.Contribution{{Kind: "doc", Item: plan, Person: ana, Role: domain.RoleWrote}}}
	w.install(t)
	broken := func(context.Context, Asker, string, int) ([]domain.Seed, error) { return nil, fmt.Errorf("down") }
	got, err := Ask(context.Background(), asker, "plan", 0, broken)
	if err != nil || len(got.People) != 1 {
		t.Fatalf("%v, %v; want Ana from the keyword seeds", got, err)
	}
}

// The asker is never in their own answer: asking yourself is noise. Their
// own choice still comes back (you_hidden).
func TestWhoKnowsLeavesTheAskerOut(t *testing.T) {
	plan, ana := id(1), id(11)
	me := asker.Subject.UserUUID
	w := &world{open: map[string]string{plan: "Plan"}, people: map[string]string{ana: "Ana", me: "Me"}, seeds: []domain.Seed{{Kind: "doc", ID: plan}},
		did: []domain.Contribution{{Kind: "doc", Item: plan, Person: me, Role: domain.RoleWrote}, {Kind: "doc", Item: plan, Person: ana, Role: domain.RoleCommented}}}
	w.install(t)
	got, err := Ask(context.Background(), asker, "plan", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names(got), ",") != "Ana" {
		t.Errorf("named %v, want Ana alone (the asker wrote the plan, but asked)", names(got))
	}
}

// To the demo's shared visitor, only the demo's own people are named
// (helpers.DemoListsPerson, as the unified search): anyone else's work on the
// demo server stays theirs.
func TestWhoKnowsShowsTheDemoVisitorOnlyTheDemosPeople(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")
	plan, cast, stranger := id(1), id(11), id(12)
	w := &world{open: map[string]string{plan: "Plan"}, people: map[string]string{cast: "Maya", stranger: "Someone"}, seeds: []domain.Seed{{Kind: "doc", ID: plan}},
		did: []domain.Contribution{{Kind: "doc", Item: plan, Person: cast, Role: domain.RoleWrote}, {Kind: "doc", Item: plan, Person: stranger, Role: domain.RoleWrote}}}
	w.install(t)
	emails := map[string]string{cast: "maya" + helpers.DemoCastEmailDomain, stranger: "someone@example.com"}
	inner := count
	count = func(ctx context.Context, items domain.Items) (*domain.Counted, error) {
		c, err := inner(ctx, items)
		for i := range c.People {
			c.People[i].Email = emails[c.People[i].UUID]
		}
		return c, err
	}
	visitor := asker
	visitor.Email = "visitor@demo.example"
	got, err := Ask(context.Background(), visitor, "plan", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names(got), ",") != "Maya" {
		t.Errorf("the demo visitor hears %v, want the demo's Maya alone", names(got))
	}
	// Anyone else on the demo server hears both.
	got, _ = Ask(context.Background(), asker, "plan", 0)
	if len(got.People) != 2 {
		t.Errorf("a member hears %v, want both", names(got))
	}
}
