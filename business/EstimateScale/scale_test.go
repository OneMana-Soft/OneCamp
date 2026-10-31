package business

import (
	"slices"
	"testing"
)

func TestTheSizesAreLinearsFourScales(t *testing.T) {
	for _, c := range []struct {
		scale Scale
		want  []float64
	}{
		{Scale{Kind: "exponential"}, []float64{1, 2, 4, 8, 16}},
		{Scale{Kind: "exponential", Extended: true}, []float64{1, 2, 4, 8, 16, 32, 64}},
		{Scale{Kind: "fibonacci", AllowZero: true}, []float64{0, 1, 2, 3, 5, 8}},
		{Scale{Kind: "fibonacci", Extended: true}, []float64{1, 2, 3, 5, 8, 13, 21}},
		{Scale{Kind: "linear", Extended: true, AllowZero: true}, []float64{0, 1, 2, 3, 4, 5, 6, 7}},
		{Scale{Kind: "tshirt", Extended: true}, []float64{1, 2, 3, 5, 8, 13, 21}},
		{Scale{Kind: "days"}, nil},
	} {
		if got := Sizes(&c.scale); !slices.Equal(got, c.want) {
			t.Errorf("%+v: sizes %v, want %v", c.scale, got, c.want)
		}
	}
}

func TestATShirtSizeIsNamedAndTheOthersAreTheirNumbers(t *testing.T) {
	shirt := &Scale{Kind: "tshirt", Extended: true, AllowZero: true}
	v := ViewOf(shirt)
	if want := []string{"0", "XS", "S", "M", "L", "XL", "XXL", "XXXL"}; !slices.Equal(v.Labels, want) {
		t.Fatalf("labels %v, want %v", v.Labels, want)
	}
	// Points off the scale (an import's, or from before the scale changed) are their number.
	if got := Label(shirt, 4); got != "4" {
		t.Fatalf("4 on the T-shirt scale is %q, want its number", got)
	}
	if got := Label(&Scale{Kind: "fibonacci"}, 13); got != "13" {
		t.Fatalf("13 is %q", got)
	}
	if ViewOf(nil) != nil {
		t.Fatal("hours have no view")
	}
}

func TestOnlyASizeIsAllowed(t *testing.T) {
	fib := &Scale{Kind: "fibonacci"}
	for p, want := range map[float64]bool{1: true, 8: true, 4: false, 13: false, 0: false, 2.5: false} {
		if Allows(fib, p) != want {
			t.Errorf("fibonacci allows %v = %v, want %v", p, !want, want)
		}
	}
	if !Allows(&Scale{Kind: "fibonacci", Extended: true, AllowZero: true}, 0) || !Allows(&Scale{Kind: "fibonacci", Extended: true}, 21) {
		t.Fatal("zero and 21 are sizes once allowed and extended")
	}
}

func TestATaskWithNoPointsCountsAsTheScaleSays(t *testing.T) {
	three := 3.0
	counting, notCounting := &Scale{Kind: "linear", CountUnestimated: true}, &Scale{Kind: "linear"}
	if Counted(counting, nil) != 1 || Counted(notCounting, nil) != 0 {
		t.Fatal("an unestimated task counts as one only when the scale counts them")
	}
	if Counted(counting, &three) != 3 || Counted(notCounting, &three) != 3 {
		t.Fatal("a task's own points count as they are")
	}
}

// Counted for many tasks at once, from a graph's counts and sums: the points
// there are, and each task with none as the scale counts it.
func TestCountedAllIsCountedForEachTask(t *testing.T) {
	counts := &Scale{Kind: "fibonacci", CountUnestimated: true}
	if got := CountedAll(counts, 5, 2, 8); got != 11 {
		t.Errorf("8 points and three tasks with none counted as one each: %v", got)
	}
	if got := CountedAll(&Scale{Kind: "fibonacci"}, 5, 2, 8); got != 8 {
		t.Errorf("tasks with none counted as nothing: %v", got)
	}
	if got := CountedAll(counts, 0, 0, 0); got != 0 {
		t.Errorf("no tasks: %v", got)
	}
}
