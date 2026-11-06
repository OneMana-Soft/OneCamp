package business

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	"github.com/google/uuid"
)

// pool is a fake kind's items: how many a rule may remove, the cutoffs its
// removals were asked for, and a removal that fails from a given call on.
type pool struct {
	left    int64
	cutoffs []time.Time
	failAt  int
}

func (p *pool) kind() Kind {
	return Kind{Name: "fake", Label: "fake items", Policy: policyBusiness.IntPolicy{Key: "fake.retention.days"},
		Count: func(context.Context, time.Time) (int64, error) { return p.left, nil },
		Purge: func(_ context.Context, cutoff time.Time, limit int) (int64, error) {
			p.cutoffs = append(p.cutoffs, cutoff)
			if p.failAt > 0 && len(p.cutoffs) >= p.failAt {
				return 0, errors.New("database gone")
			}
			n := min(int64(limit), p.left)
			p.left -= n
			return n, nil
		},
		Search: func(context.Context, []uuid.UUID, Query, int) ([]Item, error) { return nil, nil },
		Export: func(context.Context, io.Writer, []uuid.UUID, Query, string, Part) (Written, error) {
			return Written{}, nil
		},
		Formats: []string{"json"},
	}
}

type row struct {
	actorKind, action, category, summary string
	meta                                 map[string]interface{}
}

// harness swaps the registry and every seam for the test's.
func harness(t *testing.T, p *pool, days int, held int) *[]row {
	t.Helper()
	kindsMu.Lock()
	prevKinds := kinds
	kinds = map[string]Kind{}
	kindsMu.Unlock()
	prevDays, prevHeld, prevSys, prevNow := rulesOf, heldNow, recordSystem, now
	t.Cleanup(func() {
		kindsMu.Lock()
		kinds = prevKinds
		kindsMu.Unlock()
		rulesOf, heldNow, recordSystem, now = prevDays, prevHeld, prevSys, prevNow
	})
	Register(p.kind())
	at := time.Date(2026, 11, 2, 23, 0, 0, 0, time.UTC)
	now = func() time.Time { return at }
	rulesOf = func(_ context.Context, ps []policyBusiness.IntPolicy) map[string]int {
		out := map[string]int{}
		for _, p := range ps {
			out[p.Key] = days
		}
		return out
	}
	heldNow = func(context.Context) ([]uuid.UUID, error) { return make([]uuid.UUID, held), nil }
	rows := &[]row{}
	recordSystem = func(_ context.Context, id *uuid.UUID, email, kind, action, category, summary string, meta map[string]interface{}) error {
		if id != nil || email != "" {
			t.Errorf("a sweep's row names a person: %v %q", id, email)
		}
		*rows = append(*rows, row{kind, action, category, summary, meta})
		return nil
	}
	return rows
}

func TestRegisterRefusesAKindMissingAPart(t *testing.T) {
	p := &pool{}
	harness(t, p, 0, 0)
	k := p.kind()
	k.Name, k.Purge = "nopurge", nil
	Register(k)
	k = p.kind()
	k.Name, k.Formats = "noformats", nil
	Register(k)
	if got := Kinds(); len(got) != 1 || got[0].Name != "fake" {
		t.Fatalf("kinds: %+v", got)
	}
}

// Off by default: with every rule at 0 a sweep removes nothing and writes
// nothing.
func TestASweepWithEveryRuleOffRemovesAndRecordsNothing(t *testing.T) {
	p := &pool{left: 40}
	rows := harness(t, p, 0, 3)
	if err := Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.cutoffs) != 0 || p.left != 40 || len(*rows) != 0 {
		t.Fatalf("removals %d, left %d, rows %+v", len(p.cutoffs), p.left, *rows)
	}
}

// A sweep removes in batches up to what the rule allows, with the rule's
// cutoff, and writes ONE row with its counts, as the workspace itself.
func TestASweepRemovesInBatchesAndWritesOneRowWithItsCounts(t *testing.T) {
	p := &pool{left: 1234}
	rows := harness(t, p, 30, 2)
	if err := Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := now().Add(-30 * 24 * time.Hour)
	if len(p.cutoffs) != 3 || p.left != 0 || !p.cutoffs[0].Equal(want) {
		t.Fatalf("removals %v, left %d", p.cutoffs, p.left)
	}
	if len(*rows) != 1 {
		t.Fatalf("rows: %+v", *rows)
	}
	r := (*rows)[0]
	if r.actorKind != auditBusiness.ActorSystem || r.action != "retention.swept" || r.category != auditBusiness.CategorySecurity ||
		r.summary != "Retention removed 1234 fake items; 2 people on hold kept theirs" {
		t.Fatalf("row: %+v", r)
	}
	if removed, _ := r.meta["removed"].(map[string]int64); removed["fake"] != 1234 || r.meta["held_people"] != 2 {
		t.Fatalf("meta: %+v", r.meta)
	}
}

