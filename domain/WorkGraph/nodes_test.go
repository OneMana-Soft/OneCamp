package workGraphDomain

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/models/dgexec"
)

const (
	evA = "44444444-4444-4444-8444-444444444444"
	evB = "55555555-5555-4555-8555-555555555555"
	pA  = "66666666-6666-4666-8666-666666666666"
	pB  = "77777777-7777-4777-8777-777777777777"
	prA = "88888888-8888-4888-8888-888888888888"
)

var at = time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)

// meeting is a projection of a meeting organised by pA, with pB, on prA's
// calendar, whose notes link dA.
func meeting() *Projection {
	return &Projection{Changed: at, Edges: map[string][]Ref{
		"cal_event_organiser": {{KindPerson, pA}},
		"cal_event_with":      {{KindPerson, pB}},
		"cal_event_on":        {{KindProject, prA}},
		RefersTo:              {{KindDoc, dA}},
	}}
}

// asStored is p as the graph holds it once written, its edges' targets at
// made-up uids.
func asStored(p *Projection) *Node {
	n := &Node{UID: "0x10", UUID: evA, Deleted: p.Deleted, Changed: p.Changed, Have: map[string][]Target{}}
	uid := 0x20
	for pred, refs := range p.Edges {
		for _, r := range refs {
			n.Have[pred] = append(n.Have[pred], Target{UID: fmt.Sprintf("0x%x", uid), Ref: r})
			uid++
		}
	}
	return n
}

// TestANodeIsMadeKeptAndDroppedAsItsProjectionSays: a new projection makes
// the node with every edge; the node as it should be changes nothing; an
// edge no longer wanted, a second edge to one target and an edge to no kind
// are taken off; a thing with no projection loses its node; and a change of
// times alone is still a change.
func TestANodeIsMadeKeptAndDroppedAsItsProjectionSays(t *testing.T) {
	f, changed, err := NodeDiff(KindEvent, strings.ToUpper(evA), nil, meeting())
	if err != nil || !changed || f.Drop || f.UUID != evA || len(f.Edges) != 4 || !f.Changed.Equal(at) {
		t.Fatalf("a new meeting: %+v changed %v err %v", f, changed, err)
	}
	if f.Edges[0].Pred != "cal_event_organiser" || f.Edges[3].Pred != RefersTo {
		t.Errorf("edges out of the allow-list's order: %+v", f.Edges)
	}

	stored := asStored(meeting())
	if f, changed, err := NodeDiff(KindEvent, evA, stored, meeting()); err != nil || changed || len(f.Edges) != 0 {
		t.Errorf("a node as it should be changed: %+v %v %v", f, changed, err)
	}

	stored.Have["cal_event_with"] = append(stored.Have["cal_event_with"], Target{UID: "0x90", Ref: Ref{KindPerson, pB}}, Target{UID: "0x91"}, Target{UID: "0x92", Ref: Ref{KindPerson, pA}})
	f, changed, err = NodeDiff(KindEvent, evA, stored, meeting())
	if err != nil || !changed || len(f.Edges) != 1 || f.Edges[0].Pred != "cal_event_with" || strings.Join(f.Edges[0].Del, ",") != "0x90,0x91,0x92" || len(f.Edges[0].Add) != 0 {
		t.Errorf("extra edges: %+v changed %v err %v", f, changed, err)
	}

	if f, changed, _ := NodeDiff(KindEvent, evA, asStored(meeting()), nil); !changed || !f.Drop {
		t.Errorf("a thing with no projection kept its node: %+v", f)
	}
	if _, changed, _ := NodeDiff(KindEvent, evA, nil, nil); changed {
		t.Error("nothing and no projection changed something")
	}

	gone := meeting()
	gone.Deleted = at.Add(time.Hour)
	f, changed, _ = NodeDiff(KindEvent, evA, asStored(meeting()), gone)
	if !changed || len(f.Edges) != 0 || !f.Deleted.Equal(gone.Deleted) {
		t.Errorf("a meeting deleted: %+v changed %v", f, changed)
	}

	for _, pred := range []string{"~cal_event_on", "task_project", "nonsense"} {
		bad := meeting()
		bad.Edges[pred] = []Ref{{KindProject, prA}}
		if _, _, err := NodeDiff(KindEvent, evA, nil, bad); err == nil {
			t.Errorf("a projection naming %s was taken", pred)
		}
	}
}

