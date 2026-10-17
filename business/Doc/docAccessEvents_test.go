package businness

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
)

// A change to a doc's sharing is told once as itself, then as doc.shared to
// the people who couldn't open the doc before and can now: one event for each
// level they were given, saying it (A5×A6), with the change's own data (an
// invite's, a request's) kept beside it. Someone whose level only moved, or
// who lost it, is the change's alone.
func TestASharingChangeTellsEachLevelGivenAsShared(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	by := Actor{UserUUID: "olu"}
	data := map[string]string{"invite": "true"}
	ex := Explained{People: []Shift{
		{UserUUID: "ann", From: level.None, To: level.Edit},
		{UserUUID: "bea", From: level.None, To: level.View},
		{UserUUID: "cy", From: level.None, To: level.Edit},
		{UserUUID: "di", From: level.View, To: level.Edit}, // could open it already
		{UserUUID: "eve", From: level.Comment, To: level.None},
		{UserUUID: "fay", From: level.None, To: level.Full},
	}}
	got := accessEvents("doc", by, resourceEventBusiness.DocAccessChanged, ex, data, at)
	if len(got) != 4 {
		t.Fatalf("%d events, want the change and three doc.shared: %+v", len(got), got)
	}
	if c := got[0]; c.Kind != resourceEventBusiness.DocAccessChanged || c.Data["invite"] != "true" || c.Data["level"] != "" || len(c.Subjects) != 0 {
		t.Errorf("the change itself: %+v", c)
	}
	for i, want := range []struct {
		level    string
		subjects []string
	}{{"full", []string{"fay"}}, {"edit", []string{"ann", "cy"}}, {"view", []string{"bea"}}} {
		e := got[i+1]
		if e.Kind != resourceEventBusiness.DocShared || e.Data["level"] != want.level || !slices.Equal(e.Subjects, want.subjects) ||
			e.Data["invite"] != "true" || e.Actor != by || !e.At.Equal(at) || e.ResourceUUID != "doc" {
			t.Errorf("doc.shared at %s: %+v", want.level, e)
		}
	}
	if len(data) != 1 {
		t.Errorf("the caller's data was written to: %v", data)
	}

	// Nobody gained: the change alone, as a subpage's or a lock's is.
	if got := accessEvents("doc", by, resourceEventBusiness.DocAccessChanged, Explained{Everyone: &Shift{}}, nil, at); len(got) != 1 {
		t.Errorf("with nobody gaining, %d events", len(got))
	}
}

// A change takes a doc from someone when a person who could read it can't
// now, or the workspace's level falls below view; then the doc's topic is
// rechecked at the broker. Everyone's shift with no levels (a page under one
// whose space moved) may have taken it from anyone.
func TestAChangeThatTakesADocFromAnyoneIsRecheckedAtTheBroker(t *testing.T) {
	for _, c := range []struct {
		name string
		ex   Explained
		want bool
	}{
		{"someone given it", Explained{People: []Shift{{UserUUID: "ann", From: level.None, To: level.View}}}, false},
		{"someone moved from edit to view", Explained{People: []Shift{{UserUUID: "bea", From: level.Edit, To: level.View}}}, false},
		{"someone who could read it can't", Explained{People: []Shift{{UserUUID: "cy", From: level.Comment, To: level.None}}}, true},
		{"made Restricted", Explained{Everyone: &Shift{From: level.View, To: level.None}}, true},
		{"opened to the workspace", Explained{Everyone: &Shift{From: level.None, To: level.View}}, false},
		{"the workspace's level lowered", Explained{Everyone: &Shift{From: level.Edit, To: level.View}}, false},
		{"pages under a page that changed space", Explained{Everyone: &Shift{}}, true},
		{"nothing", Explained{}, false},
	} {
		if got := tookFromAnyone(c.ex); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// A topic that isn't a doc's is nobody's to read, without a graph read.
	if ok, err := readsDocTopic(context.Background(), "ann", "not a topic"); ok || err != nil {
		t.Errorf("a topic that isn't a doc's: %v %v", ok, err)
	}
}
