//go:build integration

package business

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/tests/integration"
)

// Widgets go on a dashboard together, against Postgres 12 with every
// migration: AddWidgets keeps every one or none, at the cap (MaxWidgets) and
// when one of them fails its check, so a seed or a template that puts many on
// at once never leaves a dashboard part made. AddWidget is one of these.
//
// Run: go test -tags=integration ./business/Dashboard/ -run TestWidgetsGoOnTogether -v
func TestWidgetsGoOnTogether(t *testing.T) {
	ctx := context.Background()
	integration.ConnectServerPostgres(t)
	ana := uuid.New()
	d, err := Add(ctx, Dashboard{Scope: ScopeMine, MineOf: ana, Name: "Ana's week"}, ana)
	if err != nil {
		t.Fatalf("adding ana's dashboard: %v", err)
	}
	row := func(y int) Widget { return Widget{Kind: KindThroughput, X: 0, Y: y, W: 6, H: 1} }
	rows := func(from, n int) []Widget {
		out := make([]Widget, n)
		for i := range out {
			out[i] = row(from + i)
		}
		return out
	}
	count := func(want int, after string) {
		t.Helper()
		ws, err := Widgets(ctx, d)
		if err != nil {
			t.Fatalf("reading the widgets after %s: %v", after, err)
		}
		if len(ws) != want {
			t.Fatalf("after %s the dashboard has %d widgets, want %d", after, len(ws), want)
		}
		for i := 1; i < len(ws); i++ {
			if ws[i].Y < ws[i-1].Y {
				t.Fatalf("after %s the widgets aren't in reading order: %v", after, ws)
			}
		}
	}

	t.Run("three at once all go on, each with its own id", func(t *testing.T) {
		made, err := AddWidgets(ctx, d, rows(0, 3), ana)
		if err != nil || len(made) != 3 {
			t.Fatalf("AddWidgets(3) = %d, %v", len(made), err)
		}
		seen := map[string]bool{}
		for _, w := range made {
			if _, err := uuid.Parse(w.ID); err != nil || seen[w.ID] {
				t.Fatalf("widget ids %v: each must be a new uuid", made)
			}
			seen[w.ID] = true
		}
		count(3, "three at once")
	})

	t.Run("one that fails its check keeps them all off", func(t *testing.T) {
		bad := rows(3, 3)
		bad[1].W = Columns + 1
		if _, err := AddWidgets(ctx, d, bad, ana); err == nil {
			t.Fatal("a batch with a widget off the grid went on")
		}
		count(3, "a refused batch")
	})

	t.Run("at the cap: a batch that doesn't fit takes none, one that fits fills it", func(t *testing.T) {
		if _, err := AddWidgets(ctx, d, rows(3, MaxWidgets-5), ana); err != nil {
			t.Fatalf("filling to %d: %v", MaxWidgets-2, err)
		}
		count(MaxWidgets-2, "filling")
		if _, err := AddWidgets(ctx, d, rows(100, 3), ana); !errors.Is(err, ErrFull) {
			t.Fatalf("three more with room for two = %v, want ErrFull", err)
		}
		count(MaxWidgets-2, "a batch with no room")
		if _, err := AddWidgets(ctx, d, rows(100, 2), ana); err != nil {
			t.Fatalf("two more with room for two: %v", err)
		}
		count(MaxWidgets, "the last two")
		if _, err := AddWidget(ctx, d, row(200), ana); !errors.Is(err, ErrFull) {
			t.Fatalf("one more on a full dashboard = %v, want ErrFull", err)
		}
		count(MaxWidgets, "one more on a full one")
	})
}
