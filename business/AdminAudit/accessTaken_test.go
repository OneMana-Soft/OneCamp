package business

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// asRow is one row RecordedAs wrote through its seam.
type asRow struct {
	actorID          *uuid.UUID
	email, kind      string
	action, category string
	summary          string
	metadata         map[string]interface{}
}

// recordAs swaps RecordedAs' writer for one that keeps its rows and answers
// fail (nil writes them); the real one comes back after the test.
func recordAs(t *testing.T, fail error) *[]asRow {
	t.Helper()
	var rows []asRow
	old := recordedSeams
	t.Cleanup(func() { recordedSeams = old })
	recordedSeams.as = func(_ context.Context, id *uuid.UUID, email, kind, action, category, summary string, meta map[string]interface{}) error {
		if fail != nil {
			return fail
		}
		rows = append(rows, asRow{id, email, kind, action, category, summary, meta})
		return nil
	}
	return &rows
}

func TestTakenKeepsOnlyTheLevelsThatFall(t *testing.T) {
	got := Taken([]AccessShift{
		{UserUUID: "kim", From: "edit", To: "view"},
		{UserUUID: "new", From: "", To: "edit"},
		{UserUUID: "lee", From: "view", To: ""},
		{UserUUID: "sam", From: "comment", To: "comment"},
		{UserUUID: "ava", From: "full", To: "edit"},
	})
	want := []AccessShift{
		{UserUUID: "kim", From: "edit", To: "view"},
		{UserUUID: "lee", From: "view", To: ""},
		{UserUUID: "ava", From: "full", To: "edit"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Taken = %+v, want %+v", got, want)
	}
}

// A board's or a doc's share as its owner sends it after the single-role rule:
// Kim made a viewer (so taken off edit and comment), Lee taken off, a newcomer
// made an editor, and someone who held nothing taken off edit.
func TestRoleShiftsMovesEachPersonToTheirHighestLevelLeft(t *testing.T) {
	before := map[string]string{"kim": "edit", "lee": "view", "sam": "comment"}
	got := RoleShifts(before,
		map[string][]string{"view": {"KIM"}, "edit": {"new", ""}},
		map[string][]string{"edit": {"kim", "ghost"}, "comment": {"Kim"}, "view": {"lee"}})
	want := []AccessShift{
		{UserUUID: "kim", From: "edit", To: "view"},
		{UserUUID: "lee", From: "view", To: ""},
		{UserUUID: "new", From: "", To: "edit"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RoleShifts = %+v, want %+v", got, want)
	}
	// An id in another case is the same person, as the role writer stores it.
	if got := Taken(RoleShifts(map[string]string{"abc": "edit"}, nil, map[string][]string{"edit": {"ABC"}})); len(got) != 1 {
		t.Fatalf("an upper-case id taken off edit = %+v, want one fall", got)
	}
}

func TestAccessMetaNamesThePeopleAndNoTitle(t *testing.T) {
	got := AccessMeta("board", "b-1", []AccessShift{{UserUUID: "lee", From: "view", To: ""}})
	want := map[string]interface{}{
		"board_uuid": "b-1",
		"people":     []map[string]string{{"user_uuid": "lee", "from": "view", "to": ""}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AccessMeta = %#v, want %#v", got, want)
	}
}

func TestContextPrincipalSaysWhoActs(t *testing.T) {
	if p := ContextPrincipal(context.Background(), ""); p.Kind != ActorSystem || p.ID != nil {
		t.Fatalf("no one in ctx = %+v, want the system with no id", p)
	}
	id := uuid.New()
	var ui userModels.UserInfo
	ui.UserPostgresInfo.Id = id
	ui.UserPostgresInfo.EmailID = "maya@example.com"
	ctx := context.WithValue(context.Background(), helpers.UserInfoContextKey, ui)
	if p := ContextPrincipal(ctx, ""); p.Kind != ActorHuman || p.ID == nil || *p.ID != id || p.Email != "maya@example.com" {
		t.Fatalf("a person's request = %+v, want them, human", p)
	}
	if p := ContextPrincipal(ctx, "agent-1"); p.Kind != ActorAgent || p.ID == nil || *p.ID != id {
		t.Fatalf("an agent with their authority = %+v, want them, agent", p)
	}
}

func TestRecordedAsWritesTheRowBeforeTheChange(t *testing.T) {
	rows := recordAs(t, nil)
	id := uuid.New()
	p := Principal{ID: &id, Email: "maya@example.com", Kind: ActorHuman}
	ran := false
	err := RecordedAs(context.Background(), p, "board.access.remove", CategoryDocs, "s", map[string]interface{}{"k": "v"}, func() error {
		if len(*rows) != 1 {
			t.Fatalf("the change ran before its row was written")
		}
		ran = true
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("RecordedAs = %v (ran %v), want the change made", err, ran)
	}
	r := (*rows)[0]
	if r.actorID != &id || r.email != p.Email || r.kind != ActorHuman || r.action != "board.access.remove" || r.category != CategoryDocs || r.metadata["k"] != "v" {
		t.Fatalf("row = %+v, want it to name the principal, action, category and metadata", r)
	}
}

func TestRecordedAsRefusesAChangeItCantRecord(t *testing.T) {
	recordAs(t, errors.New("db down"))
	ran := false
	err := RecordedAs(context.Background(), Principal{Kind: ActorSystem}, "doc.access.remove", CategoryDocs, "s", nil, func() error {
		ran = true
		return nil
	})
	if ran || !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("RecordedAs = %v (ran %v), want ErrNotRecorded and no change", err, ran)
	}
}

func TestRecordedAsSaysWhenTheChangeDidntTake(t *testing.T) {
	rows := recordAs(t, nil)
	boom := errors.New("dgraph down")
	err := RecordedAs(context.Background(), Principal{Kind: ActorSystem}, "doc.access.remove", CategoryDocs, "s", map[string]interface{}{"k": "v"}, func() error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("RecordedAs = %v, want the change's error", err)
	}
	if len(*rows) != 2 || (*rows)[1].action != "doc.access.remove_failed" || (*rows)[1].metadata["k"] != "v" {
		t.Fatalf("rows = %+v, want the row and its _failed row with the same metadata", *rows)
	}
}

func TestRecordedTakingRecordsOnlyWhatTakesAccessAway(t *testing.T) {
	rows := recordAs(t, nil)
	ran := 0
	change := func() error { ran++; return nil }
	grant := []AccessShift{{UserUUID: "new", From: "", To: "edit"}, {UserUUID: "kim", From: "view", To: "edit"}}
	if err := RecordedTaking(context.Background(), Principal{Kind: ActorSystem}, "doc.access.remove", "s", "doc", "d-1", grant, nil, change); err != nil || ran != 1 || len(*rows) != 0 {
		t.Fatalf("a grant: err %v, ran %d, rows %d; want it made with no row", err, ran, len(*rows))
	}
	taking := append(grant, AccessShift{UserUUID: "lee", From: "view", To: ""})
	if err := RecordedTaking(context.Background(), Principal{Kind: ActorSystem}, "doc.access.remove", "s", "doc", "d-1", taking, map[string]interface{}{"subpages": 2}, change); err != nil || ran != 2 || len(*rows) != 1 {
		t.Fatalf("a change taking access: err %v, ran %d, rows %d; want it made after one row", err, ran, len(*rows))
	}
	meta := (*rows)[0].metadata
	people, _ := meta["people"].([]map[string]string)
	if meta["doc_uuid"] != "d-1" || meta["subpages"] != 2 || len(people) != 1 || people[0]["user_uuid"] != "lee" || (*rows)[0].category != CategoryDocs {
		t.Fatalf("row metadata = %#v, want the doc, its subpages and only the person whose access fell", meta)
	}
}

// A taking is filed under its resource's own area: a board's under "boards",
// a doc's (and any kind without an area of its own) under "docs".
func TestRecordedTakingFilesTheRowUnderTheResourcesArea(t *testing.T) {
	shifts := RoleShifts(map[string]string{"u1": "edit"}, nil, map[string][]string{"edit": {"u1"}})
	for kind, want := range map[string]string{"board": CategoryBoards, "doc": CategoryDocs, "space": CategoryDocs} {
		rows := recordAs(t, nil)
		p := Principal{Kind: ActorHuman}
		if err := RecordedTaking(context.Background(), p, kind+".access.remove", "s", kind, "r1", shifts, nil, func() error { return nil }); err != nil {
			t.Fatalf("%s: RecordedTaking = %v", kind, err)
		}
		if len(*rows) != 1 || (*rows)[0].category != want {
			t.Errorf("%s: rows %+v, want one under %q", kind, *rows, want)
		}
	}
}

// Given is the other half of Taken: the people a change gives access to, or
// raises, and nobody it lowers or leaves alone.
func TestGivenKeepsOnlyTheLevelsThatRise(t *testing.T) {
	shifts := []AccessShift{{UserUUID: "a", To: "edit"}, {UserUUID: "b", From: "view", To: "edit"}, {UserUUID: "c", From: "edit", To: "view"}, {UserUUID: "d", From: "edit"}}
	got := Given(shifts)
	if len(got) != 2 || got[0].UserUUID != "a" || got[1].UserUUID != "b" {
		t.Fatalf("Given = %+v, want a then b", got)
	}
}
