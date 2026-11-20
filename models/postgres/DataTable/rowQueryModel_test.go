package models

import (
	"testing"
	"time"
)

// A budgeted transaction's settings go as one statement text, built from
// constants and a formatted integer only: this pins the text a walk sends,
// its retry without workers, and a budgeted read with no settings of its own.
func TestTxSettingsAreOneConstantText(t *testing.T) {
	for _, c := range []struct {
		name     string
		settings []string
		budget   time.Duration
		want     string
	}{
		{"a walk", walkSettings, 149 * time.Millisecond, "SET LOCAL enable_sort = off; SET LOCAL enable_bitmapscan = off; SET LOCAL enable_hashjoin = off; SELECT set_config('max_parallel_workers_per_gather', '0', true); SET LOCAL work_mem = '64kB'; SET LOCAL statement_timeout = 149"},
		{"a walk without a budget", walkSettings, 0, "SET LOCAL enable_sort = off; SET LOCAL enable_bitmapscan = off; SET LOCAL enable_hashjoin = off; SELECT set_config('max_parallel_workers_per_gather', '0', true); SET LOCAL work_mem = '64kB'"},
		{"a summaries read", nil, 2 * time.Second, "SET LOCAL statement_timeout = 2000"},
		{"under a millisecond", nil, time.Microsecond, "SET LOCAL statement_timeout = 1"},
		{"nothing to set", nil, 0, ""},
	} {
		if got := txSettings(c.settings, c.budget); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if len(walkSettings) != 5 {
		t.Errorf("walkSettings changed (%d settings): pin its joined text again", len(walkSettings))
	}
}
