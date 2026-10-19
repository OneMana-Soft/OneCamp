package domain

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	models "github.com/akashc777/OneCamp/models/postgres/Conversation"
	"github.com/google/uuid"
)

// index is the index's writes, recorded in the order they came.
type index struct {
	steps   []string
	rows    map[uuid.UUID]*models.Member
	failOn  string
	ensured []models.Conversation
	dirty   []string
}

func newIndex(t *testing.T) *index {
	t.Helper()
	ix := &index{rows: map[uuid.UUID]*models.Member{}}
	swap(t, &ensureRow, func(_ context.Context, c models.Conversation) (uuid.UUID, error) {
		ix.ensured = append(ix.ensured, c)
		return c.ID, nil
	})
	fail := func(step string) error {
		ix.steps = append(ix.steps, step)
		if ix.failOn == step {
			return errors.New(step + " failed")
		}
		return nil
	}
	swap(t, &joinRows, func(_ context.Context, _ uuid.UUID, users []uuid.UUID, role string, at time.Time) error {
		if err := fail("join:" + role); err != nil {
			return err
		}
		for _, u := range users {
			ix.rows[u] = &models.Member{UserID: u, Role: role, JoinedAt: at}
		}
		return nil
	})
	swap(t, &leaveRows, func(_ context.Context, _ uuid.UUID, users []uuid.UUID, at time.Time) error {
		if err := fail("leave"); err != nil {
			return err
		}
		for _, u := range users {
			if m := ix.rows[u]; m != nil {
				m.LeftAt, m.Role = &at, models.RoleMember
			}
		}
		return nil
	})
	swap(t, &setRole, func(_ context.Context, _ uuid.UUID, u uuid.UUID, role string, _ time.Time) error {
		if err := fail("role:" + role); err != nil {
			return err
		}
		if m := ix.rows[u]; m != nil && m.Live() {
			m.Role = role
		}
		return nil
	})
	swap(t, &memberOf, func(_ context.Context, _ uuid.UUID, u uuid.UUID) (*models.Member, error) {
		if m := ix.rows[u]; m != nil {
			c := *m
			return &c, nil
		}
		return nil, nil
	})
	swap(t, &writeState, func(_ context.Context, v string) error { ix.dirty = append(ix.dirty, v); return nil })
	swap(t, &logFlip, func(context.Context, Choice) {})
	return ix
}

func TestAChannelIsOpenedWithItsMakerAsItsManager(t *testing.T) {
	ix := newIndex(t)
	channel, maya := uuid.New(), uuid.New()
	Opened(context.Background(), KindChannel, channel.String(), maya.String(), []string{maya.String()}, []string{maya.String()})
	if len(ix.ensured) != 1 || ix.ensured[0].ID != channel || ix.ensured[0].Kind != KindChannel || *ix.ensured[0].CreatedBy != maya {
		t.Fatalf("the channel's row: %+v", ix.ensured)
	}
	if m := ix.rows[maya]; m == nil || !m.Live() || m.Role != RoleManager {
		t.Fatalf("the maker: %+v", m)
	}
	if len(ix.dirty) != 0 {
		t.Fatalf("a clean write marked the index dirty: %v", ix.dirty)
	}
}

func TestADMsRowGetsAnIdOfItsOwn(t *testing.T) {
	ix := newIndex(t)
	a, b := uuid.New(), uuid.New()
	ref := a.String() + " " + b.String()
	Opened(context.Background(), KindDM, ref, a.String(), []string{a.String(), b.String()}, nil)
	if len(ix.ensured) != 1 || ix.ensured[0].Ref != ref || ix.ensured[0].ID == uuid.Nil || ix.ensured[0].ID.String() == ref {
		t.Fatalf("the DM's row: %+v", ix.ensured)
	}
	if len(ix.rows) != 2 {
		t.Fatalf("its two people: %+v", ix.rows)
	}
}

