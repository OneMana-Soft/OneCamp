package business

import (
	"context"
	"testing"
)

// Each sweep runs what its package set, read at each pass; with nothing set,
// a pass does nothing.
func TestASweepRunsWhatItsPackageSet(t *testing.T) {
	for _, j := range jobs {
		if err := (*j.run)(context.Background()); err != nil {
			t.Errorf("%s with nothing set: %v", j.name, err)
		}
	}
	saved := Cycles
	t.Cleanup(func() { Cycles = saved })
	ran := false
	Cycles = func(context.Context) error { ran = true; return nil }
	for _, j := range jobs {
		if j.name == "projects.cycles" {
			_ = (*j.run)(context.Background())
		}
	}
	if !ran {
		t.Error("the cycles sweep didn't run what was set")
	}
}
