package ai

import (
	"context"
	"sync"
	"testing"
)

// resetReindexBuffer clears the package-level buffering state so tests are
// independent regardless of order.
func resetReindexBuffer() {
	reindexBufferMu.Lock()
	reindexBuffering = false
	reindexBuffer = nil
	reindexBufferDropped = 0
	reindexBufferMu.Unlock()
}

// TestBufferReindexWrite_DisabledByDefault verifies live writes are NOT
// buffered when no reindex is in progress (caller proceeds to the index).
func TestBufferReindexWrite_DisabledByDefault(t *testing.T) {
	resetReindexBuffer()
	if bufferReindexWrite(EmbeddingDoc{ContentType: "post", ContentUUID: "1"}, false) {
		t.Fatal("write should NOT be buffered when buffering is disabled")
	}
}

// TestBufferReindexWrite_CapturesWhileArmed verifies writes/deletes are
// captured once buffering is armed, and that arming resets prior state.
func TestBufferReindexWrite_CapturesWhileArmed(t *testing.T) {
	resetReindexBuffer()
	beginReindexBuffering()

	if !bufferReindexWrite(EmbeddingDoc{ContentType: "post", ContentUUID: "1"}, false) {
		t.Fatal("write should be buffered while armed")
	}
	if !bufferReindexWrite(EmbeddingDoc{ContentType: "chat", ContentUUID: "2"}, true) {
		t.Fatal("delete should be buffered while armed")
	}

	reindexBufferMu.Lock()
	n := len(reindexBuffer)
	tomb := reindexBuffer[1].tombstone
	reindexBufferMu.Unlock()

	if n != 2 {
		t.Fatalf("expected 2 buffered writes, got %d", n)
	}
	if !tomb {
		t.Error("second buffered entry should be a tombstone (delete)")
	}

	// Re-arming must clear prior buffer.
	beginReindexBuffering()
	reindexBufferMu.Lock()
	n = len(reindexBuffer)
	reindexBufferMu.Unlock()
	if n != 0 {
		t.Fatalf("re-arming should reset buffer, got %d entries", n)
	}
	resetReindexBuffer()
}

// TestAbortReindexBuffering_DisablesAndClears verifies abort tears down the
// buffer so subsequent live writes go direct again.
func TestAbortReindexBuffering_DisablesAndClears(t *testing.T) {
	resetReindexBuffer()
	beginReindexBuffering()
	bufferReindexWrite(EmbeddingDoc{ContentType: "post", ContentUUID: "1"}, false)

	abortReindexBuffering(context.Background())

	if bufferReindexWrite(EmbeddingDoc{ContentType: "post", ContentUUID: "2"}, false) {
		t.Fatal("after abort, writes should NOT be buffered")
	}
	reindexBufferMu.Lock()
	n := len(reindexBuffer)
	reindexBufferMu.Unlock()
	if n != 0 {
		t.Fatalf("abort should clear buffer, got %d entries", n)
	}
}

// TestBufferReindexWrite_Concurrent ensures the buffer is race-free under
// concurrent live writes while armed (run with -race).
func TestBufferReindexWrite_Concurrent(t *testing.T) {
	resetReindexBuffer()
	beginReindexBuffering()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bufferReindexWrite(EmbeddingDoc{ContentType: "post", ContentUUID: "x"}, i%2 == 0)
		}(i)
	}
	wg.Wait()

	reindexBufferMu.Lock()
	n := len(reindexBuffer)
	reindexBufferMu.Unlock()
	if n != 50 {
		t.Fatalf("expected 50 buffered writes, got %d", n)
	}
	resetReindexBuffer()
}
