package business

import (
	"context"
	"errors"
	"reflect"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/google/uuid"
)

// V-1209: a private conversation's call notes (its recap and full transcript)
// go to someone still in it, never to a workspace admin outside it.

var (
	rina  = uuid.MustParse("66666666-6666-4666-8666-666666666666") // the channel's maker, still here
	omar  = uuid.MustParse("77777777-7777-4777-8777-777777777777") // an event's organiser
	event = uuid.MustParse("88888888-8888-4888-8888-888888888888")
)

const notesTitle = "Meeting notes: #launch, 3 Nov 2026, 10:00"

// tieWorld ties Dana's docs as ties says, with the people still here as
// active and eventPeople say, and counts the reads; the admin may take work.
type tieReads struct{ ties, people int }

func tieWorld(t *testing.T, w *world, ties map[string]recordingDomain.NotesDocTie, active map[uuid.UUID]bool, eventPeople map[uuid.UUID][]uuid.UUID) (*tieReads, *[]string) {
	t.Helper()
	reads := &tieReads{}
	docTies = func(_ context.Context, ids []string, to string) (map[string]recordingDomain.NotesDocTie, error) {
		reads.ties++
		out := map[string]recordingDomain.NotesDocTie{}
		for _, id := range ids {
			if tie, ok := ties[id]; ok {
				out[id] = tie
			}
		}
		return out, nil
	}
	handoverPeople = func(_ context.Context, people, events []uuid.UUID) (map[uuid.UUID]bool, map[uuid.UUID][]uuid.UUID, error) {
		reads.people++
		out := map[uuid.UUID]bool{}
		for _, p := range people {
			if active[p] {
				out[p] = true
			}
		}
		evp := map[uuid.UUID][]uuid.UUID{}
		for _, e := range events {
			if ps, ok := eventPeople[e]; ok {
				evp[e] = ps
			}
		}
		return out, evp, nil
	}
	activeMember = func(_ context.Context, id uuid.UUID) (bool, string, error) {
		return id == maya || id.String() == admin.UserUUID, "", nil
	}
	var to []string
	prev := recoverOne
	recoverOne = func(ctx context.Context, a accessBusiness.Subject, kind, doc, who string) error {
		to = append(to, doc+">"+who)
		return prev(ctx, a, kind, doc, who)
	}
	return reads, &to
}

func TestAnOutsideAdminsHandoverGivesAPrivateChannelsNotesToSomeoneInIt(t *testing.T) {
	w := newWorld(t)
	_, to := tieWorld(t, w, map[string]recordingDomain.NotesDocTie{
		"d1": {Doc: "d1", Title: notesTitle, Order: []string{rina.String()}},
	}, map[uuid.UUID]bool{rina: true}, nil)
	adminID := uuid.MustParse(admin.UserUUID)
	m, err := HandOver(context.Background(), admin, dana, adminID)
	if err != nil || m.Docs != 3 || m.Failed != 0 || len(m.Kept) != 0 {
		t.Fatalf("%+v %v", m, err)
	}
	// The notes went to the channel's maker; the admin got only Dana's own
	// docs, and so can read nothing of the call.
	if !reflect.DeepEqual(w.docs[rina.String()], []string{"d1"}) || !reflect.DeepEqual(w.docs[admin.UserUUID], []string{"d2", "d3"}) {
		t.Fatalf("docs %v (moves %v)", w.docs, *to)
	}
}

func TestNotesNobodyIsLeftToTakeStayWithTheirOwnerAndTheAdminSeesTheTitle(t *testing.T) {
	w := newWorld(t)
	_, to := tieWorld(t, w, map[string]recordingDomain.NotesDocTie{
		"d1": {Doc: "d1", Title: notesTitle, Order: []string{rina.String(), cast.String()}},
	}, map[uuid.UUID]bool{}, nil)
	m, err := HandOver(context.Background(), admin, dana, uuid.MustParse(admin.UserUUID))
	if err != nil || m.Docs != 2 || m.Failed != 0 || !reflect.DeepEqual(m.Kept, []string{notesTitle}) {
		t.Fatalf("%+v %v", m, err)
	}
	if !reflect.DeepEqual(w.docs[dana.String()], []string{"d1"}) {
		t.Fatalf("docs %v (moves %v)", w.docs, *to)
	}
	for _, mv := range *to {
		if mv[:2] == "d1" {
			t.Fatalf("the kept notes were recovered: %v", *to)
		}
	}
}

