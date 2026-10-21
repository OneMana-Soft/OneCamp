package helpers

import (
	"context"
	"errors"
	"testing"
)

func TestIndexByAndLoadMap(t *testing.T) {
	type p struct {
		id   string
		name string
	}
	m := IndexBy([]p{{"a", "ann"}, {"b", "bo"}, {"a", "ana"}}, func(x p) string { return x.id })
	if len(m) != 2 || m["a"].name != "ana" {
		t.Fatalf("index: %v", m)
	}
	var calls [][]string
	load := func(_ context.Context, ids []string) ([]p, error) {
		calls = append(calls, append([]string(nil), ids...))
		var out []p
		for _, id := range ids {
			if id != "gone" {
				out = append(out, p{id, "n-" + id})
			}
		}
		return out, nil
	}
	got, err := LoadMap(context.Background(), []string{"a", "b", "a", "c", "gone", "d", "e"}, 2, load, func(x p) string { return x.id })
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || len(calls[0]) != 2 || len(got) != 5 || got["c"].name != "n-c" {
		t.Fatalf("calls %v, got %v", calls, got)
	}
	if _, ok := got["gone"]; ok {
		t.Fatal("an id load didn't return is missing, not zero")
	}
	calls = nil
	if _, err := LoadMap(context.Background(), []string{"a", "b"}, 0, load, func(x p) string { return x.id }); err != nil || len(calls) != 1 {
		t.Fatalf("chunk 0 is one call: %v %v", calls, err)
	}
	if m, err := LoadMap(context.Background(), nil, 2, load, func(x p) string { return x.id }); err != nil || len(m) != 0 {
		t.Fatalf("no ids: %v %v", m, err)
	}
	boom := errors.New("boom")
	if _, err := LoadMap(context.Background(), []string{"a"}, 1, func(context.Context, []string) ([]p, error) { return nil, boom }, func(x p) string { return x.id }); !errors.Is(err, boom) {
		t.Fatalf("error: %v", err)
	}
}
