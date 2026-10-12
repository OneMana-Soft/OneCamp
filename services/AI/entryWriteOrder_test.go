package ai

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Two writes to one entry land in the order they were asked for, however long
// each takes to compute: an agent's new doc is written with its title, then
// with its text, and the title-only write must not land last. Writes to other
// entries don't wait.
func TestWritesToOneEntryLandInOrder(t *testing.T) {
	var mu sync.Mutex
	var landed []string
	slow := make(chan struct{})
	restore := storeEmbeddingFn
	defer func() { storeEmbeddingFn = restore }()
	storeEmbeddingFn = func(_ context.Context, doc EmbeddingDoc) error {
		if doc.ContentText == "title only" {
			<-slow // its vector takes a while
		}
		mu.Lock()
		landed = append(landed, doc.ContentUUID+"="+doc.ContentText)
		mu.Unlock()
		return nil
	}
	wait := func(n int) string {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			mu.Lock()
			got := strings.Join(landed, ",")
			k := len(landed)
			mu.Unlock()
			if k >= n {
				return got
			}
		}
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(landed, ",")
	}

	StoreEmbeddingAsync(EmbeddingDoc{ContentType: "doc", ContentUUID: "d1", ContentText: "title only"})
	StoreEmbeddingAsync(EmbeddingDoc{ContentType: "doc", ContentUUID: "d1", ContentText: "title and text"})
	StoreEmbeddingAsync(EmbeddingDoc{ContentType: "doc", ContentUUID: "d2", ContentText: "another doc"})

	// The other doc's write doesn't wait for d1's.
	if got := wait(1); got != "d2=another doc" {
		t.Fatalf("before d1's first write finished, landed [%s], want only d2's", got)
	}
	close(slow)
	if got := wait(3); got != "d2=another doc,d1=title only,d1=title and text" {
		t.Errorf("landed [%s]; d1's writes must land in the order they were asked for", got)
	}

	left := -1
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && left != 0; time.Sleep(5 * time.Millisecond) {
		entryWrites.mu.Lock()
		left = len(entryWrites.last)
		entryWrites.mu.Unlock()
	}
	if left != 0 {
		t.Errorf("%d entries still queued after every write finished", left)
	}
}
