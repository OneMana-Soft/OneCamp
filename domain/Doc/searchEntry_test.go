package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A doc's entry in the docs index says who may find it, false and empty
// included: global search grants a public doc by doc_private:false, and an
// entry left without it was found by nobody but the people it was shared with
// (on the demo, all 14 entries lacked it). An empty sharing list is written as
// empty, or a whole write would carry nothing for it; a deleted doc's entry
// says it is deleted and a live one's says nothing about it.
func TestASearchEntrySaysWhoMayFindTheDoc(t *testing.T) {
	yes, no := true, false
	created, updated, deleted, live := time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour), time.Now().Add(-time.Minute), time.Time{}
	// Named as everywhere else: their display name before their full name.
	owner := &dgraphStruct.DgraphUser{Uuid: "owner", UserName: "Dana Whitfield", UserFullName: "Dana W."}
	entry := func(d *dgraphStruct.DgraphDoc) map[string]any {
		t.Helper()
		raw, err := json.Marshal(DocSearchEntryOf(d))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	public := entry(&dgraphStruct.DgraphDoc{Uuid: "pub", Title: "Handbook", IsPrivate: &no, CreatedBy: owner,
		CreatedAt: &created, UpdatedAt: &updated, DeletedAt: &live})
	if v, ok := public["doc_private"]; !ok || v != false {
		t.Errorf("a public doc's entry: doc_private = %v (written: %v), want false written out", v, ok)
	}
	for _, list := range []string{"doc_reading_users", "doc_editing_users", "doc_commenting_users"} {
		if v, ok := public[list].([]any); !ok || len(v) != 0 {
			t.Errorf("an unshared doc's %s = %#v, want [] written out", list, public[list])
		}
	}
	if _, ok := public["deleted_date"]; ok {
		t.Error("a live doc's entry says it is deleted")
	}
	if public["doc_created_by_user_id"] != "owner" || public["doc_created_by_user_full_name"] != "Dana Whitfield" {
		t.Errorf("the entry's maker: %v %v", public["doc_created_by_user_id"], public["doc_created_by_user_full_name"])
	}
	if public["created_date"] != float64(created.Unix()) || public["updated_date"] != float64(updated.Unix()) {
		t.Errorf("the entry's dates: %v %v", public["created_date"], public["updated_date"])
	}

	// No privacy recorded reads as CanRead reads it: not private.
	if v := entry(&dgraphStruct.DgraphDoc{Uuid: "old", CreatedBy: owner})["doc_private"]; v != false {
		t.Errorf("a doc with no privacy recorded: doc_private = %v, want false", v)
	}

	shared := entry(&dgraphStruct.DgraphDoc{Uuid: "plans", IsPrivate: &yes, CreatedBy: owner, DeletedAt: &deleted,
		ReadingUser:    []*dgraphStruct.DgraphUser{{Uuid: "reader"}},
		EditingUser:    []*dgraphStruct.DgraphUser{{Uuid: "owner"}, {Uuid: "editor"}, nil},
		CommentingUser: []*dgraphStruct.DgraphUser{{Uuid: "commenter"}}})
	if shared["doc_private"] != true {
		t.Errorf("a private doc's entry: doc_private = %v", shared["doc_private"])
	}
	want := map[string][]string{"doc_reading_users": {"reader"}, "doc_editing_users": {"owner", "editor"}, "doc_commenting_users": {"commenter"}}
	for list, people := range want {
		got, _ := shared[list].([]any)
		if len(got) != len(people) {
			t.Errorf("%s = %v, want %v", list, got, people)
			continue
		}
		for i := range people {
			if got[i] != people[i] {
				t.Errorf("%s = %v, want %v", list, got, people)
			}
		}
	}
	if shared["deleted_date"] != float64(deleted.Unix()) {
		t.Errorf("a deleted doc's entry: deleted_date = %v, want %d", shared["deleted_date"], deleted.Unix())
	}
}

