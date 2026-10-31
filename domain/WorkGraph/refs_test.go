package workGraphDomain

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/models/dgexec"
)

const (
	dA = "11111111-1111-4111-8111-111111111111"
	dB = "22222222-2222-4222-8222-222222222222"
	tA = "33333333-3333-4333-8333-333333333333"
)

// TestRefersToIsAPlainReverseList: the edge is a uid list with @reverse and
// nothing else (no @upsert, no index: neither adds anything a list edge
// needs, and either adds conflict keys to every save), and each source type
// declares it.
func TestRefersToIsAPlainReverseList(t *testing.T) {
	schema := schemaText(t)
	if decl := strings.TrimSpace(declaration(schema, RefersTo)); decl != "[uid] @reverse" {
		t.Errorf("refers_to is declared %q, want %q", decl, "[uid] @reverse")
	}
	for _, typ := range []string{"Post", "Chat", "Comment", "Doc"} {
		m := regexp.MustCompile(`(?s)type ` + typ + ` \{(.*?)\}`).FindStringSubmatch(schema)
		if m == nil || !regexp.MustCompile(`(?m)^\s*refers_to\s*$`).MatchString(m[1]) {
			t.Errorf("type %s doesn't carry refers_to", typ)
		}
	}
}

// TestEdgesOfOnePredicateLeadToTheSameKinds: a two-hop read follows a
// predicate once from a node that may be several kinds (edgesFrom), with the
// first edge's targets; an edge of the same predicate with other targets
// would silently lose them.
func TestEdgesOfOnePredicateLeadToTheSameKinds(t *testing.T) {
	to := map[string][]Kind{}
	for _, e := range Edges {
		if prev, ok := to[e.Pred]; ok && !reflect.DeepEqual(prev, e.To) {
			t.Errorf("%s leads to %v from one kind and %v from another", e.Pred, prev, e.To)
		}
		to[e.Pred] = e.To
	}
}

// TestReferencesAreWalkedBothWays: every source kind links out, every target
// kind is linked to, and a two-hop read from a doc reaches the docs the
// messages linking it link to.
func TestReferencesAreWalkedBothWays(t *testing.T) {
	for _, k := range RefSources {
		if _, ok := EdgeFrom(k, RefersTo); !ok {
			t.Errorf("%s: no refers_to edge out", k)
		}
	}
	for _, k := range RefTargets {
		if _, ok := EdgeFrom(k, "~"+RefersTo); !ok {
			t.Errorf("%s: no ~refers_to edge in", k)
		}
	}
	q, err := NeighboursQuery(KindDoc, 2, DefaultKinds)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?s)~refers_to @filter.*?\{.*?\brefers_to @filter`).MatchString(q) {
		t.Errorf("a doc's two-hop read doesn't go from a message linking it to what that links:\n%s", q)
	}
}

func TestDiffAddsWhatIsMissingAndTakesOffTheRest(t *testing.T) {
	s := Source{UID: "0x1", Have: []Target{
		{UID: "0xa", Ref: Ref{KindDoc, dA}}, // kept
		{UID: "0xb", Ref: Ref{KindDoc, dB}}, // no longer linked
		{UID: "0xc", Ref: Ref{KindDoc, dA}}, // a second edge to the same doc
		{UID: "0xd"},                        // to no target at all
	}}
	f := Diff(s, []Ref{{KindDoc, strings.ToUpper(dA)}, {KindTask, tA}})
	if !reflect.DeepEqual(f.Del, []string{"0xb", "0xc", "0xd"}) || !reflect.DeepEqual(f.Add, []Ref{{KindTask, tA}}) {
		t.Errorf("fix %+v", f)
	}
	if !Diff(Source{UID: "0x1", Have: []Target{{UID: "0xa", Ref: Ref{KindDoc, dA}}}}, []Ref{{KindDoc, dA}}).Empty() {
		t.Error("a source with exactly its links got a fix")
	}
}

// TestFixRequestNamesTargetsByUUIDAndChecksEveryUID: one request for every
// fix; an added target is found by its uuid in the request's own query, a
// removed edge names only uids read off the source's own edges, and anything
// that isn't a uid is refused before a request is made.
func TestFixRequestNamesTargetsByUUIDAndChecksEveryUID(t *testing.T) {
	q, set, del, err := FixRequest([]Fix{
		{Source: "0x1", Del: []string{"0xb"}, Add: []Ref{{KindDoc, dA}, {KindTask, tA}}},
		{Source: "0x2"},
		{Source: "0x3", Add: []Ref{{KindDoc, dB}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`a0_doc as var(func: eq(doc_uuid, ["` + dA + `"]))`, `a0_task as var(func: eq(task_uuid, ["` + tA + `"]))`, `a2_doc as var(func: eq(doc_uuid, ["` + dB + `"]))`} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %s:\n%s", want, q)
		}
	}
	if set != "<0x1> <refers_to> uid(a0_doc) .\n<0x1> <refers_to> uid(a0_task) .\n<0x3> <refers_to> uid(a2_doc) .\n" || del != "<0x1> <refers_to> <0xb> .\n" {
		t.Errorf("set %q del %q", set, del)
	}
	for _, bad := range []Fix{{Source: "_:x", Add: []Ref{{KindDoc, dA}}}, {Source: "0x1", Del: []string{"uid(x)"}}} {
		if _, _, _, err := FixRequest([]Fix{bad}); err == nil {
			t.Errorf("%+v was taken", bad)
		}
	}
	if _, _, _, err := FixRequest([]Fix{{Source: "0x1", Add: []Ref{{KindDoc, `x") } delete { * * * .`}}}}); err != nil {
		t.Fatal(err)
	} // a non-uuid is dropped by UUIDList, never written into the query
}

