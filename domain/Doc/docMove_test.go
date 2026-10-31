package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
)

// Archiving and restoring docs sent the graph one variable every doc shared,
// which the query never defined ("query { uid(doc) uid(doc) }"): not valid
// DQL, so every run failed. Each doc now has its own variable, defined in the
// query; each mutation runs only if its own variable matched one doc, so a
// uuid with no doc makes no node; and the request names the docs it changes.
func TestDocArchiveRequestDefinesAVariablePerDoc(t *testing.T) {
	ids := []string{"11111111-1111-4111-8111-111111111111", "not-a-uuid", "22222222-2222-4222-8222-222222222222"}
	req := docMoveRequest(ids, liveDocFilter, func(v, _ string) *api.Mutation {
		return &api.Mutation{SetJson: []byte(fmt.Sprintf(`{"uid": "uid(%s)"}`, v))}
	})
	if req == nil {
		t.Fatal("no request for two uuids")
	}
	q := req.Query
	for _, want := range []string{
		`d0 as var(func: eq(doc_uuid, "11111111-1111-4111-8111-111111111111")) @filter(` + liveDocFilter + `)`,
		`d2 as var(func: eq(doc_uuid, "22222222-2222-4222-8222-222222222222")) @filter(` + liveDocFilter + `)`,
		`moved(func: uid(d0, d2)) { doc_uuid }`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("the query lacks %q:\n%s", want, q)
		}
	}
	if strings.Contains(q, "not-a-uuid") || strings.Contains(q, "uid(doc)") {
		t.Errorf("the query names something that isn't a doc's own variable:\n%s", q)
	}
	if len(req.Mutations) != 2 {
		t.Fatalf("%d mutations for two docs", len(req.Mutations))
	}
	for i, v := range []string{"d0", "d2"} {
		m := req.Mutations[i]
		if m.Cond != "@if(eq(len("+v+"), 1))" || !strings.Contains(string(m.SetJson), "uid("+v+")") {
			t.Errorf("mutation %d: cond %q, set %s; want it to run on %s alone", i, m.Cond, m.SetJson, v)
		}
	}
	if docMoveRequest([]string{"nope"}, liveDocFilter, nil) != nil {
		t.Error("a request with no doc in it")
	}
}

// The archive checks again, in its own write, that each doc is live and
// unused since the cutoff: the run picks docs first and writes them after, so
// a doc edited in between was archived in use. And it writes each doc's uuid,
// as every save does, so a save and the archive of one doc conflict and can't
// both land: it wrote only its two marks, and a save that had read the doc
// live went into it after it was archived. A hosted doc (a table row's page,
// D-18) is never archived by the docs' retention: it goes with its host; nor
// is a folder, a file or a link (docs F1: pageOnly).
func TestTheArchiveChecksEachDocAgainAndConflictsWithASave(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	cutoff := time.Date(2025, 10, 12, 9, 30, 0, 0, time.UTC)
	at := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	req := docMoveRequest([]string{id}, docArchiveFilter(cutoff), docArchiveMutation(at))
	if req == nil || len(req.Mutations) != 1 {
		t.Fatalf("no archive request for a doc: %+v", req)
	}
	want := `d0 as var(func: eq(doc_uuid, "` + id + `")) @filter(` + liveDocFilter +
		` AND (lt(doc_updated_at, "2025-10-12T09:30:00Z") OR (NOT has(doc_updated_at) AND lt(doc_created_at, "2025-10-12T09:30:00Z"))) AND NOT ge(doc_restored_at, "2025-10-12T09:30:00Z") AND NOT has(doc_host_kind) AND ` + pageOnly + `)`
	if !strings.Contains(req.Query, want) {
		t.Errorf("the archive does not check the doc again:\n%s\nwant it to hold\n%s", req.Query, want)
	}
	var set map[string]any
	if err := json.Unmarshal(req.Mutations[0].SetJson, &set); err != nil {
		t.Fatalf("the archive's write is not JSON: %v: %s", err, req.Mutations[0].SetJson)
	}
	if set["uid"] != "uid(d0)" || set["doc_uuid"] != id || set["doc_deleted_at"] != "2026-10-12T09:30:00Z" || set["doc_archived_at"] != "2026-10-12T09:30:00Z" {
		t.Errorf("the archive writes %v; want the doc's uuid and both marks at the time given", set)
	}
}
