//go:build integration

package business

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/tests/integration"
)

// Against a real Redis: a ledger credits everyone added since its last drain,
// once, and an idle mark is claimed by one of many callers.
//
// Run: go test -tags=integration ./business/Snapshot/ -run TestTheLedgerAndMarksInRedis -v
func TestTheLedgerAndMarksInRedis(t *testing.T) {
	integration.SetupRedis(t)
	ctx := context.Background()
	l, m := redisLedger{}, redisMarks{}

	l.Add(ctx, "doc", "d1", []Contributor{{UserUUID: "ben"}, {UserUUID: "ana"}})
	l.Add(ctx, "doc", "d1", []Contributor{{UserUUID: "ana"}, {AgentUUID: "captain", AskerUUID: "sam"}})
	got := l.Drain(ctx, "doc", "d1")
	want := []Contributor{{UserUUID: "ana"}, {UserUUID: "ben"}, {AgentUUID: "captain", AskerUUID: "sam"}}
	if len(got) != len(want) {
		t.Fatalf("drained %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if again := l.Drain(ctx, "doc", "d1"); len(again) != 0 {
		t.Errorf("drained twice: %+v", again)
	}

	at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	m.Mark(ctx, "doc", "d2", at)
	due := m.Due(ctx, "doc", at.Add(time.Minute), 10)
	if len(due) != 1 || due[0].UUID != "d2" {
		t.Fatalf("due %+v", due)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.Claim(ctx, "doc", due[0]) {
				mu.Lock()
				claimed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if claimed != 1 {
		t.Errorf("claimed %d times", claimed)
	}
	// A mark moved by a newer edit is not taken on the old one.
	m.Mark(ctx, "doc", "d3", at)
	old := m.Due(ctx, "doc", at.Add(time.Minute), 10)[0]
	m.Mark(ctx, "doc", "d3", at.Add(30*time.Second))
	if m.Claim(ctx, "doc", old) {
		t.Error("a stale mark was claimed")
	}
}
