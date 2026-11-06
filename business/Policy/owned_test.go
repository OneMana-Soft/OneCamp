package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// An owned policy (the retention rule) is set only through its panel: the
// admin's list doesn't show it, Set doesn't take it, and SetOwned takes it only
// for its owner, through the same check, record and undo as every policy, with
// the panel's note and details in the record.
func TestOwnedPoliciesAreSetOnlyThroughTheirPanel(t *testing.T) {
	s := &store{values: map[string]string{}}
	r := withStore(t, s)
	var meta map[string]interface{}
	inner := recordIt
	recordIt = func(ctx context.Context, id *uuid.UUID, email, kind, action, category, summary string, m map[string]interface{}) error {
		meta = m
		return inner(ctx, id, email, kind, action, category, summary, m)
	}
	ctx := context.Background()
	key := CalendarRetentionDays.Key

	for _, in := range r.ListAll(ctx) {
		if in.Key == key {
			t.Fatal("the retention rule is in the admin's list of policies, where it could be saved without its count")
		}
	}
	if _, err := r.Set(ctx, key, json.RawMessage("30"), admin); !errors.Is(err, ErrUnknownPolicy) {
		t.Fatalf("Set took the retention rule: %v", err)
	}
	if _, err := r.SetOwned(ctx, "docs", key, json.RawMessage("30"), admin, "", nil); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("another panel set it: %v", err)
	}
	if _, err := r.SetOwned(ctx, OwnerRetention, key, json.RawMessage("-1"), admin, "", nil); !errors.Is(err, ErrBadValue) {
		t.Fatalf("a negative rule: %v", err)
	}
	if len(s.values) != 0 || len(s.records) != 0 {
		t.Fatalf("a refused change was stored or recorded: %v %v", s.values, s.records)
	}

	in, err := r.SetOwned(ctx, OwnerRetention, key, json.RawMessage("30"), admin,
		"9 calendar events would be removed at the next sweep", map[string]interface{}{"would_remove": 9, "policy": "spoofed"})
	if err != nil || in.Value != 30 || !in.IsSet {
		t.Fatalf("set: %+v %v", in, err)
	}
	if len(s.records) != 1 || !strings.HasSuffix(s.records[0], "from 0 to 30; 9 calendar events would be removed at the next sweep") ||
		!strings.HasPrefix(s.records[0], "human|policy.changed|settings|") {
		t.Fatalf("the record: %v", s.records)
	}
	if meta["would_remove"] != 9 || meta["policy"] != key {
		t.Fatalf("the record's details keep the panel's and never replace the policy's own: %v", meta)
	}
	if got, err := r.OwnedInfo(ctx, OwnerRetention, key); err != nil || got.Value != 30 || *got.Max != 3650 {
		t.Fatalf("OwnedInfo: %+v %v", got, err)
	}

	s.failRec = true
	if _, err := r.SetOwned(ctx, OwnerRetention, key, json.RawMessage("7"), admin, "", nil); !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("an unrecorded change: %v", err)
	}
	if s.values[key] != "30" {
		t.Fatalf("an unrecorded change wasn't undone: %q", s.values[key])
	}
}

// The sweep reads every kind's rule in ONE read before it removes anything
// (Ints), and an owned policy's stored value is in that read: the registry
// asks the store for the owned keys too, or a saved rule would read as 0 and
// never sweep.
func TestAnOwnedPolicyIsReadBackAndReadInOneBatch(t *testing.T) {
	s := &store{values: map[string]string{CalendarRetentionDays.Key: "30"}}
	r := withStore(t, s)
	ctx := context.Background()
	if got := r.Int(ctx, CalendarRetentionDays); got != 30 {
		t.Fatalf("the saved retention rule reads as %d, want 30", got)
	}
	reads := 0
	inner := readValues
	readValues = func(keys []string) (map[string]string, error) { reads++; return inner(keys) }
	r.forget()
	other := IntPolicy{Key: "never.registered", Default: 4}
	got := r.Ints(ctx, []IntPolicy{CalendarRetentionDays, other})
	if len(got) != 2 || got[CalendarRetentionDays.Key] != 30 || got[other.Key] != 4 || reads != 1 {
		t.Fatalf("Ints: %v in %d reads, want the stored 30 and the unknown key's default 4 in one", got, reads)
	}
}