// TestANodeIsWrittenByUUIDsAlone: one request finds each node by its uuid
// (made when it isn't there), sets its type and times, adds each edge to a
// target found by its uuid, takes edges off by the uids the node's own edges
// had, and deletes a dropped node with its edges. Nothing else names a node.
func TestANodeIsWrittenByUUIDsAlone(t *testing.T) {
	f, _, err := NodeDiff(KindEvent, evA, nil, meeting())
	if err != nil {
		t.Fatal(err)
	}
	f.Edges = append(f.Edges, EdgeFix{Pred: "cal_event_with", Del: []string{"0x91"}})
	q, set, del, err := NodeRequest([]NodeFix{f, {Kind: KindEvent, UUID: evB, Drop: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`n0 as var(func: eq(cal_event_uuid, "` + evA + `"))`,
		`a0_0_person as var(func: eq(user_uuid, ["` + pA + `"]))`,
		`a0_2_project as var(func: eq(project_uuid, ["` + prA + `"]))`,
		`a0_3_doc as var(func: eq(doc_uuid, ["` + dA + `"]))`,
		`n1 as var(func: eq(cal_event_uuid, "` + evB + `"))`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %s:\n%s", want, q)
		}
	}
	for _, want := range []string{
		`uid(n0) <cal_event_uuid> "` + evA + `" .`,
		`uid(n0) <dgraph.type> "CalEvent" .`,
		`uid(n0) <cal_event_deleted_at> "0001-01-01T00:00:00Z" .`,
		`uid(n0) <cal_event_updated_at> "2026-10-22T09:30:00Z" .`,
		`uid(n0) <cal_event_organiser> uid(a0_0_person) .`,
		`uid(n0) <cal_event_on> uid(a0_2_project) .`,
		`uid(n0) <refers_to> uid(a0_3_doc) .`,
	} {
		if !strings.Contains(set, want) {
			t.Errorf("set lacks %s:\n%s", want, set)
		}
	}
	if del != "uid(n0) <cal_event_with> <0x91> .\nuid(n1) * * .\n" {
		t.Errorf("del %q", del)
	}
	if regexp.MustCompile(`<0x[0-9a-f]+> <`).MatchString(set) {
		t.Errorf("a node named by a uid in set:\n%s", set)
	}

	for name, bad := range map[string]NodeFix{
		"a kind the graph owns":                 {Kind: KindDoc, UUID: evA},
		"no uuid":                               {Kind: KindEvent, UUID: `x") } delete { * * * .`},
		"a removed edge's uid":                  {Kind: KindEvent, UUID: evA, Edges: []EdgeFix{{Pred: "cal_event_with", Del: []string{"uid(x)"}}}},
		"a target of no kind the edge leads to": {Kind: KindEvent, UUID: evA, Edges: []EdgeFix{{Pred: "cal_event_with", Add: []Ref{{KindDoc, dA}}}}},
		"a reverse edge":                        {Kind: KindEvent, UUID: evA, Edges: []EdgeFix{{Pred: "~cal_event_on", Add: []Ref{{KindProject, prA}}}}},
	} {
		if _, _, _, err := NodeRequest([]NodeFix{bad}); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
}

// TestNodesAreWrittenInOneRequestOrNone: a page of nodes costs one request,
// and nothing to fix costs none.
func TestNodesAreWrittenInOneRequestOrNone(t *testing.T) {
	prev := mutate
	defer func() { mutate = prev }()
	n := 0
	mutate = func(context.Context, string, dgexec.Mutation) error { n++; return nil }
	if err := WriteNodes(context.Background(), nil); err != nil || n != 0 {
		t.Fatalf("no fixes sent %d requests (%v)", n, err)
	}
	var fixes []NodeFix
	for i := 0; i < 100; i++ {
		f, _, _ := NodeDiff(KindEvent, evA, nil, meeting())
		fixes = append(fixes, f)
	}
	if err := WriteNodes(context.Background(), fixes); err != nil || n != 1 {
		t.Fatalf("100 nodes sent %d requests (%v)", n, err)
	}
}

// TestANodeIsReadWithItsOwnEdges: one query by uuid reads each node's times
// and every edge its kind holds, each target named by its kind's uuid; an
// edge to something that is no such kind (a deleted user's node, a stray
// write) is kept with no kind, so the next write takes it off.
func TestANodeIsReadWithItsOwnEdges(t *testing.T) {
	prev := read
	defer func() { read = prev }()
	var asked string
	read = func(_ context.Context, q string, _ map[string]string) (map[string]json.RawMessage, error) {
		asked = q
		return map[string]json.RawMessage{"n": json.RawMessage(`[{"uid":"0x10","uuid":"` + strings.ToUpper(evA) + `",
			"deleted":"0001-01-01T00:00:00Z","changed":"2026-10-22T09:30:00Z",
			"cal_event_organiser":[{"uid":"0x20","user_uuid":"` + pA + `"}],
			"cal_event_with":[{"uid":"0x21","user_uuid":"` + pB + `"},{"uid":"0x22"}],
			"refers_to":[{"uid":"0x23","doc_uuid":"` + dA + `"}]}]`)}, nil
	}
	got, err := ReadNodes(context.Background(), KindEvent, []string{evA, "not a uuid"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`eq(cal_event_uuid, ["` + evA + `"])`, "cal_event_with { uid user_uuid }", "cal_event_on { uid project_uuid ch_uuid team_uuid }", "refers_to { uid doc_uuid task_uuid project_uuid }"} {
		if !strings.Contains(asked, want) {
			t.Errorf("the read lacks %s:\n%s", want, asked)
		}
	}
	n := got[evA]
	if n == nil || n.UID != "0x10" || !n.Deleted.IsZero() || !n.Changed.Equal(at) {
		t.Fatalf("node %+v", n)
	}
	if w := n.Have["cal_event_with"]; len(w) != 2 || w[0].Ref != (Ref{KindPerson, pB}) || w[1].Ref.Kind != "" || w[1].UID != "0x22" {
		t.Errorf("with %+v", w)
	}
	if _, changed, _ := NodeDiff(KindEvent, evA, n, &Projection{Changed: at, Edges: map[string][]Ref{
		"cal_event_organiser": {{KindPerson, pA}}, "cal_event_with": {{KindPerson, pB}}, RefersTo: {{KindDoc, dA}},
	}}); !changed {
		t.Error("the stray edge wasn't taken off")
	}
	if _, err := ReadNodes(context.Background(), KindDoc, []string{evA}); err == nil {
		t.Error("a kind the graph owns was read as a projection")
	}
}

// TestAProjectedKindsTypeHoldsEverythingItWrites: a projected kind's uuid
// takes exact alone as an @upsert predicate must, and its Dgraph type lists
// its uuid, its times and every edge it writes, so a dropped node goes whole
// ("* *" deletes what its type lists).
func TestAProjectedKindsTypeHoldsEverythingItWrites(t *testing.T) {
	schema := schemaText(t)
	for _, k := range ProjectedKinds() {
		s := specs[k]
		if d := declaration(schema, s.UUID); !strings.Contains(d, "@index(exact)") || !strings.Contains(d, "@upsert") || strings.Contains(d, "term") || strings.Contains(d, "trigram") {
			t.Errorf("%s: %s is %q", k, s.UUID, d)
		}
		m := regexp.MustCompile(`(?s)type ` + projectedTypes[k] + ` \{(.*?)\}`).FindStringSubmatch(schema)
		if m == nil {
			t.Errorf("%s: no type %s in the schema", k, projectedTypes[k])
			continue
		}
		fields := strings.Fields(m[1])
		want := []string{s.UUID, s.Deleted, changedPred(k)}
		for _, e := range nodeEdges(k) {
			want = append(want, e.Pred)
		}
		for _, p := range want {
			if !containsString(fields, p) {
				t.Errorf("type %s lacks %s", projectedTypes[k], p)
			}
		}
	}
}
