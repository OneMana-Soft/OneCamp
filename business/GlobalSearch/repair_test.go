package business

import "testing"

// A search's repairs end with it in a unit test. One left running in the
// background is made by whatever repairEntry is in force when it runs, so it
// lands in a later test that records repairs: a board test's repair once
// showed up in TestADroppedEventComesOffItsCountAndHasItsEntryRepairedOnce's
// list (28 Oct). TestMain runs every repair where it was asked for.
func TestNoRepairOutlivesTheSearchThatAskedForIt(t *testing.T) {
	ran := make(chan struct{}, 1)
	goRepairs(func() { ran <- struct{}{} })
	select {
	case <-ran:
	default:
		t.Fatal("a repair was still to run when goRepairs returned, so a later test could be handed it")
	}
}
