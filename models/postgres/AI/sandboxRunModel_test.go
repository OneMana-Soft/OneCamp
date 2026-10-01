package models

import "testing"

func TestMsToSecondsCeil(t *testing.T) {
	cases := []struct {
		ms   int64
		want int
	}{
		{0, 0},
		{-5, 0},
		{1, 1},    // any positive fraction rounds up
		{999, 1},  // still under a second → 1
		{1000, 1}, // exactly one second
		{1001, 2}, // just over → 2
		{2500, 3}, // 2.5s → 3 (never under-count)
		{60000, 60},
	}
	for _, c := range cases {
		if got := msToSecondsCeil(c.ms); got != c.want {
			t.Errorf("msToSecondsCeil(%d) = %d, want %d", c.ms, got, c.want)
		}
	}
}
