//go:build integration

package business

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// A pair's links named on the team side only, the person side only, both
// (Airtable's shape) or a mix, with either table made first, so each side is
// the storing one in turn: every link arrives once, none doubles, none is
// lost; the source's maps are never written; two imports in a row read the
// same.
func TestImportBaseBringsLinksNamedOnEitherSideInEitherOrder(t *testing.T) {
	w := newRelationWorld(t)
	names := map[string]string{"t1": "Design", "t2": "Sales", "t3": "Ops", "p1": "Ada", "p2": "Bo", "p3": "Cy", "p4": "Di"}
	type src struct{ team, person map[string][]string }
	cases := []struct {
		name string
		s    src
	}{
		{"team side only", src{team: map[string][]string{"t1": {"p1", "p2"}, "t2": {"p3"}}}},
		{"person side only", src{person: map[string][]string{"p1": {"t1"}, "p2": {"t1"}, "p3": {"t2"}}}},
		{"both sides", src{team: map[string][]string{"t1": {"p1", "p2"}, "t2": {"p3"}}, person: map[string][]string{"p1": {"t1"}, "p2": {"t1"}, "p3": {"t2"}}}},
		{"a mix", src{team: map[string][]string{"t1": {"p1"}}, person: map[string][]string{"p1": {"t1"}, "p2": {"t1"}, "p3": {"t2"}}}},
		{"a mix the other way", src{team: map[string][]string{"t1": {"p2"}, "t2": {"p3"}}, person: map[string][]string{"p1": {"t1"}, "p2": {"t1"}}}},
	}
	const wantTeams = "Design=[Ada, Bo]; Sales=[Cy]; Ops=[]"
	const wantPeople = "Ada=[Design]; Bo=[Design]; Cy=[Sales]; Di=[]"
	sorted := func(s string) string {
		if s == "" {
			return ""
		}
		parts := strings.Split(s, ", ")
		sort.Strings(parts)
		return strings.Join(parts, ", ")
	}
	rowsOf := func(keys []string, field string, l map[string][]string) []ImportRow {
		var out []ImportRow
		for _, k := range keys {
			r := ImportRow{Key: k, Cells: map[string]any{"fName": names[k]}}
			if to, ok := l[k]; ok {
				r.Links = map[string][]string{field: to} // the source's own slice
			}
			out = append(out, r)
		}
		return out
	}
	build := func(s src, teamsFirst bool) ImportBase {
		teams := ImportTable{Key: "tblT", Name: "Teams", Fields: []ImportField{
			{Key: "fName", Name: "Team", Type: model.FieldText},
			{Key: "fPeople", Name: "People", Type: model.FieldRelation, LinksTo: "tblP", Pair: "fTeam"},
			{Key: "fSize", Name: "Size", Type: model.FieldRollup, Through: "fPeople", Of: "fName", Aggregate: "count"},
		}}
		people := ImportTable{Key: "tblP", Name: "People", Fields: []ImportField{
			{Key: "fName", Name: "Name", Type: model.FieldText},
			{Key: "fTeam", Name: "Team", Type: model.FieldRelation, LinksTo: "tblT", Pair: "fPeople"},
		}}
		tr := rowsOf([]string{"t1", "t2", "t3"}, "fPeople", s.team)
		pr := rowsOf([]string{"p1", "p2", "p3", "p4"}, "fTeam", s.person)
		teams.Rows = func(page func([]ImportRow) error) error { return page(tr) }
		people.Rows = func(page func([]ImportRow) error) error { return page(pr) }
		if teamsFirst {
			return ImportBase{Tables: []ImportTable{teams, people}}
		}
		return ImportBase{Tables: []ImportTable{people, teams}}
	}
	read := func(made *ImportedBase) (string, string, string) {
		tt, err := model.GetTableByID(w.ctx, made.Tables["tblT"])
		if err != nil {
			t.Fatal(err)
		}
		pt, err := model.GetTableByID(w.ctx, made.Tables["tblP"])
		if err != nil {
			t.Fatal(err)
		}
		tc, pc := w.cells(tt, &w.me), w.cells(pt, &w.me)
		var ts, ps []string
		for _, k := range []string{"t1", "t2", "t3"} {
			ts = append(ts, fmt.Sprintf("%s=[%s]", names[k], sorted(labels(tc[made.Rows["tblT/"+k]][made.Fields["tblT/fPeople"].String()]))))
		}
		for _, k := range []string{"p1", "p2", "p3", "p4"} {
			ps = append(ps, fmt.Sprintf("%s=[%s]", names[k], sorted(labels(pc[made.Rows["tblP/"+k]][made.Fields["tblP/fTeam"].String()]))))
		}
		size := fmt.Sprintf("%v %v", tc[made.Rows["tblT/t1"]][made.Fields["tblT/fSize"].String()], tc[made.Rows["tblT/t2"]][made.Fields["tblT/fSize"].String()])
		return strings.Join(ts, "; "), strings.Join(ps, "; "), size
	}
	for _, c := range cases {
		for _, teamsFirst := range []bool{true, false} {
			label := fmt.Sprintf("%s, teams made first %v", c.name, teamsFirst)
			before := fmt.Sprint(c.s.team, c.s.person)
			var first string
			for run := 1; run <= 2; run++ {
				made, err := ImportBaseAs(w.ctx, w.me, build(c.s, teamsFirst))
				if err != nil || len(made.Notes) != 0 {
					t.Fatalf("%s run %d: %v %+v", label, run, err, made.Notes)
				}
				gt, gp, size := read(made)
				if gt != wantTeams {
					t.Errorf("%s run %d: teams %q, want %q", label, run, gt, wantTeams)
				}
				if gp != wantPeople {
					t.Errorf("%s run %d: people %q, want %q", label, run, gp, wantPeople)
				}
				if size != "2 1" {
					t.Errorf("%s run %d: sizes %q, want \"2 1\"", label, run, size)
				}
				if run == 1 {
					first = gt + "|" + gp + "|" + size
				} else if got := gt + "|" + gp + "|" + size; got != first {
					t.Errorf("%s: the second import reads %q, the first %q", label, got, first)
				}
			}
			if after := fmt.Sprint(c.s.team, c.s.person); after != before {
				t.Errorf("%s: the source's links were written to: %s, was %s", label, after, before)
			}
		}
	}
}

