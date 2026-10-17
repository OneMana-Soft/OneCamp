//go:build integration
// +build integration

package integration_test

// The doc tree's reads and writes, asked of a real graph with the real schema:
// a page's path, what's under it, its level, a move (and a move that would
// close a loop, alone and racing another), the sidebar's top levels for each
// section, the lists with their counts, and the trash: one delete for a
// subtree, restored together, and gone for good.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDocTreeGraph -v

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/business/Tree/graph"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

var docTreeSchema = graph.Schema{
	Kind: "doc", ID: "doc_uuid", Parent: "doc_parent", Position: "doc_position", Created: "doc_created_at",
	SpaceKind: "doc_space_kind", SpaceUUID: "doc_space_uuid", Live: domain.LiveDocFilter,
}

func titlesOf(docs []*dgraphStruct.DgraphDoc) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Title)
	}
	sort.Strings(out)
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDocTreeGraph(t *testing.T) {
	ctx := context.Background()
	dg := integration.SetupDgraph(t)

	live := "0001-01-01T00:00:00Z"
	made := func(min int) string { return time.Date(2026, 10, 1, 9, min, 0, 0, time.UTC).Format(time.RFC3339) }
	id := map[string]string{}
	for _, n := range []string{"eng", "rfcs", "rfc1", "rfc2", "notes", "plans", "q4", "x", "y", "lone", "gone"} {
		id[n] = uuid.NewString()
	}
	doc := func(key, title string, private bool, extra map[string]any) map[string]any {
		m := map[string]any{"uid": "_:" + key, "dgraph.type": "Doc", "doc_uuid": id[key], "doc_title": title,
			"doc_private": private, "doc_deleted_at": live, "doc_created_at": made(len(key)), "doc_updated_at": made(len(key)),
			"doc_created_by": map[string]any{"uid": "_:owner"}, "doc_owner": map[string]any{"uid": "_:owner"}}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	nodes := []map[string]any{
		{"uid": "_:owner", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "owner"},
		{"uid": "_:friend", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "friend"},
		{"uid": "_:stranger", "dgraph.type": "User", "user_uuid": uuid.NewString(), "user_name": "stranger"},
		// Workspace: Engineering > RFCs > {RFC 1, RFC 2}; Lone at the top.
		doc("eng", "Engineering", false, map[string]any{"doc_position": "a0"}),
		doc("rfcs", "RFCs", false, map[string]any{"doc_parent": map[string]any{"uid": "_:eng"}, "doc_position": "a0"}),
		doc("rfc1", "RFC 1", false, map[string]any{"doc_parent": map[string]any{"uid": "_:rfcs"}, "doc_position": "a1"}),
		doc("rfc2", "RFC 2", false, map[string]any{"doc_parent": map[string]any{"uid": "_:rfcs"}, "doc_position": "a0"}),
		doc("lone", "Lone", false, map[string]any{"doc_position": "a1"}),
		// The owner's private Notes; Plans (private) > Q4, shared with friend
		// alone (Plans isn't): Q4 is at the top of friend's Shared.
		doc("notes", "Notes", true, nil),
		doc("plans", "Plans", true, map[string]any{"doc_position": "a2"}),
		doc("q4", "Q4", true, map[string]any{"doc_parent": map[string]any{"uid": "_:plans"}, "doc_reading_users": []map[string]any{{"uid": "_:friend"}}}),
		doc("x", "X", false, map[string]any{"doc_position": "a3"}),
		doc("y", "Y", false, map[string]any{"doc_position": "a4"}),
		// In the trash already, on its own (before batches existed).
		doc("gone", "Gone", false, map[string]any{"doc_deleted_at": made(30)}),
	}
	uids := dg.Mutate(t, nodes)
	owner, friend, stranger := uids["owner"], uids["friend"], uids["stranger"]

	t.Run("chain, subtree and siblings", func(t *testing.T) {
		chain, truncated, err := graph.Chain(ctx, docTreeSchema, id["rfc1"])
		if err != nil || truncated || len(chain) != 3 || chain[0].UUID != id["rfc1"] || chain[1].UUID != id["rfcs"] || chain[2].UUID != id["eng"] {
			t.Fatalf("chain of RFC 1 = %+v %v %v", chain, truncated, err)
		}
		under, height, err := graph.Subtree(ctx, docTreeSchema, id["eng"], 100)
		if err != nil || height != 2 || len(under) != 3 || under[0].UUID != id["rfcs"] || under[0].ParentUUID != id["eng"] {
			t.Fatalf("subtree of Engineering = %+v height %d %v", under, height, err)
		}
		sibs, err := graph.Siblings(ctx, docTreeSchema, id["rfcs"], 100)
		if err != nil || len(sibs) != 2 || sibs[0].ID != id["rfc2"] || sibs[1].ID != id["rfc1"] {
			t.Fatalf("RFCs' children in order = %+v %v", sibs, err)
		}
		if _, _, err := graph.Chain(ctx, docTreeSchema, id["gone"]); !errors.Is(err, graph.ErrNotFound) {
			t.Fatalf("a deleted doc has a chain: %v", err)
		}
	})

	t.Run("a move lands, and one that would close a loop is refused", func(t *testing.T) {
		if err := graph.Apply(ctx, docTreeSchema, graph.Move{UUID: id["eng"], Parent: id["rfc1"], Key: "a0"}, time.Now()); !errors.Is(err, graph.ErrCycle) {
			t.Fatalf("Engineering under its own grandchild: %v, want ErrCycle", err)
		}
		if err := graph.Apply(ctx, docTreeSchema, graph.Move{UUID: id["eng"], Parent: id["eng"], Key: "a0"}, time.Now()); !errors.Is(err, graph.ErrCycle) {
			t.Fatalf("Engineering under itself: %v, want ErrCycle", err)
		}
		if err := graph.Apply(ctx, docTreeSchema, graph.Move{UUID: id["lone"], Parent: id["rfcs"], Key: "a0V",
			Set: map[string]string{"doc_space_kind": "project", "doc_space_uuid": "p1"}}, time.Now()); err != nil {
			t.Fatalf("Lone under RFCs: %v", err)
		}
		sibs, _ := graph.Siblings(ctx, docTreeSchema, id["rfcs"], 100)
		if len(sibs) != 3 || sibs[1].ID != id["lone"] {
			t.Fatalf("RFCs' children after the move = %+v", sibs)
		}
		if err := graph.Apply(ctx, docTreeSchema, graph.Move{UUID: id["lone"], Key: "a1", Renumber: map[string]string{id["x"]: "a5"}}, time.Now()); err != nil {
			t.Fatalf("Lone back to the top: %v", err)
		}
		if sibs, _ := graph.Siblings(ctx, docTreeSchema, id["rfcs"], 100); len(sibs) != 2 {
			t.Fatalf("Lone still under RFCs: %+v", sibs)
		}
		got := dg.Query(t, `{ q(func: eq(doc_uuid, "`+id["x"]+`")) { doc_position } s(func: eq(doc_uuid, "`+id["lone"]+`")) { doc_space_kind doc_parent { uid } } }`)
		if !jsonHas(got, `"doc_position":"a5"`) || !jsonHas(got, `"doc_space_kind":"project"`) || jsonHas(got, `"doc_parent"`) {
			t.Fatalf("renumbered sibling and moved doc read back as %s", got)
		}
		if err := graph.Apply(ctx, docTreeSchema, graph.Move{UUID: id["gone"], Parent: id["eng"], Key: "a9"}, time.Now()); !errors.Is(err, graph.ErrNotFound) {
			t.Fatalf("moving a deleted doc: %v, want ErrNotFound", err)
		}
	})

	t.Run("two moves racing to close a loop: one of them fails", func(t *testing.T) {
		// Without the lock: the stamp both write is what makes them collide.
		for round := 0; round < 6; round++ {
			a, b := uuid.NewString(), uuid.NewString()
			dg.Mutate(t, []map[string]any{doc("x", "A", false, map[string]any{"uid": "_:a", "doc_uuid": a}), doc("y", "B", false, map[string]any{"uid": "_:b", "doc_uuid": b})})
			var wg sync.WaitGroup
			errs := make([]error, 2)
			start := make(chan struct{})
			for i, m := range []graph.Move{{UUID: a, Parent: b, Key: "a0"}, {UUID: b, Parent: a, Key: "a0"}} {
				wg.Add(1)
				go func(i int, m graph.Move) {
					defer wg.Done()
					<-start
					errs[i] = graph.Apply(ctx, docTreeSchema, m, time.Now())
				}(i, m)
			}
			close(start)
			wg.Wait()
			ok := 0
			for _, err := range errs {
				if err == nil {
					ok++
				} else if !errors.Is(err, graph.ErrCycle) {
					t.Fatalf("round %d: %v", round, err)
				}
			}
			if ok != 1 {
				t.Fatalf("round %d: %d of the two moves landed (%v); a loop formed or both failed", round, ok, errs)
			}
		}
	})

	t.Run("a level, and the sidebar's top level for each person and section", func(t *testing.T) {
		parent, err := domain.GetDocWithChildren(ctx, id["rfcs"], owner, 100)
		if err != nil || len(parent.Children) != 2 || parent.ChildCount != 2 {
			t.Fatalf("RFCs with its children = %+v %v", parent, err)
		}
		eng, _ := domain.GetDocWithChildren(ctx, id["eng"], owner, 100)
		if eng == nil || len(eng.Children) != 1 || eng.Children[0].ChildCount != 2 {
			t.Fatalf("Engineering's RFCs should count 2 children: %+v", eng)
		}
		for _, c := range []struct {
			who, section, kind, space string
			want                      []string
		}{
			// Lone moved into project p1's space above: it's at the top there.
			{owner, domain.SectionWorkspace, "", "", []string{"Engineering", "X", "Y"}},
			{owner, domain.SectionSpace, "project", "p1", []string{"Lone"}},
			{owner, domain.SectionPrivate, "", "", []string{"Notes", "Plans"}},
			{friend, domain.SectionShared, "", "", []string{"Q4"}},
			{friend, domain.SectionPrivate, "", "", nil},
			{stranger, domain.SectionShared, "", "", nil},
		} {
			top, err := domain.GetDocTreeTop(ctx, c.section, c.who, c.kind, c.space, 100)
			if err != nil {
				t.Fatalf("%s's %s: %v", c.who, c.section, err)
			}
			got := titlesOf(top)
			// The race above leaves pairs of A and B at the top of the workspace.
			if c.section == domain.SectionWorkspace {
				var kept []string
				for _, g := range got {
					if g != "A" && g != "B" {
						kept = append(kept, g)
					}
				}
				got = kept
			}
			if !sameStrings(got, c.want) {
				t.Errorf("%s section %s = %v, want %v", c.who, c.section, got, c.want)
			}
		}
	})

	t.Run("lists and their counts", func(t *testing.T) {
		for _, c := range []struct {
			scope, who string
			want       []string
		}{
			{domain.ScopeMine, owner, nil}, // checked by count below: every live doc is the owner's
			{domain.ScopeShared, friend, []string{"Q4"}},
			{domain.ScopeShared, owner, []string{}},
			{domain.ScopeShared, stranger, []string{}},
		} {
			list, err := domain.ListDocs(ctx, domain.DocListQuery{Scope: c.scope, ViewerUID: c.who, Sort: domain.SortEdited, First: 100})
			if err != nil {
				t.Fatalf("%s for %s: %v", c.scope, c.who, err)
			}
			if c.want != nil && !sameStrings(titlesOf(list.Docs), c.want) {
				t.Errorf("%s for %s = %v, want %v", c.scope, c.who, titlesOf(list.Docs), c.want)
			}
			if int(list.Count) != len(list.Docs) {
				t.Errorf("%s for %s counts %d with %d docs listed", c.scope, c.who, list.Count, len(list.Docs))
			}
		}
		ws, err := domain.ListDocs(ctx, domain.DocListQuery{Scope: domain.ScopeWorkspace, ViewerUID: friend, Sort: domain.SortTitle, First: 2})
		if err != nil || len(ws.Docs) != 2 || ws.Count < 7 || ws.Docs[0].Title > ws.Docs[1].Title {
			t.Fatalf("workspace, two at a time by title: %+v %v", ws, err)
		}
		for _, d := range ws.Docs {
			if d.Title == "Notes" || d.Title == "Q4" || d.Title == "Gone" {
				t.Fatalf("workspace lists %q", d.Title)
			}
		}
		rows, err := domain.GetDocRowsByUUID(ctx, []string{id["rfc1"], id["gone"], "not-a-uuid"}, friend)
		if err != nil || len(rows) != 2 {
			t.Fatalf("rows by uuid = %d %v", len(rows), err)
		}
		for _, r := range rows {
			if r.Uuid == id["rfc1"] && (r.Parent == nil || r.Parent.Title != "RFCs" || r.Parent.Parent == nil || r.Parent.Parent.Title != "Engineering") {
				t.Fatalf("RFC 1's path = %+v", r.Parent)
			}
		}
	})

	t.Run("a subtree goes to the trash as one delete, and comes back as one", func(t *testing.T) {
		under, _, err := graph.Subtree(ctx, docTreeSchema, id["rfcs"], 100)
		if err != nil || len(under) != 2 {
			t.Fatalf("under RFCs: %+v %v", under, err)
		}
		uidsUnder := []string{under[0].UID, under[1].UID}
		batch := uuid.NewString()
		at := time.Now()
		if err := domain.TrashDocs(ctx, id["rfcs"], uidsUnder, batch, owner, at); err != nil {
			t.Fatalf("trash RFCs: %v", err)
		}
		if err := domain.TrashDocs(ctx, id["rfcs"], nil, uuid.NewString(), owner, at); !errors.Is(err, domain.ErrDocDeleted) {
			t.Fatalf("trashing it twice: %v, want ErrDocDeleted", err)
		}
		trash, err := domain.GetDocTrash(ctx, owner, at.Add(-time.Hour), 100)
		if err != nil {
			t.Fatal(err)
		}
		inBatch := 0
		for _, d := range trash {
			if d.DeletedBatch == batch {
				inBatch++
				if d.DeletedBy == nil || d.DeletedBy.Uid != owner {
					t.Errorf("%s went without saying who deleted it", d.Title)
				}
			}
		}
		if inBatch != 3 {
			t.Fatalf("the trash holds %d of the 3 deleted together: %v", inBatch, titlesOf(trash))
		}
		if eng, _ := domain.GetDocWithChildren(ctx, id["eng"], owner, 100); len(eng.Children) != 0 {
			t.Fatalf("Engineering still lists a deleted child")
		}
		got, err := domain.GetDocBatch(ctx, batch, owner)
		if err != nil || len(got) != 3 {
			t.Fatalf("the batch = %v %v", titlesOf(got), err)
		}
		var back []string
		for _, d := range got {
			back = append(back, d.Uid)
		}
		restored, err := domain.RestoreDocs(ctx, back, nil, time.Now())
		if err != nil || len(restored) != 3 {
			t.Fatalf("restored %v %v", restored, err)
		}
		if eng, _ := domain.GetDocWithChildren(ctx, id["eng"], owner, 100); len(eng.Children) != 1 || eng.Children[0].ChildCount != 2 {
			t.Fatalf("RFCs didn't come back whole under Engineering: %+v", eng)
		}
		raw := dg.Query(t, `{ q(func: eq(doc_uuid, "`+id["rfc1"]+`")) { doc_deleted_batch doc_deleted_by { uid } doc_deleted_at } }`)
		if jsonHas(raw, "doc_deleted_batch") || jsonHas(raw, "doc_deleted_by") {
			t.Fatalf("a restored doc kept its trash marks: %s", raw)
		}
	})

	t.Run("archived subtrees, the archive's links, and what the trash's retention takes", func(t *testing.T) {
		old := time.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
		a, b, c := uuid.NewString(), uuid.NewString(), uuid.NewString()
		got := dg.Mutate(t, []map[string]any{
			{"uid": "_:a", "dgraph.type": "Doc", "doc_uuid": a, "doc_title": "Old", "doc_private": false, "doc_deleted_at": old, "doc_archived_at": old, "doc_created_by": map[string]any{"uid": owner}},
			{"uid": "_:b", "dgraph.type": "Doc", "doc_uuid": b, "doc_title": "Old child", "doc_private": false, "doc_deleted_at": old, "doc_archived_at": old, "doc_parent": map[string]any{"uid": "_:a"}, "doc_created_by": map[string]any{"uid": owner}},
			{"uid": "_:c", "dgraph.type": "Doc", "doc_uuid": c, "doc_title": "Trashed long ago", "doc_private": false, "doc_deleted_at": old, "doc_deleted_batch": "b-old", "doc_created_by": map[string]any{"uid": owner},
				"doc_comments": []map[string]any{{"uid": "_:cm", "dgraph.type": "Comment", "comment_uuid": uuid.NewString(), "comment_text": "hi"}}},
			{"uid": "_:linker", "dgraph.type": "Task", "task_uuid": uuid.NewString(), "linked_docs": []map[string]any{{"uid": "_:c"}}},
		})
		sub, err := domain.GetArchivedSubtree(ctx, a, owner)
		if err != nil || !sameStrings(titlesOf(sub), []string{"Old", "Old child"}) {
			t.Fatalf("archived subtree = %v %v", titlesOf(sub), err)
		}
		arch, err := domain.ListDocs(ctx, domain.DocListQuery{Scope: domain.ScopeArchived, ViewerUID: owner, First: 100})
		if err != nil || int(arch.Count) != len(arch.Docs) || !containsTitle(arch.Docs, "Old") || containsTitle(arch.Docs, "Trashed long ago") {
			t.Fatalf("archived list = %v (count %d) %v", titlesOf(arch.Docs), arch.Count, err)
		}
		links, err := domain.GetDocTreeLinks(ctx, []string{id["rfcs"], id["eng"]})
		if err != nil || len(links) != 2 {
			t.Fatalf("links = %+v %v", links, err)
		}
		for _, l := range links {
			if l.Uuid == id["rfcs"] && (l.Parent == nil || l.Parent.Uuid != id["eng"] || len(l.Children) != 2) {
				t.Fatalf("RFCs' links = %+v", l)
			}
		}
		due, err := domain.GetDocsTrashedBefore(ctx, time.Now().Add(-30*24*time.Hour), 100)
		if err != nil || len(due) != 1 || due[0].Uuid != c || len(due[0].Comments) != 1 {
			t.Fatalf("due for good = %+v %v (archived docs and live ones never are)", due, err)
		}
		if err := domain.PurgeDocNodes(ctx, []string{due[0].Uid}, []string{due[0].Comments[0].Uid}, time.Now(), nil, nil); err != nil {
			t.Fatal(err)
		}
		raw := dg.Query(t, `{ d(func: eq(doc_uuid, "`+c+`")) { uid } l(func: uid(`+got["linker"]+`)) { linked_docs { uid } } }`)
		if jsonHas(raw, `"d":[{`) || jsonHas(raw, "linked_docs") {
			t.Fatalf("after the purge: %s", raw)
		}
	})

	t.Run("the lock holds one change at a time", func(t *testing.T) {
		unlock, err := graph.Lock(ctx, "doc-test")
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan error, 1)
		go func() {
			u, err := graph.Lock(ctx, "doc-test")
			if err == nil {
				u()
			}
			got <- err
		}()
		select {
		case err := <-got:
			t.Fatalf("a second change took the lock while the first held it: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
		unlock()
		if err := <-got; err != nil {
			t.Fatalf("the second change never got the lock: %v", err)
		}
	})
}

func containsTitle(docs []*dgraphStruct.DgraphDoc, title string) bool {
	for _, d := range docs {
		if d.Title == title {
			return true
		}
	}
	return false
}

func jsonHas(raw []byte, s string) bool { return strings.Contains(string(raw), s) }
