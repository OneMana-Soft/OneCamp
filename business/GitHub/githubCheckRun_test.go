package business

import "testing"

func TestAggregateCheckRuns(t *testing.T) {
	t.Run("all success", func(t *testing.T) {
		agg := aggregateCheckRuns([]githubCheckRun{
			{Name: "lint", Status: "completed", Conclusion: "success"},
			{Name: "test", Status: "completed", Conclusion: "success"},
			{Name: "docs", Status: "completed", Conclusion: "skipped"},
		})
		if !agg.AllComplete || agg.Conclusion != "success" || agg.Total != 3 || agg.Passed != 3 || agg.Failed != 0 {
			t.Fatalf("unexpected aggregate: %+v", agg)
		}
	})

	t.Run("one still running → not complete", func(t *testing.T) {
		agg := aggregateCheckRuns([]githubCheckRun{
			{Name: "lint", Status: "completed", Conclusion: "success"},
			{Name: "test", Status: "in_progress"},
		})
		if agg.AllComplete {
			t.Fatalf("should not be complete while a check is in_progress: %+v", agg)
		}
	})

	t.Run("a failure makes the whole thing fail", func(t *testing.T) {
		agg := aggregateCheckRuns([]githubCheckRun{
			{Name: "lint", Status: "completed", Conclusion: "success"},
			{Name: "test", Status: "completed", Conclusion: "failure"},
			{Name: "build", Status: "completed", Conclusion: "timed_out"},
		})
		if !agg.AllComplete || agg.Conclusion != "failure" || agg.Passed != 1 || agg.Failed != 2 {
			t.Fatalf("unexpected aggregate: %+v", agg)
		}
	})

	t.Run("empty", func(t *testing.T) {
		agg := aggregateCheckRuns(nil)
		// No runs → vacuously complete, success, zero counts (caller guards on len==0 before dispatch).
		if !agg.AllComplete || agg.Total != 0 {
			t.Fatalf("unexpected aggregate for empty: %+v", agg)
		}
	})
}