// Every change asks for its doc's entry to be written, and each write reads
// the doc afresh. Asked again while a write is running, it writes once more
// after that one, however many times it was asked: so the last write is of
// the doc as it last was, and a burst of saves costs two writes, not one each.
func TestDocSearchWritesFollowTheLastChange(t *testing.T) {
	old := writeDocSearch
	t.Cleanup(func() { writeDocSearch = old })

	var mu sync.Mutex
	var calls []bool
	started, release := make(chan struct{}, 10), make(chan struct{})
	done := make(chan struct{}, 10)
	writeDocSearch = func(_ context.Context, docUUID string, children bool) error {
		mu.Lock()
		calls = append(calls, children)
		n := len(calls)
		mu.Unlock()
		started <- struct{}{}
		if n == 1 {
			<-release // the first write is slow: everything else arrives meanwhile
		}
		done <- struct{}{}
		return nil
	}

	SyncDocSearch("doc-1", false)
	<-started
	for i := 0; i < 5; i++ {
		SyncDocSearch("doc-1", i == 2) // one of them changed the doc's sharing
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the writes never finished")
		}
	}
	// Let any extra write show itself.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("%d writes for one running and five more asks, want 2", len(calls))
	}
	if calls[0] || !calls[1] {
		t.Errorf("which writes covered comments and files: %v, want [false true]", calls)
	}
	docSearchSyncMu.Lock()
	defer docSearchSyncMu.Unlock()
	if len(docSearchSyncs) != 0 {
		t.Errorf("%d docs still marked as being written", len(docSearchSyncs))
	}
}

// swapDocSearchWrite has the doc search writes made by write for the rest of
// the test, and returns a release that lets every held write go on. When the
// test ends it releases them, waits for them, and puts the real write back.
func swapDocSearchWrite(t *testing.T, write func(held <-chan struct{}, docUUID string, children bool) error) (release func()) {
	t.Helper()
	old := writeDocSearch
	held := make(chan struct{})
	release = sync.OnceFunc(func() { close(held) })
	t.Cleanup(func() {
		release()
		if n := WaitDocSearchWrites(context.Background(), 10*time.Second); n != 0 {
			t.Errorf("%d docs still being written when the test ended", n)
		}
		writeDocSearch = old
	})
	writeDocSearch = func(_ context.Context, docUUID string, children bool) error {
		return write(held, docUUID, children)
	}
	return release
}

// The retention archive and restore ask for a write for every doc they move,
// up to 5000 a run, and each write holds the doc's whole body in memory: all
// at once, a 4 GB install ran out. At most docSearchWriters run at once; the
// rest wait their turn, and every doc is still written.
func TestDocSearchWritesRunAFewAtATime(t *testing.T) {
	var running, most, wrote atomic.Int32
	release := swapDocSearchWrite(t, func(held <-chan struct{}, _ string, _ bool) error {
		n := running.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		<-held
		running.Add(-1)
		wrote.Add(1)
		return nil
	})

	const docs = 100
	for i := 0; i < docs; i++ {
		SyncDocSearch(fmt.Sprintf("archived-%d", i), false)
	}
	// Let as many writes start as will.
	for deadline := time.Now().Add(5 * time.Second); running.Load() < docSearchWriters && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if n := most.Load(); n > docSearchWriters {
		t.Errorf("%d writes ran at once for %d docs, want at most %d", n, docs, docSearchWriters)
	}
	release()
	if n := WaitDocSearchWrites(context.Background(), 10*time.Second); n != 0 {
		t.Fatalf("%d docs still being written", n)
	}
	if n := most.Load(); n > docSearchWriters {
		t.Errorf("%d writes ran at once for %d docs, want at most %d", n, docs, docSearchWriters)
	}
	if n := wrote.Load(); n != docs {
		t.Errorf("%d of %d docs written", n, docs)
	}
}

