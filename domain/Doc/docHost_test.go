package domain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
)

const (
	hostedDoc = "5f7a1c2e-0d3b-4c9e-9a51-3b2d7e8f9c01"
	hostRow   = "8e2b4d6f-1a3c-4e5f-8b7a-9c0d1e2f3a4b"
)

// A hosted doc (a table row's page, D-18) is open to nobody by the rule:
// its maker, its owner, the people and spaces it might name and the
// workspace all find none, so a path that asks the rule alone fails closed.
// The doc says it is hosted, and a deletion of its own still shows.
func TestAHostedDocIsOpenToNobodyByTheRule(t *testing.T) {
	maya := &dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "11111111-1111-4111-8111-111111111111"}
	public := false
	doc := &dgraphStruct.DgraphDoc{
		Uuid: hostedDoc, CreatedBy: maya, Owner: maya, IsPrivate: &public,
		HasFullAccess: 1, HasEditAccess: 1, HasReadAccess: 1, InProjectSpace: "p",
	}
	// Without a host, its owner has full access: the doc is the control.
	if d := DecideDoc(doc, Viewer{UID: "0x1", UUID: maya.Uuid}); d.Level != level.Full || !d.Owner || d.Hosted {
		t.Fatalf("the control doc: %+v", d)
	}
	doc.HostKind, doc.HostUUID = "table_row", hostRow
	for _, v := range []Viewer{{UID: "0x1", UUID: maya.Uuid}, {UID: "0x2"}, {UUID: "22222222-2222-4222-8222-222222222222"}} {
		d := DecideDoc(doc, v)
		if d.Level != level.None || d.Was != level.None || d.Unlocked != level.None || d.Owner || !d.Hosted || len(d.Reasons) != 0 {
			t.Errorf("%+v decides a hosted doc as %+v; want none, hosted, owned by nobody", v, d)
		}
	}
	gone := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	doc.DeletedAt = &gone
	if d := DecideDoc(doc, Viewer{UID: "0x1"}); !d.Deleted || d.Level != level.None {
		t.Errorf("a hosted doc's own deletion: %+v", d)
	}
}

// A host's doc is made in one conditional write, only when the host has none:
// with no maker, owner, shares, space or parent, private, live, naming its
// host. A host that isn't one, or a doc id that isn't, is refused before
// anything is sent.
func TestAHostsDocIsMadeOnlyWhenItHasNone(t *testing.T) {
	at := time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)
	req, err := hostedDocRequest("table_row", hostRow, hostedDoc, "Acme", at)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req.Query, `eq(doc_host_uuid, $host)) @filter(eq(doc_host_kind, $kind))`) ||
		req.Vars["$host"] != hostRow || req.Vars["$kind"] != "table_row" {
		t.Errorf("the host's doc isn't looked for by its host: %s %v", req.Query, req.Vars)
	}
	if len(req.Mutations) != 1 || req.Mutations[0].Cond != "@if(eq(len(hosted), 0))" {
		t.Fatalf("the make isn't conditional on the host having none: %+v", req.Mutations)
	}
	var set map[string]any
	if err := json.Unmarshal(req.Mutations[0].SetJson, &set); err != nil {
		t.Fatal(err)
	}
	if set["doc_uuid"] != hostedDoc || set["doc_host_kind"] != "table_row" || set["doc_host_uuid"] != hostRow ||
		set["doc_private"] != true || set["doc_title"] != "Acme" || set["doc_deleted_at"] != "0001-01-01T00:00:00Z" {
		t.Errorf("the doc made: %v", set)
	}
	for _, edge := range []string{"doc_created_by", "doc_owner", "doc_editing_users", "doc_reading_users", "doc_commenting_users", "doc_space_kind", "doc_space_uuid", "doc_parent"} {
		if _, ok := set[edge]; ok {
			t.Errorf("a hosted doc is made with %s: %v", edge, set[edge])
		}
	}
	for _, bad := range [][3]string{
		{"", hostRow, hostedDoc}, {"Table Row", hostRow, hostedDoc}, {"table_row", "row-1", hostedDoc}, {"table_row", hostRow, "doc-1"},
	} {
		if _, err := hostedDocRequest(bad[0], bad[1], bad[2], "", at); err == nil {
			t.Errorf("%v made a request", bad)
		} else if bad[2] == hostedDoc && !errors.Is(err, ErrNoHost) {
			t.Errorf("%v: %v, want ErrNoHost", bad, err)
		}
	}
}

// The lists that walk docs leave hosted docs out by their host: the tree's
// candidates (notInternalFilter) and the retention archive's (a hosted doc
// goes with its host, never on its own).
func TestTheTreeAndTheRetentionLeaveHostedDocsOut(t *testing.T) {
	if !strings.Contains(notInternalFilter, "NOT has(doc_host_kind)") {
		t.Errorf("the tree's filter %q lists hosted docs", notInternalFilter)
	}
	if !strings.Contains(inactiveDocFilter("$cutoff"), "NOT has(doc_host_kind)") {
		t.Errorf("the retention archive's filter %q takes hosted docs", inactiveDocFilter("$cutoff"))
	}
	if !strings.Contains(accessScalars, "doc_host_kind") || !strings.Contains(accessScalars, "doc_host_uuid") {
		t.Error("a decision's read doesn't carry the doc's host, so the rule can't refuse a hosted doc")
	}
	if !strings.Contains(docSearchFields, "doc_host_kind") {
		t.Error("the search entry's read doesn't carry the doc's host, so a hosted doc would be indexed")
	}
}

// A make answers which doc the host has: the one it made, or, when the host
// had one (a make that conflicted with another's, run again, finds it), that
// one, made false; and a commit that failed is an error, never a doc.
func TestAHostsDocIsTheOneItHas(t *testing.T) {
	defer func(c func(context.Context, *api.Request) (*api.Response, error)) { commitHostedDoc = c }(commitHostedDoc)
	at := time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)
	commitHostedDoc = func(_ context.Context, req *api.Request) (*api.Response, error) {
		if len(req.Mutations) != 1 || req.Mutations[0].Cond == "" {
			t.Error("the make isn't conditional")
		}
		return &api.Response{Json: []byte(`{"found":[]}`)}, nil
	}
	if id, made, err := EnsureHostedDoc(context.Background(), "table_row", hostRow, hostedDoc, "Acme", at); err != nil || !made || id != hostedDoc {
		t.Errorf("a host with no doc: %q %v %v", id, made, err)
	}
	other := "9d8c7b6a-5f4e-4d3c-8b2a-1f0e9d8c7b6a"
	commitHostedDoc = func(context.Context, *api.Request) (*api.Response, error) {
		return &api.Response{Json: []byte(`{"found":[{"doc_uuid":"` + other + `"}]}`)}, nil
	}
	if id, made, err := EnsureHostedDoc(context.Background(), "table_row", hostRow, hostedDoc, "Acme", at); err != nil || made || id != other {
		t.Errorf("a host with a doc: %q %v %v, want %s", id, made, err, other)
	}
	failed := errors.New("Transaction has been aborted. Please retry")
	commitHostedDoc = func(context.Context, *api.Request) (*api.Response, error) { return nil, failed }
	if id, _, err := EnsureHostedDoc(context.Background(), "table_row", hostRow, hostedDoc, "Acme", at); !errors.Is(err, failed) || id != "" {
		t.Errorf("a failed make: %q %v", id, err)
	}
}
