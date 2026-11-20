package business

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// HoldAll is every door's one intake: one gate (triage on, one count) and one
// insert for a batch, however many arrive (the mail door hands a poll's
// messages in at once; the 7th check: no store call in a loop). Hold is its
// one-item case. What doesn't fit the room is not held (the team's work).
func TestHoldAllOneGateOneInsert(t *testing.T) {
	w := newWorld(t)
	ctx, project := context.Background(), uuid.New()
	w.on[project.String()] = true
	w.count = MaxWaiting - 2
	me := uuid.New()
	in := []Arrival{{TaskID: uuid.New(), From: Requester{UserUUID: &me}}, {TaskID: uuid.New(), From: Requester{Email: "out@side.example"}}, {TaskID: uuid.New()}}
	held, err := HoldAll(ctx, project, FromMail, in)
	if err != nil || held != 2 {
		t.Fatalf("held %d, %v; want the room's 2", held, err)
	}
	if w.counts != 1 || w.inserts != 1 || len(w.puts) != 2 {
		t.Fatalf("counts %d, inserts %d, kept %d; want one gate and one insert of 2", w.counts, w.inserts, len(w.puts))
	}
	if w.puts[0].Source != "mail" || w.puts[1].RequesterEmail != "out@side.example" || w.puts[1].RequesterUUID != nil {
		t.Errorf("kept %+v", w.puts)
	}
	if held, err := HoldAll(ctx, uuid.New(), FromMail, in); held != 0 || err != nil || w.inserts != 1 {
		t.Errorf("triage off: held %d, %v, inserts %d", held, err, w.inserts)
	}
	if held, err := HoldAll(ctx, project, FromMail, nil); held != 0 || err != nil || w.counts != 1 {
		t.Errorf("nothing to hold read the store: held %d, %v, counts %d", held, err, w.counts)
	}
}