// A sweep that stops early still writes its row, with what it removed.
func TestASweepThatStopsEarlyStillRecordsWhatItRemoved(t *testing.T) {
	p := &pool{left: 1234, failAt: 2}
	rows := harness(t, p, 30, 1)
	if err := Sweep(context.Background()); err == nil {
		t.Fatal("a sweep that stopped early answered no error")
	}
	if len(*rows) != 1 || !strings.Contains((*rows)[0].summary, "500 fake items; 1 person on hold kept theirs (stopped early: fake)") {
		t.Fatalf("rows: %+v", *rows)
	}
}

// fakePolicies is the Policy Registry's owned half, recording what's set.
type fakePolicies struct {
	days        int
	note, value string
	extra       map[string]interface{}
}

func (f *fakePolicies) OwnedInfo(_ context.Context, owner, key string) (policyBusiness.Info, error) {
	lo, hi := 0, 3650
	return policyBusiness.Info{Key: key, Label: "Remove fake items", Value: f.days, Min: &lo, Max: &hi}, nil
}

func (f *fakePolicies) SetOwned(_ context.Context, owner, key string, value json.RawMessage, _ policyBusiness.Changer, note string, extra map[string]interface{}) (policyBusiness.Info, error) {
	if owner != policyBusiness.OwnerRetention {
		return policyBusiness.Info{}, policyBusiness.ErrNotOwner
	}
	f.value, f.note, f.extra = string(value), note, extra
	return policyBusiness.Info{}, nil
}

// A rule is saved only with the count the panel showed, and only while that is
// still what it would remove: a count that moved is refused with the count
// now, nothing saved. Its record says both.
func TestARuleIsSavedOnlyAfterItsCountWasShown(t *testing.T) {
	p := &pool{left: 9}
	harness(t, p, 0, 0)
	f := &fakePolicies{}
	prev := policies
	policies = f
	t.Cleanup(func() { policies = prev })
	ctx := context.Background()
	by := policyBusiness.Changer{UserID: uuid.New()}

	if _, err := SetRule(ctx, by, "fake", 30, nil); !errors.Is(err, ErrBadRequest) || f.value != "" {
		t.Fatalf("saved without its count: %v %q", err, f.value)
	}
	if _, err := SetRule(ctx, by, "nope", 30, new(int64)); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("an unknown kind: %v", err)
	}
	if _, err := Preview(ctx, "fake", -1); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("a negative preview: %v", err)
	}
	if n, err := Preview(ctx, "fake", 0); n != 0 || err != nil {
		t.Fatalf("a rule of 0 removes nothing: %d %v", n, err)
	}
	shown := int64(7)
	_, err := SetRule(ctx, by, "fake", 30, &shown)
	var moved *CountMoved
	if !errors.Is(err, ErrCountMoved) || !errors.As(err, &moved) || moved.Now != 9 || f.value != "" {
		t.Fatalf("7 shown and 9 now: %v %+v %q, want the count now and nothing saved", err, moved, f.value)
	}
	shown = 9
	f.days = 30
	r, err := SetRule(ctx, by, "fake", 30, &shown)
	if err != nil || r.Days != 30 || r.Max != 3650 {
		t.Fatalf("set: %+v %v", r, err)
	}
	if f.value != "30" || f.note != "9 fake items would be removed at the next sweep" ||
		f.extra["would_remove"] != int64(9) || f.extra["shown"] != int64(9) || f.extra["kind"] != "fake" {
		t.Fatalf("recorded: %q %q %+v", f.value, f.note, f.extra)
	}
	// Off removes nothing, so its count is 0: 9 shown is a count that moved.
	if _, err := SetRule(ctx, by, "fake", 0, &shown); !errors.Is(err, ErrCountMoved) || f.value != "30" {
		t.Fatalf("turning it off with 9 shown: %v %q", err, f.value)
	}
	if _, err := SetRule(ctx, by, "fake", 0, new(int64)); err != nil || f.value != "0" || f.note != "nothing is removed" {
		t.Fatalf("turning it off: %v %q %q", err, f.value, f.note)
	}
}