func TestAPublicChannelsNotesAndAPersonalDocGoWhereTheAdminSays(t *testing.T) {
	w := newWorld(t)
	tieWorld(t, w, map[string]recordingDomain.NotesDocTie{
		"d1": {Doc: "d1", Title: notesTitle, Public: true, Order: []string{rina.String()}},
	}, map[uuid.UUID]bool{rina: true}, nil)
	m, err := HandOver(context.Background(), admin, dana, maya)
	if err != nil || m.Docs != 3 || len(m.Kept) != 0 {
		t.Fatalf("%+v %v", m, err)
	}
	if !reflect.DeepEqual(w.docs[maya.String()], []string{"d1", "d2", "d3"}) || len(w.docs[rina.String()]) != 0 {
		t.Fatalf("docs %v", w.docs)
	}
}

func TestSomeoneInTheConversationGetsTheNotesAsTheAdminAsked(t *testing.T) {
	w := newWorld(t)
	tieWorld(t, w, map[string]recordingDomain.NotesDocTie{
		"d1": {Doc: "d1", Title: notesTitle, ToIn: true, Order: []string{rina.String()}},
	}, map[uuid.UUID]bool{rina: true}, nil)
	if m, err := HandOver(context.Background(), admin, dana, maya); err != nil || m.Docs != 3 {
		t.Fatalf("%+v %v", m, err)
	}
	if !reflect.DeepEqual(w.docs[maya.String()], []string{"d1", "d2", "d3"}) {
		t.Fatalf("docs %v", w.docs)
	}
}

func TestADMsNotesGoToTheFirstParticipantStillHere(t *testing.T) {
	w := newWorld(t)
	tieWorld(t, w, map[string]recordingDomain.NotesDocTie{
		"d2": {Doc: "d2", Title: notesTitle, Order: []string{cast.String(), rina.String()}},
	}, map[uuid.UUID]bool{rina: true}, nil)
	if m, err := HandOver(context.Background(), admin, dana, maya); err != nil || m.Docs != 3 {
		t.Fatalf("%+v %v", m, err)
	}
	if !reflect.DeepEqual(w.docs[rina.String()], []string{"d2"}) {
		t.Fatalf("docs %v", w.docs)
	}
}

func TestAnEventCallsNotesGoToItsOrganiserThenAnAttendee(t *testing.T) {
	w := newWorld(t)
	tieWorld(t, w, map[string]recordingDomain.NotesDocTie{
		"d1": {Doc: "d1", Title: notesTitle, Event: event.String()},
		"d3": {Doc: "d3", Title: notesTitle, Event: uuid.NewString()},
	}, nil, map[uuid.UUID][]uuid.UUID{event: {omar, rina}})
	m, err := HandOver(context.Background(), admin, dana, uuid.MustParse(admin.UserUUID))
	// d3's event is gone (or nobody of it is here): kept, title only.
	if err != nil || m.Docs != 2 || !reflect.DeepEqual(m.Kept, []string{notesTitle}) {
		t.Fatalf("%+v %v", m, err)
	}
	if !reflect.DeepEqual(w.docs[omar.String()], []string{"d1"}) || !reflect.DeepEqual(w.docs[admin.UserUUID], []string{"d2"}) {
		t.Fatalf("docs %v", w.docs)
	}
	// Someone the event has gets it as asked.
	w2 := newWorld(t)
	tieWorld(t, w2, map[string]recordingDomain.NotesDocTie{
		"d1": {Doc: "d1", Title: notesTitle, Event: event.String()},
	}, nil, map[uuid.UUID][]uuid.UUID{event: {omar, maya}})
	if _, err := HandOver(context.Background(), admin, dana, maya); err != nil || !reflect.DeepEqual(w2.docs[maya.String()], []string{"d1", "d2", "d3"}) {
		t.Fatalf("docs %v %v", w2.docs, err)
	}
}

// One read of the ties and one of the people, whatever the number of docs;
// a tie that can't be read moves nothing.
func TestAHandoverReadsTiesAndPeopleOnceAndFailsClosed(t *testing.T) {
	w := newWorld(t)
	reads, _ := tieWorld(t, w, map[string]recordingDomain.NotesDocTie{
		"d1": {Doc: "d1", Title: notesTitle, Order: []string{rina.String()}},
		"d2": {Doc: "d2", Title: notesTitle, Order: []string{rina.String()}},
	}, map[uuid.UUID]bool{rina: true}, nil)
	if _, err := HandOver(context.Background(), admin, dana, maya); err != nil || reads.ties != 1 || reads.people != 1 {
		t.Fatalf("reads %+v %v", *reads, err)
	}
	w2 := newWorld(t)
	tieWorld(t, w2, nil, nil, nil)
	docTies = func(context.Context, []string, string) (map[string]recordingDomain.NotesDocTie, error) {
		return nil, errors.New("dgraph down")
	}
	if m, err := HandOver(context.Background(), admin, dana, maya); err == nil || m.Docs != 0 || len(w2.docs[dana.String()]) != 3 {
		t.Fatalf("%+v %v %v", m, err, w2.docs)
	}
}
