package helpers

import (
	"math"
	"testing"
)

// A percentile sits between the two values either side of it, as Postgres's
// percentile_cont puts it.
func TestPercentileInterpolatesAsPostgresDoes(t *testing.T) {
	ten := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	// SELECT percentile_cont(p) WITHIN GROUP (ORDER BY v) FROM generate_series(1, 10) v
	for p, want := range map[float64]float64{0: 1, 0.25: 3.25, 0.5: 5.5, 0.85: 8.65, 0.95: 9.55, 1: 10} {
		if got := Percentile(ten, p); math.Abs(got-want) > 1e-9 {
			t.Errorf("p%v of 1..10 = %v, want %v", p*100, got, want)
		}
	}
	if got := Percentile([]float64{7}, 0.85); got != 7 {
		t.Errorf("one value is every percentile: %v", got)
	}
	if got := Percentile(nil, 0.5); got != 0 {
		t.Errorf("none: %v", got)
	}
}

// A spread counts its values and gives their 25th, 50th, 85th and 95th
// percentiles, whatever order they came in.
func TestSpreadOf(t *testing.T) {
	s := SpreadOf([]float64{10, 1, 9, 2, 8, 3, 7, 4, 6, 5})
	if s.N != 10 || s.P25 != 3.25 || s.P50 != 5.5 || math.Abs(s.P85-8.65) > 1e-9 || math.Abs(s.P95-9.55) > 1e-9 {
		t.Errorf("%+v", s)
	}
	if s := SpreadOf(nil); s != (Spread{}) {
		t.Errorf("none spreads to nothing: %+v", s)
	}
}
