package business

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// withTitleReader stands a reader in for a kind for one test.
func withTitleReader(t *testing.T, kind string, read TitleReader) {
	t.Helper()
	titleReadersMu.Lock()
	titleReaders[kind] = read
	titleReadersMu.Unlock()
	t.Cleanup(func() {
		titleReadersMu.Lock()
		delete(titleReaders, kind)
		titleReadersMu.Unlock()
	})
}

var (
	sam   = Subject{UserUID: "0x1", UserUUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}
	agent = Subject{UserUID: "0x2", UserUUID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}
)

// shelf is a kind whose things each reader may open by a table, and the ids each read was asked.
type shelf struct {
	open  map[string]map[string]string // reader uuid -> id -> title
	asked [][]string
}

func (s *shelf) read(_ context.Context, ids []string, who Subject) (map[string]string, error) {
	s.asked = append(s.asked, ids)
	out := map[string]string{}
	for _, id := range ids {
		if title, ok := s.open[who.UserUUID][id]; ok {
			out[id] = title
		}
	}
	return out, nil
}

func TestATitleIsNamedOnlyWhenEveryReaderMayOpenIt(t *testing.T) {
	s := &shelf{open: map[string]map[string]string{
		sam.UserUUID:   {"plan": "Launch plan", "pay": "Salaries"},
		agent.UserUUID: {"plan": "Launch plan, as the agent reads it"},
	}}
	withTitleReader(t, "shelf", s.read)

	got, err := ReadableTitles(context.Background(), []Subject{sam, agent}, []ItemRef{{"shelf", "PLAN"}, {"shelf", "pay"}, {"shelf", "plan"}})
	if err != nil {
		t.Fatal(err)
	}
	// Keyed as asked, named as the first reader reads it; Salaries is Sam's alone.
	want := map[ItemRef]string{{"shelf", "PLAN"}: "Launch plan", {"shelf", "plan"}: "Launch plan"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// One read a reader, each thing once.
	if !reflect.DeepEqual(s.asked, [][]string{{"plan", "pay"}, {"plan", "pay"}}) {
		t.Fatalf("asked %v", s.asked)
	}

	got, err = ReadableTitles(context.Background(), []Subject{sam}, []ItemRef{{"shelf", "pay"}})
	if err != nil || got[ItemRef{"shelf", "pay"}] != "Salaries" {
		t.Fatalf("Sam alone: %v %v", got, err)
	}
}

func TestReadableTitlesFailsClosed(t *testing.T) {
	s := &shelf{open: map[string]map[string]string{sam.UserUUID: {"plan": "Launch plan"}, "": {"plan": "Launch plan"}}}
	withTitleReader(t, "shelf", s.read)
	broken := errors.New("the graph didn't answer")
	withTitleReader(t, "broken", func(context.Context, []string, Subject) (map[string]string, error) { return nil, broken })

	for name, readers := range map[string][]Subject{"no readers": nil, "a reader who is no one": {sam, {UserUID: "0x9"}}} {
		got, err := ReadableTitles(context.Background(), readers, []ItemRef{{"shelf", "plan"}})
		if err != nil || len(got) != 0 {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	got, err := ReadableTitles(context.Background(), []Subject{sam}, []ItemRef{{"shelf", "plan"}, {"spaceship", "plan"}})
	if err != nil || !reflect.DeepEqual(got, map[ItemRef]string{{"shelf", "plan"}: "Launch plan"}) {
		t.Errorf("a kind nobody registered: %v %v", got, err)
	}
	got, err = ReadableTitles(context.Background(), []Subject{sam}, []ItemRef{{"shelf", "plan"}, {"broken", "x"}})
	if !errors.Is(err, broken) || got != nil {
		t.Errorf("a read that failed: %v %v", got, err)
	}
}

func TestReadableTitlesReadsInBatches(t *testing.T) {
	s := &shelf{open: map[string]map[string]string{sam.UserUUID: {}}}
	refs := make([]ItemRef, 0, titleBatch+1)
	for i := range titleBatch + 1 {
		id := fmt.Sprintf("thing-%03d", i)
		s.open[sam.UserUUID][id] = strings.ToUpper(id)
		refs = append(refs, ItemRef{"shelf", id})
	}
	withTitleReader(t, "shelf", s.read)
	got, err := ReadableTitles(context.Background(), []Subject{sam}, refs)
	if err != nil || len(got) != titleBatch+1 {
		t.Fatalf("named %d of %d: %v", len(got), titleBatch+1, err)
	}
	if len(s.asked) != 2 || len(s.asked[0]) != titleBatch || len(s.asked[1]) != 1 {
		t.Fatalf("reads of %d ids", len(s.asked))
	}
}

func TestATitleReaderIsRegisteredOnce(t *testing.T) {
	withTitleReader(t, "shelf", (&shelf{}).read)
	defer func() {
		if recover() == nil {
			t.Fatal("a second reader for one kind was taken")
		}
	}()
	RegisterTitleReader("shelf", (&shelf{}).read)
}
