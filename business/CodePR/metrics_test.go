package codepr

import "testing"

func TestAggregate_Empty(t *testing.T) {
	s := Aggregate(nil)
	if s.Total != 0 || s.OpenRate != 0 || s.VerifyRate != 0 || s.MergeRate != 0 {
		t.Fatalf("empty aggregate must be all-zero, no NaN: %+v", s)
	}
	if Grade(s, 5) != GradeUnproven {
		t.Fatal("empty scorecard must grade unproven")
	}
}

func okSignal() RunSignal {
	return RunSignal{Status: StatusOK, HasPR: true, AllPassed: true, HadTests: true, InScope: true}
}

func TestAggregate_AllSuccessHealthy(t *testing.T) {
	signals := make([]RunSignal, 10)
	for i := range signals {
		s := okSignal()
		s.Outcome = OutcomeMerged
		signals[i] = s
	}
	sc := Aggregate(signals)
	if sc.Opened != 10 || sc.Verified != 10 || sc.InScope != 10 {
		t.Fatalf("counts wrong: %+v", sc)
	}
	if sc.OpenRate != 1 || sc.VerifyRate != 1 || sc.InScopeRate != 1 || sc.MergeRate != 1 {
		t.Fatalf("rates should be 1: %+v", sc)
	}
	if Grade(sc, 5) != GradeHealthy {
		t.Fatalf("all-success should be healthy, got %s", Grade(sc, 5))
	}
}

func TestAggregate_Mixed(t *testing.T) {
	signals := []RunSignal{
		okSignal(),              // opened, verified, in-scope
		{Status: StatusNoGreen}, // no green, no PR
		{Status: StatusBlocked}, // needs human
		{Status: StatusError},   // failed
		{Status: StatusOK, HasPR: true, AllPassed: false, Draft: true, InScope: true}, // draft, unverified
	}
	sc := Aggregate(signals)
	if sc.Total != 5 || sc.Opened != 2 || sc.NoGreen != 1 || sc.Blocked != 1 || sc.Failed != 1 || sc.Draft != 1 {
		t.Fatalf("mixed counts wrong: %+v", sc)
	}
	// Verified 1 of 2 opened.
	if sc.VerifyRate != 0.5 {
		t.Fatalf("verify rate should be 0.5: %v", sc.VerifyRate)
	}
	if sc.OpenRate != 0.4 {
		t.Fatalf("open rate should be 0.4: %v", sc.OpenRate)
	}
}

func TestAggregate_MergeRateOnlyOverKnownOutcomes(t *testing.T) {
	signals := []RunSignal{
		{Status: StatusOK, HasPR: true, Outcome: OutcomeMerged},
		{Status: StatusOK, HasPR: true, Outcome: OutcomeMergedEdited},
		{Status: StatusOK, HasPR: true, Outcome: OutcomeClosed},
		{Status: StatusOK, HasPR: true, Outcome: OutcomeOpen},    // not resolved → not counted
		{Status: StatusOK, HasPR: true, Outcome: OutcomeUnknown}, // no webhook → not counted
	}
	sc := Aggregate(signals)
	if sc.OutcomeKnown != 3 {
		t.Fatalf("only merged/edited/closed are known outcomes, got %d", sc.OutcomeKnown)
	}
	// 2 merged of 3 known = 0.666…
	if sc.MergeRate < 0.66 || sc.MergeRate > 0.67 {
		t.Fatalf("merge rate should be ~0.667 over known outcomes: %v", sc.MergeRate)
	}
}

func TestGrade_MinSampleGuard(t *testing.T) {
	// A few perfect runs are still "unproven" below the minimum sample.
	signals := []RunSignal{okSignal(), okSignal()}
	sc := Aggregate(signals)
	if Grade(sc, 10) != GradeUnproven {
		t.Fatalf("below min sample must be unproven, got %s", Grade(sc, 10))
	}
	// minSample <1 is coerced to 1.
	if Grade(Aggregate([]RunSignal{okSignal()}), 0) == GradeUnproven {
		t.Fatal("one sample with minSample coerced to 1 should be gradeable")
	}
}

func TestGrade_NeedsAttention(t *testing.T) {
	// Enough runs but low verify rate → needs attention.
	signals := make([]RunSignal, 10)
	for i := range signals {
		s := okSignal()
		if i < 5 {
			s.AllPassed = false // half unverified
		}
		signals[i] = s
	}
	sc := Aggregate(signals)
	if Grade(sc, 5) != GradeNeedsAttention {
		t.Fatalf("low verify rate should need attention, got %s (%+v)", Grade(sc, 5), sc)
	}
}

func TestGrade_HealthyButPoorMergeRate(t *testing.T) {
	// Opens verified in-scope PRs, but once merge outcomes are known they mostly
	// get rejected → not healthy.
	signals := make([]RunSignal, 10)
	for i := range signals {
		s := okSignal()
		if i < 8 {
			s.Outcome = OutcomeClosed // rejected
		} else {
			s.Outcome = OutcomeMerged
		}
		signals[i] = s
	}
	sc := Aggregate(signals)
	if Grade(sc, 5) != GradeNeedsAttention {
		t.Fatalf("poor merge rate should downgrade from healthy, got %s (merge=%v)", Grade(sc, 5), sc.MergeRate)
	}
}

func TestSignalFromRun(t *testing.T) {
	r := SignalFromRun(StatusOK, "https://github.com/o/r/pull/1", true, true, true, false, OutcomeMerged)
	if !r.HasPR || !r.AllPassed || !r.InScope || r.Outcome != OutcomeMerged {
		t.Fatalf("mapping wrong: %+v", r)
	}
	// No PR url → HasPR false.
	if SignalFromRun(StatusNoGreen, "", false, false, false, false, OutcomeUnknown).HasPR {
		t.Fatal("empty PR url must yield HasPR=false")
	}
}
