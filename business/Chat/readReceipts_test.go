package business

import (
	"testing"
	"time"
)

func TestReadReceiptsFollowTheRules(t *testing.T) {
	at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	never := time.Unix(0, 0).UTC()
	me, maya, jonas, agent := "me", "maya", "jonas", "agent"
	people := []receiptPerson{{me, false, true}, {maya, false, true}, {jonas, false, false}, {agent, true, true}}
	seen := map[string]time.Time{me: at, maya: at, jonas: at, agent: at}

	r := receiptsFor(me, people, seen, true)
	if !r.On || len(r.Seen) != 1 || r.Seen[0].UserUUID != maya || !r.Seen[0].At.Equal(at) {
		t.Fatalf("I see Maya's receipt only: not mine, not Jonas's (he doesn't share), not the agent's: %+v", r)
	}
	if r := receiptsFor(me, people, map[string]time.Time{maya: never}, true); !r.On || len(r.Seen) != 0 {
		t.Fatalf("a mark at nothing is a conversation never opened: %+v", r)
	}
	if r := receiptsFor(jonas, people, seen, true); r.On || len(r.Seen) != 0 {
		t.Fatalf("Jonas doesn't share his, so he sees nobody's: %+v", r)
	}
	if r := receiptsFor(me, people, seen, false); r.On || len(r.Seen) != 0 {
		t.Fatalf("the workspace has turned them off: %+v", r)
	}
	if r.Seen == nil {
		t.Fatal("no receipts is an empty list, not null")
	}

	// Twenty people are fine; twenty-one, and receipts stop meaning much. Bots don't count.
	crowd := []receiptPerson{{me, false, true}, {agent, true, true}}
	for i := 0; i < MaxReceiptPeople-1; i++ {
		crowd = append(crowd, receiptPerson{uuid: string(rune('a' + i)), shares: true})
	}
	if r := receiptsFor(me, crowd, seen, true); !r.On {
		t.Fatalf("%d people and an agent show receipts", MaxReceiptPeople)
	}
	crowd = append(crowd, receiptPerson{uuid: "one too many", shares: true})
	if r := receiptsFor(me, crowd, seen, true); r.On {
		t.Fatalf("%d people don't", MaxReceiptPeople+1)
	}
}