// A hold change is only who it changes: those added who weren't on the hold
// and those taken off who were, each once, nobody (the nil id) never; and its
// row's words are what changed, or no row at all when nothing did.
func TestAHoldChangeIsOnlyWhatItChanges(t *testing.T) {
	maya, lee, sam := uuid.New(), uuid.New(), uuid.New()
	adds, removes := peopleChange([]uuid.UUID{maya, lee}, []uuid.UUID{maya, sam, sam}, []uuid.UUID{lee, lee, sam, uuid.Nil})
	if len(adds) != 1 || adds[0] != sam || len(removes) != 1 || removes[0] != lee {
		t.Fatalf("Sam added and Lee taken off a hold on Maya and Lee: %v %v", adds, removes)
	}
	if adds, removes := peopleChange([]uuid.UUID{maya}, []uuid.UUID{maya}, []uuid.UUID{sam, uuid.Nil}); len(adds)+len(removes) != 0 {
		t.Fatalf("Maya (on it) added and Sam (not on it) taken off changed %v %v", adds, removes)
	}
	for _, c := range []struct {
		change holdChange
		want   string
	}{
		{holdChange{was: "Vendor dispute", name: "Vendor dispute"}, ""},
		{holdChange{was: "Vendor dispute", name: "Vendor dispute 2026", removed: []uuid.UUID{sam}}, "Changed hold “Vendor dispute 2026”: renamed it from “Vendor dispute”, took off 1 person"},
		{holdChange{was: "Vendor dispute", name: "Vendor dispute", noted: true, added: []uuid.UUID{sam, lee}}, "Changed hold “Vendor dispute”: changed its note, added 2 people"},
	} {
		if got := c.change.summary(); got != c.want {
			t.Errorf("%+v reads %q, want %q", c.change, got, c.want)
		}
	}
}

// V-388 (1): a part of a hold's export that isn't the whole export says so in
// words an admin reads, and names the part that holds the rest; a whole export
// says nothing.
func TestAPartOfAnExportSaysItIsOne(t *testing.T) {
	const label = "calendar events"
	if note := PartNote(label, Part{Number: 1, Size: 20000}, Written{N: 12}); note != "" {
		t.Fatalf("a whole export says %q, want nothing", note)
	}
	for _, c := range []struct {
		part Part
		w    Written
		want string
	}{
		{Part{Number: 1, Size: 20000}, Written{N: 20000, Next: "c1"},
			"Part 1 of this hold's export: the first 20,000 calendar events that match, not all of them. More remain: export part 2 from the hold's search for the rest."},
		{Part{Number: 2, Size: 20000, After: "c1"}, Written{N: 20000, Next: "c2"},
			"Part 2 of this hold's export: the next 20,000 calendar events that match, not all of them. More remain: export part 3 from the hold's search for the rest."},
		{Part{Number: 3, Size: 20000, After: "c2"}, Written{N: 1234},
			"Part 3 of this hold's export, the last: 1,234 more calendar events that match. Parts 1 to 3 hold all of them."},
		{Part{Number: 2, Size: 20000, After: "c1"}, Written{N: 1},
			"Part 2 of this hold's export, the last: 1 more of the calendar events that match. Parts 1 to 2 hold all of them."},
		{Part{Number: 2, Size: 20000, After: "c1"}, Written{N: 0},
			"Part 2 of this hold's export, the last: 0 more calendar events that match. Parts 1 to 2 hold all of them."},
	} {
		if got := PartNote(label, c.part, c.w); got != c.want {
			t.Errorf("part %d (%+v):\n got %q\nwant %q", c.part.Number, c.w, got, c.want)
		}
	}
}

// The token for the next part gives back the part and the kind's cursor, and a
// token no export offered is refused (400), never read as part 1.
func TestAnExportsPartTokenRoundTripsAndRefusesOthers(t *testing.T) {
	const cursor = "2026-01-02T03:04:05.000006Z_7d0c2b1e-4b8a-4c1d-9e57-2f1f3c1a9b10"
	if p, err := partOf(partToken(2, cursor)); err != nil || p.Number != 2 || p.After != cursor {
		t.Fatalf("round trip: %+v %v", p, err)
	}
	if p, err := partOf(""); err != nil || p != (Part{Number: 1}) {
		t.Fatalf("no token: %+v %v, want part 1", p, err)
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, token := range []string{
		"1." + enc("x"), "0." + enc("x"), "-2." + enc("x"), "1000001." + enc("x"), "x." + enc("x"),
		"2", "2.", "2.!!", "2." + enc(strings.Repeat("x", maxCursor+1)), "2." + enc("\xff\xfe"),
	} {
		if p, err := partOf(token); !errors.Is(err, ErrBadRequest) {
			t.Errorf("token %q: %+v %v, want ErrBadRequest", token, p, err)
		}
	}
}
