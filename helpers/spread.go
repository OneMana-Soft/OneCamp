package helpers

import "sort"

// Spread is how a set of numbers (hours a task took, most often) spreads:
// how many there are, and their 25th, 50th, 85th and 95th percentiles. The
// percentiles Linear's and Jira's charts draw, so a team can say "85% of our
// bugs close within three days". All zero for none.
type Spread struct {
	N   int     `json:"n"`
	P25 float64 `json:"p25"`
	P50 float64 `json:"p50"`
	P85 float64 `json:"p85"`
	P95 float64 `json:"p95"`
}

// SpreadOf is values' Spread. It sorts values in place.
func SpreadOf(values []float64) Spread {
	if len(values) == 0 {
		return Spread{}
	}
	sort.Float64s(values)
	return Spread{N: len(values), P25: Percentile(values, 0.25), P50: Percentile(values, 0.5), P85: Percentile(values, 0.85), P95: Percentile(values, 0.95)}
}

// Percentile is the fraction p (0 to 1) of sorted values, between the two
// values either side of it as Postgres's percentile_cont puts it: the median
// of 1, 2, 3, 4 is 2.5. Zero for none.
func Percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	at := p * float64(len(sorted)-1)
	lo := int(at)
	if lo >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	return sorted[lo] + (sorted[lo+1]-sorted[lo])*(at-float64(lo))
}
