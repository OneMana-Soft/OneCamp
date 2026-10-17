package helpers

import (
	"slices"
	"testing"
	"time"
)

func TestCompareTimesPutsMissingLast(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	for _, newest := range []bool{false, true} {
		list := []*time.Time{nil, &t2, &t1, nil}
		slices.SortStableFunc(list, func(a, b *time.Time) int { return CompareTimes(a, b, newest) })
		first, second := &t1, &t2
		if newest {
			first, second = &t2, &t1
		}
		if list[0] != first || list[1] != second || list[2] != nil || list[3] != nil {
			t.Errorf("newestFirst=%v: got %v", newest, list)
		}
	}
}