// A write that panics (helpers.GoSafeNamed recovers it) leaves its doc no
// longer marked as being written: left marked, every later ask only marked it
// again, and its entry was not written until a restart. An ask that came
// during the write that panicked is written after it.
func TestADocSearchWriteThatPanicsDoesNotStopTheNext(t *testing.T) {
	var calls atomic.Int32
	wrote := make(chan bool, 10)
	inPanicky := make(chan struct{})
	release := swapDocSearchWrite(t, func(held <-chan struct{}, _ string, children bool) error {
		switch calls.Add(1) {
		case 1:
			panic("a doc shaped as no write expected")
		case 3:
			close(inPanicky)
			<-held
			panic("a doc shaped as no write expected")
		}
		wrote <- children
		return nil
	})
	written := func(failure string) (children bool) {
		t.Helper()
		select {
		case children = <-wrote:
		case <-time.After(3 * time.Second):
			t.Fatal(failure)
		}
		return children
	}

	SyncDocSearch("doc-p", false)
	if n := WaitDocSearchWrites(context.Background(), 3*time.Second); n != 0 {
		t.Errorf("after its write panicked, %d docs still marked as being written", n)
	}
	SyncDocSearch("doc-p", false)
	written("after a write that panicked, the doc's entry was never written again")
	if n := WaitDocSearchWrites(context.Background(), 3*time.Second); n != 0 {
		t.Fatalf("%d docs still being written", n)
	}

	// The third write panics with an ask that changed the doc's sharing
	// waiting behind it.
	SyncDocSearch("doc-p", false)
	<-inPanicky
	SyncDocSearch("doc-p", true)
	release()
	if !written("an ask made during a write that panicked was never written") {
		t.Error("the ask made during a write that panicked was written without its doc's comments and files")
	}
}

// repairdocs exits once it has restored the docs, and their search entries
// are written in the background: the wait it makes first returns once every
// write has finished, those waiting their turn included, and not before.
func TestWaitingForDocSearchWritesEndsWhenTheyHaveFinished(t *testing.T) {
	var finished atomic.Int32
	release := swapDocSearchWrite(t, func(held <-chan struct{}, _ string, _ bool) error {
		<-held
		finished.Add(1)
		return nil
	})

	const docs = 3 * docSearchWriters // most of them wait their turn
	for i := 0; i < docs; i++ {
		SyncDocSearch(fmt.Sprintf("restored-%d", i), false)
	}
	if n := WaitDocSearchWrites(context.Background(), 50*time.Millisecond); n != docs {
		t.Errorf("with every write held, the wait gave up with %d docs left, want %d", n, docs)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if n := WaitDocSearchWrites(ended, time.Minute); n != docs {
		t.Errorf("with its context ended, the wait gave up with %d docs left, want %d", n, docs)
	}

	time.AfterFunc(50*time.Millisecond, release)
	if n := WaitDocSearchWrites(context.Background(), 10*time.Second); n != 0 {
		t.Fatalf("the wait gave up with %d docs left", n)
	}
	if n := finished.Load(); n != docs {
		t.Errorf("the wait returned with %d of %d writes finished", n, docs)
	}
}

// A preview stored as markup by older saves is written as its text; a plain
// one is kept as it is, "<" and all.
func TestASearchEntryPreviewIsText(t *testing.T) {
	old := DocSearchEntryOf(&dgraphStruct.DgraphDoc{Uuid: "d", Snippet: `<p xmlns="http://www.w3.org/1999/xhtml">Test</p><p>Plan</p>`})
	if strings.Contains(old.DocSnippet, "<") || !strings.Contains(old.DocSnippet, "Test") || !strings.Contains(old.DocSnippet, "Plan") {
		t.Errorf("a markup preview became %q", old.DocSnippet)
	}
	plain := DocSearchEntryOf(&dgraphStruct.DgraphDoc{Uuid: "d", Snippet: "if a<b then ship"})
	if plain.DocSnippet != "if a<b then ship" {
		t.Errorf("a plain preview became %q", plain.DocSnippet)
	}
}
