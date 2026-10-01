package business

import "testing"

// The floor exists so a retention setting cannot be used to fail a compliance
// obligation by accident. Six months is the minimum the AI Act requires for
// automatically generated logs, and a UI that silently accepted 30 days would be
// worse than one with no setting at all.
func TestClampRetention(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero keeps everything, the default and previous behaviour", 0, 0},
		{"negative is not a short window, it is no window", -30, 0},
		{"below the floor is raised, never accepted", 30, MinRetentionDays},
		{"one day below the floor is still below it", MinRetentionDays - 1, MinRetentionDays},
		{"exactly the floor is allowed", MinRetentionDays, MinRetentionDays},
		{"above the floor is kept as asked", 365, 365},
		{"a long window is not capped: keeping more is never the risk", 3650, 3650},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clampRetention(c.in); got != c.want {
				t.Fatalf("clampRetention(%d) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}