// A one-way relation (no pair, as a Notion-shaped relation that names one
// side only), and two one-way relations between the same tables, keep their
// meaning: nothing is mirrored across them.
func TestImportBaseKeepsOneWayLinksAsTheyAre(t *testing.T) {
	w := newRelationWorld(t)
	teams := ImportTable{Key: "tblT", Name: "Teams", Fields: []ImportField{
		{Key: "fName", Name: "Team", Type: model.FieldText},
		{Key: "fLead", Name: "Lead", Type: model.FieldRelation, LinksTo: "tblP"},
	}}
	people := ImportTable{Key: "tblP", Name: "People", Fields: []ImportField{
		{Key: "fName", Name: "Name", Type: model.FieldText},
		{Key: "fTeam", Name: "Team", Type: model.FieldRelation, LinksTo: "tblT"},
	}}
	teams.Rows = func(page func([]ImportRow) error) error {
		return page([]ImportRow{{Key: "t1", Cells: map[string]any{"fName": "Design"}, Links: map[string][]string{"fLead": {"p2"}}}})
	}
	people.Rows = func(page func([]ImportRow) error) error {
		return page([]ImportRow{
			{Key: "p1", Cells: map[string]any{"fName": "Ada"}, Links: map[string][]string{"fTeam": {"t1"}}},
			{Key: "p2", Cells: map[string]any{"fName": "Bo"}},
		})
	}
	made, err := ImportBaseAs(w.ctx, w.me, ImportBase{Tables: []ImportTable{teams, people}})
	if err != nil || len(made.Notes) != 0 {
		t.Fatalf("import: %v %+v", err, made.Notes)
	}
	tt, _ := model.GetTableByID(w.ctx, made.Tables["tblT"])
	pt, _ := model.GetTableByID(w.ctx, made.Tables["tblP"])
	tc, pc := w.cells(tt, &w.me), w.cells(pt, &w.me)
	if got := labels(tc[made.Rows["tblT/t1"]][made.Fields["tblT/fLead"].String()]); got != "Bo" {
		t.Errorf("Design's lead %q, want Bo", got)
	}
	if got := labels(pc[made.Rows["tblP/p1"]][made.Fields["tblP/fTeam"].String()]); got != "Design" {
		t.Errorf("Ada's team %q, want Design", got)
	}
	if got := labels(pc[made.Rows["tblP/p2"]][made.Fields["tblP/fTeam"].String()]); got != "" {
		t.Errorf("Bo's team %q, want none (Bo leads Design through another relation)", got)
	}
	tf, _ := model.ListFields(w.ctx, tt.Id)
	pf, _ := model.ListFields(w.ctx, pt.Id)
	if len(tf) != 2 || len(pf) != 2 {
		t.Errorf("one-way relations made other fields: teams %d, people %d", len(tf), len(pf))
	}
}