func TestWriteFixesSendsOneRequestOrNone(t *testing.T) {
	prev := mutate
	defer func() { mutate = prev }()
	n := 0
	mutate = func(context.Context, string, dgexec.Mutation) error { n++; return nil }
	if err := WriteFixes(context.Background(), []Fix{{Source: "0x1"}}); err != nil || n != 0 {
		t.Fatalf("an empty fix sent %d requests (%v)", n, err)
	}
	var fixes []Fix
	for i := 0; i < 50; i++ {
		fixes = append(fixes, Fix{Source: "0x1", Add: []Ref{{KindDoc, dA}}, Del: []string{"0x9"}})
	}
	if err := WriteFixes(context.Background(), fixes); err != nil || n != 1 {
		t.Fatalf("50 fixes sent %d requests (%v)", n, err)
	}
}

func TestReadSourcesQueryPagesByUID(t *testing.T) {
	q := ReadSourcesQuery(KindMessage, "0x2a", 500)
	for _, want := range []string{"has(post_uuid), first: 500, after: 0x2a", "body: post_text", "refers_to { uid doc_uuid task_uuid project_uuid }"} {
		if !strings.Contains(q, want) {
			t.Errorf("%q not in\n%s", want, q)
		}
	}
	if _, err := ReadSources(context.Background(), KindMessage, "x) { uid }", 1); err == nil {
		t.Error("a page after something that isn't a uid was read")
	}
	if _, err := ReadSource(context.Background(), KindProject, dA); err == nil {
		t.Error("a project, which has no body, was read as a source")
	}
}

// TestATaskDescriptionIsReadForLinks (DG15a): a doc chip in a task's
// description shows on the doc as "linked from" the task, as a message's does,
// and the task's panel lists what its description links to.
func TestATaskDescriptionIsReadForLinks(t *testing.T) {
	if !IsRefSource(KindTask) {
		t.Fatal("a task's description isn't read for links")
	}
	if bodyOf[KindTask] != "task_description" {
		t.Errorf("a task's body is read from %q, want task_description", bodyOf[KindTask])
	}
	if e, ok := EdgeFrom(KindTask, RefersTo); !ok || e.Rel != "links_to" {
		t.Errorf("a task has no links_to edge out over refers_to (%+v, %v)", e, ok)
	}
	// A task linked from another task's description is walked back to it.
	if e, ok := EdgeFrom(KindTask, "~"+RefersTo); !ok || !slices.Contains(e.To, KindTask) {
		t.Errorf("a task's linked_from doesn't reach the tasks whose descriptions link it (%+v, %v)", e, ok)
	}
}

// The reconciler reads pages' bodies only (docs F1): a folder, a file or a
// link is a doc node with no body to read links from. Every doc source read
// (a page of them, one, or several by id) takes pages only, through
// domain/Doc's filter; the other kinds read every node as before.
func TestADocSourceReadTakesPagesOnly(t *testing.T) {
	var asked []string
	prev := readSources
	t.Cleanup(func() { readSources = prev })
	readSources = func(_ context.Context, q string, _ map[string]string) ([]sourceNode, error) {
		asked = append(asked, q)
		return nil, nil
	}
	if _, err := ReadSources(context.Background(), KindDoc, "", 50); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSource(context.Background(), KindDoc, dA); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSourcesByUUID(context.Background(), KindDoc, []string{dA, dB}); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 3 {
		t.Fatalf("asked %d reads", len(asked))
	}
	for _, q := range asked {
		if !strings.Contains(q, ") @filter("+docDomain.PageOnly+") {") {
			t.Errorf("a doc source read takes more than pages:\n%s", q)
		}
	}
	if q := ReadSourcesQuery(KindMessage, "", 500); strings.Contains(q, "@filter") {
		t.Errorf("a message source read is filtered:\n%s", q)
	}
}
