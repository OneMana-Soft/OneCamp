package business

import (
	"context"
	"testing"
)

// The pass runs once: done, it reads nothing; while an index hasn't taken its
// copies (this server's mapping failed), it rewrites nothing there and stays
// open for the next run rather than being recorded.
func TestTheCopiesPassIsRecordedOnlyWhenEveryIndexHasItsCopies(t *testing.T) {
	savedDone, savedMark, savedRewrite := copiesDone, markCopiesDone, rewriteMissing
	t.Cleanup(func() { copiesDone, markCopiesDone, rewriteMissing = savedDone, savedMark, savedRewrite })
	rewrites, marks := 0, 0
	rewriteMissing = func(context.Context, []string, string) (int, error) { rewrites++; return 0, nil }
	markCopiesDone = func() error { marks++; return nil }

	copiesDone = func(string) (bool, error) { return true, nil }
	if err := BackfillQueryCopies(context.Background()); err != nil || rewrites != 0 || marks != 0 {
		t.Fatalf("a recorded pass ran again: %v, %d rewrites, %d marks", err, rewrites, marks)
	}
	// No index here took its copies (nothing mapped them in this test).
	copiesDone = func(string) (bool, error) { return false, nil }
	if err := BackfillQueryCopies(context.Background()); err != nil || rewrites != 0 || marks != 0 {
		t.Errorf("without the copies mapped: %v, %d rewrites, %d marks; want none and the pass left open", err, rewrites, marks)
	}
}
