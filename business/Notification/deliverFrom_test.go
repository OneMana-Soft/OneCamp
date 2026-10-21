package notification

import (
	"context"
	"errors"
	"testing"
)

func TestDeliverFromSendsVIPsThroughTheGate(t *testing.T) {
	oldVIP, oldNow, oldThrough := whoseVIP, deliverNow, deliverThrough
	t.Cleanup(func() { whoseVIP, deliverNow, deliverThrough = oldVIP, oldNow, oldThrough })

	var now, through []string
	deliverNow = func(_ context.Context, r []string, _ map[string]string, _ string) { now = r }
	deliverThrough = func(_ context.Context, r []string, d map[string]string, _ string) error {
		through = r
		d["written"] = "by the push"
		return nil
	}
	whoseVIP = func(_ context.Context, sender string, users []string) (map[string]bool, error) {
		if sender != "S" {
			t.Fatalf("asked about %q", sender)
		}
		return map[string]bool{"b0000000-0000-0000-0000-00000000000b": true}, nil
	}
	data := map[string]string{"title": "x"}
	DeliverFrom(context.Background(), "S", []string{"a", "B0000000-0000-0000-0000-00000000000B", "c"}, data, "n")
	if len(through) != 1 || through[0] != "B0000000-0000-0000-0000-00000000000B" {
		t.Fatalf("through the gate: %v", through)
	}
	if len(now) != 2 || now[0] != "a" || now[1] != "c" {
		t.Fatalf("as Deliver: %v", now)
	}
	if _, ok := data["written"]; ok {
		t.Fatal("the VIP push wrote into the others' data")
	}
}

func TestDeliverFromTellsEveryoneAsUsualWhenVIPsCantBeRead(t *testing.T) {
	oldVIP, oldNow, oldThrough := whoseVIP, deliverNow, deliverThrough
	t.Cleanup(func() { whoseVIP, deliverNow, deliverThrough = oldVIP, oldNow, oldThrough })
	var now []string
	deliverNow = func(_ context.Context, r []string, _ map[string]string, _ string) { now = r }
	deliverThrough = func(context.Context, []string, map[string]string, string) error {
		t.Fatal("went through the gate without knowing")
		return nil
	}
	whoseVIP = func(context.Context, string, []string) (map[string]bool, error) { return nil, errors.New("down") }
	DeliverFrom(context.Background(), "S", []string{"a", "b"}, map[string]string{}, "n")
	if len(now) != 2 {
		t.Fatalf("told %v", now)
	}
}