// A removal leaves the index before the graph: until the graph has taken it
// too, the index must never be the one that still says yes.
func TestARemovalLeavesTheIndexFirst(t *testing.T) {
	ix := newIndex(t)
	ctx := context.Background()
	channel, sam := uuid.New().String(), uuid.New()
	Added(ctx, KindChannel, channel, RoleMember, sam.String())
	ix.steps = nil
	err := Remove(ctx, KindChannel, channel, []string{sam.String()}, func() error {
		ix.steps = append(ix.steps, "graph")
		if ix.rows[sam].Live() {
			t.Error("the graph was asked while the index still had them")
		}
		return nil
	})
	if err != nil || !slices.Equal(ix.steps, []string{"leave", "graph"}) {
		t.Fatalf("steps %v, err %v", ix.steps, err)
	}
}

// When the graph keeps them, so does the index, as they were.
func TestARemovalTheGraphRefusedIsPutBack(t *testing.T) {
	ix := newIndex(t)
	ctx := context.Background()
	channel, jonas := uuid.New().String(), uuid.New()
	Added(ctx, KindChannel, channel, RoleManager, jonas.String())
	refused := errors.New("the graph is away")
	err := Remove(ctx, KindChannel, channel, []string{jonas.String()}, func() error { return refused })
	if !errors.Is(err, refused) {
		t.Fatalf("the graph's error wasn't answered: %v", err)
	}
	if m := ix.rows[jonas]; m == nil || !m.Live() || m.Role != RoleManager {
		t.Fatalf("not put back as they were: %+v", m)
	}
}

// A removal the index can't take isn't made: the graph is never asked.
func TestARemovalTheIndexCantTakeIsntMade(t *testing.T) {
	ix := newIndex(t)
	ix.failOn = "leave"
	asked := false
	err := Remove(context.Background(), KindGroup, "g1", []string{uuid.NewString()}, func() error { asked = true; return nil })
	if err == nil || asked {
		t.Fatalf("err %v, the graph asked %v", err, asked)
	}
}

// An addition that the index can't record doesn't fail the change (the graph
// has it), and sends decisions back to the graph.
func TestAnAdditionTheIndexMissedMarksItDirty(t *testing.T) {
	ix := newIndex(t)
	ix.failOn = "join:" + RoleMember
	Added(context.Background(), KindChannel, uuid.NewString(), RoleMember, uuid.NewString())
	if len(ix.dirty) != 1 {
		t.Fatalf("the index wasn't marked dirty: %v", ix.dirty)
	}
	if s, ok := ParseState(ix.dirty[0]); !ok || s.Kind != StateDirty {
		t.Fatalf("marked %q", ix.dirty[0])
	}
}

func TestAPromotionNeedsAMemberAndADemotionGoesFirst(t *testing.T) {
	ix := newIndex(t)
	ctx := context.Background()
	channel, priya, ghost := uuid.New().String(), uuid.New(), uuid.New()
	swap(t, &uuidsByUID, func(_ context.Context, uids []string) ([]string, error) {
		switch uids[0] {
		case "0x1":
			return []string{priya.String()}, nil
		case "0x2":
			return []string{ghost.String()}, nil
		}
		return nil, nil
	})
	Added(ctx, KindChannel, channel, RoleMember, priya.String())
	Promoted(ctx, KindChannel, channel, "0x1")
	Promoted(ctx, KindChannel, channel, "0x2")
	if ix.rows[priya].Role != RoleManager {
		t.Fatalf("a member wasn't made a manager: %+v", ix.rows[priya])
	}
	if _, in := ix.rows[ghost]; in {
		t.Fatalf("a manager's edge alone put someone in the index")
	}

	ix.steps = nil
	err := Demote(ctx, KindChannel, channel, "0x1", func() error {
		ix.steps = append(ix.steps, "graph")
		return errors.New("refused")
	})
	if err == nil || !slices.Equal(ix.steps, []string{"role:" + RoleMember, "graph", "role:" + RoleManager}) {
		t.Fatalf("steps %v, err %v", ix.steps, err)
	}
	if ix.rows[priya].Role != RoleManager {
		t.Fatalf("a demotion the graph refused wasn't put back: %+v", ix.rows[priya])
	}
}

func TestPeopleNamedByAnythingButTheirIdAreLeftOut(t *testing.T) {
	got := people(context.Background(), []string{uuid.NewString(), "not-an-id", " " + uuid.NewString() + " "})
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}
