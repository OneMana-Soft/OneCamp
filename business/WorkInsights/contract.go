// Package business (WorkInsights) is Work Insights
// (design/projects-vs-asana-jira/PROGRAM.md §4.2 #99): flow metrics from the
// Work Change Log, rolled up daily, queried through the Query Spec's filters.
//
// B9-a defines the metrics, and the report already answers them:
// business/Project insights.go measures cycle time, lead time, triage time,
// age and time in status from each task's status history, as helpers.Spread
// percentiles sliced by project, person, priority and type. When Series is
// built (daily rollups over a Query Spec), it measures with those
// definitions and spreads, never a second copy. Until then it answers
// ErrNotBuilt, and nothing calls Insights.
package business

import (
	"context"
	"errors"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
)

// Metric is what is measured.
type Metric string

const (
	CycleTime   Metric = "cycle_time"
	LeadTime    Metric = "lead_time"
	TriageTime  Metric = "triage_time"
	Throughput  Metric = "throughput"
	Age         Metric = "age"
	ScopeChange Metric = "scope_change"
	Flow        Metric = "flow"
)

// Point is a metric's value on a day, with its percentiles where it has them.
type Point struct {
	Day   time.Time          `json:"day"`
	Value float64            `json:"value"`
	P     map[string]float64 `json:"p,omitempty"`
}

// Measures answers metrics over the work a spec selects.
type Measures interface {
	Series(ctx context.Context, who accessBusiness.Subject, m Metric, over querySpec.Spec, from, to time.Time) ([]Point, error)
}

// ErrNotBuilt is Series before its daily rollups are built.
var ErrNotBuilt = errors.New("insights: that isn't built yet")

// Insights is the measures everything asks.
var Insights Measures = noInsights{}

type noInsights struct{}

func (noInsights) Series(context.Context, accessBusiness.Subject, Metric, querySpec.Spec, time.Time, time.Time) ([]Point, error) {
	return nil, ErrNotBuilt
}
