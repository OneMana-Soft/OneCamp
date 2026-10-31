package models

import (
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
)

func TestStampLastChatsMovesEachDMForwardToItsNewestMessage(t *testing.T) {
	older := time.Date(2026, 10, 25, 9, 0, 0, 0, time.FixedZone("IST", 5*3600+1800))
	newer := older.Add(time.Minute)
	req := &api.Request{Query: "query {\n\tdm as var(func: eq(dm_grouping_id, \"a b\"))\n}", Mutations: []*api.Mutation{{SetJson: []byte("{}")}}}
	StampLastChats(req,
		&DgraphDm{Uid: "uid(dm)", Chats: []*DgraphChat{{CreatedAt: &older}, {CreatedAt: &newer}}},
		&DgraphDm{Uid: "0x2a", Chats: []*DgraphChat{{CreatedAt: &older}}},
		&DgraphDm{Uid: "uid(empty)"},
		&DgraphDm{Uid: "_:blank", Chats: []*DgraphChat{{CreatedAt: &older}}},
	)

	if !strings.HasSuffix(req.Query, "\n}") || strings.Count(req.Query, "{") != strings.Count(req.Query, "}") {
		t.Fatalf("the query is no longer one block:\n%s", req.Query)
	}
	for _, want := range []string{
		`dm_last_newer_0 as var(func: uid(dm)) @filter(ge(dm_last_chat_at, "2026-10-25T03:31:00Z"))`,
		`dm_last_newer_1 as var(func: uid(0x2a)) @filter(ge(dm_last_chat_at, "2026-10-25T03:30:00Z"))`,
	} {
		if !strings.Contains(req.Query, want) {
			t.Errorf("the query lacks %s:\n%s", want, req.Query)
		}
	}
	if strings.Contains(req.Query, "empty") || strings.Contains(req.Query, "blank") {
		t.Errorf("a DM with no messages, or named by neither a variable nor a uid, was stamped:\n%s", req.Query)
	}
	if len(req.Mutations) != 3 {
		t.Fatalf("%d mutations, want the write and two stamps", len(req.Mutations))
	}
	for i, want := range []struct{ cond, nquads string }{
		{"@if(eq(len(dm_last_newer_0), 0))", `uid(dm) <dm_last_chat_at> "2026-10-25T03:31:00Z"^^<xs:dateTime> .`},
		{"@if(eq(len(dm_last_newer_1), 0))", `<0x2a> <dm_last_chat_at> "2026-10-25T03:30:00Z"^^<xs:dateTime> .`},
	} {
		if m := req.Mutations[i+1]; m.Cond != want.cond || string(m.SetNquads) != want.nquads {
			t.Errorf("stamp %d: %s %s, want %s %s", i, m.Cond, m.SetNquads, want.cond, want.nquads)
		}
	}
}

func TestStampLastChatsLeavesAWriteWithNoMessagesAlone(t *testing.T) {
	req := &api.Request{Query: "query { dm as var(func: eq(dm_grouping_id, \"a b\")) }"}
	StampLastChats(req, &DgraphDm{Uid: "uid(dm)"})
	if req.Query != "query { dm as var(func: eq(dm_grouping_id, \"a b\")) }" || len(req.Mutations) != 0 {
		t.Fatalf("a DM with no new message changed the request: %q, %d mutations", req.Query, len(req.Mutations))
	}
}
