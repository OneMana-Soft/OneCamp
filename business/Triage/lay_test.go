package business

import (
	"context"
	"errors"
	"testing"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	triageModel "github.com/akashc777/OneCamp/models/postgres/Triage"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A project read carries its triage (linear-21 H1b): on, and how many wait
// that aren't snoozed, for a reader the queue's own read admits; none for one
// it refuses, nor where triage is off. Its store calls are counted: one count
// when on, none when off, and nothing the read already holds is read again.

// laySpy counts what Lay asks of the stores.
type laySpy struct {
	policyReads, counts, rereads, lists int
	counted                             []uuid.UUID
	// readers is whom each count was for: uuid.Nil for a count of every
	// waiting task (one the rule lets into all the project's restricted ones).
	readers []uuid.UUID
	awake   int
	err     error
}

func newLay(t *testing.T) (*world, *laySpy) {
	t.Helper()
	w := newWorld(t)
	s := &laySpy{}
	awake0, awakeFor0 := countAwake, countAwakeFor
	t.Cleanup(func() { countAwake, countAwakeFor = awake0, awakeFor0 })
	triageOn = func(_ context.Context, id string) bool { s.policyReads++; return w.on[id] }
	countAwake = func(_ context.Context, id uuid.UUID) (int, error) {
		s.counts++
		s.counted, s.readers = append(s.counted, id), append(s.readers, uuid.Nil)
		return s.awake, s.err
	}
	countAwakeFor = func(_ context.Context, id, reader uuid.UUID) (int, error) {
		s.counts++
		s.counted, s.readers = append(s.counted, id), append(s.readers, reader)
		return s.awake, s.err
	}
	// The read holds the project and the reader's standing: neither is read again,
	// and nothing is listed to be counted.
	projectFor = func(context.Context, string, *userModels.UserInfo) (*dgraphStruct.DgraphProject, error) {
		s.rereads++
		return nil, errors.New("the project was read again")
	}
	standingIn = func(context.Context, *userModels.UserInfo, *dgraphStruct.DgraphProject) (workAccess.Standing, error) {
		s.rereads++
		return workAccess.Standing{}, errors.New("the standing was read again")
	}
	waitingIn = func(context.Context, uuid.UUID, int) ([]triageModel.Item, error) { s.lists++; return nil, nil }
	countWaiting = func(context.Context, uuid.UUID) (int, error) { s.lists++; return 0, nil }
	return w, s
}

// reader is whom the tests' reads are for.
var reader = uuid.MustParse("6f1d0c3e-2b4a-4c5d-8e9f-0a1b2c3d4e5f")

func standing(id uuid.UUID, r workAccess.Role) workAccess.Standing {
	return workAccess.Standing{ProjectUUID: id.String(), Role: r}
}

func TestAProjectReadCarriesItsTriageForItsPeople(t *testing.T) {
	for _, role := range []workAccess.Role{workAccess.RoleAdmin, workAccess.RoleMember, workAccess.RoleCommenter, workAccess.RoleViewer} {
		w, s := newLay(t)
		id := uuid.New()
		w.on[id.String()] = true
		// Three wait, one of them snoozed: the count (the queue's own query, less
		// what's snoozed) answers two.
		s.awake = 2
		p := &dgraphStruct.DgraphProject{Uuid: id.String()}
		Lay(context.Background(), reader.String(), standing(id, role), p)
		if p.Triage == nil || !p.Triage.On || p.Triage.Waiting != 2 {
			t.Fatalf("%s reads %+v, want on with 2 waiting", role, p.Triage)
		}
		if s.counts != 1 || s.counted[0] != id || s.policyReads != 1 || s.rereads != 0 || s.lists != 0 {
			t.Errorf("%s: %d counts of %v, %d policy reads, %d re-reads, %d lists; want one count of the project, one policy read, nothing else", role, s.counts, s.counted, s.policyReads, s.rereads, s.lists)
		}
		// The project's admin sees every restricted task in it, so counts them
		// all; anyone else's count is theirs, by their id.
		want := reader
		if role == workAccess.RoleAdmin {
			want = uuid.Nil
		}
		if len(s.readers) != 1 || s.readers[0] != want {
			t.Errorf("%s: counted for %v, want %v", role, s.readers, want)
		}
	}
}

func TestTriageOffLaysNothingAndCountsNothing(t *testing.T) {
	_, s := newLay(t)
	id := uuid.New()
	s.awake = 3
	p := &dgraphStruct.DgraphProject{Uuid: id.String()}
	Lay(context.Background(), reader.String(), standing(id, workAccess.RoleAdmin), p)
	if p.Triage != nil {
		t.Errorf("triage off, the read carries %+v; want none", p.Triage)
	}
	if s.counts != 0 || s.lists != 0 || s.rereads != 0 || s.policyReads != 1 {
		t.Errorf("triage off: %d counts, %d lists, %d re-reads, %d policy reads; want the policy alone", s.counts, s.lists, s.rereads, s.policyReads)
	}
}

func TestOneTheQueueRefusesGetsNoTriage(t *testing.T) {
	w, s := newLay(t)
	id := uuid.New()
	w.on[id.String()] = true
	s.awake = 2
	// Outside the project (no role), as a guest or a team admin who isn't in it
	// stands: the queue's read refuses them, so the project's read lays nothing,
	// and asks nothing to decide it.
	p := &dgraphStruct.DgraphProject{Uuid: id.String()}
	Lay(context.Background(), reader.String(), standing(id, workAccess.RoleNone), p)
	if p.Triage != nil || s.counts != 0 || s.policyReads != 0 {
		t.Errorf("outside the project: %+v after %d counts and %d policy reads; want none, asked nothing", p.Triage, s.counts, s.policyReads)
	}
	// A steward of the team, not in the project, is refused the queue too.
	st := standing(id, workAccess.RoleNone)
	st.Steward = true
	Lay(context.Background(), reader.String(), st, p)
	if p.Triage != nil || s.counts != 0 {
		t.Errorf("a team steward outside the project reads %+v", p.Triage)
	}
	// No project, or one with no id, is left alone.
	Lay(context.Background(), reader.String(), standing(id, workAccess.RoleMember), nil)
	none := &dgraphStruct.DgraphProject{}
	Lay(context.Background(), reader.String(), standing(id, workAccess.RoleMember), none)
	if none.Triage != nil || s.counts != 0 {
		t.Errorf("a project with no id reads %+v after %d counts", none.Triage, s.counts)
	}
}

func TestACountThatFailsLaysNoTriage(t *testing.T) {
	w, s := newLay(t)
	id := uuid.New()
	w.on[id.String()] = true
	s.err = errors.New("postgres went away")
	p := &dgraphStruct.DgraphProject{Uuid: id.String()}
	Lay(context.Background(), reader.String(), standing(id, workAccess.RoleMember), p)
	if p.Triage != nil || s.counts != 1 {
		t.Errorf("a failed count lays %+v after %d counts; want none (the app reads it as off), never a guessed count", p.Triage, s.counts)
	}
}
